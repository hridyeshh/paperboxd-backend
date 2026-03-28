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

// ListsHandler holds dependencies for list endpoints.
type ListsHandler struct {
	Queries     *db.Queries
	ISBNdb      *external.ISBNdbClient
	GoogleBooks *external.GoogleBooksClient
}

// NewListsHandler creates a ListsHandler.
func NewListsHandler(queries *db.Queries, isbndb *external.ISBNdbClient, google *external.GoogleBooksClient) *ListsHandler {
	return &ListsHandler{
		Queries:     queries,
		ISBNdb:      isbndb,
		GoogleBooks: google,
	}
}

// ── Authorization helpers ─────────────────────────────────────────────────────

// resolveListOwner parses listId from the URL and verifies the authenticated user owns it.
// Returns listID, ownerUserID, ok.
func (h *ListsHandler) resolveListOwner(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return uuid.UUID{}, uuid.UUID{}, false
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return uuid.UUID{}, uuid.UUID{}, false
	}

	listID, err := uuid.Parse(chi.URLParam(r, "listId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid list ID")
		return uuid.UUID{}, uuid.UUID{}, false
	}

	ownerID, err := h.Queries.CheckListOwnership(r.Context(), listID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "List not found")
			return uuid.UUID{}, uuid.UUID{}, false
		}
		slog.Error("check list ownership", "error", err)
		types.WriteInternalError(w)
		return uuid.UUID{}, uuid.UUID{}, false
	}

	if ownerID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return uuid.UUID{}, uuid.UUID{}, false
	}

	return listID, userID, true
}

