package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// RequireInternalSecret gates analytics endpoints.
// Uses constant-time comparison to prevent timing attacks.
// Returns 401 with the same error envelope as other middleware if secret
// is wrong or empty.
func RequireInternalSecret(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := r.Header.Get("X-Internal-Secret")
			if secret == "" || provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) != 1 {
				types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
