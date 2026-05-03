package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"crypto/rand"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SendOTP handles POST /api/v1/auth/otp/send.
// Generates a 6-digit OTP, stores its SHA-256 hash, and returns the plaintext
// code to the Next.js proxy so it can deliver it via Resend. Always returns 200
// to avoid leaking whether an account exists; code is only present when found.
func (h *Handler) SendOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
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
			// Don't reveal whether the account exists.
			types.WriteJSON(w, http.StatusOK, map[string]any{"sent": false})
			return
		}
		slog.Error("send otp: get user", "error", err)
		types.WriteInternalError(w)
		return
	}

	code, err := generateOTPCode()
	if err != nil {
		slog.Error("send otp: generate code", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Delete any pending OTPs for this email before creating a fresh one.
	_ = h.queries.DeleteOTPByEmail(r.Context(), email)

	codeHash := hashToken(code)
	expiresAt := pgtype.Timestamptz{Time: time.Now().Add(10 * time.Minute), Valid: true}

	if _, err := h.queries.CreateOTP(r.Context(), db.CreateOTPParams{
		Email:       email,
		CodeHash:    codeHash,
		ExpiresAt:   expiresAt,
		MaxAttempts: 5,
	}); err != nil {
		slog.Error("send otp: create record", "error", err)
		types.WriteInternalError(w)
		return
	}

	name := ""
	if user.Name.Valid {
		name = user.Name.String
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"sent":     true,
		"code":     code,
		"email":    user.Email,
		"username": user.Username,
		"name":     name,
	})
}

// VerifyOTP handles POST /api/v1/auth/otp/verify.
// Verifies the 6-digit code, marks the OTP used, and returns JWT tokens.
func (h *Handler) VerifyOTP(w http.ResponseWriter, r *http.Request) {
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
		slog.Error("verify otp: lookup", "error", err)
		types.WriteInternalError(w)
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

	_ = h.queries.MarkOTPUsed(r.Context(), otp.ID)

	user, err := h.queries.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		slog.Error("verify otp: get user", "error", err)
		types.WriteInternalError(w)
		return
	}

	accessToken, refreshToken, err := h.issueTokens(r, user.ID)
	if err != nil {
		slog.Error("verify otp: issue tokens", "error", err)
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

// generateOTPCode returns a cryptographically random 6-digit decimal string.
func generateOTPCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