// optionalUserID extracts the authenticated user ID from context without requiring auth.
func optionalUserID(r *http.Request) *uuid.UUID {
	idStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		return nil
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil
	}
	return &id
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// CreateList handles POST /api/v1/users/:username/lists.
func (h *ListsHandler) CreateList(w http.ResponseWriter, r *http.Request) {
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
		slog.Error("get user for create list", "error", err)
		types.WriteInternalError(w)
		return
	}
	if target.ID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "Forbidden")
		return
	}

	var req types.CreateListRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	if len(req.Title) == 0 || len(req.Title) > 50 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "title must be 1–50 characters")
		return
	}
	if req.Description != nil && len(*req.Description) > 200 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "description must be max 200 characters")
		return
	}

	var descParam pgtype.Text
	if req.Description != nil {
		descParam = pgtype.Text{String: *req.Description, Valid: true}
	}

	list, err := h.Queries.CreateList(r.Context(), db.CreateListParams{
		UserID:      userID,
		Title:       req.Title,
		Description: descParam,
		IsPrivate:   req.IsPrivate,
	})
	if err != nil {
		slog.Error("create list", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() {
		_ = h.Queries.IncrementUserListsCount(context.Background(), userID)
	}()

	types.WriteJSON(w, http.StatusCreated, listToResponse(list, username, 0, 0, false, true, true))
}

// GetUserLists handles GET /api/v1/users/:username/lists.
func (h *ListsHandler) GetUserLists(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for lists", "error", err)
		types.WriteInternalError(w)
		return
	}

	requesterID := optionalUserID(r)
	isOwner := requesterID != nil && *requesterID == target.ID

	page, pageSize := parsePagination(r)
	offset := int32((page - 1) * pageSize)
	limit := int32(pageSize)

	ownRows, err := h.Queries.GetUserLists(r.Context(), db.GetUserListsParams{
		UserID: target.ID,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		slog.Error("get user lists", "error", err)
		types.WriteInternalError(w)
		return
	}

	ownLists := make([]types.ListResponse, 0, len(ownRows))
	for _, row := range ownRows {
		// Filter private lists for non-owners
		if row.IsPrivate && !isOwner {
			if requesterID == nil {
				continue
			}
			hasAccess, accessErr := h.Queries.CheckListAccess(r.Context(), db.CheckListAccessParams{
				ListID: row.ID,
				UserID: *requesterID,
			})
			if accessErr != nil || !hasAccess {
				continue
			}
		}
		isSaved := false
		if requesterID != nil && !isOwner {
			isSaved, _ = h.Queries.CheckListSaved(r.Context(), db.CheckListSavedParams{
				UserID: *requesterID,
				ListID: row.ID,
			})
		}
		canEdit := isOwner
		ownLists = append(ownLists, listRowToResponse(row, username, isSaved, canEdit))
	}

	savedRows, err := h.Queries.GetUserSavedLists(r.Context(), db.GetUserSavedListsParams{
		UserID: target.ID,
		Limit:  limit,
		Offset: 0,
	})
	if err != nil {
		slog.Error("get saved lists", "error", err)
		types.WriteInternalError(w)
		return
	}

	savedLists := make([]types.ListResponse, 0, len(savedRows))
	for _, row := range savedRows {
		if row.IsPrivate && !isOwner {
			canAccess := false
			if requesterID != nil {
				canAccess, _ = h.Queries.CheckListAccess(r.Context(), db.CheckListAccessParams{
					ListID: row.ID,
					UserID: *requesterID,
				})
				if !canAccess && *requesterID != row.UserID {
					continue
				}
			} else {
				continue
			}
		}
		savedLists = append(savedLists, savedListRowToResponse(row, isOwner))
	}

	types.WriteJSON(w, http.StatusOK, types.UserListsResponse{
		OwnLists:   ownLists,
		SavedLists: savedLists,
	})
}

// GetListDetails handles GET /api/v1/users/:username/lists/:listId.
func (h *ListsHandler) GetListDetails(w http.ResponseWriter, r *http.Request) {
	listID, err := uuid.Parse(chi.URLParam(r, "listId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid list ID")
		return
	}

	username := chi.URLParam(r, "username")
	requesterID := optionalUserID(r)

	list, err := h.Queries.GetListByID(r.Context(), listID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "List not found")
			return
		}
		slog.Error("get list by id", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Check access
	if list.IsPrivate {
		canAccess := false
		if requesterID != nil {
			canAccess, err = h.Queries.CheckCanAccessList(r.Context(), db.CheckCanAccessListParams{
				ID:     listID,
				UserID: *requesterID,
			})
			if err != nil {
				slog.Error("check can access list", "error", err)
				types.WriteInternalError(w)
				return
			}
		}
		if !canAccess {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "List not found")
			return
		}
	}

	books, err := h.Queries.GetListBooks(r.Context(), listID)
	if err != nil {
		slog.Error("get list books", "error", err)
		types.WriteInternalError(w)
		return
	}

	saveCount, _ := h.Queries.CountListSaves(r.Context(), listID)

	isSaved := false
	if requesterID != nil && *requesterID != list.UserID {
		isSaved, _ = h.Queries.CheckListSaved(r.Context(), db.CheckListSavedParams{
			UserID: *requesterID,
			ListID: listID,
		})
	}

	isOwner := requesterID != nil && *requesterID == list.UserID
	canEdit := isOwner

	bookResponses := make([]types.BookResponse, len(books))
	for i, b := range books {
		bookResponses[i] = listBookRowToBookResponse(b)
	}

	var desc *string
	if list.Description.Valid {
		desc = &list.Description.String
	}

	resp := types.ListWithBooksResponse{
		ListResponse: types.ListResponse{
			ID:          list.ID.String(),
			UserID:      list.UserID.String(),
			Username:    username,
			Title:       list.Title,
			Description: desc,
			IsPrivate:   list.IsPrivate,
			BookCount:   int64(len(books)),
			SaveCount:   saveCount,
			IsSaved:     isSaved,
			CanEdit:     canEdit,
			CanView:     true,
			CreatedAt:   list.CreatedAt.Time,
			UpdatedAt:   list.UpdatedAt.Time,
		},
		Books: bookResponses,
	}

	types.WriteJSON(w, http.StatusOK, resp)
}

