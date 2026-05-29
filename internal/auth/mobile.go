// Mobile-specific auth handlers. They share business logic with the existing
// Handler methods (db queries, password hashing, OTP generation) but return a
// flat {token, user} response shape suitable for native clients, never touch
// cookies, and use a longer JWT expiry (cfg.AccessTokenExpiryMobile, default
// 30d) so the apps don't need a refresh dance every hour.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/token"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

const otpExpiry = 10 * time.Minute

// MobileHandler holds dependencies for /api/mobile/auth/* endpoints. It reuses
// the same *Handler internals via embedding so we don't duplicate logic.
type MobileHandler struct {
	*Handler
	mailer service.Mailer
}

// NewMobileHandler wires a MobileHandler on top of an existing auth.Handler.
// Pass service.NoopMailer{} for the mailer if no email provider is configured;
// the OTP endpoint will still 200 but the email will not actually go out.
func NewMobileHandler(h *Handler, mailer service.Mailer) *MobileHandler {
	if mailer == nil {
		mailer = service.NoopMailer{}
	}
	return &MobileHandler{Handler: h, mailer: mailer}
}

// mobileUser is the trimmed user payload returned by mobile auth endpoints.
type mobileUser struct {
	ID                  string  `json:"id"`
	Username            string  `json:"username"`
	Email               string  `json:"email"`
	Name                *string `json:"name,omitempty"`
	AvatarURL           *string `json:"avatar_url,omitempty"`
	Level               int32   `json:"level"`
	XP                  int32   `json:"xp"`
	OnboardingCompleted bool    `json:"onboarding_completed"`
}

func toMobileUser(u db.User) mobileUser {
	mu := mobileUser{
		ID:                  u.ID.String(),
		Username:            u.Username,
		Email:               u.Email,
		Level:               u.Level.Int32,
		XP:                  u.TotalXp.Int32,
		OnboardingCompleted: u.OnboardingCompleted,
	}
	if u.Name.Valid {
		v := u.Name.String
		mu.Name = &v
	}
	if u.AvatarUrl.Valid {
		v := u.AvatarUrl.String
		mu.AvatarURL = &v
	}
	return mu
}

// issueMobileAccessToken signs a long-lived (cfg.AccessTokenExpiryMobile) JWT.
// No refresh-token row is persisted — mobile clients call /refresh to re-mint.
func (m *MobileHandler) issueMobileAccessToken(userID uuid.UUID) (string, error) {
	return token.Generate(userID, m.cfg.JWTSecret, m.cfg.AccessTokenExpiryMobile)
}

// MobileLogin handles POST /api/mobile/auth/login.
func (m *MobileHandler) MobileLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid JSON body")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || req.Password == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Email and password are required")
		return
	}

	user, err := m.queries.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Invalid credentials")
			return
		}
		slog.Error("mobile login: get user", "error", err)
		types.WriteInternalError(w)
		return
	}
	if !user.PasswordHash.Valid || !CheckPassword(req.Password, user.PasswordHash.String) {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Invalid credentials")
		return
	}

	tok, err := m.issueMobileAccessToken(user.ID)
	if err != nil {
		slog.Error("mobile login: issue token", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() { _ = m.queries.UpdateUserLastActive(context.Background(), user.ID) }()

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"token": tok,
		"user":  toMobileUser(user),
	})
}

// MobileRegister handles POST /api/mobile/auth/register.
// Auto-generates a username from the email so mobile sign-up is single-step.
func (m *MobileHandler) MobileRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid JSON body")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if !emailRegex.MatchString(req.Email) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid email address")
		return
	}
	if err := validatePassword(req.Password); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, err.Error())
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		slog.Error("mobile register: hash", "error", err)
		types.WriteInternalError(w)
		return
	}

	username, err := m.generateUniqueUsername(r.Context(), req.Email)
	if err != nil {
		slog.Error("mobile register: username", "error", err)
		types.WriteInternalError(w)
		return
	}

	user, err := m.queries.CreateUser(r.Context(), db.CreateUserParams{
		Username:       username,
		Email:          req.Email,
		PasswordHash:   pgtype.Text{String: hash, Valid: true},
		Name:           pgtype.Text{},
		FavoriteGenres: []string{},
	})
	if err != nil {
		if isUniqueViolation(err) {
			types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Email already in use")
			return
		}
		slog.Error("mobile register: create user", "error", err)
		types.WriteInternalError(w)
		return
	}

	tok, err := m.issueMobileAccessToken(user.ID)
	if err != nil {
		slog.Error("mobile register: issue token", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusCreated, map[string]any{
		"token": tok,
		"user":  toMobileUser(user),
	})
}

