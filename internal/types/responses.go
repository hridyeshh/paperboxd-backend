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

// BookResponse is the public-facing book object.
type BookResponse struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Slug          string   `json:"slug"`
	Authors       []string `json:"authors"`
	Description   string   `json:"description"`
	CoverURL      string   `json:"cover_url"`
	ISBN13        string   `json:"isbn_13"`
	GoogleBooksID string   `json:"google_books_id"`
	PublishedDate string   `json:"published_date"`
	PageCount     int      `json:"page_count"`
	Language      string   `json:"language"`
	Categories    []string `json:"categories"`
	ViewCount     int      `json:"view_count"`
	LikeCount     int      `json:"like_count"`
}

// BookWithStatus extends BookResponse with bookshelf-specific fields.
type BookWithStatus struct {
	BookResponse
	Status     string  `json:"status"`
	Rating     *int    `json:"rating"`
	FinishedAt *string `json:"finished_at"`
	AddedAt    string  `json:"added_at"`
}

// BookWithLikedAt extends BookResponse with a liked_at timestamp.
type BookWithLikedAt struct {
	BookResponse
	LikedAt string `json:"liked_at"`
}

// BookshelfResponse is returned from GET /api/v1/users/:username/bookshelf.
type BookshelfResponse struct {
	Books      []BookWithStatus `json:"books"`
	TotalCount int64            `json:"total_count"`
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
}

// LikesResponse is returned from GET /api/v1/users/:username/likes.
type LikesResponse struct {
	Books      []BookWithLikedAt `json:"books"`
	TotalCount int64             `json:"total_count"`
	Page       int               `json:"page"`
	PageSize   int               `json:"page_size"`
}

// UserListResponse is returned from user list endpoints (followers, following, search).
type UserListResponse struct {
	Users      []UserResponse `json:"users"`
	TotalCount int64          `json:"total_count"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
}

// BookListResponse is returned from book search endpoints.
type BookListResponse struct {
	Books    []BookResponse `json:"books"`
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
	Source   string         `json:"source"`
}

// WriteJSON writes a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(v)
}
