package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// appleJWKSURL serves Apple's signing keys. Overridable in tests.
var appleJWKSURL = "https://appleid.apple.com/auth/keys"

const appleIssuer = "https://appleid.apple.com"

type appleJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type appleClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
}

// fetchAppleKey downloads Apple's JWKS and returns the RSA public key with the
// given kid. Keys rotate rarely; a fetch per login is fine at current scale.
// ponytail: no JWKS cache — add one if apple logins ever show up in latency traces.
func fetchAppleKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, appleJWKSURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build jwks request: %w", err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}

	var payload struct {
		Keys []appleJWK `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}

	for _, k := range payload.Keys {
		if k.Kid != kid || k.Kty != "RSA" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("decode modulus: %w", err)
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("decode exponent: %w", err)
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: int(new(big.Int).SetBytes(eBytes).Int64()),
		}, nil
	}
	return nil, fmt.Errorf("no apple key with kid %q", kid)
}

// verifyAppleIdentityToken validates the identity token's signature against
// Apple's JWKS, its issuer, expiry, and audience (must be in the configured
// allowlist so tokens minted for other apps can't authenticate here).
func (m *MobileHandler) verifyAppleIdentityToken(ctx context.Context, tokenStr string) (*appleClaims, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("identity token missing kid header")
		}
		return fetchAppleKey(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(appleIssuer), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("parse identity token: %w", err)
	}

	aud, _ := claims.GetAudience()
	audOK := false
	for _, a := range aud {
		if slices.Contains(m.cfg.AllowedAppleAudiences, a) {
			audOK = true
			break
		}
	}
	if !audOK {
		slog.Warn("apple_auth_audience_mismatch", "got", aud)
		return nil, fmt.Errorf("apple token audience mismatch: got %v", aud)
	}

	out := &appleClaims{}
	if v, ok := claims["sub"].(string); ok {
		out.Subject = v
	}
	if v, ok := claims["email"].(string); ok {
		out.Email = v
	}
	// Apple sends email_verified as bool or string "true".
	switch v := claims["email_verified"].(type) {
	case bool:
		out.EmailVerified = v
	case string:
		out.EmailVerified = strings.EqualFold(v, "true")
	}
	return out, nil
}

// MobileAppleAuth handles POST /api/mobile/auth/apple.
//
// Matching is keyed on Apple's `sub` (the stable per-app user identifier),
// NOT email. Apple only includes the email claim on a user's FIRST
// authorization; on every later sign-in it may be absent. Upserting by email
// would therefore fail the second login or create duplicate accounts. Email,
// when present, is only used to link a first-time Apple sign-in to an account
// that already exists (e.g. created via Google or password), or to seed a new
// account. `name` likewise only arrives on the first authorization.
func (m *MobileHandler) MobileAppleAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IdentityToken string `json:"identity_token"`
		Name          string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid JSON body")
		return
	}
	if strings.TrimSpace(req.IdentityToken) == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "identity_token is required")
		return
	}

	claims, err := m.verifyAppleIdentityToken(r.Context(), req.IdentityToken)
	if err != nil {
		slog.Warn("mobile apple auth: verify", "error", err)
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeInvalidToken, "Apple identity token verification failed")
		return
	}
	sub := strings.TrimSpace(claims.Subject)
	if sub == "" {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Apple identity token has no subject")
		return
	}
	email := strings.TrimSpace(strings.ToLower(claims.Email)) // may be empty on repeat sign-ins
	appleID := pgtype.Text{String: sub, Valid: true}

	// 1. Stable path: match the Apple subject we stored on a previous sign-in.
	user, err := m.queries.GetUserByAppleUserID(r.Context(), appleID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("mobile apple auth: get by apple id", "error", err)
		types.WriteInternalError(w)
		return
	}

	isNew := false
	if errors.Is(err, pgx.ErrNoRows) {
		// 2. No Apple subject on file. This should only happen on the first
		// authorization, when Apple supplies the email. Without an email we
		// cannot safely resolve which account this is — fail closed.
		if email == "" {
			slog.Warn("mobile apple auth: unmatched subject with no email", "sub", sub)
			types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized,
				"Could not match Apple sign-in to an account. Sign in with your email once to re-link Apple.")
			return
		}
		user, isNew, err = m.resolveAppleUserByEmail(r.Context(), email, sub, req.Name)
		if err != nil {
			slog.Error("mobile apple auth: resolve by email", "error", err)
			types.WriteInternalError(w)
			return
		}
	}

	tok, refreshTok, err := m.issueMobileSession(r, user.ID)
	if err != nil {
		slog.Error("mobile apple auth: issue token", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() { _ = m.queries.UpdateUserLastActive(context.Background(), user.ID) }()

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"token":       tok,
		"refresh_token": refreshTok,
		"user":        toMobileUser(user),
		"is_new_user": isNew,
	})
}

// resolveAppleUserByEmail handles a first-time Apple authorization: link the
// Apple subject to an existing account with this email, or create a new account
// and store the subject on it. Returns the user and whether it was newly created.
func (m *MobileHandler) resolveAppleUserByEmail(ctx context.Context, email, sub, rawName string) (db.User, bool, error) {
	appleID := pgtype.Text{String: sub, Valid: true}

	existing, err := m.queries.GetUserByEmail(ctx, email)
	if err == nil {
		// Account already exists (Google/password/prior Apple) — link, don't duplicate.
		if linkErr := m.queries.LinkAppleUserID(ctx, db.LinkAppleUserIDParams{ID: existing.ID, AppleUserID: appleID}); linkErr != nil {
			return db.User{}, false, fmt.Errorf("link apple id to existing user: %w", linkErr)
		}
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.User{}, false, fmt.Errorf("get user by email: %w", err)
	}

	username, err := m.generateUniqueUsername(ctx, email)
	if err != nil {
		return db.User{}, false, fmt.Errorf("generate username: %w", err)
	}
	name := strings.TrimSpace(rawName)
	if name == "" {
		name = username
	}
	user, err := m.queries.CreateUser(ctx, db.CreateUserParams{
		Username:       username,
		Email:          email,
		PasswordHash:   pgtype.Text{},
		Name:           pgtype.Text{String: name, Valid: true},
		FavoriteGenres: []string{},
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Race: a concurrent first sign-in created this email. Fetch and link.
			raced, getErr := m.queries.GetUserByEmail(ctx, email)
			if getErr != nil {
				return db.User{}, false, fmt.Errorf("retry get after race: %w", getErr)
			}
			if linkErr := m.queries.LinkAppleUserID(ctx, db.LinkAppleUserIDParams{ID: raced.ID, AppleUserID: appleID}); linkErr != nil {
				return db.User{}, false, fmt.Errorf("link apple id after race: %w", linkErr)
			}
			return raced, false, nil
		}
		return db.User{}, false, fmt.Errorf("create user: %w", err)
	}

	if err := m.queries.LinkAppleUserID(ctx, db.LinkAppleUserIDParams{ID: user.ID, AppleUserID: appleID}); err != nil {
		return db.User{}, false, fmt.Errorf("store apple id on new user: %w", err)
	}
	return user, true, nil
}
