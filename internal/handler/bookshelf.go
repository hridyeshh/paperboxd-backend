package handler

import (
	"context"
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
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
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
			book, err = cacheBookFromGoogleBooks(r.Context(), h.Queries, h.GoogleBooks, *req.GoogleBooksID)
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
			book, err = cacheBookFromISBNdb(r.Context(), h.Queries, h.ISBNdb, *req.ISBN)
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

	bID := bookID
	status := req.Status
	go func() {
		_, _ = h.Queries.CreateActivity(context.Background(), db.CreateActivityParams{
			UserID:       userID,
			ActivityType: "added_book",
			BookID:       pgtype.UUID{Bytes: bID, Valid: true},
		})
		xpSvc := service.NewXPService(h.Queries)
		switch status {
		case "read":
			_ = xpSvc.AwardXP(context.Background(), userID, "book_read", service.XPBookRead, &bID)
			_, _ = h.Queries.RebuildUserLeaderboardStats(context.Background(), userID)
		case "to-read":
			_ = xpSvc.AwardXP(context.Background(), userID, "add_to_tbr", service.XPAddToTBR, &bID)
		}
		if h.RecommendationService != nil {
			h.RecommendationService.InvalidateUserPool(context.Background(), userID.String())
		}
	}()

	types.WriteJSON(w, http.StatusOK, entry)
}

// cacheBookFromISBNdb fetches a book from ISBNdb by ISBN and persists it to the DB.
func cacheBookFromISBNdb(ctx context.Context, q *db.Queries, isbndb *external.ISBNdbClient, isbn string) (db.Book, error) {
	if isbndb == nil {
		return db.Book{}, fmt.Errorf("isbndb client not configured")
	}

	b, err := isbndb.GetByISBN(ctx, isbn)
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

	return q.CreateBookFromISBNdb(ctx, params)
}

// cacheBookFromGoogleBooks fetches a book from Google Books by volume ID and persists it to the DB.
func cacheBookFromGoogleBooks(ctx context.Context, q *db.Queries, gb *external.GoogleBooksClient, volumeID string) (db.Book, error) {
	if gb == nil {
		return db.Book{}, fmt.Errorf("google books client not configured")
	}

	book, err := gb.GetByID(ctx, volumeID)
	if err != nil {
		return db.Book{}, err
	}

	params := googleBookToCreateParams(book)
	return q.CreateBook(ctx, params)
}


