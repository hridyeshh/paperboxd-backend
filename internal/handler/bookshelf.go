package handler

import (
	"encoding/json"
	"errors"
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

// AddToBookshelf handles POST /api/v1/users/:username/bookshelf
func (h *UserHandler) AddToBookshelf(w http.ResponseWriter, r *http.Request) {
	userID, target, ok := h.resolveOwner(w, r)
	if !ok {
		return
	}
	_ = target

	var req types.AddToBookshelfRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	bookID, err := uuid.Parse(req.BookID)
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid book_id")
		return
	}

	if !validStatuses[req.Status] {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "status must be 'read', 'reading', or 'to-read'")
		return
	}

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
		if strings.Contains(err.Error(), "bookshelf_book_id_fkey") ||
			strings.Contains(err.Error(), "23503") {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not found")
			return
		}
		slog.Error("add to bookshelf", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, entry)
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
	resp := types.BookResponse{
		ID:      row.ID.String(),
		Title:   row.Title,
		Slug:    row.Slug,
		Authors: row.Authors,
	}
	if row.Description.Valid {
		resp.Description = row.Description.String
	}
	if row.CoverUrl.Valid {
		resp.CoverURL = row.CoverUrl.String
	}
	if row.Isbn13.Valid {
		resp.ISBN13 = row.Isbn13.String
	}
	if row.GoogleBooksID.Valid {
		resp.GoogleBooksID = row.GoogleBooksID.String
	}
	if row.PublishedDate.Valid {
		resp.PublishedDate = row.PublishedDate.Time.Format("2006-01-02")
	}
	if row.PageCount.Valid {
		resp.PageCount = int(row.PageCount.Int32)
	}
	if row.Language.Valid {
		resp.Language = row.Language.String
	}
	if row.ViewCount.Valid {
		resp.ViewCount = int(row.ViewCount.Int32)
	}
	if row.LikeCount.Valid {
		resp.LikeCount = int(row.LikeCount.Int32)
	}
	resp.Categories = row.Categories
	if resp.Categories == nil {
		resp.Categories = []string{}
	}
	if resp.Authors == nil {
		resp.Authors = []string{}
	}
	return resp
}

func likesRowToResponse(row db.GetUserLikesRow) types.BookResponse {
	resp := types.BookResponse{
		ID:      row.ID.String(),
		Title:   row.Title,
		Slug:    row.Slug,
		Authors: row.Authors,
	}
	if row.Description.Valid {
		resp.Description = row.Description.String
	}
	if row.CoverUrl.Valid {
		resp.CoverURL = row.CoverUrl.String
	}
	if row.Isbn13.Valid {
		resp.ISBN13 = row.Isbn13.String
	}
	if row.GoogleBooksID.Valid {
		resp.GoogleBooksID = row.GoogleBooksID.String
	}
	if row.PublishedDate.Valid {
		resp.PublishedDate = row.PublishedDate.Time.Format("2006-01-02")
	}
	if row.PageCount.Valid {
		resp.PageCount = int(row.PageCount.Int32)
	}
	if row.Language.Valid {
		resp.Language = row.Language.String
	}
	if row.ViewCount.Valid {
		resp.ViewCount = int(row.ViewCount.Int32)
	}
	if row.LikeCount.Valid {
		resp.LikeCount = int(row.LikeCount.Int32)
	}
	resp.Categories = row.Categories
	if resp.Categories == nil {
		resp.Categories = []string{}
	}
	if resp.Authors == nil {
		resp.Authors = []string{}
	}
	return resp
}
