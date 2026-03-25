package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var validStatuses = map[string]bool{
	"read":    true,
	"reading": true,
	"to-read": true,
}

// AddToBookshelf handles POST /api/v1/users/:username/bookshelf.
// Accepts book_id (UUID), isbn, or google_books_id to identify the book.
// If the book isn't cached in the DB it will be fetched from ISBNdb / Google Books and stored.
func (h *UserHandler) AddToBookshelf(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := h.resolveOwner(w, r)
	if !ok {
		return
	}

	var req types.AddToBookshelfRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	if !validStatuses[req.Status] {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "status must be 'read', 'reading', or 'to-read'")
		return
	}

	// ── Resolve book ID ───────────────────────────────────────────────────────
	var bookID uuid.UUID

	switch {
	case req.BookID != nil:
		// Direct UUID lookup
		id, err := uuid.Parse(*req.BookID)
		if err != nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid book_id")
			return
		}
		if _, err := h.Queries.GetBookByID(r.Context(), id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not found")
				return
			}
			slog.Error("get book by id", "error", err)
			types.WriteInternalError(w)
			return
		}
		bookID = id

	case req.GoogleBooksID != nil:
		book, err := h.Queries.GetBookByGoogleID(r.Context(), pgtype.Text{String: *req.GoogleBooksID, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			// Not cached — fetch and store
			book, err = h.createBookFromGoogleBooks(r, *req.GoogleBooksID)
			if err != nil {
				slog.Error("auto-cache from google books", "error", err, "google_books_id", *req.GoogleBooksID)
				types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not found in Google Books")
				return
			}
		} else if err != nil {
			slog.Error("get book by google id", "error", err)
			types.WriteInternalError(w)
			return
		}
		bookID = book.ID

	case req.ISBN != nil:
		book, err := h.Queries.GetBookByISBN(r.Context(), pgtype.Text{String: *req.ISBN, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			// Not cached — fetch and store
			book, err = h.createBookFromISBNdb(r, *req.ISBN)
			if err != nil {
				slog.Error("auto-cache from isbndb", "error", err, "isbn", *req.ISBN)
				types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not found in ISBNdb")
				return
			}
		} else if err != nil {
			slog.Error("get book by isbn", "error", err)
			types.WriteInternalError(w)
			return
		}
		bookID = book.ID

	default:
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "provide book_id, isbn, or google_books_id")
		return
	}

	// ── Build bookshelf entry ─────────────────────────────────────────────────
	params := db.AddToBookshelfParams{
		UserID: userID,
		BookID: bookID,
		Status: req.Status,
	}

	if req.Rating != nil {
		if *req.Rating < 1 || *req.Rating > 5 {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "rating must be between 1 and 5")
			return
		}
		params.Rating = pgtype.Int4{Int32: int32(*req.Rating), Valid: true}
	}

	if req.StartedAt != nil {
		t, err := time.Parse(time.RFC3339, *req.StartedAt)
		if err != nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "started_at must be RFC3339 format")
			return
		}
		params.StartedAt = pgtype.Timestamptz{Time: t, Valid: true}
	}

	if req.FinishedAt != nil {
		t, err := time.Parse(time.RFC3339, *req.FinishedAt)
		if err != nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "finished_at must be RFC3339 format")
			return
		}
		params.FinishedAt = pgtype.Timestamptz{Time: t, Valid: true}
	}

	entry, err := h.Queries.AddToBookshelf(r.Context(), params)
	if err != nil {
		slog.Error("add to bookshelf", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, entry)
}