// MobileSendOTP handles POST /api/mobile/auth/otp/send.
// Stores a hashed OTP and asks the mailer to deliver it. The plaintext code
// is never returned to the client. The response is always 200 to avoid
// leaking whether an account exists.
func (m *MobileHandler) MobileSendOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid JSON body")
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if !emailRegex.MatchString(email) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Valid email is required")
		return
	}

	resp := map[string]any{
		"message":            "If an account exists for that email, a code has been sent.",
		"expires_in_seconds": int(otpExpiry.Seconds()),
	}

	user, err := m.queries.GetUserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Mirror response so we don't leak account existence.
			types.WriteJSON(w, http.StatusOK, resp)
			return
		}
		slog.Error("mobile send otp: get user", "error", err)
		types.WriteInternalError(w)
		return
	}

	code, err := generateOTPCode()
	if err != nil {
		slog.Error("mobile send otp: generate code", "error", err)
		types.WriteInternalError(w)
		return
	}

	_ = m.queries.DeleteOTPByEmail(r.Context(), email)
	if _, err := m.queries.CreateOTP(r.Context(), db.CreateOTPParams{
		Email:       email,
		CodeHash:    hashToken(code),
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(otpExpiry), Valid: true},
		MaxAttempts: 5,
	}); err != nil {
		slog.Error("mobile send otp: create record", "error", err)
		types.WriteInternalError(w)
		return
	}

	if err := m.mailer.SendOTP(r.Context(), user.Email, code); err != nil {
		// Don't fail the request — the OTP is stored. Mobile client should
		// see the generic success message and prompt user to check spam.
		slog.Warn("mobile send otp: mailer", "error", err, "email", user.Email)
	}

	types.WriteJSON(w, http.StatusOK, resp)
}

// MobileVerifyOTP handles POST /api/mobile/auth/otp/verify.
func (m *MobileHandler) MobileVerifyOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid JSON body")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Code = strings.TrimSpace(req.Code)
	if req.Email == "" || req.Code == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Email and code are required")
		return
	}

	otp, err := m.queries.GetOTPByEmail(r.Context(), req.Email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Invalid or expired code")
			return
		}
		slog.Error("mobile verify otp: lookup", "error", err)
		types.WriteInternalError(w)
		return
	}
	if otp.Attempts >= otp.MaxAttempts {
		types.WriteError(w, http.StatusTooManyRequests, types.ErrCodeRateLimited, "Too many failed attempts. Please request a new code.")
		return
	}
	if hashToken(req.Code) != otp.CodeHash {
		_ = m.queries.IncrementOTPAttempts(r.Context(), otp.ID)
		remaining := otp.MaxAttempts - otp.Attempts - 1
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized,
			fmt.Sprintf("Invalid code. %d attempt(s) remaining.", remaining))
		return
	}
	_ = m.queries.MarkOTPUsed(r.Context(), otp.ID)

	user, err := m.queries.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		slog.Error("mobile verify otp: get user", "error", err)
		types.WriteInternalError(w)
		return
	}

	tok, err := m.issueMobileAccessToken(user.ID)
	if err != nil {
		slog.Error("mobile verify otp: issue token", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() { _ = m.queries.UpdateUserLastActive(context.Background(), user.ID) }()

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"token": tok,
		"user":  toMobileUser(user),
	})
}

// MobileGoogleAuth handles POST /api/mobile/auth/google.
// Verifies the Google ID token directly against Google's tokeninfo endpoint
// (stdlib HTTP, no extra deps) and either logs in the matching user or
// auto-creates an account from the verified email.
func (m *MobileHandler) MobileGoogleAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid JSON body")
		return
	}
	if strings.TrimSpace(req.IDToken) == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "id_token is required")
		return
	}

	claims, err := verifyGoogleIDToken(r.Context(), req.IDToken)
	if err != nil {
		slog.Warn("mobile google auth: verify", "error", err)
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeInvalidToken, "Google id_token verification failed")
		return
	}
	email := strings.TrimSpace(strings.ToLower(claims.Email))
	if email == "" || !claims.EmailVerified {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Google account has no verified email")
		return
	}

	isNew := false
	user, err := m.queries.GetUserByEmail(r.Context(), email)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("mobile google auth: get user", "error", err)
			types.WriteInternalError(w)
			return
		}
		isNew = true
		username, genErr := m.generateUniqueUsername(r.Context(), email)
		if genErr != nil {
			slog.Error("mobile google auth: username", "error", genErr)
			types.WriteInternalError(w)
			return
		}
		name := strings.TrimSpace(claims.Name)
		if name == "" {
			name = username
		}
		user, err = m.queries.CreateUser(r.Context(), db.CreateUserParams{
			Username:       username,
			Email:          email,
			PasswordHash:   pgtype.Text{},
			Name:           pgtype.Text{String: name, Valid: true},
			FavoriteGenres: []string{},
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Race: another concurrent request beat us to the insert.
				user, err = m.queries.GetUserByEmail(r.Context(), email)
				if err != nil {
					slog.Error("mobile google auth: retry get", "error", err)
					types.WriteInternalError(w)
					return
				}
				isNew = false
			} else {
				slog.Error("mobile google auth: create user", "error", err)
				types.WriteInternalError(w)
				return
			}
		}
	}

	tok, err := m.issueMobileAccessToken(user.ID)
	if err != nil {
		slog.Error("mobile google auth: issue token", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() { _ = m.queries.UpdateUserLastActive(context.Background(), user.ID) }()

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"token":       tok,
		"user":        toMobileUser(user),
		"is_new_user": isNew,
	})
}

