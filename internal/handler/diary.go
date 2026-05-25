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
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// DiaryHandler holds dependencies for diary entry endpoints.
type DiaryHandler struct {
	Queries               *db.Queries
	ISBNdb                *external.ISBNdbClient
	GoogleBooks           *external.GoogleBooksClient
	RecommendationService *service.RecommendationService
}

// NewDiaryHandler creates a DiaryHandler.
func NewDiaryHandler(queries *db.Queries, isbndb *external.ISBNdbClient, google *external.GoogleBooksClient, rec *service.RecommendationService) *DiaryHandler {
	return &DiaryHandler{
		Queries:               queries,
		ISBNdb:                isbndb,
		GoogleBooks:           google,
		RecommendationService: rec,
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// uuidToPgtype converts a uuid.UUID to pgtype.UUID.
func uuidToPgtype(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// resolveBookForDiary resolves a book from the request (book_id / isbn / google_books_id).
// Returns pgtype.UUID{} (null) if no book fields are set.
func (h *DiaryHandler) resolveBookForDiary(ctx context.Context, bookID, isbn, googleBooksID *string) (pgtype.UUID, error) {
	switch {
	case bookID != nil:
		id, err := uuid.Parse(*bookID)
		if err != nil {
			return pgtype.UUID{}, errors.New("invalid book_id")
		}
		if _, err := h.Queries.GetBookByID(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return pgtype.UUID{}, errors.New("book not found")
			}
			return pgtype.UUID{}, err
		}
		return uuidToPgtype(id), nil

	case googleBooksID != nil:
		book, err := h.Queries.GetBookByGoogleID(ctx, pgtype.Text{String: *googleBooksID, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			book, err = cacheBookFromGoogleBooks(ctx, h.Queries, h.GoogleBooks, *googleBooksID, nil)
		}
		if err != nil {
			return pgtype.UUID{}, err
		}
		return uuidToPgtype(book.ID), nil

	case isbn != nil:
		book, err := h.Queries.GetBookByISBN(ctx, pgtype.Text{String: *isbn, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			book, err = cacheBookFromISBNdb(ctx, h.Queries, h.ISBNdb, *isbn, nil)
		}
		if err != nil {
			return pgtype.UUID{}, err
		}
		return uuidToPgtype(book.ID), nil
	}

	return pgtype.UUID{}, nil // no book
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// CreateDiaryEntry handles POST /api/v1/users/:username/diary.
func (h *DiaryHandler) CreateDiaryEntry(w http.ResponseWriter, r *http.Request) {
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
		slog.Error("get user for diary", "error", err)
		types.WriteInternalError(w)
		return
	}
	if target.ID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return
	}

	var req types.CreateDiaryEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	if len(req.Content) == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "content is required")
		return
	}
	if req.Title != nil && len(*req.Title) > 100 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "title must be max 100 characters")
		return
	}
	if req.Rating != nil && (*req.Rating < 1 || *req.Rating > 5) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "rating must be between 1 and 5")
		return
	}

	bookPgID, err := h.resolveBookForDiary(r.Context(), req.BookID, req.ISBN, req.GoogleBooksID)
	if err != nil {
		slog.Error("resolve book for diary", "error", err)
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not found")
		return
	}

	var titleParam pgtype.Text
	if req.Title != nil {
		titleParam = pgtype.Text{String: *req.Title, Valid: true}
	}
	var ratingParam pgtype.Int4
	if req.Rating != nil {
		ratingParam = pgtype.Int4{Int32: int32(*req.Rating), Valid: true}
	}

	entry, err := h.Queries.CreateDiaryEntry(r.Context(), db.CreateDiaryEntryParams{
		UserID:    userID,
		BookID:    bookPgID,
		Title:     titleParam,
		Content:   req.Content,
		IsPrivate: req.IsPrivate,
		Rating:    ratingParam,
	})
	if err != nil {
		slog.Error("create diary entry", "error", err)
		types.WriteInternalError(w)
		return
	}

	entryID := entry.ID
	isBookSpecific := entry.BookID.Valid
	go func() {
		_ = h.Queries.IncrementUserDiaryCount(context.Background(), userID)
		if !req.IsPrivate {
			entryPgID := uuidToPgtype(entryID)
			_, _ = h.Queries.CreateActivity(context.Background(), db.CreateActivityParams{
				UserID:       userID,
				ActivityType: "created_diary_entry",
				BookID:       bookPgID,
				EntryID:      entryPgID,
			})
		}
		xpSvc := service.NewXPService(h.Queries)
		xpAmount := service.XPDiaryEntry
		if isBookSpecific {
			xpAmount = service.XPBookDiary
		}
		_ = xpSvc.AwardXP(context.Background(), userID, "diary_entry", xpAmount, &entryID)
		_, _ = h.Queries.RebuildUserLeaderboardStats(context.Background(), userID)
	}()

	if h.RecommendationService != nil && entry.BookID.Valid {
		entryIDStr := entryID.String()
		bookUUID := entry.BookID.Bytes
		content := req.Content
		go func() {
			ctx := context.Background()
			var bookTitle string
			var bookAuthors []string
			if book, err := h.Queries.GetBookByID(ctx, bookUUID); err == nil {
				bookTitle = book.Title
				bookAuthors = book.Authors
			}
			h.RecommendationService.EmbedDiaryEntryAsync(entryIDStr, bookTitle, bookAuthors, content)
		}()
	}

	likesCount, _ := h.Queries.CountEntryLikes(r.Context(), entry.ID)
	resp := diaryEntryToResponse(entry, username, target.Name.String, target.AvatarUrl, likesCount, false, true)
	types.WriteJSON(w, http.StatusCreated, resp)
}