// UpdateBookshelfRating handles PATCH /api/v1/users/:username/bookshelf/:bookId
func (h *UserHandler) UpdateBookshelfRating(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := h.resolveOwner(w, r)
	if !ok {
		return
	}

	bookID, err := resolveBookIDParam(r.Context(), h.Queries, h.GoogleBooks, h.ISBNdb, chi.URLParam(r, "bookId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	var req struct {
		Rating *int    `json:"rating"`
		Review *string `json:"review"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	if req.Rating != nil && (*req.Rating < 0 || *req.Rating > 5) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "rating must be between 0 and 5 (0 clears)")
		return
	}
	if req.Review != nil && len([]rune(*req.Review)) > 500 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "review must be 500 characters or fewer")
		return
	}

	params := db.UpdateBookshelfRatingParams{
		UserID: userID,
		BookID: bookID,
	}
	if req.Rating != nil && *req.Rating > 0 {
		params.Rating = pgtype.Int4{Int32: int32(*req.Rating), Valid: true}
	}
	if req.Review != nil {
		params.Review = pgtype.Text{String: *req.Review, Valid: true}
	}

	entry, err := h.Queries.UpdateBookshelfRating(r.Context(), params)
	if errors.Is(err, pgx.ErrNoRows) {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not in bookshelf")
		return
	}
	if err != nil {
		slog.Error("update bookshelf rating", "error", err)
		types.WriteInternalError(w)
		return
	}

	var ratingPtr *int
	if entry.Rating.Valid {
		v := int(entry.Rating.Int32)
		ratingPtr = &v
	}
	var reviewPtr *string
	if entry.Review.Valid {
		reviewPtr = &entry.Review.String
	}
	types.WriteJSON(w, http.StatusOK, map[string]any{
		"rating": ratingPtr,
		"review": reviewPtr,
		"edited": entry.ReviewEdited,
	})
}

// RemoveFromBookshelf handles DELETE /api/v1/users/:username/bookshelf/:bookId
func (h *UserHandler) RemoveFromBookshelf(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := h.resolveOwner(w, r)
	if !ok {
		return
	}

	bookID, err := resolveBookIDParam(r.Context(), h.Queries, h.GoogleBooks, h.ISBNdb, chi.URLParam(r, "bookId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
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

	if h.RecommendationService != nil {
		go h.RecommendationService.InvalidateUserPool(context.Background(), userID.String())
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

// ── TBR handlers ─────────────────────────────────────────────────────────────

// GetUserTBR handles GET /api/v1/users/:username/tbr
func (h *UserHandler) GetUserTBR(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for tbr", "error", err)
		types.WriteInternalError(w)
		return
	}

	rows, err := h.Queries.GetUserTBR(r.Context(), target.ID)
	if err != nil {
		slog.Error("get user tbr", "error", err)
		types.WriteInternalError(w)
		return
	}

	resp := make([]types.TBRResponse, len(rows))
	for i, row := range rows {
		resp[i] = tbrRowToResponse(row)
	}
	types.WriteJSON(w, http.StatusOK, resp)
}

// UpdateTBRNotes handles PUT /api/v1/users/:username/bookshelf/:bookId/tbr
func (h *UserHandler) UpdateTBRNotes(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := h.resolveOwner(w, r)
	if !ok {
		return
	}

	bookID, err := resolveBookIDParam(r.Context(), h.Queries, h.GoogleBooks, h.ISBNdb, chi.URLParam(r, "bookId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	var req types.UpdateTBRRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	if req.Priority != nil {
		switch *req.Priority {
		case "high", "medium", "low":
		default:
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "priority must be 'high', 'medium', or 'low'")
			return
		}
	}

	var notesParam, priorityParam pgtype.Text
	if req.Notes != nil {
		notesParam = pgtype.Text{String: *req.Notes, Valid: true}
	}
	if req.Priority != nil {
		priorityParam = pgtype.Text{String: *req.Priority, Valid: true}
	}

	entry, err := h.Queries.UpdateTBRNotes(r.Context(), db.UpdateTBRNotesParams{
		UserID:      userID,
		BookID:      bookID,
		TbrNotes:    notesParam,
		TbrPriority: priorityParam,
	})
	if err != nil {
		slog.Error("update tbr notes", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, entry)
}

// ── Currently Reading handlers ────────────────────────────────────────────────

// GetCurrentlyReading handles GET /api/v1/users/:username/reading
func (h *UserHandler) GetCurrentlyReading(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for currently reading", "error", err)
		types.WriteInternalError(w)
		return
	}

	rows, err := h.Queries.GetCurrentlyReading(r.Context(), target.ID)
	if err != nil {
		slog.Error("get currently reading", "error", err)
		types.WriteInternalError(w)
		return
	}

	resp := make([]types.CurrentlyReadingResponse, len(rows))
	for i, row := range rows {
		resp[i] = currentlyReadingRowToResponse(row)
	}
	types.WriteJSON(w, http.StatusOK, resp)
}

// UpdateReadingProgress handles PUT /api/v1/users/:username/bookshelf/:bookId/progress
func (h *UserHandler) UpdateReadingProgress(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := h.resolveOwner(w, r)
	if !ok {
		return
	}

	bookID, err := resolveBookIDParam(r.Context(), h.Queries, h.GoogleBooks, h.ISBNdb, chi.URLParam(r, "bookId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	var req types.UpdateReadingProgressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	// Read old page count before update so we can compute delta for reading_log.
	oldPage := int32(0)
	if oldEntry, err := h.Queries.GetBookshelfEntry(r.Context(), db.GetBookshelfEntryParams{
		UserID: userID, BookID: bookID,
	}); err == nil && oldEntry.CurrentPage.Valid {
		oldPage = oldEntry.CurrentPage.Int32
	}

	params := db.UpdateReadingProgressParams{UserID: userID, BookID: bookID}

	if req.CurrentPage != nil {
		params.CurrentPage = pgtype.Int4{Int32: *req.CurrentPage, Valid: true}

		// Calculate simple estimated finish (50 pages/day)
		book, bookErr := h.Queries.GetBookByID(r.Context(), bookID)
		if bookErr == nil && book.PageCount.Valid && book.PageCount.Int32 > 0 {
			pagesRemaining := book.PageCount.Int32 - *req.CurrentPage
			if pagesRemaining > 0 {
				params.ReadingVelocity = pgtype.Float8{Float64: 50.0, Valid: true}
				daysRemaining := int(pagesRemaining/50) + 1
				params.EstimatedFinishDate = pgtype.Date{Time: time.Now().AddDate(0, 0, daysRemaining), Valid: true}
			}
		}
	}

	entry, err := h.Queries.UpdateReadingProgress(r.Context(), params)
	if errors.Is(err, pgx.ErrNoRows) {
		// No bookshelf row yet — auto-add then retry.
		if _, addErr := h.Queries.AddToBookshelf(r.Context(), db.AddToBookshelfParams{
			UserID: userID,
			BookID: bookID,
			Status: "to-read",
		}); addErr != nil {
			slog.Error("auto-add to bookshelf for progress", "error", addErr)
			types.WriteInternalError(w)
			return
		}
		entry, err = h.Queries.UpdateReadingProgress(r.Context(), params)
	}
	if err != nil {
		slog.Error("update reading progress", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Keep bookshelf status in sync with reading progress:
	//   0 pages       → leave status as-is (user may have explicitly set it)
	//   1…(n-1) pages → "to-read" (shows in DNF section on profile)
	//   n pages       → "read"   (finished)
	if req.CurrentPage != nil {
		cp := *req.CurrentPage
		var newStatus string
		book, bookErr := h.Queries.GetBookByID(r.Context(), bookID)
		if bookErr == nil && book.PageCount.Valid && book.PageCount.Int32 > 0 {
			if cp >= book.PageCount.Int32 {
				newStatus = "read"
			} else if cp > 0 {
				newStatus = "to-read"
			}
		} else if cp > 0 {
			newStatus = "to-read"
		}
		if newStatus != "" && newStatus != entry.Status {
			if updated, statusErr := h.Queries.UpdateBookshelfStatus(r.Context(), db.UpdateBookshelfStatusParams{
				UserID: userID,
				BookID: bookID,
				Status: newStatus,
			}); statusErr == nil {
				entry = updated
			}
		}
	}

	// Log progress delta to reading_log for home-page stats.
	// Synchronous so insert failures surface in logs (was async goroutine that
	// silently swallowed errors when the reading_log table wasn't migrated).
	if req.CurrentPage != nil && *req.CurrentPage > oldPage {
		delta := *req.CurrentPage - oldPage
		if logErr := h.Queries.LogReadingProgress(r.Context(), db.LogReadingProgressParams{
			UserID:     userID,
			BookID:     bookID,
			PagesDelta: delta,
		}); logErr != nil {
			slog.Error("log reading progress", "error", logErr, "user_id", userID, "book_id", bookID, "delta", delta)
		}
	}

	bID := bookID
	go func() {
		xpSvc := service.NewXPService(h.Queries)
		_ = xpSvc.AwardXP(context.Background(), userID, "read_progress", service.XPReadProgress, &bID)
	}()

	types.WriteJSON(w, http.StatusOK, entry)
}

// MarkAsStarted handles POST /api/v1/users/:username/bookshelf/:bookId/start
func (h *UserHandler) MarkAsStarted(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := h.resolveOwner(w, r)
	if !ok {
		return
	}

	bookID, err := resolveBookIDParam(r.Context(), h.Queries, h.GoogleBooks, h.ISBNdb, chi.URLParam(r, "bookId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	entry, err := h.Queries.MarkAsStarted(r.Context(), db.MarkAsStartedParams{
		UserID: userID,
		BookID: bookID,
	})
	if err != nil {
		slog.Error("mark as started", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, entry)
}

// MarkAsFinished handles POST /api/v1/users/:username/bookshelf/:bookId/finish
func (h *UserHandler) MarkAsFinished(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := h.resolveOwner(w, r)
	if !ok {
		return
	}

	bookID, err := resolveBookIDParam(r.Context(), h.Queries, h.GoogleBooks, h.ISBNdb, chi.URLParam(r, "bookId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	entry, err := h.Queries.MarkAsFinished(r.Context(), db.MarkAsFinishedParams{
		UserID: userID,
		BookID: bookID,
	})
	if err != nil {
		slog.Error("mark as finished", "error", err)
		types.WriteInternalError(w)
		return
	}

	bID := bookID
	go func() {
		xpSvc := service.NewXPService(h.Queries)
		_ = xpSvc.AwardXP(context.Background(), userID, "book_read", service.XPBookRead, &bID)
		_, _ = h.Queries.RebuildUserLeaderboardStats(context.Background(), userID)
		count, _ := h.Queries.CountUserBooks(context.Background(), db.CountUserBooksParams{UserID: userID, Status: "read"})
		if count == 1 {
			refSvc := service.NewReferralService(h.Queries, xpSvc)
			_ = refSvc.CheckAndAwardReferralMilestone(context.Background(), userID, "first_book")
		}
		if h.RecommendationService != nil {
			h.RecommendationService.InvalidateUserPool(context.Background(), userID.String())
		}
	}()

	types.WriteJSON(w, http.StatusOK, entry)
}

// ── Row conversion helpers ────────────────────────────────────────────────────

func currentlyReadingRowToResponse(row db.GetCurrentlyReadingRow) types.CurrentlyReadingResponse {
	bookResp := currentlyReadingBookToResponse(row)

	var progressPct float64
	var pagesRemaining int32
	var currentPagePtr *int32
	if row.CurrentPage.Valid {
		cp := row.CurrentPage.Int32
		currentPagePtr = &cp
		if row.PageCount.Valid && row.PageCount.Int32 > 0 {
			progressPct = float64(cp) / float64(row.PageCount.Int32) * 100
			rem := row.PageCount.Int32 - cp
			if rem < 0 {
				rem = 0
			}
			pagesRemaining = rem
		}
	}

	resp := types.CurrentlyReadingResponse{
		ID:                 row.ID.String(),
		BookID:             row.BookID.String(),
		Book:               bookResp,
		Status:             row.Status,
		CurrentPage:        currentPagePtr,
		ProgressPercentage: progressPct,
		PagesRemaining:     pagesRemaining,
		CreatedAt:          row.CreatedAt.Time,
		UpdatedAt:          row.UpdatedAt.Time,
	}
	if row.EstimatedFinishDate.Valid {
		t := row.EstimatedFinishDate.Time
		resp.EstimatedFinishDate = &t
	}
	if row.StartedAt.Valid {
		t := row.StartedAt.Time
		resp.StartedAt = &t
	}
	return resp
}

func tbrRowToResponse(row db.GetUserTBRRow) types.TBRResponse {
	bookResp := tbrBookToResponse(row)
	resp := types.TBRResponse{
		ID:        row.ID.String(),
		BookID:    row.BookID.String(),
		Book:      bookResp,
		Status:    row.Status,
		CreatedAt: row.CreatedAt.Time,
	}
	if row.TbrNotes.Valid {
		resp.TBRNotes = &row.TbrNotes.String
	}
	if row.TbrPriority.Valid {
		resp.TBRPriority = &row.TbrPriority.String
	}
	if row.TbrAddedAt.Valid {
		t := row.TbrAddedAt.Time
		resp.TBRAddedAt = &t
	}
	if row.CurrentPage.Valid {
		cp := row.CurrentPage.Int32
		resp.CurrentPage = &cp
	}
	return resp
}

func currentlyReadingBookToResponse(row db.GetCurrentlyReadingRow) types.BookResponse {
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
			Type: "ISBN_13", Identifier: row.Isbn13.String,
		})
	}
	resp := types.BookResponse{
		ID:        row.BookID.String(),
		MongoID:   row.BookID.String(),
		VolumeInfo: vi,
		APISource: "db",
		FromCache: true,
		Slug:      row.Slug,
	}
	if row.GoogleBooksID.Valid {
		resp.GoogleBooksID = row.GoogleBooksID.String
	}
	if row.IsbndbID.Valid {
		resp.ISBNdbID = row.IsbndbID.String
	}
	return resp
}

func tbrBookToResponse(row db.GetUserTBRRow) types.BookResponse {
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
			Type: "ISBN_13", Identifier: row.Isbn13.String,
		})
	}
	resp := types.BookResponse{
		ID:        row.BookID.String(),
		MongoID:   row.BookID.String(),
		VolumeInfo: vi,
		APISource: "db",
		FromCache: true,
		Slug:      row.Slug,
	}
	if row.GoogleBooksID.Valid {
		resp.GoogleBooksID = row.GoogleBooksID.String
	}
	if row.IsbndbID.Valid {
		resp.ISBNdbID = row.IsbndbID.String
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