// UpdateList handles PUT /api/v1/users/:username/lists/:listId.
func (h *ListsHandler) UpdateList(w http.ResponseWriter, r *http.Request) {
	listID, _, ok := h.resolveListOwner(w, r)
	if !ok {
		return
	}

	list, err := h.Queries.GetListByID(r.Context(), listID)
	if err != nil {
		slog.Error("get list for update", "error", err)
		types.WriteInternalError(w)
		return
	}

	var req types.UpdateListRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	// Apply updates on top of current values
	title := list.Title
	if req.Title != nil {
		if len(*req.Title) == 0 || len(*req.Title) > 50 {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "title must be 1–50 characters")
			return
		}
		title = *req.Title
	}

	descParam := list.Description
	if req.Description != nil {
		if len(*req.Description) > 200 {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "description must be max 200 characters")
			return
		}
		descParam = pgtype.Text{String: *req.Description, Valid: true}
	}

	isPrivate := list.IsPrivate
	if req.IsPrivate != nil {
		isPrivate = *req.IsPrivate
	}

	updated, err := h.Queries.UpdateList(r.Context(), db.UpdateListParams{
		ID:          listID,
		Title:       title,
		Description: descParam,
		IsPrivate:   isPrivate,
	})
	if err != nil {
		slog.Error("update list", "error", err)
		types.WriteInternalError(w)
		return
	}

	username := chi.URLParam(r, "username")
	var desc *string
	if updated.Description.Valid {
		desc = &updated.Description.String
	}
	types.WriteJSON(w, http.StatusOK, types.ListResponse{
		ID:          updated.ID.String(),
		UserID:      updated.UserID.String(),
		Username:    username,
		Title:       updated.Title,
		Description: desc,
		IsPrivate:   updated.IsPrivate,
		CanEdit:     true,
		CanView:     true,
		CreatedAt:   updated.CreatedAt.Time,
		UpdatedAt:   updated.UpdatedAt.Time,
	})
}

// DeleteList handles DELETE /api/v1/users/:username/lists/:listId.
func (h *ListsHandler) DeleteList(w http.ResponseWriter, r *http.Request) {
	listID, userID, ok := h.resolveListOwner(w, r)
	if !ok {
		return
	}

	if err := h.Queries.DeleteList(r.Context(), listID); err != nil {
		slog.Error("delete list", "error", err)
		types.WriteInternalError(w)
		return
	}

	go func() {
		_ = h.Queries.DecrementUserListsCount(context.Background(), userID)
	}()

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "List deleted"})
}

// AddBookToList handles POST /api/v1/users/:username/lists/:listId/books.
func (h *ListsHandler) AddBookToList(w http.ResponseWriter, r *http.Request) {
	listID, _, ok := h.resolveListOwner(w, r)
	if !ok {
		return
	}

	var req types.AddBookToListRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	var bookID uuid.UUID
	var resolveErr error

	switch {
	case req.BookID != nil:
		id, parseErr := uuid.Parse(*req.BookID)
		if parseErr != nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid book_id")
			return
		}
		if _, resolveErr = h.Queries.GetBookByID(r.Context(), id); resolveErr == nil {
			bookID = id
		}
	case req.GoogleBooksID != nil:
		book, fetchErr := h.Queries.GetBookByGoogleID(r.Context(), pgtype.Text{String: *req.GoogleBooksID, Valid: true})
		if errors.Is(fetchErr, pgx.ErrNoRows) {
			book, fetchErr = cacheBookFromGoogleBooks(r.Context(), h.Queries, h.GoogleBooks, *req.GoogleBooksID)
		}
		resolveErr = fetchErr
		if resolveErr == nil {
			bookID = book.ID
		}
	case req.ISBN != nil:
		book, fetchErr := h.Queries.GetBookByISBN(r.Context(), pgtype.Text{String: *req.ISBN, Valid: true})
		if errors.Is(fetchErr, pgx.ErrNoRows) {
			book, fetchErr = cacheBookFromISBNdb(r.Context(), h.Queries, h.ISBNdb, *req.ISBN)
		}
		resolveErr = fetchErr
		if resolveErr == nil {
			bookID = book.ID
		}
	default:
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "provide book_id, isbn, or google_books_id")
		return
	}

	if resolveErr != nil {
		slog.Error("resolve book for list", "error", resolveErr)
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "Book not found")
		return
	}

	// Check if already in list
	exists, err := h.Queries.CheckBookInList(r.Context(), db.CheckBookInListParams{
		ListID: listID,
		BookID: bookID,
	})
	if err != nil {
		slog.Error("check book in list", "error", err)
		types.WriteInternalError(w)
		return
	}
	if exists {
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "Book already in list")
		return
	}

	displayOrder := int32(0)
	if req.DisplayOrder != nil {
		displayOrder = int32(*req.DisplayOrder)
	}

	entry, err := h.Queries.AddBookToList(r.Context(), db.AddBookToListParams{
		ListID:       listID,
		BookID:       bookID,
		DisplayOrder: displayOrder,
	})
	if err != nil {
		slog.Error("add book to list", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusCreated, entry)
}