// GetDiaryEntry handles GET /api/v1/users/:username/diary/:entryId.
func (h *DiaryHandler) GetDiaryEntry(w http.ResponseWriter, r *http.Request) {
	entryID, err := uuid.Parse(chi.URLParam(r, "entryId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid entry ID")
		return
	}

	requesterID := optionalUserID(r)

	entry, err := h.Queries.GetDiaryEntryByID(r.Context(), entryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Entry not found")
			return
		}
		slog.Error("get diary entry", "error", err)
		types.WriteInternalError(w)
		return
	}

	if entry.IsPrivate {
		if requesterID == nil || *requesterID != entry.UserID {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Entry not found")
			return
		}
	}

	owner, err := h.Queries.GetUserByID(r.Context(), entry.UserID)
	if err != nil {
		slog.Error("get owner for diary entry", "error", err)
		types.WriteInternalError(w)
		return
	}

	likesCount, _ := h.Queries.CountEntryLikes(r.Context(), entry.ID)
	isLiked := false
	if requesterID != nil && *requesterID != entry.UserID {
		isLiked, _ = h.Queries.CheckEntryLiked(r.Context(), db.CheckEntryLikedParams{
			UserID:  *requesterID,
			EntryID: entry.ID,
		})
	}
	canEdit := requesterID != nil && *requesterID == entry.UserID

	resp := diaryEntryToResponse(entry, owner.Username, owner.Name.String, owner.AvatarUrl, likesCount, isLiked, canEdit)
	types.WriteJSON(w, http.StatusOK, resp)
}

// GetUserDiaryEntries handles GET /api/v1/users/:username/diary.
func (h *DiaryHandler) GetUserDiaryEntries(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for diary entries", "error", err)
		types.WriteInternalError(w)
		return
	}

	requesterID := optionalUserID(r)
	viewerID := uuid.Nil
	if requesterID != nil {
		viewerID = *requesterID
	}

	page, pageSize := parsePagination(r)
	offset := int32((page - 1) * pageSize)

	rows, err := h.Queries.GetUserDiaryEntries(r.Context(), db.GetUserDiaryEntriesParams{
		UserID:   target.ID,
		UserID_2: viewerID,
		Limit:    int32(pageSize),
		Offset:   offset,
	})
	if err != nil {
		slog.Error("get user diary entries", "error", err)
		types.WriteInternalError(w)
		return
	}

	totalCount, _ := h.Queries.CountUserDiaryEntries(r.Context(), target.ID)

	entries := make([]types.DiaryEntryResponse, len(rows))
	for i, row := range rows {
		resp := types.DiaryEntryResponse{
			ID:        row.ID.String(),
			UserID:    row.UserID.String(),
			Username:  username,
			Name:      target.Name.String,
			Content:   row.Content,
			IsPrivate: row.IsPrivate,
			CanEdit:   requesterID != nil && *requesterID == target.ID,
			CreatedAt: row.CreatedAt.Time,
			UpdatedAt: row.UpdatedAt.Time,
		}
		if target.AvatarUrl.Valid {
			resp.AvatarURL = &target.AvatarUrl.String
		}
		if row.Title.Valid {
			resp.Title = &row.Title.String
		}
		if row.Rating.Valid {
			v := int(row.Rating.Int32)
			resp.Rating = &v
		}
		if row.BookID.Valid {
			idStr := uuid.UUID(row.BookID.Bytes).String()
			resp.BookID = &idStr
			if row.BookTitle.Valid {
				resp.Book = &types.BookResponse{
					ID:      idStr,
					MongoID: idStr,
					Slug:    row.BookSlug.String,
					VolumeInfo: types.VolumeInfo{
						Title:   row.BookTitle.String,
						Authors: row.BookAuthors,
					},
				}
				if row.BookCoverUrl.Valid {
					resp.Book.VolumeInfo.ImageLinks = types.ImageLinks{
						Thumbnail: row.BookCoverUrl.String,
					}
				}
			}
		}
		entries[i] = resp
	}

	types.WriteJSON(w, http.StatusOK, types.DiaryEntriesResponse{
		Entries:    entries,
		TotalCount: totalCount,
		Page:       page,
		PageSize:   pageSize,
	})
}

