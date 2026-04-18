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
)

// FavoritesHandler holds dependencies for favorites endpoints.
type FavoritesHandler struct {
	Queries     *db.Queries
	ISBNdb      *external.ISBNdbClient
	GoogleBooks *external.GoogleBooksClient
}

// NewFavoritesHandler creates a FavoritesHandler.
func NewFavoritesHandler(queries *db.Queries, isbndb *external.ISBNdbClient, google *external.GoogleBooksClient) *FavoritesHandler {
	return &FavoritesHandler{
		Queries:     queries,
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

	// Auto-assign next slot when caller omits display_order
	if req.DisplayOrder < 1 || req.DisplayOrder > 4 {
		req.DisplayOrder = int(count) + 1
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

	for _, fav := range req.Favorites {
		if fav.DisplayOrder < 1 || fav.DisplayOrder > 4 {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "display_order must be between 1 and 4")
			return
		}
	}

	for _, fav := range req.Favorites {
		bookID, parseErr := uuid.Parse(fav.BookID)
		if parseErr != nil {
			continue
		}
		existing, favErr := h.Queries.GetFavoriteByUserAndBook(r.Context(), db.GetFavoriteByUserAndBookParams{
			UserID: userID,
			BookID: bookID,
		})
		if favErr != nil {
			slog.Warn("get favorite for reorder", "book_id", fav.BookID, "error", favErr)
			continue
		}
		if reorderErr := h.Queries.ReorderFavorites(r.Context(), db.ReorderFavoritesParams{
			ID:           existing.ID,
			DisplayOrder: int32(fav.DisplayOrder),
		}); reorderErr != nil {
			slog.Warn("reorder favorite", "error", reorderErr)
		}
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