// createBookFromISBNdb fetches a book from ISBNdb by ISBN and persists it.
func (h *UserHandler) createBookFromISBNdb(r *http.Request, isbn string) (db.Book, error) {
	if h.ISBNdb == nil {
		return db.Book{}, fmt.Errorf("isbndb client not configured")
	}

	b, err := h.ISBNdb.GetByISBN(r.Context(), isbn)
	if err != nil {
		return db.Book{}, err
	}

	isbn13 := b.ISBN13
	if isbn13 == "" {
		isbn13 = b.ISBN
	}

	authors := b.Authors
	if len(authors) == 0 {
		authors = []string{}
	}
	subjects := b.Subjects
	if len(subjects) == 0 {
		subjects = []string{}
	}

	params := db.CreateBookFromISBNdbParams{
		Title:      b.Title,
		Slug:       generateSlug(b.Title, isbn13),
		Authors:    authors,
		Isbn13:     pgtype.Text{String: isbn13, Valid: isbn13 != ""},
		Categories: subjects,
		Publisher:  pgtype.Text{String: b.Publisher, Valid: b.Publisher != ""},
		IsbndbID:   pgtype.Text{String: isbn, Valid: true},
		Metadata:   []byte("{}"),
	}
	if b.Synopsis != "" {
		params.Description = pgtype.Text{String: b.Synopsis, Valid: true}
	}
	if b.Pages > 0 {
		params.PageCount = pgtype.Int4{Int32: int32(b.Pages), Valid: true}
	}
	if b.Language != "" {
		params.Language = pgtype.Text{String: b.Language, Valid: true}
	}
	if b.Image != "" {
		params.CoverUrl = pgtype.Text{String: b.Image, Valid: true}
	}
	if b.DatePublished != "" {
		t := parsePublishedDate(b.DatePublished)
		if t != nil {
			params.PublishedDate = pgtype.Date{Time: *t, Valid: true}
		}
	}

	return h.Queries.CreateBookFromISBNdb(r.Context(), params)
}

// createBookFromGoogleBooks fetches a book from Google Books by volume ID and persists it.
func (h *UserHandler) createBookFromGoogleBooks(r *http.Request, volumeID string) (db.Book, error) {
	if h.GoogleBooks == nil {
		return db.Book{}, fmt.Errorf("google books client not configured")
	}

	gb, err := h.GoogleBooks.GetByID(r.Context(), volumeID)
	if err != nil {
		return db.Book{}, err
	}

	params := googleBookToCreateParams(gb)
	return h.Queries.CreateBook(r.Context(), params)
}


