package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/config"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Handler holds dependencies for auth endpoints.
type Handler struct {
	queries *db.Queries
	cfg     *config.Config
	mailer  service.Mailer
}

// NewHandler creates a new auth Handler. Pass service.NoopMailer{} (or nil) for
// the mailer where no email provider is configured; ForgotPassword still 200s
// but no mail goes out.
func NewHandler(queries *db.Queries, cfg *config.Config, mailer service.Mailer) *Handler {
	if mailer == nil {
		mailer = service.NoopMailer{}
	}
	return &Handler{queries: queries, cfg: cfg, mailer: mailer}
}

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// Register handles POST /api/v1/auth/register
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req types.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	// Validate inputs
	req.Username = strings.TrimSpace(strings.ToLower(req.Username))
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Name = strings.TrimSpace(req.Name)

	if errs := validateRegister(req); len(errs) > 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, strings.Join(errs, "; "))
		return
	}

	// Hash password
	hash, err := HashPassword(req.Password)
	if err != nil {
		slog.Error("hash password", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Create user
	user, err := h.queries.CreateUser(r.Context(), db.CreateUserParams{
		Username:       req.Username,
		Email:          req.Email,
		PasswordHash:   pgtype.Text{String: hash, Valid: true},
		Name:           pgtype.Text{String: req.Name, Valid: req.Name != ""},
		FavoriteGenres: []string{},
	})
	if err != nil {
		if isUniqueViolation(err) {
			msg := "Email or username already in use"
			if strings.Contains(err.Error(), "users_username_key") {
				msg = "Username already taken"
			} else if strings.Contains(err.Error(), "users_email_key") {
				msg = "Email already in use"
			}
			types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, msg)
			return
		}
		slog.Error("create user", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Issue tokens
	accessToken, refreshToken, err := h.issueTokens(r, user.ID)
	if err != nil {
		slog.Error("issue tokens", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusCreated, types.AuthResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    int(h.cfg.AccessTokenExpiry.Seconds()),
		User:         toUserResponse(user),
	})
}

// CheckUsername handles GET /api/v1/auth/check-username?username=x.
// Returns {"available": true|false} — no authentication required.
func (h *Handler) CheckUsername(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("username")))

	if username == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "username query param is required")
		return
	}

	if len(username) < 3 || len(username) > 50 || !isValidUsername(username) {
		types.WriteJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"username":  username,
			"reason":    "Username must be 3-50 chars and contain only letters, numbers, underscores, and hyphens",
		})
		return
	}

	_, err := h.queries.GetUserByUsername(r.Context(), username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteJSON(w, http.StatusOK, map[string]any{
				"available": true,
				"username":  username,
			})
			return
		}
		slog.Error("check username", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"available": false,
		"username":  username,
	})
}

// Login handles POST /api/v1/auth/login
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req types.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))

	if req.Email == "" || req.Password == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Email and password are required")
		return
	}

	user, err := h.queries.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Invalid credentials")
			return
		}
		slog.Error("get user by email", "error", err)
		types.WriteInternalError(w)
		return
	}

	if !user.PasswordHash.Valid || !CheckPassword(req.Password, user.PasswordHash.String) {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Invalid credentials")
		return
	}

	accessToken, refreshToken, err := h.issueTokens(r, user.ID)
	if err != nil {
		slog.Error("issue tokens", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Fire and forget — update last active
	go func() {
		_ = h.queries.UpdateUserLastActive(r.Context(), user.ID)
	}()

	types.WriteJSON(w, http.StatusOK, types.AuthResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    int(h.cfg.AccessTokenExpiry.Seconds()),
		User:         toUserResponse(user),
	})
}

