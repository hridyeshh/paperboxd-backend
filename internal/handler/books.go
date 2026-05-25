package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/config"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// BookHandler holds dependencies for book endpoints.
type BookHandler struct {
	Queries               *db.Queries
	Config                *config.Config
	ISBNdb                *external.ISBNdbClient
	GoogleBooks           *external.GoogleBooksClient
	RecommendationService *service.RecommendationService
	Enricher              *service.Enricher
}

// NewBookHandler creates a BookHandler with the given clients.
func NewBookHandler(queries *db.Queries, cfg *config.Config, isbndb *external.ISBNdbClient, googleBooks *external.GoogleBooksClient) *BookHandler {
	return &BookHandler{
		Queries:     queries,
		Config:      cfg,
		ISBNdb:      isbndb,
		GoogleBooks: googleBooks,
	}
}

// embedCallback returns a fire-and-forget func for newly cached books, or nil if embedding is disabled.
func (h *BookHandler) embedCallback() func(db.Book) {
	if h.RecommendationService == nil {
		return nil
	}
	svc := h.RecommendationService
	enricher := h.Enricher
	return func(book db.Book) {
		svc.EmbedBookAsync(bookToEnrichable(book), enricher)
	}
}

// searchQueryString reads ?query= or ?q= (web client uses q).
func searchQueryString(r *http.Request) string {
	q := strings.TrimSpace(r.URL.Query().Get("query"))
	if q != "" {
		return q
	}
	return strings.TrimSpace(r.URL.Query().Get("q"))
}

// Search handles GET /api/v1/books/search?query=...&page=...&page_size=...
// Priority: DB cache → ISBNdb → Google Books
func (h *BookHandler) Search(w http.ResponseWriter, r *http.Request) {
	query := searchQueryString(r)
	if query == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "query parameter is required")
		return
	}

	page, pageSize := parsePagination(r)
	ctx := r.Context()

	// 1. DB cache first
	offset := int32((page - 1) * pageSize)
	limit := int32(pageSize)

	dbBooks, err := h.Queries.SearchBooksInDB(ctx, db.SearchBooksInDBParams{
		Column1: pgtype.Text{String: query, Valid: true},
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		slog.Error("search books in db", "error", err)
		types.WriteInternalError(w)
		return
	}
	if len(dbBooks) > 0 {
		items := make([]types.BookResponse, len(dbBooks))
		for i, b := range dbBooks {
			items[i] = bookToResponse(b)
		}
		types.WriteJSON(w, http.StatusOK, types.BookListResponse{
			Kind:       "books#volumes",
			TotalItems: len(items),
			Items:      items,
			Page:       page,
			PageSize:   pageSize,
			Source:     "db",
		})
		return
	}

	// 2. ISBNdb (primary external source)
	if h.ISBNdb != nil {
		isbndbBooks, err := h.ISBNdb.Search(ctx, query, page, pageSize)
		if err != nil {
			slog.Warn("isbndb search failed", "error", err)
		} else if len(isbndbBooks) > 0 {
			items := make([]types.BookResponse, len(isbndbBooks))
			for i, b := range isbndbBooks {
				items[i] = isbndbBookToResponse(b)
			}
			types.WriteJSON(w, http.StatusOK, types.BookListResponse{
				Kind:       "books#volumes",
				TotalItems: len(items),
				Items:      items,
				Page:       page,
				PageSize:   pageSize,
				Source:     "isbndb",
			})
			return
		}
	}

	// 3. Google Books fallback
	if h.GoogleBooks != nil {
		googleBooks, err := h.GoogleBooks.Search(ctx, query, pageSize)
		if err != nil {
			slog.Warn("google books search failed", "error", err)
		} else if len(googleBooks) > 0 {
			items := make([]types.BookResponse, len(googleBooks))
			for i, b := range googleBooks {
				items[i] = googleBookToResponse(b)
			}
			types.WriteJSON(w, http.StatusOK, types.BookListResponse{
				Kind:       "books#volumes",
				TotalItems: len(items),
				Items:      items,
				Page:       page,
				PageSize:   pageSize,
				Source:     "google",
			})
			return
		}
	}

	// No results from any source
	types.WriteJSON(w, http.StatusOK, types.BookListResponse{
		Kind:       "books#volumes",
		TotalItems: 0,
		Items:      []types.BookResponse{},
		Page:       page,
		PageSize:   pageSize,
		Source:     "none",
	})
}