// MobileRefresh handles POST /api/mobile/auth/refresh.
// Accepts the current access token via the Authorization header and re-mints
// a new long-lived token if the current one is still valid. Mobile clients
// should refresh before expiry rather than after.
func (m *MobileHandler) MobileRefresh(w http.ResponseWriter, r *http.Request) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Authorization header required")
		return
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Authorization header must be: Bearer <token>")
		return
	}
	claims, err := token.Parse(strings.TrimSpace(parts[1]), m.cfg.JWTSecret)
	if err != nil {
		if errors.Is(err, token.ErrTokenExpired) {
			types.WriteError(w, http.StatusUnauthorized, types.ErrCodeExpiredToken, "Token has expired; please log in again")
			return
		}
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeInvalidToken, "Invalid access token")
		return
	}
	tok, err := m.issueMobileAccessToken(claims.UserID)
	if err != nil {
		slog.Error("mobile refresh: issue token", "error", err)
		types.WriteInternalError(w)
		return
	}
	types.WriteJSON(w, http.StatusOK, map[string]any{"token": tok})
}

// MobileUpdateMe handles PATCH /api/mobile/users/me.
// Lets authenticated mobile users set or change their username. This is the
// critical path for the onboarding flow because MobileRegister auto-generates
// a username, so ChooseUsernameView must call this endpoint — not the web
// PUT /api/v1/users/{slug} — to avoid path-coupling to the generated slug.
func (m *MobileHandler) MobileUpdateMe(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Not authenticated")
		return
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Invalid user ID in token")
		return
	}

	// name is an optional pointer so we can tell "field omitted" (leave name
	// unchanged) apart from "explicit empty string". The onboarding flow sends
	// username + display name together in one PATCH.
	var req struct {
		Username string  `json:"username"`
		Name     *string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid JSON body")
		return
	}
	username := strings.TrimSpace(strings.ToLower(req.Username))
	if len(username) < 3 || len(username) > 50 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Username must be 3–50 characters")
		return
	}
	if !usernameRe.MatchString(username) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Username may only contain letters, numbers, _ and -")
		return
	}

	// Uniqueness check: if someone already owns this username and it's not us,
	// return a 409 with a user-facing reason.
	existing, err := m.queries.GetUserByUsername(r.Context(), username)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("mobile update me: check username", "error", err)
		types.WriteInternalError(w)
		return
	}
	if err == nil && existing.ID != userID {
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Username already taken")
		return
	}

	updated, err := m.queries.UpdateUsername(r.Context(), db.UpdateUsernameParams{
		ID:       userID,
		Username: username,
	})
	if err != nil {
		if isUniqueViolation(err) {
			types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Username already taken")
			return
		}
		slog.Error("mobile update me: update username", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Persist the display name when provided. UpdateUser uses COALESCE, so the
	// other (NULL) fields are left untouched.
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if len([]rune(name)) > 100 {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Display name must be 100 characters or fewer")
			return
		}
		updated, err = m.queries.UpdateUser(r.Context(), db.UpdateUserParams{
			ID:   userID,
			Name: pgtype.Text{String: name, Valid: true},
		})
		if err != nil {
			slog.Error("mobile update me: update name", "error", err)
			types.WriteInternalError(w)
			return
		}
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"user": toMobileUser(updated),
	})
}

// googleClaims is the subset of fields we use from Google's tokeninfo response.
type googleClaims struct {
	Audience      string `json:"aud"`
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"-"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// verifyGoogleIDToken calls Google's tokeninfo endpoint. A 200 response means
// the id_token's signature, expiry, issuer, and audience were all validated
// by Google's edge — we just enforce that an email is present and verified.
//
// Note: tokeninfo returns email_verified as either bool or string ("true").
// We unmarshal into a raw map to handle both.
func verifyGoogleIDToken(ctx context.Context, idToken string) (*googleClaims, error) {
	endpoint := "https://oauth2.googleapis.com/tokeninfo?" + url.Values{"id_token": {idToken}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call tokeninfo: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tokeninfo status %d: %s", resp.StatusCode, string(body))
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode tokeninfo: %w", err)
	}

	out := &googleClaims{}
	if v, ok := raw["aud"].(string); ok {
		out.Audience = v
	}
	if v, ok := raw["sub"].(string); ok {
		out.Subject = v
	}
	if v, ok := raw["email"].(string); ok {
		out.Email = v
	}
	if v, ok := raw["name"].(string); ok {
		out.Name = v
	}
	if v, ok := raw["picture"].(string); ok {
		out.Picture = v
	}
	switch v := raw["email_verified"].(type) {
	case bool:
		out.EmailVerified = v
	case string:
		out.EmailVerified = strings.EqualFold(v, "true")
	}
	return out, nil
}
