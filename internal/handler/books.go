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
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// BookHandler holds dependencies for book endpoints.
type BookHandler struct {
	Queries     *db.Queries
	Config      *config.Config
	ISBNdb      *external.ISBNdbClient
	GoogleBooks *external.GoogleBooksClient
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

// Search handles GET /api/v1/books/search?query=...&page=...&page_size=...
// Priority: DB cache → ISBNdb → Google Books
func (h *BookHandler) Search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
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
		books := make([]types.BookResponse, len(dbBooks))
		for i, b := range dbBooks {
			books[i] = bookToResponse(b)
		}
		types.WriteJSON(w, http.StatusOK, types.BookListResponse{
			Books:    books,
			Page:     page,
			PageSize: pageSize,
			Source:   "db",
		})
		return
	}

	// 2. ISBNdb (primary external source)
	if h.ISBNdb != nil {
		isbndbBooks, err := h.ISBNdb.Search(ctx, query, page, pageSize)
		if err != nil {
			slog.Warn("isbndb search failed", "error", err)
		} else if len(isbndbBooks) > 0 {
			books := make([]types.BookResponse, len(isbndbBooks))
			for i, b := range isbndbBooks {
				books[i] = isbndbBookToResponse(b)
			}
			types.WriteJSON(w, http.StatusOK, types.BookListResponse{
				Books:    books,
				Page:     page,
				PageSize: pageSize,
				Source:   "isbndb",
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
			books := make([]types.BookResponse, len(googleBooks))
			for i, b := range googleBooks {
				books[i] = googleBookToResponse(b)
			}
			types.WriteJSON(w, http.StatusOK, types.BookListResponse{
				Books:    books,
				Page:     page,
				PageSize: pageSize,
				Source:   "google",
			})
			return
		}
	}

	// No results from any source
	types.WriteJSON(w, http.StatusOK, types.BookListResponse{
		Books:    []types.BookResponse{},
		Page:     page,
		PageSize: pageSize,
		Source:   "none",
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

	if req.GoogleBooksID == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "google_books_id is required")
		return
	}

	// Check if already in DB
	existing, err := h.Queries.GetBookByGoogleID(r.Context(), pgtype.Text{String: req.GoogleBooksID, Valid: true})
	if err == nil {
		types.WriteJSON(w, http.StatusOK, bookToResponse(existing))
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("get book by google id", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Fetch from Google Books
	gb, err := h.GoogleBooks.GetByID(r.Context(), req.GoogleBooksID)
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

	bookID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid book ID")
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

	bookID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid book ID")
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

// ── Helpers ──────────────────────────────────────────────────────────────────

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

func bookToResponse(b db.Book) types.BookResponse {
	resp := types.BookResponse{
		ID:         b.ID.String(),
		Title:      b.Title,
		Slug:       b.Slug,
		Authors:    b.Authors,
		Categories: b.Categories,
	}
	if b.Description.Valid {
		resp.Description = b.Description.String
	}
	if b.CoverUrl.Valid {
		resp.CoverURL = b.CoverUrl.String
	}
	if b.Isbn13.Valid {
		resp.ISBN13 = b.Isbn13.String
	}
	if b.GoogleBooksID.Valid {
		resp.GoogleBooksID = b.GoogleBooksID.String
	}
	if b.PublishedDate.Valid {
		resp.PublishedDate = b.PublishedDate.Time.Format("2006-01-02")
	}
	if b.PageCount.Valid {
		resp.PageCount = int(b.PageCount.Int32)
	}
	if b.Language.Valid {
		resp.Language = b.Language.String
	}
	if b.ViewCount.Valid {
		resp.ViewCount = int(b.ViewCount.Int32)
	}
	if b.LikeCount.Valid {
		resp.LikeCount = int(b.LikeCount.Int32)
	}
	if resp.Categories == nil {
		resp.Categories = []string{}
	}
	if resp.Authors == nil {
		resp.Authors = []string{}
	}
	return resp
}

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

	resp := types.BookResponse{
		GoogleBooksID: gb.ID,
		Title:         vi.Title,
		Slug:          generateSlug(vi.Title, gb.ID),
		Authors:       authors,
		Description:   vi.Description,
		CoverURL:      vi.ImageLinks.Thumbnail,
		PublishedDate: vi.PublishedDate,
		PageCount:     vi.PageCount,
		Language:      vi.Language,
		Categories:    categories,
	}

	for _, id := range vi.IndustryIdentifiers {
		if id.Type == "ISBN_13" {
			resp.ISBN13 = id.Identifier
			break
		}
	}

	return resp
}

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

	return types.BookResponse{
		Title:         b.Title,
		Slug:          generateSlug(b.Title, isbn13),
		Authors:       authors,
		Description:   b.Synopsis,
		CoverURL:      b.Image,
		ISBN13:        isbn13,
		PublishedDate: b.DatePublished,
		PageCount:     b.Pages,
		Language:      b.Language,
		Categories:    subjects,
	}
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