// RemoveBookFromList handles DELETE /api/v1/users/:username/lists/:listId/books/:bookId.
func (h *ListsHandler) RemoveBookFromList(w http.ResponseWriter, r *http.Request) {
	listID, _, ok := h.resolveListOwner(w, r)
	if !ok {
		return
	}

	bookID, err := uuid.Parse(chi.URLParam(r, "bookId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid book ID")
		return
	}

	if err := h.Queries.RemoveBookFromList(r.Context(), db.RemoveBookFromListParams{
		ListID: listID,
		BookID: bookID,
	}); err != nil {
		slog.Error("remove book from list", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Book removed from list"})
}

// ShareList handles POST /api/v1/users/:username/lists/:listId/share.
// Grants access to multiple followers for a private list.
func (h *ListsHandler) ShareList(w http.ResponseWriter, r *http.Request) {
	listID, userID, ok := h.resolveListOwner(w, r)
	if !ok {
		return
	}

	var req types.ShareListRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}
	if len(req.Usernames) == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "usernames must not be empty")
		return
	}

	granted := 0
	for _, uname := range req.Usernames {
		target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(uname))
		if err != nil {
			continue
		}
		if target.ID == userID {
			continue
		}
		_, err = h.Queries.GrantListAccess(r.Context(), db.GrantListAccessParams{
			ListID:    listID,
			UserID:    target.ID,
			GrantedBy: userID,
		})
		if err == nil {
			granted++
		}
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"message": "List shared",
		"granted": granted,
	})
}

// SaveList handles POST /api/v1/users/:username/lists/:listId/save.
func (h *ListsHandler) SaveList(w http.ResponseWriter, r *http.Request) {
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

	listID, err := uuid.Parse(chi.URLParam(r, "listId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid list ID")
		return
	}

	// Verify list exists and user can view it
	canAccess, err := h.Queries.CheckCanAccessList(r.Context(), db.CheckCanAccessListParams{
		ID:     listID,
		UserID: userID,
	})
	if err != nil || !canAccess {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "List not found")
		return
	}

	// Don't save own lists
	ownerID, err := h.Queries.CheckListOwnership(r.Context(), listID)
	if err != nil {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "List not found")
		return
	}
	if ownerID == userID {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Cannot save your own list")
		return
	}

	already, err := h.Queries.CheckListSaved(r.Context(), db.CheckListSavedParams{
		UserID: userID,
		ListID: listID,
	})
	if err != nil {
		slog.Error("check list saved", "error", err)
		types.WriteInternalError(w)
		return
	}
	if already {
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "List already saved")
		return
	}

	if _, err := h.Queries.SaveList(r.Context(), db.SaveListParams{
		UserID: userID,
		ListID: listID,
	}); err != nil {
		slog.Error("save list", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusCreated, types.SuccessResponse{Message: "List saved"})
}

// UnsaveList handles DELETE /api/v1/users/:username/lists/:listId/save.
func (h *ListsHandler) UnsaveList(w http.ResponseWriter, r *http.Request) {
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

	listID, err := uuid.Parse(chi.URLParam(r, "listId"))
	if err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid list ID")
		return
	}

	if err := h.Queries.UnsaveList(r.Context(), db.UnsaveListParams{
		UserID: userID,
		ListID: listID,
	}); err != nil {
		slog.Error("unsave list", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "List unsaved"})
}

