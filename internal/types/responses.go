package types

import (
	"encoding/json"
	"net/http"
	"time"
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

// UserResponse with all frontend-expected fields.
type UserResponse struct {
	ID             string   `json:"id"`
	MongoID        string   `json:"_id"`
	Username       string   `json:"username"`
	Email          string   `json:"email,omitempty"`
	Name           string   `json:"name"`
	AvatarURL      *string  `json:"avatar_url,omitempty"`
	Bio            *string  `json:"bio,omitempty"`
	Pronouns       []string `json:"pronouns"`
	Birthday       *string  `json:"birthday,omitempty"`
	Gender         *string  `json:"gender,omitempty"`
	Links          []string `json:"links"`
	IsPublic       bool     `json:"is_public"`
	BooksReadCount    int32  `json:"books_read_count"`
	TotalPagesRead    int32  `json:"total_pages_read"`
	FavoritesCount    int32  `json:"favorites_count"`
	ListsCount        int32  `json:"lists_count"`
	DiaryEntriesCount int32  `json:"diary_entries_count"`
	FollowersCount    int32  `json:"followers_count"`
	FollowingCount    int32  `json:"following_count"`
	CreatedAt         string `json:"created_at"`
}

// SuccessResponse is a generic success envelope.
type SuccessResponse struct {
	Message string `json:"message"`
}

// VolumeInfo matches Google Books API structure.
type VolumeInfo struct {
	Title               string               `json:"title"`
	Subtitle            string               `json:"subtitle,omitempty"`
	Authors             []string             `json:"authors"`
	Publisher           string               `json:"publisher,omitempty"`
	PublishedDate       string               `json:"publishedDate,omitempty"`
	Description         string               `json:"description,omitempty"`
	PageCount           int                  `json:"pageCount"`
	Categories          []string             `json:"categories"`
	Language            string               `json:"language,omitempty"`
	ImageLinks          ImageLinks           `json:"imageLinks"`
	AverageRating       *float64             `json:"averageRating,omitempty"`
	RatingsCount        *int                 `json:"ratingsCount,omitempty"`
	PreviewLink         string               `json:"previewLink,omitempty"`
	IndustryIdentifiers []IndustryIdentifier `json:"industryIdentifiers,omitempty"`
}

// ImageLinks holds book cover image URLs at various sizes.
type ImageLinks struct {
	SmallThumbnail string `json:"smallThumbnail,omitempty"`
	Thumbnail      string `json:"thumbnail,omitempty"`
	Small          string `json:"small,omitempty"`
	Medium         string `json:"medium,omitempty"`
	Large          string `json:"large,omitempty"`
	ExtraLarge     string `json:"extraLarge,omitempty"`
}

// IndustryIdentifier holds an ISBN or other book identifier.
type IndustryIdentifier struct {
	Type       string `json:"type"`
	Identifier string `json:"identifier"`
}

// PaperboxdStats holds aggregated activity stats for a book.
type PaperboxdStats struct {
	Rating       *float64 `json:"rating,omitempty"`
	RatingsCount *int     `json:"ratingsCount,omitempty"`
	TotalReads   *int     `json:"totalReads,omitempty"`
	TotalLikes   *int     `json:"totalLikes,omitempty"`
	TotalTBR     *int     `json:"totalTBR,omitempty"`
}

// BookResponse matches frontend expectations (Google Books API shape).
type BookResponse struct {
	ID             string         `json:"id"`
	MongoID        string         `json:"_id"`
	VolumeInfo     VolumeInfo     `json:"volumeInfo"`
	PaperboxdStats PaperboxdStats `json:"paperboxdStats"`
	APISource      string         `json:"apiSource"`
	FromCache      bool           `json:"fromCache"`
	GoogleBooksID  string         `json:"googleBooksId,omitempty"`
	ISBNdbID       string         `json:"isbndbId,omitempty"`
	OpenLibraryID  string         `json:"openLibraryId,omitempty"`
	Slug           string         `json:"slug"`
}

// BookListResponse matches Google Books API list format.
type BookListResponse struct {
	Kind       string         `json:"kind"` // "books#volumes"
	TotalItems int            `json:"totalItems"`
	Items      []BookResponse `json:"items"`
	Page       int            `json:"page,omitempty"`
	PageSize   int            `json:"pageSize,omitempty"`
	Source     string         `json:"source,omitempty"`
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

// FollowResponse with enriched follower/following counts.
type FollowResponse struct {
	Message        string `json:"message"`
	IsFollowing    bool   `json:"isFollowing"`
	FollowersCount int32  `json:"followersCount"`
	FollowingCount int32  `json:"followingCount"`
}

// FavoriteResponse is returned from GET /api/v1/users/:username/favorites.
type FavoriteResponse struct {
	ID           string       `json:"id"`
	BookID       string       `json:"book_id"`
	DisplayOrder int          `json:"display_order"`
	Note         *string      `json:"note,omitempty"`
	Book         BookResponse `json:"book"`
	CreatedAt    time.Time    `json:"created_at"`
}

// CurrentlyReadingResponse is returned from GET /api/v1/users/:username/reading.
type CurrentlyReadingResponse struct {
	ID                  string       `json:"id"`
	BookID              string       `json:"book_id"`
	Book                BookResponse `json:"book"`
	Status              string       `json:"status"`
	CurrentPage         *int32       `json:"current_page,omitempty"`
	ProgressPercentage  float64      `json:"progress_percentage"`
	PagesRemaining      int32        `json:"pages_remaining"`
	EstimatedFinishDate *time.Time   `json:"estimated_finish_date,omitempty"`
	StartedAt           *time.Time   `json:"started_at,omitempty"`
	CreatedAt           time.Time    `json:"created_at"`
}

// TBRResponse is returned from GET /api/v1/users/:username/tbr.
type TBRResponse struct {
	ID          string       `json:"id"`
	BookID      string       `json:"book_id"`
	Book        BookResponse `json:"book"`
	Status      string       `json:"status"`
	CurrentPage *int32       `json:"current_page,omitempty"`
	TBRNotes    *string      `json:"tbr_notes,omitempty"`
	TBRPriority *string      `json:"tbr_priority,omitempty"`
	TBRAddedAt  *time.Time   `json:"tbr_added_at,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
}

// ListResponse is a single reading list.
type ListResponse struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Username    string    `json:"username"`
	Title       string    `json:"title"`
	Description *string   `json:"description"`
	IsPrivate   bool      `json:"is_private"`
	BookCount   int64     `json:"book_count"`
	SaveCount   int64     `json:"save_count"`
	IsSaved     bool      `json:"is_saved"`
	CanEdit     bool      `json:"can_edit"`
	CanView     bool      `json:"can_view"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ListWithBooksResponse extends ListResponse with the books in the list.
type ListWithBooksResponse struct {
	ListResponse
	Books []BookResponse `json:"books"`
}

// UserListsResponse is returned from GET /api/v1/users/:username/lists.
type UserListsResponse struct {
	OwnLists   []ListResponse `json:"own_lists"`
	SavedLists []ListResponse `json:"saved_lists"`
}

// ListAccessResponse is returned from GET /api/v1/users/:username/lists/:listId/access.
type ListAccessResponse struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Name      string    `json:"name"`
	AvatarURL *string   `json:"avatar_url"`
	GrantedAt time.Time `json:"granted_at"`
}

// DiaryEntryResponse is returned from diary entry endpoints.
type DiaryEntryResponse struct {
	ID         string        `json:"id"`
	UserID     string        `json:"user_id"`
	Username   string        `json:"username"`
	Name       string        `json:"name"`
	AvatarURL  *string       `json:"avatar_url"`
	BookID     *string       `json:"book_id"`
	Book       *BookResponse `json:"book,omitempty"`
	Title      *string       `json:"title"`
	Content    string        `json:"content"`
	IsPrivate  bool          `json:"is_private"`
	Rating     *int          `json:"rating"`
	LikesCount int64         `json:"likes_count"`
	IsLiked    bool          `json:"is_liked"`
	CanEdit    bool          `json:"can_edit"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
}

// DiaryEntriesResponse is the paginated list from GET /api/v1/users/:username/diary.
type DiaryEntriesResponse struct {
	Entries    []DiaryEntryResponse `json:"entries"`
	TotalCount int64                `json:"total_count"`
	Page       int                  `json:"page"`
	PageSize   int                  `json:"page_size"`
}

// ActivityResponse is a single item in the activity feed.
type ActivityResponse struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	Username       string    `json:"username"`
	Name           string    `json:"name"`
	AvatarURL      *string   `json:"avatar_url"`
	ActivityType   string    `json:"activity_type"`
	BookID         *string   `json:"book_id"`
	BookTitle      *string   `json:"book_title"`
	BookSlug       *string   `json:"book_slug"`
	ListID         *string   `json:"list_id"`
	ListTitle      *string   `json:"list_title"`
	EntryID        *string   `json:"entry_id"`
	EntryTitle     *string   `json:"entry_title"`
	TargetUserID   *string   `json:"target_user_id"`
	TargetUsername *string   `json:"target_username"`
	CreatedAt      time.Time `json:"created_at"`
}

// WriteJSON writes a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(v)
}
