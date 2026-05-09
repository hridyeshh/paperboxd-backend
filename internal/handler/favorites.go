package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FavoritesHandler holds dependencies for favorites endpoints.
type FavoritesHandler struct {
	Queries     *db.Queries
	Pool        *pgxpool.Pool
	ISBNdb      *external.ISBNdbClient
	GoogleBooks *external.GoogleBooksClient
}

// NewFavoritesHandler creates a FavoritesHandler.
func NewFavoritesHandler(pool *pgxpool.Pool, queries *db.Queries, isbndb *external.ISBNdbClient, google *external.GoogleBooksClient) *FavoritesHandler {
	return &FavoritesHandler{
		Queries:     queries,
		Pool:        pool,
		ISBNdb:      isbndb,
		GoogleBooks: google,
	}
}

// GetUserFavorites handles GET /api/v1/users/:username/favorites
func (h *FavoritesHandler) GetUserFavorites(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for favorites", "error", err)
		types.WriteInternalError(w)
		return
	}

	favorites, err := h.Queries.GetUserFavorites(r.Context(), target.ID)
	if err != nil {
		slog.Error("get user favorites", "error", err)
		types.WriteInternalError(w)
		return
	}

	resp := make([]types.FavoriteResponse, len(favorites))
	for i, fav := range favorites {
		resp[i] = favRowToResponse(fav)
	}
	types.WriteJSON(w, http.StatusOK, resp)
}