// Create handles POST /api/v1/books
func (h *BookHandler) Create(w http.ResponseWriter, r *http.Request) {
	_, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	var req types.CreateBookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	isbn := strings.TrimSpace(req.ISBN)
	googleID := strings.TrimSpace(req.GoogleBooksID)
	if isbn == "" && googleID == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "google_books_id or isbn is required")
		return
	}

	// ── ISBN: resolve or cache (same behaviour as bookshelf add) ─────────────
	if isbn != "" {
		existing, err := h.Queries.GetBookByISBN(r.Context(), pgtype.Text{String: isbn, Valid: true})
		if err == nil {
			types.WriteJSON(w, http.StatusOK, bookToResponse(existing))
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("get book by isbn", "error", err)
			types.WriteInternalError(w)
			return
		}
		if h.ISBNdb == nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "ISBN lookup is not configured")
			return
		}
		book, err := cacheBookFromISBNdb(r.Context(), h.Queries, h.ISBNdb, isbn, h.embedCallback())
		if err != nil {
			slog.Error("cache book from isbn", "error", err, "isbn", isbn)
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Book not found for ISBN")
			return
		}
		types.WriteJSON(w, http.StatusCreated, bookToResponse(book))
		return
	}

	// ── Google Books volume id ────────────────────────────────────────────────
	// Check if already in DB
	existing, err := h.Queries.GetBookByGoogleID(r.Context(), pgtype.Text{String: googleID, Valid: true})
	if err == nil {
		types.WriteJSON(w, http.StatusOK, bookToResponse(existing))
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("get book by google id", "error", err)
		types.WriteInternalError(w)
		return
	}

	if h.GoogleBooks == nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Google Books client is not configured")
		return
	}
	// Fetch from Google Books
	gb, err := h.GoogleBooks.GetByID(r.Context(), googleID)
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Book not found in Google Books")
		return
	}

	params := googleBookToCreateParams(gb)
	book, err := h.Queries.CreateBook(r.Context(), params)
	if err != nil {
		slog.Error("create book", "error", err)
		types.WriteInternalError(w)
		return
	}

	if cb := h.embedCallback(); cb != nil {
		go cb(book)
	}

	types.WriteJSON(w, http.StatusCreated, bookToResponse(book))
}

// GetByID handles GET /api/v1/books/:id
func (h *BookHandler) GetByID(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	bookID, err := uuid.Parse(idStr)
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid book ID")
		return
	}

	book, err := h.Queries.GetBookByID(r.Context(), bookID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not found")
			return
		}
		slog.Error("get book by id", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Increment view count async (background ctx — request ctx will be cancelled)
	go func() {
		_ = h.Queries.IncrementBookViews(context.Background(), bookID)
	}()

	types.WriteJSON(w, http.StatusOK, bookToResponse(book))
}

