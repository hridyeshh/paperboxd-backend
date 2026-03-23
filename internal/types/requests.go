package types

// RegisterRequest is the payload for POST /api/v1/auth/register.
type RegisterRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

// LoginRequest is the payload for POST /api/v1/auth/login.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// RefreshRequest is the payload for POST /api/v1/auth/refresh.
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// LogoutRequest is the payload for POST /api/v1/auth/logout.
type LogoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// AddToBookshelfRequest is the payload for POST /api/v1/users/:username/bookshelf.
type AddToBookshelfRequest struct {
	BookID     string  `json:"book_id"`
	Status     string  `json:"status"`
	Rating     *int    `json:"rating"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
}

// UpdateUserRequest is the payload for PUT /api/v1/users/:username.
type UpdateUserRequest struct {
	Name      *string `json:"name"`
	Bio       *string `json:"bio"`
	Pronouns  *string `json:"pronouns"`
	AvatarURL *string `json:"avatar_url"`
}

// CreateBookRequest is the payload for POST /api/v1/books.
type CreateBookRequest struct {
	GoogleBooksID string `json:"google_books_id"`
}