// GrantAccess handles POST /api/v1/users/:username/lists/:listId/access.
func (h *ListsHandler) GrantAccess(w http.ResponseWriter, r *http.Request) {
	listID, userID, ok := h.resolveListOwner(w, r)
	if !ok {
		return
	}

	var req types.GrantAccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}
	if req.Username == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "username is required")
		return
	}

	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(req.Username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for grant access", "error", err)
		types.WriteInternalError(w)
		return
	}

	if target.ID == userID {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Cannot grant access to yourself")
		return
	}

	alreadyGranted, err := h.Queries.CheckListAccess(r.Context(), db.CheckListAccessParams{
		ListID: listID,
		UserID: target.ID,
	})
	if err != nil {
		slog.Error("check list access", "error", err)
		types.WriteInternalError(w)
		return
	}
	if alreadyGranted {
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "User already has access")
		return
	}

	if _, err := h.Queries.GrantListAccess(r.Context(), db.GrantListAccessParams{
		ListID:    listID,
		UserID:    target.ID,
		GrantedBy: userID,
	}); err != nil {
		slog.Error("grant list access", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusCreated, types.SuccessResponse{Message: "Access granted"})
}

// RevokeAccess handles DELETE /api/v1/users/:username/lists/:listId/access.
func (h *ListsHandler) RevokeAccess(w http.ResponseWriter, r *http.Request) {
	listID, _, ok := h.resolveListOwner(w, r)
	if !ok {
		return
	}

	username := r.URL.Query().Get("username")
	if username == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "username query param is required")
		return
	}

	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for revoke access", "error", err)
		types.WriteInternalError(w)
		return
	}

	if err := h.Queries.RevokeListAccess(r.Context(), db.RevokeListAccessParams{
		ListID: listID,
		UserID: target.ID,
	}); err != nil {
		slog.Error("revoke list access", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Access revoked"})
}

// GetAccessUsers handles GET /api/v1/users/:username/lists/:listId/access.
func (h *ListsHandler) GetAccessUsers(w http.ResponseWriter, r *http.Request) {
	listID, _, ok := h.resolveListOwner(w, r)
	if !ok {
		return
	}

	rows, err := h.Queries.GetListAccessUsers(r.Context(), listID)
	if err != nil {
		slog.Error("get list access users", "error", err)
		types.WriteInternalError(w)
		return
	}

	resp := make([]types.ListAccessResponse, len(rows))
	for i, row := range rows {
		entry := types.ListAccessResponse{
			ID:        row.ID.String(),
			Username:  row.Username,
			GrantedAt: row.GrantedAt.Time,
		}
		if row.Name.Valid {
			entry.Name = row.Name.String
		}
		if row.AvatarUrl.Valid {
			entry.AvatarURL = &row.AvatarUrl.String
		}
		resp[i] = entry
	}

	types.WriteJSON(w, http.StatusOK, resp)
}

// ── Row converters ────────────────────────────────────────────────────────────

func listToResponse(list db.List, username string, bookCount, saveCount int64, isSaved, canEdit, canView bool) types.ListResponse {
	r := types.ListResponse{
		ID:        list.ID.String(),
		UserID:    list.UserID.String(),
		Username:  username,
		Title:     list.Title,
		IsPrivate: list.IsPrivate,
		BookCount: bookCount,
		SaveCount: saveCount,
		IsSaved:   isSaved,
		CanEdit:   canEdit,
		CanView:   canView,
		CreatedAt: list.CreatedAt.Time,
		UpdatedAt: list.UpdatedAt.Time,
	}
	if list.Description.Valid {
		r.Description = &list.Description.String
	}
	return r
}

func listRowToResponse(row db.GetUserListsRow, username string, isSaved, canEdit bool) types.ListResponse {
	r := types.ListResponse{
		ID:        row.ID.String(),
		UserID:    row.UserID.String(),
		Username:  username,
		Title:     row.Title,
		IsPrivate: row.IsPrivate,
		BookCount: row.BookCount,
		IsSaved:   isSaved,
		CanEdit:   canEdit,
		CanView:   true,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
	if row.Description.Valid {
		r.Description = &row.Description.String
	}
	return r
}

func savedListRowToResponse(row db.GetUserSavedListsRow, canEdit bool) types.ListResponse {
	r := types.ListResponse{
		ID:        row.ID.String(),
		UserID:    row.UserID.String(),
		Username:  row.OwnerUsername,
		Title:     row.Title,
		IsPrivate: row.IsPrivate,
		BookCount: row.BookCount,
		IsSaved:   true,
		CanEdit:   canEdit,
		CanView:   true,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
	if row.Description.Valid {
		r.Description = &row.Description.String
	}
	return r
}

func listBookRowToBookResponse(row db.GetListBooksRow) types.BookResponse {
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