// Refresh handles POST /api/v1/auth/refresh
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req types.RefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	if req.RefreshToken == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "refresh_token is required")
		return
	}

	tokenHash := hashToken(req.RefreshToken)

	stored, err := h.queries.GetRefreshToken(r.Context(), tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusUnauthorized, types.ErrCodeInvalidToken, "Refresh token is invalid or expired")
			return
		}
		slog.Error("get refresh token", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Rotate: revoke old, issue new
	if err := h.queries.RevokeRefreshToken(r.Context(), tokenHash); err != nil {
		slog.Error("revoke refresh token", "error", err)
		types.WriteInternalError(w)
		return
	}

	accessToken, newRefreshToken, err := h.issueTokens(r, stored.UserID)
	if err != nil {
		slog.Error("issue tokens on refresh", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, types.TokenResponse{
		AccessToken:  accessToken,
		RefreshToken: newRefreshToken,
		ExpiresIn:    int(h.cfg.AccessTokenExpiry.Seconds()),
	})
}

// forgotPasswordResponse is returned by ForgotPassword for every well-formed
// request — hit or miss, mail sent or not. It carries no reset token, no email,
// and no username: this route is public, and any field that varies with account
// existence turns it into an enumeration oracle.
var forgotPasswordResponse = map[string]any{
	"message": "If an account exists with this email, a password reset link has been sent.",
}

// ForgotPassword handles POST /api/v1/auth/forgot-password.
// Generates a single-use reset token, stores its SHA-256 hash, and returns the
// plaintext token to the caller (the Next.js proxy) so it can email it via
// Resend. Always returns 200 even when the email does not exist, to avoid
// leaking account existence — but only includes a token in the response when
// we actually created one.
func (h *Handler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req types.ForgotPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	email := strings.TrimSpace(strings.ToLower(req.Email))
	if email == "" || !emailRegex.MatchString(email) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Valid email is required")
		return
	}

	user, err := h.queries.GetUserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Identical to the success response below — a differing body (or a
			// `sent` flag) is an account-enumeration oracle on a public route.
			types.WriteJSON(w, http.StatusOK, forgotPasswordResponse)
			return
		}
		slog.Error("forgot password: get user", "error", err)
		types.WriteInternalError(w)
		return
	}

	rawToken, err := generateSecureToken()
	if err != nil {
		slog.Error("forgot password: generate token", "error", err)
		types.WriteInternalError(w)
		return
	}

	tokenHash := hashToken(rawToken)
	expiresAt := pgtype.Timestamptz{Time: time.Now().Add(1 * time.Hour), Valid: true}

	if _, err := h.queries.CreatePasswordResetToken(r.Context(), db.CreatePasswordResetTokenParams{
		UserID:    user.ID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
	}); err != nil {
		slog.Error("forgot password: create token", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Deliver the link ourselves. The plaintext token leaves this process only
	// inside the email — never in the HTTP response. Returning it would hand a
	// live account-takeover capability to any caller of this public route.
	//
	// A mailer failure is logged, not surfaced: the response must not differ
	// between "email exists" and "email doesn't", or it leaks account existence.
	if err := h.mailer.SendPasswordReset(r.Context(), user.Email, rawToken); err != nil {
		slog.Warn("forgot password: mailer", "error", err, "user_id", user.ID)
	}

	types.WriteJSON(w, http.StatusOK, forgotPasswordResponse)
}

// ResetPassword handles POST /api/v1/auth/reset-password.
// Verifies the plaintext token against the stored hash, updates the user's
// password, marks the token used, and revokes all refresh tokens for safety.
func (h *Handler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req types.ResetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Token) == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Token is required")
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, err.Error())
		return
	}

	tokenHash := hashToken(req.Token)
	stored, err := h.queries.GetPasswordResetToken(r.Context(), tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusUnauthorized, types.ErrCodeInvalidToken, "Reset token is invalid or expired")
			return
		}
		slog.Error("reset password: lookup token", "error", err)
		types.WriteInternalError(w)
		return
	}

	newHash, err := HashPassword(req.NewPassword)
	if err != nil {
		slog.Error("reset password: hash", "error", err)
		types.WriteInternalError(w)
		return
	}

	if err := h.queries.UpdateUserPasswordByID(r.Context(), db.UpdateUserPasswordByIDParams{
		ID:           stored.UserID,
		PasswordHash: pgtype.Text{String: newHash, Valid: true},
	}); err != nil {
		slog.Error("reset password: update user", "error", err)
		types.WriteInternalError(w)
		return
	}

	if err := h.queries.MarkPasswordResetTokenUsed(r.Context(), stored.ID); err != nil {
		slog.Warn("reset password: mark used", "error", err)
	}

	// Revoke all refresh tokens — anyone logged in with the old password is kicked.
	if err := h.queries.RevokeAllUserTokens(r.Context(), stored.UserID); err != nil {
		slog.Warn("reset password: revoke tokens", "error", err)
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Password reset successfully"})
}