// RemoveFromBookshelf handles DELETE /api/v1/users/:username/bookshelf/:bookId
func (h *UserHandler) RemoveFromBookshelf(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := h.resolveOwner(w, r)
	if !ok {
		return
	}

	bookID, err := uuid.Parse(chi.URLParam(r, "bookId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid book ID")
		return
	}

	if err := h.Queries.RemoveFromBookshelf(r.Context(), db.RemoveFromBookshelfParams{
		UserID: userID,
		BookID: bookID,
	}); err != nil {
		slog.Error("remove from bookshelf", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Removed from bookshelf"})
}

// GetBookshelf handles GET /api/v1/users/:username/bookshelf?status=read&page=1&page_size=20
func (h *UserHandler) GetBookshelf(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for bookshelf", "error", err)
		types.WriteInternalError(w)
		return
	}

	status := r.URL.Query().Get("status")
	if status == "" {
		status = "read"
	}
	if !validStatuses[status] {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "status must be 'read', 'reading', or 'to-read'")
		return
	}

	page, pageSize := parsePagination(r)
	offset := int32((page - 1) * pageSize)
	limit := int32(pageSize)

	rows, err := h.Queries.GetUserBookshelf(r.Context(), db.GetUserBookshelfParams{
		UserID: target.ID,
		Status: status,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		slog.Error("get user bookshelf", "error", err)
		types.WriteInternalError(w)
		return
	}

	total, err := h.Queries.CountUserBooks(r.Context(), db.CountUserBooksParams{
		UserID: target.ID,
		Status: status,
	})
	if err != nil {
		slog.Error("count user books", "error", err)
		types.WriteInternalError(w)
		return
	}

	books := make([]types.BookWithStatus, len(rows))
	for i, row := range rows {
		bk := types.BookWithStatus{
			BookResponse: bookRowToResponse(row),
			Status:       row.Status,
			AddedAt:      row.AddedAt.Time.Format(time.RFC3339),
		}
		if row.Rating.Valid {
			v := int(row.Rating.Int32)
			bk.Rating = &v
		}
		if row.FinishedAt.Valid {
			s := row.FinishedAt.Time.Format(time.RFC3339)
			bk.FinishedAt = &s
		}
		books[i] = bk
	}

	types.WriteJSON(w, http.StatusOK, types.BookshelfResponse{
		Books:      books,
		TotalCount: total,
		Page:       page,
		PageSize:   pageSize,
	})
}

// GetLikes handles GET /api/v1/users/:username/likes
func (h *UserHandler) GetLikes(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for likes", "error", err)
		types.WriteInternalError(w)
		return
	}

	page, pageSize := parsePagination(r)
	offset := int32((page - 1) * pageSize)
	limit := int32(pageSize)

	rows, err := h.Queries.GetUserLikes(r.Context(), db.GetUserLikesParams{
		UserID: target.ID,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		slog.Error("get user likes", "error", err)
		types.WriteInternalError(w)
		return
	}

	books := make([]types.BookWithLikedAt, len(rows))
	for i, row := range rows {
		books[i] = types.BookWithLikedAt{
			BookResponse: likesRowToResponse(row),
			LikedAt:      row.LikedAt.Time.Format(time.RFC3339),
		}
	}

	types.WriteJSON(w, http.StatusOK, types.LikesResponse{
		Books:      books,
		TotalCount: int64(len(books)),
		Page:       page,
		PageSize:   pageSize,
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// resolveOwner verifies the JWT user matches the :username route param.
// Returns the authenticated userID, the target db.User, and ok=true on success.
func (h *UserHandler) resolveOwner(w http.ResponseWriter, r *http.Request) (uuid.UUID, db.User, bool) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return uuid.UUID{}, db.User{}, false
	}

	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return uuid.UUID{}, db.User{}, false
	}

	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
		} else {
			slog.Error("get user by username", "error", err)
			types.WriteInternalError(w)
		}
		return uuid.UUID{}, db.User{}, false
	}

	if target.ID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return uuid.UUID{}, db.User{}, false
	}

	return userID, target, true
}

func bookRowToResponse(row db.GetUserBookshelfRow) types.BookResponse {
	vi := types.VolumeInfo{
		Title:      row.Title,
		Authors:    row.Authors,
		Categories: row.Categories,
	}
	if vi.Authors == nil {
		vi.Authors = []string{}
	}
	if vi.Categories == nil {
		vi.Categories = []string{}
	}
	if row.Description.Valid {
		vi.Description = row.Description.String
	}
	if row.CoverUrl.Valid {
		vi.ImageLinks = types.ImageLinks{
			Thumbnail: row.CoverUrl.String,
			Small:     row.CoverUrl.String,
			Medium:    row.CoverUrl.String,
		}
	}
	if row.PublishedDate.Valid {
		vi.PublishedDate = row.PublishedDate.Time.Format("2006-01-02")
	}
	if row.PageCount.Valid {
		vi.PageCount = int(row.PageCount.Int32)
	}
	if row.Language.Valid {
		vi.Language = row.Language.String
	}
	if row.Subtitle.Valid {
		vi.Subtitle = row.Subtitle.String
	}
	if row.Publisher.Valid {
		vi.Publisher = row.Publisher.String
	}
	if row.PreviewLink.Valid {
		vi.PreviewLink = row.PreviewLink.String
	}
	if row.AverageRating.Valid {
		v := row.AverageRating.Float64
		vi.AverageRating = &v
	}
	if row.RatingsCount.Valid {
		v := int(row.RatingsCount.Int32)
		vi.RatingsCount = &v
	}
	if row.Isbn13.Valid && row.Isbn13.String != "" {
		vi.IndustryIdentifiers = append(vi.IndustryIdentifiers, types.IndustryIdentifier{
			Type:       "ISBN_13",
			Identifier: row.Isbn13.String,
		})
	}

	stats := types.PaperboxdStats{}
	if row.LikeCount.Valid {
		v := int(row.LikeCount.Int32)
		stats.TotalLikes = &v
	}
	if row.TotalReadsCount.Valid {
		v := int(row.TotalReadsCount.Int32)
		stats.TotalReads = &v
	}
	if row.TotalTbrCount.Valid {
		v := int(row.TotalTbrCount.Int32)
		stats.TotalTBR = &v
	}

	resp := types.BookResponse{
		ID:             row.ID.String(),
		MongoID:        row.ID.String(),
		VolumeInfo:     vi,
		PaperboxdStats: stats,
		APISource:      "db",
		FromCache:      true,
		Slug:           row.Slug,
	}
	if row.GoogleBooksID.Valid {
		resp.GoogleBooksID = row.GoogleBooksID.String
	}
	if row.IsbndbID.Valid {
		resp.ISBNdbID = row.IsbndbID.String
	}
	if row.OpenLibraryID.Valid {
		resp.OpenLibraryID = row.OpenLibraryID.String
	}
	return resp
}

