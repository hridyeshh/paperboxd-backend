package types

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// AuthResponse is returned after a successful login or register.
type AuthResponse struct {
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	ExpiresIn    int          `json:"expires_in"` // seconds
	User         UserResponse `json:"user"`
}

// TokenResponse is returned after a successful token refresh.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// UserResponse is the public-facing user object.
type UserResponse struct {
	ID             uuid.UUID `json:"id"`
	Username       string    `json:"username"`
	Email          string    `json:"email"`
	Name           *string   `json:"name"`
	AvatarURL      *string   `json:"avatar_url"`
	Bio            *string   `json:"bio"`
	Pronouns       *string   `json:"pronouns"`
	IsPublic       bool      `json:"is_public"`
	BooksReadCount int32     `json:"books_read_count"`
	FollowersCount int32     `json:"followers_count"`
	FollowingCount int32     `json:"following_count"`
	CreatedAt      time.Time `json:"created_at"`
}

// SuccessResponse is a generic success envelope.
type SuccessResponse struct {
	Message string `json:"message"`
}

// WriteJSON writes a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(v)
}
