package auth

import (
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
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Handler holds dependencies for auth endpoints.
type Handler struct {
	queries *db.Queries
	cfg     *config.Config
}

// NewHandler creates a new auth Handler.
func NewHandler(queries *db.Queries, cfg *config.Config) *Handler {
	return &Handler{queries: queries, cfg: cfg}
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
		ID:             u.ID,
		Username:       u.Username,
		Email:          u.Email,
		IsPublic:       u.IsPublic.Bool,
		BooksReadCount: u.BooksReadCount.Int32,
		FollowersCount: u.FollowersCount.Int32,
		FollowingCount: u.FollowingCount.Int32,
		CreatedAt:      u.CreatedAt.Time,
	}
	if u.Name.Valid {
		resp.Name = &u.Name.String
	}
	if u.AvatarUrl.Valid {
		resp.AvatarURL = &u.AvatarUrl.String
	}
	if u.Bio.Valid {
		resp.Bio = &u.Bio.String
	}
	if u.Pronouns.Valid {
		resp.Pronouns = &u.Pronouns.String
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