// Logout handles POST /api/v1/auth/logout
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	var req types.LogoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	if req.RefreshToken == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "refresh_token is required")
		return
	}

	tokenHash := hashToken(req.RefreshToken)
	if err := h.queries.RevokeRefreshToken(r.Context(), tokenHash); err != nil {
		// Non-fatal: token may already be revoked or not exist
		slog.Warn("revoke token on logout", "error", err)
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Logged out successfully"})
}

// GoogleAuth handles POST /api/v1/auth/google
// Server-to-server only. Next.js calls this after verifying Google identity via NextAuth.
// Finds or creates a user by email, then issues Go JWT tokens.
func (h *Handler) GoogleAuth(w http.ResponseWriter, r *http.Request) {
	// Verify shared secret
	if h.cfg.InternalSecret == "" || r.Header.Get("X-Internal-Secret") != h.cfg.InternalSecret {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	var req types.GoogleAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || !emailRegex.MatchString(req.Email) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Valid email is required")
		return
	}

	// Try to find existing user
	user, err := h.queries.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("google auth: get user by email", "error", err)
			types.WriteInternalError(w)
			return
		}

		// New user — auto-create from Google profile
		username, genErr := h.generateUniqueUsername(r.Context(), req.Email)
		if genErr != nil {
			slog.Error("google auth: generate username", "error", genErr)
			types.WriteInternalError(w)
			return
		}

		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = username
		}

		user, err = h.queries.CreateUser(r.Context(), db.CreateUserParams{
			Username:       username,
			Email:          req.Email,
			PasswordHash:   pgtype.Text{}, // no password for OAuth users
			Name:           pgtype.Text{String: name, Valid: true},
			FavoriteGenres: []string{},
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Race condition: another request created the user between our check and insert.
				// Retry the lookup.
				user, err = h.queries.GetUserByEmail(r.Context(), req.Email)
				if err != nil {
					slog.Error("google auth: retry get user", "error", err)
					types.WriteInternalError(w)
					return
				}
			} else {
				slog.Error("google auth: create user", "error", err)
				types.WriteInternalError(w)
				return
			}
		}
	}

	accessToken, refreshToken, err := h.issueTokens(r, user.ID)
	if err != nil {
		slog.Error("google auth: issue tokens", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() {
		_ = h.queries.UpdateUserLastActive(r.Context(), user.ID)
	}()

	types.WriteJSON(w, http.StatusOK, types.AuthResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    int(h.cfg.AccessTokenExpiry.Seconds()),
		User:         toUserResponse(user),
	})
}

var invalidUsernameChars = regexp.MustCompile(`[^a-z0-9_\-]`)
var multiUnderscore = regexp.MustCompile(`_+`)

func usernameFromEmail(email string) string {
	atIdx := strings.Index(email, "@")
	if atIdx <= 0 {
		return "user"
	}
	base := strings.ToLower(email[:atIdx])
	base = invalidUsernameChars.ReplaceAllString(base, "_")
	base = multiUnderscore.ReplaceAllString(base, "_")
	base = strings.Trim(base, "_-")
	if len(base) < 3 {
		base += "_user"
	}
	if len(base) > 45 {
		base = base[:45]
	}
	return base
}

func (h *Handler) generateUniqueUsername(ctx context.Context, email string) (string, error) {
	base := usernameFromEmail(email)
	for i := 0; i <= 99; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s%d", base, i)
		}
		_, err := h.queries.GetUserByUsername(ctx, candidate)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return candidate, nil
			}
			return "", fmt.Errorf("check username availability: %w", err)
		}
	}
	// Last resort: random hex suffix
	b := make([]byte, 4)
	if _, randErr := rand.Read(b); randErr != nil {
		return "", randErr
	}
	suffix := base
	if len(suffix) > 38 {
		suffix = suffix[:38]
	}
	return fmt.Sprintf("%s_%x", suffix, b), nil
}

