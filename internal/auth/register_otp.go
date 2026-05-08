package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// registerMetadata is stored as JSONB in otp_codes.metadata during the
// two-step registration flow. Email is already the otp_codes.email column.
type registerMetadata struct {
	Name         string `json:"name"`
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	ReferralCode string `json:"referral_code,omitempty"`
}

// SendRegistrationOTP handles POST /api/v1/auth/register/send-otp.
// Validates registration data, checks uniqueness, hashes the password, then
// stores the data + a hashed OTP in otp_codes. Returns the plaintext code so
// the Next.js proxy can email it via Resend without Resend config in Go.
func (h *Handler) SendRegistrationOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Username     string `json:"username"`
		Email        string `json:"email"`
		Password     string `json:"password"`
		ReferralCode string `json:"referral_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Username = strings.TrimSpace(strings.ToLower(req.Username))
	req.Name = strings.TrimSpace(req.Name)

	// Validate fields
	if req.Name == "" || req.Username == "" || req.Email == "" || req.Password == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "All fields are required")
		return
	}
	if !emailRegex.MatchString(req.Email) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid email address")
		return
	}
	if len(req.Username) < 3 || len(req.Username) > 50 || !isValidUsername(req.Username) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation,
			"Username must be 3-50 characters: letters, numbers, underscores, hyphens only")
		return
	}
	if err := validatePassword(req.Password); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, err.Error())
		return
	}

	// Uniqueness checks
	if _, err := h.queries.GetUserByUsername(r.Context(), req.Username); err == nil {
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Username is already taken")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("register otp: check username", "error", err)
		types.WriteInternalError(w)
		return
	}
	if _, err := h.queries.GetUserByEmail(r.Context(), req.Email); err == nil {
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Email is already registered")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("register otp: check email", "error", err)
		types.WriteInternalError(w)
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		slog.Error("register otp: hash password", "error", err)
		types.WriteInternalError(w)
		return
	}

	code, err := generateOTPCode()
	if err != nil {
		slog.Error("register otp: generate code", "error", err)
		types.WriteInternalError(w)
		return
	}

	meta, err := json.Marshal(registerMetadata{
		Name:         req.Name,
		Username:     req.Username,
		PasswordHash: hash,
		ReferralCode: strings.ToUpper(strings.TrimSpace(req.ReferralCode)),
	})
	if err != nil {
		slog.Error("register otp: marshal metadata", "error", err)
		types.WriteInternalError(w)
		return
	}

	_ = h.queries.DeleteOTPByEmail(r.Context(), req.Email)

	if _, err := h.queries.CreateOTPWithMetadata(r.Context(), db.CreateOTPWithMetadataParams{
		Email:       req.Email,
		CodeHash:    hashToken(code),
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(10 * time.Minute), Valid: true},
		MaxAttempts: 5,
		Metadata:    meta,
	}); err != nil {
		slog.Error("register otp: create record", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"sent":  true,
		"code":  code,
		"email": req.Email,
		"name":  req.Name,
	})
}

// VerifyRegistrationOTP handles POST /api/v1/auth/register/verify-otp.
// Verifies the OTP, creates the user account from stored metadata, and returns
// JWT tokens.
func (h *Handler) VerifyRegistrationOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Code = strings.TrimSpace(req.Code)

	if req.Email == "" || req.Code == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Email and code are required")
		return
	}

	otp, err := h.queries.GetOTPByEmail(r.Context(), req.Email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Invalid or expired code")
			return
		}
		slog.Error("verify registration otp: lookup", "error", err)
		types.WriteInternalError(w)
		return
	}

	if len(otp.Metadata) == 0 {
		// This OTP belongs to a login flow, not registration.
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Invalid or expired code")
		return
	}

	if otp.Attempts >= otp.MaxAttempts {
		types.WriteError(w, http.StatusTooManyRequests, types.ErrCodeInvalidRequest, "Too many failed attempts. Please request a new code.")
		return
	}

	if hashToken(req.Code) != otp.CodeHash {
		_ = h.queries.IncrementOTPAttempts(r.Context(), otp.ID)
		remaining := otp.MaxAttempts - otp.Attempts - 1
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized,
			fmt.Sprintf("Invalid code. %d attempt(s) remaining.", remaining))
		return
	}

	var meta registerMetadata
	if err := json.Unmarshal(otp.Metadata, &meta); err != nil || meta.Username == "" {
		slog.Error("verify registration otp: parse metadata", "error", err)
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Invalid or expired code")
		return
	}

	user, err := h.queries.CreateUser(r.Context(), db.CreateUserParams{
		Username:       meta.Username,
		Email:          req.Email,
		PasswordHash:   pgtype.Text{String: meta.PasswordHash, Valid: true},
		Name:           pgtype.Text{String: meta.Name, Valid: meta.Name != ""},
		FavoriteGenres: []string{},
	})
	if err != nil {
		if isUniqueViolation(err) {
			_ = h.queries.MarkOTPUsed(r.Context(), otp.ID)
			types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Username or email already in use")
			return
		}
		slog.Error("verify registration otp: create user", "error", err)
		types.WriteInternalError(w)
		return
	}

	_ = h.queries.MarkOTPUsed(r.Context(), otp.ID)

	if meta.ReferralCode != "" {
		referrer, refErr := h.queries.GetUserByReferralCode(r.Context(), pgtype.Text{String: meta.ReferralCode, Valid: true})
		if refErr == nil {
			_ = h.queries.SetUserReferredBy(r.Context(), db.SetUserReferredByParams{
				ID:         user.ID,
				ReferredBy: pgtype.UUID{Bytes: referrer.ID, Valid: true},
			})
			referrerID := referrer.ID
			newUserID := user.ID
			go func() {
				refSvc := service.NewReferralService(h.queries, service.NewXPService(h.queries))
				_ = refSvc.ProcessReferralSignup(context.Background(), referrerID, newUserID)
			}()
		}
	}

	accessToken, refreshToken, err := h.issueTokens(r, user.ID)
	if err != nil {
		slog.Error("verify registration otp: issue tokens", "error", err)
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