// GetBookDiaryEntries handles GET /api/v1/books/:id/diary.
func (h *DiaryHandler) GetBookDiaryEntries(w http.ResponseWriter, r *http.Request) {
	bookID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid book ID")
		return
	}

	requesterID := optionalUserID(r)
	page, pageSize := parsePagination(r)

	rows, err := h.Queries.GetBookDiaryEntries(r.Context(), db.GetBookDiaryEntriesParams{
		BookID: uuidToPgtype(bookID),
		Limit:  int32(pageSize),
		Offset: int32((page - 1) * pageSize),
	})
	if err != nil {
		slog.Error("get book diary entries", "error", err)
		types.WriteInternalError(w)
		return
	}

	entries := make([]types.DiaryEntryResponse, len(rows))
	for i, row := range rows {
		resp := types.DiaryEntryResponse{
			ID:        row.ID.String(),
			UserID:    row.UserID.String(),
			Username:  row.Username,
			Name:      row.Name.String,
			Content:   row.Content,
			IsPrivate: false,
			CanEdit:   requesterID != nil && *requesterID == row.UserID,
			CreatedAt: row.CreatedAt.Time,
			UpdatedAt: row.UpdatedAt.Time,
		}
		if row.AvatarUrl.Valid {
			resp.AvatarURL = &row.AvatarUrl.String
		}
		if row.Title.Valid {
			resp.Title = &row.Title.String
		}
		if row.Rating.Valid {
			v := int(row.Rating.Int32)
			resp.Rating = &v
		}
		if row.BookID.Valid {
			idStr := uuid.UUID(row.BookID.Bytes).String()
			resp.BookID = &idStr
		}
		entries[i] = resp
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"entries":   entries,
		"page":      page,
		"page_size": pageSize,
	})
}

// UpdateDiaryEntry handles PUT /api/v1/users/:username/diary/:entryId.
func (h *DiaryHandler) UpdateDiaryEntry(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	userID, _ := uuid.Parse(userIDStr)

	entryID, err := uuid.Parse(chi.URLParam(r, "entryId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid entry ID")
		return
	}

	entry, err := h.Queries.GetDiaryEntryByID(r.Context(), entryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Entry not found")
			return
		}
		slog.Error("get diary entry for update", "error", err)
		types.WriteInternalError(w)
		return
	}
	if entry.UserID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return
	}

	var req types.UpdateDiaryEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}
	if req.Rating != nil && (*req.Rating < 1 || *req.Rating > 5) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "rating must be between 1 and 5")
		return
	}
	if req.Title != nil && len(*req.Title) > 100 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "title must be max 100 characters")
		return
	}

	// Merge updates onto current values
	titleParam := entry.Title
	if req.Title != nil {
		titleParam = pgtype.Text{String: *req.Title, Valid: true}
	}
	content := entry.Content
	if req.Content != nil {
		if len(*req.Content) == 0 {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "content cannot be empty")
			return
		}
		content = *req.Content
	}
	isPrivate := entry.IsPrivate
	if req.IsPrivate != nil {
		isPrivate = *req.IsPrivate
	}
	ratingParam := entry.Rating
	if req.Rating != nil {
		ratingParam = pgtype.Int4{Int32: int32(*req.Rating), Valid: true}
	}

	updated, err := h.Queries.UpdateDiaryEntry(r.Context(), db.UpdateDiaryEntryParams{
		ID:        entryID,
		Title:     titleParam,
		Content:   content,
		IsPrivate: isPrivate,
		Rating:    ratingParam,
	})
	if err != nil {
		slog.Error("update diary entry", "error", err)
		types.WriteInternalError(w)
		return
	}

	owner, _ := h.Queries.GetUserByID(r.Context(), userID)
	likesCount, _ := h.Queries.CountEntryLikes(r.Context(), entryID)
	resp := diaryEntryToResponse(updated, owner.Username, owner.Name.String, owner.AvatarUrl, likesCount, false, true)
	types.WriteJSON(w, http.StatusOK, resp)
}

