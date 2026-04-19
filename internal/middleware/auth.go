// Package middleware provides HTTP middleware for the PaperBoxd API.
package middleware

import (
	"net/http"
	"strings"

	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/token"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// Authenticate validates a JWT Bearer token and injects the user_id into
// the request context. Downstream handlers retrieve it via reqctx.GetUserID.
func Authenticate(jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Authorization header required")
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Authorization header must be: Bearer <token>")
				return
			}

			claims, err := token.Parse(parts[1], jwtSecret)
			if err != nil {
				if err == token.ErrTokenExpired {
					types.WriteError(w, http.StatusUnauthorized, types.ErrCodeExpiredToken, "Access token has expired")
					return
				}
				types.WriteError(w, http.StatusUnauthorized, types.ErrCodeInvalidToken, "Invalid access token")
				return
			}

			ctx := reqctx.WithUserID(r.Context(), claims.UserID.String())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// OptionalAuthenticate parses a Bearer JWT when present and attaches user_id to
// context. Missing or invalid tokens are ignored (no 401) so the route stays public.
// Use on endpoints that need requester identity when logged in (e.g. list visibility).
func OptionalAuthenticate(jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
			if authHeader == "" {
				next.ServeHTTP(w, r)
				return
			}
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
				next.ServeHTTP(w, r)
				return
			}
			claims, err := token.Parse(strings.TrimSpace(parts[1]), jwtSecret)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			ctx := reqctx.WithUserID(r.Context(), claims.UserID.String())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
