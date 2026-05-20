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

// ForgotPasswordRequest is the payload for POST /api/v1/auth/forgot-password.
type ForgotPasswordRequest struct {
	Email string `json:"email"`
}

// ResetPasswordRequest is the payload for POST /api/v1/auth/reset-password.
type ResetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// DeleteAccountRequest is the optional payload for DELETE /api/v1/users/me.
// Reasons are persisted in the account_deletions audit table for retention analysis.
type DeleteAccountRequest struct {
	Reasons []string `json:"reasons"`
}

// AddToBookshelfRequest is the payload for POST /api/v1/users/:username/bookshelf.
// One of BookID, ISBN, or GoogleBooksID must be provided to identify the book.
type AddToBookshelfRequest struct {
	BookID        *string `json:"book_id"`
	ISBN          *string `json:"isbn"`
	GoogleBooksID *string `json:"google_books_id"`
	Status        string  `json:"status"`
	Rating        *int    `json:"rating"`
	StartedAt     *string `json:"started_at"`
	FinishedAt    *string `json:"finished_at"`
}

// UpdateUserRequest is the payload for PUT/PATCH /api/v1/users/:username.
type UpdateUserRequest struct {
	Username  *string   `json:"username"`
	Name      *string   `json:"name"`
	Bio       *string   `json:"bio"`
	Pronouns  *[]string `json:"pronouns"`
	AvatarURL *string   `json:"avatar_url"`
	Birthday  *string   `json:"birthday"`
	Gender    *string   `json:"gender"`
	Links     *[]string `json:"links"`
}

// CreateBookRequest is the payload for POST /api/v1/books.
// Provide either google_books_id (Google volume id) or isbn (ISBN-10/13) to get-or-create a cached row.
type CreateBookRequest struct {
	GoogleBooksID string `json:"google_books_id,omitempty"`
	ISBN            string `json:"isbn,omitempty"`
}

// UpdateTBRRequest is the payload for PUT /api/v1/users/:username/bookshelf/:bookId/tbr.
type UpdateTBRRequest struct {
	Notes    *string `json:"notes"`
	Priority *string `json:"priority"` // "high", "medium", "low"
}

// UpdateReadingProgressRequest is the payload for PUT /api/v1/users/:username/bookshelf/:bookId/progress.
type UpdateReadingProgressRequest struct {
	CurrentPage *int32 `json:"current_page"`
}

// AddToFavoritesRequest is the payload for POST /api/v1/users/:username/favorites.
// One of BookID, ISBN, or GoogleBooksID must be provided.
type AddToFavoritesRequest struct {
	BookID        *string `json:"book_id"`
	ISBN          *string `json:"isbn"`
	GoogleBooksID *string `json:"google_books_id"`
	DisplayOrder  int     `json:"display_order"` // 1–4 only
	Note          *string `json:"note"`
}

// ReorderFavoritesRequest is the payload for PUT /api/v1/users/:username/favorites/reorder.
type ReorderFavoritesRequest struct {
	Favorites []struct {
		BookID       string `json:"book_id"`
		DisplayOrder int    `json:"display_order"` // 1–4 only
	} `json:"favorites"`
}

// CreateListRequest is the payload for POST /api/v1/users/:username/lists.
type CreateListRequest struct {
	Title       string  `json:"title"`       // Max 50 chars
	Description *string `json:"description"` // Max 200 chars
	IsPrivate   bool    `json:"is_private"`
}

// UpdateListRequest is the payload for PUT /api/v1/users/:username/lists/:listId.
type UpdateListRequest struct {
	Title       *string `json:"title"`       // Max 50 chars
	Description *string `json:"description"` // Max 200 chars
	IsPrivate   *bool   `json:"is_private"`
}

// AddBookToListRequest is the payload for POST /api/v1/users/:username/lists/:listId/books.
// One of BookID, ISBN, or GoogleBooksID must be provided.
type AddBookToListRequest struct {
	BookID        *string `json:"book_id"`
	ISBN          *string `json:"isbn"`
	GoogleBooksID *string `json:"google_books_id"`
	DisplayOrder  *int    `json:"display_order"`
}

// GrantAccessRequest is the payload for POST /api/v1/users/:username/lists/:listId/access.
type GrantAccessRequest struct {
	Username string `json:"username"`
}

// ShareListRequest is the payload for POST /api/v1/users/:username/lists/:listId/share.
type ShareListRequest struct {
	Usernames []string `json:"usernames"`
}

// CreateDiaryEntryRequest is the payload for POST /api/v1/users/:username/diary.
// One of BookID, ISBN, or GoogleBooksID may optionally be provided to link a book.
type CreateDiaryEntryRequest struct {
	BookID        *string `json:"book_id"`
	ISBN          *string `json:"isbn"`
	GoogleBooksID *string `json:"google_books_id"`
	Title         *string `json:"title"`     // Max 100 chars
	Content       string  `json:"content"`   // Required; rich text HTML
	IsPrivate     bool    `json:"is_private"`
	Rating        *int    `json:"rating"`    // 1–5
}

// UpdateDiaryEntryRequest is the payload for PUT /api/v1/users/:username/diary/:entryId.
type UpdateDiaryEntryRequest struct {
	Title     *string `json:"title"`
	Content   *string `json:"content"`
	IsPrivate *bool   `json:"is_private"`
	Rating    *int    `json:"rating"` // 1–5; pass null to clear
}

// GoogleAuthRequest is the payload for POST /api/v1/auth/google.
// Only accepted from the Next.js server with the correct X-Internal-Secret header.
type GoogleAuthRequest struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}