// DeleteDiaryEntry handles DELETE /api/v1/users/:username/diary/:entryId.
func (h *DiaryHandler) DeleteDiaryEntry(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	userID, _ := uuid.Parse(userIDStr)

	entryID, err := uuid.Parse(chi.URLParam(r, "entryId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid entry ID")
		return
	}

	ownerID, err := h.Queries.CheckEntryOwnership(r.Context(), entryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Entry not found")
			return
		}
		slog.Error("check entry ownership", "error", err)
		types.WriteInternalError(w)
		return
	}
	if ownerID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return
	}

	if err := h.Queries.DeleteDiaryEntry(r.Context(), entryID); err != nil {
		slog.Error("delete diary entry", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() {
		_ = h.Queries.DecrementUserDiaryCount(context.Background(), userID)
	}()

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Entry deleted"})
}

// LikeDiaryEntry handles POST /api/v1/users/:username/diary/:entryId/like.
func (h *DiaryHandler) LikeDiaryEntry(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	userID, _ := uuid.Parse(userIDStr)

	entryID, err := uuid.Parse(chi.URLParam(r, "entryId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid entry ID")
		return
	}

	entry, err := h.Queries.GetDiaryEntryByID(r.Context(), entryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Entry not found")
			return
		}
		slog.Error("get diary entry for like", "error", err)
		types.WriteInternalError(w)
		return
	}
	if entry.IsPrivate && entry.UserID != userID {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Entry not found")
		return
	}
	already, err := h.Queries.CheckEntryLiked(r.Context(), db.CheckEntryLikedParams{
		UserID:  userID,
		EntryID: entryID,
	})
	if err != nil {
		slog.Error("check entry liked", "error", err)
		types.WriteInternalError(w)
		return
	}
	if already {
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Already liked")
		return
	}

	if _, err := h.Queries.LikeDiaryEntry(r.Context(), db.LikeDiaryEntryParams{
		UserID:  userID,
		EntryID: entryID,
	}); err != nil {
		slog.Error("like diary entry", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() {
		ownerID := entry.UserID
		_, _ = h.Queries.CreateActivity(context.Background(), db.CreateActivityParams{
			UserID:       userID,
			ActivityType: "liked_diary_entry",
			EntryID:      uuidToPgtype(entryID),
			TargetUserID: uuidToPgtype(ownerID),
		})
		xpSvc := service.NewXPService(h.Queries)
		eid := entryID
		_ = xpSvc.AwardXP(context.Background(), ownerID, "diary_liked", service.XPDiaryLiked, &eid)
	}()

	likesCount, _ := h.Queries.CountEntryLikes(r.Context(), entryID)
	types.WriteJSON(w, http.StatusCreated, map[string]any{
		"message":     "Entry liked",
		"likes_count": likesCount,
	})
}

// UnlikeDiaryEntry handles DELETE /api/v1/users/:username/diary/:entryId/like.
func (h *DiaryHandler) UnlikeDiaryEntry(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	userID, _ := uuid.Parse(userIDStr)

	entryID, err := uuid.Parse(chi.URLParam(r, "entryId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid entry ID")
		return
	}

	if err := h.Queries.UnlikeDiaryEntry(r.Context(), db.UnlikeDiaryEntryParams{
		UserID:  userID,
		EntryID: entryID,
	}); err != nil {
		slog.Error("unlike diary entry", "error", err)
		types.WriteInternalError(w)
		return
	}

	likesCount, _ := h.Queries.CountEntryLikes(r.Context(), entryID)
	types.WriteJSON(w, http.StatusOK, map[string]any{
		"message":     "Entry unliked",
		"likes_count": likesCount,
	})
}

// ── Row converters ────────────────────────────────────────────────────────────

func diaryEntryToResponse(entry db.DiaryEntry, username, name string, avatarUrl pgtype.Text, likesCount int64, isLiked, canEdit bool) types.DiaryEntryResponse {
	resp := types.DiaryEntryResponse{
		ID:         entry.ID.String(),
		UserID:     entry.UserID.String(),
		Username:   username,
		Name:       name,
		Content:    entry.Content,
		IsPrivate:  entry.IsPrivate,
		LikesCount: likesCount,
		IsLiked:    isLiked,
		CanEdit:    canEdit,
		CreatedAt:  entry.CreatedAt.Time,
		UpdatedAt:  entry.UpdatedAt.Time,
	}
	if avatarUrl.Valid {
		resp.AvatarURL = &avatarUrl.String
	}
	if entry.Title.Valid {
		resp.Title = &entry.Title.String
	}
	if entry.Rating.Valid {
		v := int(entry.Rating.Int32)
		resp.Rating = &v
	}
	if entry.BookID.Valid {
		idStr := uuid.UUID(entry.BookID.Bytes).String()
		resp.BookID = &idStr
	}
	return resp
}