// issueTokens generates an access token + refresh token and persists the refresh token.
func (h *Handler) issueTokens(r *http.Request, userID uuid.UUID) (accessToken, refreshToken string, err error) {
	accessToken, err = GenerateAccessToken(userID, h.cfg.JWTSecret, h.cfg.AccessTokenExpiry)
	if err != nil {
		return "", "", fmt.Errorf("generate access token: %w", err)
	}

	rawRefresh, err := generateSecureToken()
	if err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}

	tokenHash := hashToken(rawRefresh)
	expiresAt := pgtype.Timestamptz{Time: time.Now().Add(h.cfg.RefreshTokenExpiry), Valid: true}

	deviceInfo := extractDeviceInfo(r)

	if _, err = h.queries.CreateRefreshToken(r.Context(), db.CreateRefreshTokenParams{
		UserID:     userID,
		TokenHash:  tokenHash,
		ExpiresAt:  expiresAt,
		DeviceInfo: deviceInfo,
	}); err != nil {
		return "", "", fmt.Errorf("store refresh token: %w", err)
	}

	return accessToken, rawRefresh, nil
}

// generateSecureToken creates a 32-byte cryptographically random hex string.
func generateSecureToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken SHA-256 hashes a token for safe storage.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// extractDeviceInfo captures basic request metadata as JSON.
func extractDeviceInfo(r *http.Request) []byte {
	info := map[string]string{
		"user_agent": r.Header.Get("User-Agent"),
		"ip":         r.RemoteAddr,
	}
	b, _ := json.Marshal(info)
	return b
}

// toUserResponse converts a db.User to the public API representation.
func toUserResponse(u db.User) types.UserResponse {
	resp := types.UserResponse{
		ID:             u.ID.String(),
		MongoID:        u.ID.String(),
		Username:       u.Username,
		Email:          u.Email,
		IsPublic:       u.IsPublic.Bool,
		BooksReadCount: u.BooksReadCount.Int32,
		TotalPagesRead: u.TotalPagesRead.Int32,
		FavoritesCount: u.FavoritesCount,
		FollowersCount: u.FollowersCount.Int32,
		FollowingCount: u.FollowingCount.Int32,
		FavoriteGenres: u.FavoriteGenres,
		CreatedAt:      u.CreatedAt.Time.Format(time.RFC3339),
		Pronouns:       u.Pronouns,
		Links:          u.Links,
	}
	if resp.Pronouns == nil {
		resp.Pronouns = []string{}
	}
	if resp.Links == nil {
		resp.Links = []string{}
	}
	if resp.FavoriteGenres == nil {
		resp.FavoriteGenres = []string{}
	}
	if u.Name.Valid {
		resp.Name = u.Name.String
	}
	if u.AvatarUrl.Valid {
		resp.AvatarURL = &u.AvatarUrl.String
	}
	if u.Bio.Valid {
		resp.Bio = &u.Bio.String
	}
	if u.Birthday.Valid {
		s := u.Birthday.Time.Format("2006-01-02")
		resp.Birthday = &s
	}
	if u.Gender.Valid {
		resp.Gender = &u.Gender.String
	}
	return resp
}

// validateRegister returns a list of validation error messages.
func validateRegister(req types.RegisterRequest) []string {
	var errs []string

	if len(req.Username) < 3 || len(req.Username) > 50 {
		errs = append(errs, "Username must be 3-50 characters")
	}
	if !isValidUsername(req.Username) {
		errs = append(errs, "Username may only contain letters, numbers, underscores, and hyphens")
	}
	if !emailRegex.MatchString(req.Email) {
		errs = append(errs, "Invalid email address")
	}
	if err := validatePassword(req.Password); err != nil {
		errs = append(errs, err.Error())
	}

	return errs
}

var usernameRegex = regexp.MustCompile(`^[a-z0-9_\-]+$`)

func isValidUsername(s string) bool {
	return usernameRegex.MatchString(s)
}

func validatePassword(p string) error {
	if len(p) < 8 {
		return fmt.Errorf("Password must be at least 8 characters")
	}
	var hasUpper, hasLower, hasDigit bool
	for _, ch := range p {
		switch {
		case unicode.IsUpper(ch):
			hasUpper = true
		case unicode.IsLower(ch):
			hasLower = true
		case unicode.IsDigit(ch):
			hasDigit = true
		}
	}
	if !hasUpper || !hasLower || !hasDigit {
		return fmt.Errorf("Password must contain uppercase, lowercase, and a digit")
	}
	return nil
}

// isUniqueViolation checks if the error is a Postgres unique constraint violation.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "23505")
}