// AddToFavorites handles POST /api/v1/users/:username/favorites
func (h *FavoritesHandler) AddToFavorites(w http.ResponseWriter, r *http.Request) {
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

	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for add favorites", "error", err)
		types.WriteInternalError(w)
		return
	}
	if target.ID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return
	}

	var req types.AddToFavoritesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	count, err := h.Queries.CountUserFavorites(r.Context(), userID)
	if err != nil {
		slog.Error("count user favorites", "error", err)
		types.WriteInternalError(w)
		return
	}
	if count >= 4 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Maximum 4 favorites allowed. Please remove one before adding another.")
		return
	}

	// Auto-assign next slot when caller omits display_order.
	// We cannot use count+1 because removals leave gaps (e.g. positions 1,3
	// after position 2 was deleted → count=2, count+1=3 collides).
	if req.DisplayOrder < 1 || req.DisplayOrder > 4 {
		existingFavs, favErr := h.Queries.GetUserFavorites(r.Context(), userID)
		if favErr != nil {
			slog.Error("get favorites for slot assignment", "error", favErr)
			types.WriteInternalError(w)
			return
		}
		used := make(map[int]bool)
		for _, f := range existingFavs {
			used[int(f.DisplayOrder)] = true
		}
		req.DisplayOrder = 0
		for i := 1; i <= 4; i++ {
			if !used[i] {
				req.DisplayOrder = i
				break
			}
		}
		if req.DisplayOrder == 0 {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Maximum 4 favorites allowed. Please remove one before adding another.")
			return
		}
	}

	// Resolve book
	var book db.Book
	switch {
	case req.BookID != nil:
		id, parseErr := uuid.Parse(*req.BookID)
		if parseErr != nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid book_id")
			return
		}
		book, err = h.Queries.GetBookByID(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not found")
			return
		}
	case req.GoogleBooksID != nil:
		book, err = h.Queries.GetBookByGoogleID(r.Context(), pgtype.Text{String: *req.GoogleBooksID, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			book, err = cacheBookFromGoogleBooks(r.Context(), h.Queries, h.GoogleBooks, *req.GoogleBooksID)
		}
	case req.ISBN != nil:
		book, err = h.Queries.GetBookByISBN(r.Context(), pgtype.Text{String: *req.ISBN, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			book, err = cacheBookFromISBNdb(r.Context(), h.Queries, h.ISBNdb, *req.ISBN)
		}
	default:
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "provide book_id, isbn, or google_books_id")
		return
	}
	if err != nil {
		slog.Error("resolve book for favorites", "error", err)
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not found")
		return
	}

	exists, err := h.Queries.CheckFavoriteExists(r.Context(), db.CheckFavoriteExistsParams{
		UserID: userID,
		BookID: book.ID,
	})
	if err != nil {
		slog.Error("check favorite exists", "error", err)
		types.WriteInternalError(w)
		return
	}
	if exists {
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Book already in favorites")
		return
	}

	var noteParam pgtype.Text
	if req.Note != nil {
		noteParam = pgtype.Text{String: *req.Note, Valid: true}
	}

	fav, err := h.Queries.AddToFavorites(r.Context(), db.AddToFavoritesParams{
		UserID:       userID,
		BookID:       book.ID,
		DisplayOrder: int32(req.DisplayOrder),
		FavoriteNote: noteParam,
	})
	if err != nil {
		slog.Error("add to favorites", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() {
		_ = h.Queries.IncrementUserFavoritesCount(context.Background(), userID)
	}()

	types.WriteJSON(w, http.StatusCreated, fav)
}

// RemoveFromFavorites handles DELETE /api/v1/users/:username/favorites/:bookId
func (h *FavoritesHandler) RemoveFromFavorites(w http.ResponseWriter, r *http.Request) {
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

	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for remove favorites", "error", err)
		types.WriteInternalError(w)
		return
	}
	if target.ID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return
	}

	bookID, err := resolveBookIDParam(r.Context(), h.Queries, h.GoogleBooks, h.ISBNdb, chi.URLParam(r, "bookId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, err.Error())
		return
	}

	if err := h.Queries.RemoveFromFavorites(r.Context(), db.RemoveFromFavoritesParams{
		UserID: userID,
		BookID: bookID,
	}); err != nil {
		slog.Error("remove from favorites", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() {
		_ = h.Queries.DecrementUserFavoritesCount(context.Background(), userID)
	}()

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Removed from favorites"})
}

// ReorderFavorites handles PUT /api/v1/users/:username/favorites/reorder
func (h *FavoritesHandler) ReorderFavorites(w http.ResponseWriter, r *http.Request) {
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

	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for reorder favorites", "error", err)
		types.WriteInternalError(w)
		return
	}
	if target.ID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return
	}

	var req types.ReorderFavoritesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	if len(req.Favorites) == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "favorites list is empty")
		return
	}

	// Parse and validate all book IDs upfront before touching the DB.
	type entry struct {
		bookID       uuid.UUID
		displayOrder int32
		note         pgtype.Text
	}
	entries := make([]entry, 0, len(req.Favorites))
	for i, fav := range req.Favorites {
		bookID, parseErr := uuid.Parse(fav.BookID)
		if parseErr != nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "invalid book_id: "+fav.BookID)
			return
		}
		entries = append(entries, entry{bookID: bookID, displayOrder: int32(i + 1)})
	}

	// Fetch existing favorites to preserve notes.
	existing, err := h.Queries.GetUserFavorites(r.Context(), userID)
	if err != nil {
		slog.Error("get favorites for reorder", "error", err)
		types.WriteInternalError(w)
		return
	}
	noteMap := make(map[uuid.UUID]pgtype.Text, len(existing))
	for _, f := range existing {
		noteMap[f.BookID] = f.FavoriteNote
	}
	for i := range entries {
		entries[i].note = noteMap[entries[i].bookID]
	}

	// The UNIQUE(user_id, display_order) constraint makes sequential UPDATEs
	// conflict mid-loop (e.g. swapping positions 2 and 4 is impossible without
	// a temporary free slot). Fix: delete all then re-insert in new order inside
	// a single transaction so the constraint is satisfied at commit time.
	tx, err := h.Pool.Begin(r.Context())
	if err != nil {
		slog.Error("begin tx for reorder favorites", "error", err)
		types.WriteInternalError(w)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck

	qtx := h.Queries.WithTx(tx)

	for _, f := range existing {
		if err := qtx.RemoveFromFavorites(r.Context(), db.RemoveFromFavoritesParams{
			UserID: userID,
			BookID: f.BookID,
		}); err != nil {
			slog.Error("delete favorite for reorder", "error", err)
			types.WriteInternalError(w)
			return
		}
	}

	for _, e := range entries {
		if _, err := qtx.AddToFavorites(r.Context(), db.AddToFavoritesParams{
			UserID:       userID,
			BookID:       e.bookID,
			DisplayOrder: e.displayOrder,
			FavoriteNote: e.note,
		}); err != nil {
			slog.Error("re-insert favorite for reorder", "error", err)
			types.WriteInternalError(w)
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("commit reorder favorites", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Favorites reordered"})
}

// ── Row conversion ────────────────────────────────────────────────────────────

func favRowToResponse(row db.GetUserFavoritesRow) types.FavoriteResponse {
	resp := types.FavoriteResponse{
		ID:           row.ID.String(),
		BookID:       row.BookID.String(),
		DisplayOrder: int(row.DisplayOrder),
		Book:         favoritesBookToResponse(row),
		CreatedAt:    row.CreatedAt.Time,
	}
	if row.FavoriteNote.Valid {
		resp.Note = &row.FavoriteNote.String
	}
	return resp
}

func favoritesBookToResponse(row db.GetUserFavoritesRow) types.BookResponse {
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
