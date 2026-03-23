// Package auth provides authentication helpers that delegate to internal/token.
package auth

import (
	"time"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/token"
)

// Re-export token sentinel errors so callers can use auth.ErrTokenExpired etc.
var (
	ErrTokenExpired = token.ErrTokenExpired
	ErrTokenInvalid = token.ErrTokenInvalid
)

// GenerateAccessToken creates a signed JWT access token.
func GenerateAccessToken(userID uuid.UUID, secret string, expiry time.Duration) (string, error) {
	return token.Generate(userID, secret, expiry)
}