// resolveBookIDParam resolves an arbitrary book identifier (URL param or body
// field) to a Postgres book UUID. It accepts a UUID, a Google Books volume ID,
// or an ISBN (10 or 13 digits, hyphens allowed). When the book isn't cached
// it is fetched from Google Books / ISBNdb and stored.
func resolveBookIDParam(
	ctx context.Context,
	q *db.Queries,
	gb *external.GoogleBooksClient,
	isbndb *external.ISBNdbClient,
	raw string,
) (uuid.UUID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return uuid.UUID{}, errors.New("book id is required")
	}

	if id, err := uuid.Parse(raw); err == nil {
		if _, err := q.GetBookByID(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return uuid.UUID{}, errors.New("book not found")
			}
			return uuid.UUID{}, err
		}
		return id, nil
	}

	// Try Google Books volume ID (short alphanumeric, no hyphens)
	if !strings.ContainsAny(raw, "-") && len(raw) <= 20 {
		book, err := q.GetBookByGoogleID(ctx, pgtype.Text{String: raw, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) && gb != nil {
			book, err = cacheBookFromGoogleBooks(ctx, q, gb, raw, nil)
		}
		if err == nil {
			return book.ID, nil
		}
	}

	// Try ISBN (10 or 13 digits, possibly with hyphens)
	isbn := strings.ReplaceAll(raw, "-", "")
	if len(isbn) == 10 || len(isbn) == 13 {
		book, err := q.GetBookByISBN(ctx, pgtype.Text{String: isbn, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) && isbndb != nil {
			book, err = cacheBookFromISBNdb(ctx, q, isbndb, isbn, nil)
		}
		if err == nil {
			return book.ID, nil
		}
	}

	return uuid.UUID{}, errors.New("could not resolve book: send a Postgres UUID, Google Books volume ID, or ISBN")
}

// resolveBookID resolves the {id} URL param using the shared helper.
func (h *BookHandler) resolveBookID(ctx context.Context, raw string) (uuid.UUID, error) {
	return resolveBookIDParam(ctx, h.Queries, h.GoogleBooks, h.ISBNdb, raw)
}

// Like handles POST /api/v1/books/:id/like
func (h *BookHandler) Like(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	bookID, err := h.resolveBookID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	_, err = h.Queries.LikeBook(r.Context(), db.LikeBookParams{
		UserID: userID,
		BookID: bookID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("like book", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Book liked"})
}

// Unlike handles DELETE /api/v1/books/:id/like
func (h *BookHandler) Unlike(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	bookID, err := h.resolveBookID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	if err := h.Queries.UnlikeBook(r.Context(), db.UnlikeBookParams{
		UserID: userID,
		BookID: bookID,
	}); err != nil {
		slog.Error("unlike book", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Book unliked"})
}

// GetLatest handles GET /api/v1/books/latest
func (h *BookHandler) GetLatest(w http.ResponseWriter, r *http.Request) {
	page, pageSize := parsePagination(r)
	books, err := h.Queries.GetLatestBooks(r.Context(), db.GetLatestBooksParams{
		Limit:  int32(pageSize),
		Offset: int32((page - 1) * pageSize),
	})
	if err != nil {
		slog.Error("get latest books", "error", err)
		types.WriteInternalError(w)
		return
	}
	items := make([]types.BookResponse, len(books))
	for i, b := range books {
		items[i] = bookToResponse(b)
	}
	types.WriteJSON(w, http.StatusOK, types.BookListResponse{
		Kind:       "books#volumes",
		TotalItems: len(items),
		Items:      items,
		Page:       page,
		PageSize:   pageSize,
		Source:     "db",
	})
}

// GetPublic handles GET /api/v1/books/public
// Returns two carousels: recently added books and most-viewed books.
func (h *BookHandler) GetPublic(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const carouselSize = int32(12)

	newBooks, err := h.Queries.GetLatestBooks(ctx, db.GetLatestBooksParams{Limit: carouselSize, Offset: 0})
	if err != nil {
		slog.Error("get latest books for public", "error", err)
		types.WriteInternalError(w)
		return
	}
	popularBooks, err := h.Queries.GetPopularBooks(ctx, db.GetPopularBooksParams{Limit: carouselSize, Offset: 0})
	if err != nil {
		slog.Error("get popular books for public", "error", err)
		types.WriteInternalError(w)
		return
	}

	toItems := func(books []db.Book) []types.BookResponse {
		items := make([]types.BookResponse, len(books))
		for i, b := range books {
			items[i] = bookToResponse(b)
		}
		return items
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"new_releases": toItems(newBooks),
		"popular":      toItems(popularBooks),
	})
}

// GetByAuthor handles GET /api/v1/books/by-author?author=...
func (h *BookHandler) GetByAuthor(w http.ResponseWriter, r *http.Request) {
	author := r.URL.Query().Get("author")
	if author == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "author parameter is required")
		return
	}
	page, pageSize := parsePagination(r)
	books, err := h.Queries.GetBooksByAuthor(r.Context(), db.GetBooksByAuthorParams{
		Column1: pgtype.Text{String: author, Valid: true},
		Limit:   int32(pageSize),
		Offset:  int32((page - 1) * pageSize),
	})
	if err != nil {
		slog.Error("get books by author", "error", err)
		types.WriteInternalError(w)
		return
	}
	items := make([]types.BookResponse, len(books))
	for i, b := range books {
		items[i] = bookToResponse(b)
	}
	types.WriteJSON(w, http.StatusOK, types.BookListResponse{
		Kind:       "books#volumes",
		TotalItems: len(items),
		Items:      items,
		Page:       page,
		PageSize:   pageSize,
		Source:     "db",
	})
}

// ShareBook handles POST /api/v1/books/{id}/share
func (h *BookHandler) ShareBook(w http.ResponseWriter, r *http.Request) {
	callerIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	callerID, err := uuid.Parse(callerIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	bookID, err := h.resolveBookID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	var req struct {
		TargetUsername string `json:"targetUsername"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TargetUsername == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "targetUsername is required")
		return
	}

	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(req.TargetUsername))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for share book", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() {
		_, _ = h.Queries.CreateActivity(r.Context(), db.CreateActivityParams{
			UserID:       callerID,
			ActivityType: "shared_book",
			BookID:       uuidToPgtype(bookID),
			TargetUserID: uuidToPgtype(target.ID),
			Metadata:     []byte("{}"),
		})
	}()

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Book shared successfully"})
}

// GetBySlug handles GET /api/v1/books/by-slug/{slug}
// Slug format from frontend: "title+words+hexid" (e.g. "deep+work+dd04")
func (h *BookHandler) GetBySlug(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "slug is required")
		return
	}

	ctx := r.Context()

	// 1. Try exact slug match (handles Go-style stored slugs)
	book, err := h.Queries.GetBookBySlug(ctx, slug)
	if err == nil {
		types.WriteJSON(w, http.StatusOK, bookToResponse(book))
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("get book by slug", "error", err)
		types.WriteInternalError(w)
		return
	}

	// 2. If slug has no + or spaces it may be an ISBN or Google Books ID — try
	//    resolveBookIDParam which handles both before falling back to title search.
	if !strings.Contains(slug, "+") && !strings.Contains(slug, " ") {
		if bookID, resolveErr := resolveBookIDParam(ctx, h.Queries, h.GoogleBooks, h.ISBNdb, slug); resolveErr == nil {
			if resolved, fetchErr := h.Queries.GetBookByID(ctx, bookID); fetchErr == nil {
				types.WriteJSON(w, http.StatusOK, bookToResponse(resolved))
				return
			}
		}
	}

	// 3. Parse title from frontend slug and search DB cache
	title := titleFromSlug(slug)
	dbBooks, err := h.Queries.SearchBooksInDB(ctx, db.SearchBooksInDBParams{
		Column1: pgtype.Text{String: title, Valid: true},
		Limit:   1,
		Offset:  0,
	})
	if err == nil && len(dbBooks) > 0 {
		types.WriteJSON(w, http.StatusOK, bookToResponse(dbBooks[0]))
		return
	}

	// 4. ISBNdb external search + cache result in DB
	if h.ISBNdb != nil {
		isbndbBooks, isbndbErr := h.ISBNdb.Search(ctx, title, 1, 1)
		if isbndbErr == nil && len(isbndbBooks) > 0 {
			b := isbndbBooks[0]
			isbn := b.ISBN13
			if isbn == "" {
				isbn = b.ISBN
			}
			if isbn != "" {
				if cached, cacheErr := cacheBookFromISBNdb(ctx, h.Queries, h.ISBNdb, isbn, h.embedCallback()); cacheErr == nil {
					types.WriteJSON(w, http.StatusOK, bookToResponse(cached))
					return
				}
			}
			types.WriteJSON(w, http.StatusOK, isbndbBookToResponse(b))
			return
		}
	}

	// 5. Google Books fallback + cache result in DB
	if h.GoogleBooks != nil {
		googleBooks, gbErr := h.GoogleBooks.Search(ctx, title, 1)
		if gbErr == nil && len(googleBooks) > 0 {
			gb := googleBooks[0]
			if gb.ID != "" {
				if cached, cacheErr := cacheBookFromGoogleBooks(ctx, h.Queries, h.GoogleBooks, gb.ID, h.embedCallback()); cacheErr == nil {
					types.WriteJSON(w, http.StatusOK, bookToResponse(cached))
					return
				}
			}
			types.WriteJSON(w, http.StatusOK, googleBookToResponse(gb))
			return
		}
	}

	types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "book not found")
}

// titleFromSlug converts a frontend book slug to a search title.
// Frontend slug format: "deep+work+dd04" → "deep work"
// Strips the trailing +hex segment if present (4–8 hex chars).
func titleFromSlug(slug string) string {
	parts := strings.Split(slug, "+")
	if len(parts) > 1 && isHexSegment(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, " ")
}

func isHexSegment(s string) bool {
	if len(s) < 4 || len(s) > 8 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// bookToEnrichable converts a db.Book to the service.EnrichableBook used by the embedding pipeline.
func bookToEnrichable(b db.Book) service.EnrichableBook {
	return service.EnrichableBook{
		ID:            b.ID.String(),
		Title:         b.Title,
		Subtitle:      b.Subtitle.String,
		Authors:       b.Authors,
		Publisher:     b.Publisher.String,
		Categories:    b.Categories,
		Description:   b.Description.String,
		OpenLibraryID: b.OpenLibraryID.String,
		ISBNdbID:      b.IsbndbID.String,
		ISBN13:        b.Isbn13.String,
		GoogleBooksID: b.GoogleBooksID.String,
		Metadata:      b.Metadata,
	}
}

var nonAlphanumRegex = regexp.MustCompile(`[^a-z0-9]+`)

func generateSlug(title, googleBooksID string) string {
	slug := strings.ToLower(title)
	slug = nonAlphanumRegex.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if googleBooksID != "" && len(googleBooksID) >= 6 {
		slug = slug + "-" + strings.ToLower(googleBooksID[:6])
	}
	return slug
}

func googleBookToCreateParams(gb *external.GoogleBook) db.CreateBookParams {
	vi := gb.VolumeInfo

	authors := vi.Authors
	if len(authors) == 0 {
		authors = []string{}
	}

	categories := vi.Categories
	if len(categories) == 0 {
		categories = []string{}
	}

	// Find ISBN-13
	var isbn13 pgtype.Text
	for _, id := range vi.IndustryIdentifiers {
		if id.Type == "ISBN_13" {
			isbn13 = pgtype.Text{String: id.Identifier, Valid: true}
			break
		}
	}

	params := db.CreateBookParams{
		Title:         vi.Title,
		Slug:          generateSlug(vi.Title, gb.ID),
		Authors:       authors,
		Isbn13:        isbn13,
		GoogleBooksID: pgtype.Text{String: gb.ID, Valid: gb.ID != ""},
		Categories:    categories,
		Metadata:      []byte("{}"),
	}

	if vi.Description != "" {
		params.Description = pgtype.Text{String: vi.Description, Valid: true}
	}
	if vi.PageCount > 0 {
		params.PageCount = pgtype.Int4{Int32: int32(vi.PageCount), Valid: true}
	}
	if vi.Language != "" {
		params.Language = pgtype.Text{String: vi.Language, Valid: true}
	}
	if vi.ImageLinks.Thumbnail != "" {
		params.CoverUrl = pgtype.Text{String: vi.ImageLinks.Thumbnail, Valid: true}
	}
	if vi.PublishedDate != "" {
		t := parsePublishedDate(vi.PublishedDate)
		if t != nil {
			params.PublishedDate = pgtype.Date{Time: *t, Valid: true}
		}
	}

	return params
}

func parsePublishedDate(s string) *time.Time {
	formats := []string{"2006-01-02", "2006-01", "2006"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return &t
		}
	}
	return nil
}

// bookToResponse converts a db.Book to the frontend-compatible BookResponse.
func bookToResponse(b db.Book) types.BookResponse {
	vi := types.VolumeInfo{
		Title:      b.Title,
		Authors:    b.Authors,
		Categories: b.Categories,
	}
	if vi.Authors == nil {
		vi.Authors = []string{}
	}
	if vi.Categories == nil {
		vi.Categories = []string{}
	}
	if b.Description.Valid {
		vi.Description = b.Description.String
	}
	if b.CoverUrl.Valid {
		vi.ImageLinks = types.ImageLinks{
			Thumbnail: b.CoverUrl.String,
			Small:     b.CoverUrl.String,
			Medium:    b.CoverUrl.String,
		}
	}
	if b.PublishedDate.Valid {
		vi.PublishedDate = b.PublishedDate.Time.Format("2006-01-02")
	}
	if b.PageCount.Valid {
		vi.PageCount = int(b.PageCount.Int32)
	}
	if b.Language.Valid {
		vi.Language = b.Language.String
	}
	if b.Subtitle.Valid {
		vi.Subtitle = b.Subtitle.String
	}
	if b.Publisher.Valid {
		vi.Publisher = b.Publisher.String
	}
	if b.PreviewLink.Valid {
		vi.PreviewLink = b.PreviewLink.String
	}
	if b.AverageRating.Valid {
		v := b.AverageRating.Float64
		vi.AverageRating = &v
	}
	if b.RatingsCount.Valid {
		v := int(b.RatingsCount.Int32)
		vi.RatingsCount = &v
	}
	if b.Isbn13.Valid && b.Isbn13.String != "" {
		vi.IndustryIdentifiers = append(vi.IndustryIdentifiers, types.IndustryIdentifier{
			Type:       "ISBN_13",
			Identifier: b.Isbn13.String,
		})
	}

	stats := types.PaperboxdStats{}
	if b.LikeCount.Valid {
		v := int(b.LikeCount.Int32)
		stats.TotalLikes = &v
	}
	if b.TotalReadsCount.Valid {
		v := int(b.TotalReadsCount.Int32)
		stats.TotalReads = &v
	}
	if b.TotalTbrCount.Valid {
		v := int(b.TotalTbrCount.Int32)
		stats.TotalTBR = &v
	}

	resp := types.BookResponse{
		ID:             b.ID.String(),
		MongoID:        b.ID.String(),
		VolumeInfo:     vi,
		PaperboxdStats: stats,
		APISource:      "db",
		FromCache:      true,
		Slug:           b.Slug,
	}
	if b.GoogleBooksID.Valid {
		resp.GoogleBooksID = b.GoogleBooksID.String
	}
	if b.IsbndbID.Valid {
		resp.ISBNdbID = b.IsbndbID.String
	}
	if b.OpenLibraryID.Valid {
		resp.OpenLibraryID = b.OpenLibraryID.String
	}
	return resp
}

// googleBookToResponse converts a Google Books API result to BookResponse.
func googleBookToResponse(gb external.GoogleBook) types.BookResponse {
	vi := gb.VolumeInfo
	authors := vi.Authors
	if authors == nil {
		authors = []string{}
	}
	categories := vi.Categories
	if categories == nil {
		categories = []string{}
	}

	volumeInfo := types.VolumeInfo{
		Title:         vi.Title,
		Authors:       authors,
		Description:   vi.Description,
		PublishedDate: vi.PublishedDate,
		PageCount:     vi.PageCount,
		Language:      vi.Language,
		Categories:    categories,
		ImageLinks: types.ImageLinks{
			Thumbnail: vi.ImageLinks.Thumbnail,
		},
	}

	for _, id := range vi.IndustryIdentifiers {
		volumeInfo.IndustryIdentifiers = append(volumeInfo.IndustryIdentifiers, types.IndustryIdentifier{
			Type:       id.Type,
			Identifier: id.Identifier,
		})
	}

	return types.BookResponse{
		ID:            gb.ID,
		MongoID:       gb.ID,
		VolumeInfo:    volumeInfo,
		APISource:     "google",
		FromCache:     false,
		GoogleBooksID: gb.ID,
		Slug:          generateSlug(vi.Title, gb.ID),
	}
}

// isbndbBookToResponse converts an ISBNdb result to BookResponse.
func isbndbBookToResponse(b external.ISBNdbBook) types.BookResponse {
	authors := b.Authors
	if authors == nil {
		authors = []string{}
	}
	subjects := b.Subjects
	if subjects == nil {
		subjects = []string{}
	}

	isbn13 := b.ISBN13
	if isbn13 == "" {
		isbn13 = b.ISBN
	}

	volumeInfo := types.VolumeInfo{
		Title:         b.Title,
		Authors:       authors,
		Publisher:     b.Publisher,
		PublishedDate: b.DatePublished,
		Description:   b.Synopsis,
		PageCount:     b.Pages,
		Language:      b.Language,
		Categories:    subjects,
		ImageLinks: types.ImageLinks{
			Thumbnail: b.Image,
			Small:     b.Image,
			Medium:    b.Image,
		},
		IndustryIdentifiers: []types.IndustryIdentifier{
			{Type: "ISBN_13", Identifier: isbn13},
		},
	}

	return types.BookResponse{
		ID:        isbn13,
		MongoID:   isbn13,
		VolumeInfo: volumeInfo,
		APISource: "isbndb",
		FromCache: false,
		ISBNdbID:  isbn13,
		Slug:      generateSlug(b.Title, isbn13),
	}
}

// GetBookReviews handles GET /api/v1/books/{id}/reviews (public)
func (h *BookHandler) GetBookReviews(w http.ResponseWriter, r *http.Request) {
	bookID, err := h.resolveBookID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	rows, err := h.Queries.GetBookReviews(r.Context(), bookID)
	if err != nil {
		slog.Error("get book reviews", "error", err)
		types.WriteInternalError(w)
		return
	}

	type ReviewItem struct {
		UserID     string  `json:"user_id"`
		Username   string  `json:"username"`
		AvatarURL  *string `json:"avatar_url"`
		Rating     *int    `json:"rating"`
		Review     *string `json:"review"`
		ReviewedAt *string `json:"reviewed_at"`
		Edited     bool    `json:"edited"`
	}

	items := make([]ReviewItem, len(rows))
	for i, row := range rows {
		item := ReviewItem{
			UserID:   row.UserID.String(),
			Username: row.Username,
			Edited:   row.ReviewEdited,
		}
		if row.AvatarUrl.Valid {
			item.AvatarURL = &row.AvatarUrl.String
		}
		if row.Rating.Valid {
			v := int(row.Rating.Int32)
			item.Rating = &v
		}
		if row.Review.Valid && row.Review.String != "" {
			item.Review = &row.Review.String
		}
		if row.ReviewedAt.Valid {
			s := row.ReviewedAt.Time.Format(time.RFC3339)
			item.ReviewedAt = &s
		}
		items[i] = item
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{"reviews": items})
}

// CleanupStaleBooks handles DELETE /api/v1/admin/cleanup-books
// Removes books older than 15 days that are not in any user's collection.
func (h *BookHandler) CleanupStaleBooks(w http.ResponseWriter, r *http.Request) {
	deleted, err := h.Queries.CleanupStaleBooks(r.Context())
	if err != nil {
		slog.Error("cleanup stale books", "error", err)
		types.WriteInternalError(w)
		return
	}
	types.WriteJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func parsePagination(r *http.Request) (page, pageSize int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ = strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