func likesRowToResponse(row db.GetUserLikesRow) types.BookResponse {
	vi := types.VolumeInfo{
		Title:      row.Title,
		Authors:    row.Authors,
		Categories: row.Categories,
	}
	if vi.Authors == nil {
		vi.Authors = []string{}
	}
	if vi.Categories == nil {
		vi.Categories = []string{}
	}
	if row.Description.Valid {
		vi.Description = row.Description.String
	}
	if row.CoverUrl.Valid {
		vi.ImageLinks = types.ImageLinks{
			Thumbnail: row.CoverUrl.String,
			Small:     row.CoverUrl.String,
			Medium:    row.CoverUrl.String,
		}
	}
	if row.PublishedDate.Valid {
		vi.PublishedDate = row.PublishedDate.Time.Format("2006-01-02")
	}
	if row.PageCount.Valid {
		vi.PageCount = int(row.PageCount.Int32)
	}
	if row.Language.Valid {
		vi.Language = row.Language.String
	}
	if row.Subtitle.Valid {
		vi.Subtitle = row.Subtitle.String
	}
	if row.Publisher.Valid {
		vi.Publisher = row.Publisher.String
	}
	if row.PreviewLink.Valid {
		vi.PreviewLink = row.PreviewLink.String
	}
	if row.AverageRating.Valid {
		v := row.AverageRating.Float64
		vi.AverageRating = &v
	}
	if row.RatingsCount.Valid {
		v := int(row.RatingsCount.Int32)
		vi.RatingsCount = &v
	}
	if row.Isbn13.Valid && row.Isbn13.String != "" {
		vi.IndustryIdentifiers = append(vi.IndustryIdentifiers, types.IndustryIdentifier{
			Type:       "ISBN_13",
			Identifier: row.Isbn13.String,
		})
	}

	stats := types.PaperboxdStats{}
	if row.LikeCount.Valid {
		v := int(row.LikeCount.Int32)
		stats.TotalLikes = &v
	}
	if row.TotalReadsCount.Valid {
		v := int(row.TotalReadsCount.Int32)
		stats.TotalReads = &v
	}
	if row.TotalTbrCount.Valid {
		v := int(row.TotalTbrCount.Int32)
		stats.TotalTBR = &v
	}

	resp := types.BookResponse{
		ID:             row.ID.String(),
		MongoID:        row.ID.String(),
		VolumeInfo:     vi,
		PaperboxdStats: stats,
		APISource:      "db",
		FromCache:      true,
		Slug:           row.Slug,
	}
	if row.GoogleBooksID.Valid {
		resp.GoogleBooksID = row.GoogleBooksID.String
	}
	if row.IsbndbID.Valid {
		resp.ISBNdbID = row.IsbndbID.String
	}
	if row.OpenLibraryID.Valid {
		resp.OpenLibraryID = row.OpenLibraryID.String
	}
	return resp
}
