package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/config"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// UserHandler holds dependencies for user endpoints.
type UserHandler struct {
	Queries *db.Queries
	Config  *config.Config
}

// GetByUsername handles GET /api/v1/users/:username
func (h *UserHandler) GetByUsername(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Username is required")
		return
	}

	user, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user by username", "error", err, "username", username)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, userToResponse(user))
}

// Update handles PUT /api/v1/users/:username (owner only)
func (h *UserHandler) Update(w http.ResponseWriter, r *http.Request) {
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

	// Verify the authenticated user owns this profile
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user by username for update", "error", err)
		types.WriteInternalError(w)
		return
	}

	if target.ID != userID {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "You can only update your own profile")
		return
	}

	var req types.UpdateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid JSON body")
		return
	}

	params := db.UpdateUserParams{ID: userID}
	if req.Name != nil {
		params.Name = pgtype.Text{String: *req.Name, Valid: true}
	}
	if req.Bio != nil {
		params.Bio = pgtype.Text{String: *req.Bio, Valid: true}
	}
	if req.Pronouns != nil {
		params.Pronouns = pgtype.Text{String: *req.Pronouns, Valid: true}
	}
	if req.AvatarURL != nil {
		params.AvatarUrl = pgtype.Text{String: *req.AvatarURL, Valid: true}
	}

	updated, err := h.Queries.UpdateUser(r.Context(), params)
	if err != nil {
		slog.Error("update user", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, userToResponse(updated))
}

// Search handles GET /api/v1/users/search?query=...
func (h *UserHandler) Search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "query parameter is required")
		return
	}

	page, pageSize := parsePagination(r)
	offset := int32((page - 1) * pageSize)
	limit := int32(pageSize)

	users, err := h.Queries.SearchUsers(r.Context(), db.SearchUsersParams{
		Column1: pgtype.Text{String: query, Valid: true},
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		slog.Error("search users", "error", err)
		types.WriteInternalError(w)
		return
	}

	resp := make([]types.UserResponse, len(users))
	for i, u := range users {
		resp[i] = userToResponse(u)
	}

	types.WriteJSON(w, http.StatusOK, types.UserListResponse{
		Users:      resp,
		TotalCount: int64(len(resp)),
		Page:       page,
		PageSize:   pageSize,
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func userToResponse(u db.User) types.UserResponse {
	resp := types.UserResponse{
		ID:             u.ID,
		Username:       u.Username,
		Email:          u.Email,
		IsPublic:       u.IsPublic.Bool,
		BooksReadCount: u.BooksReadCount.Int32,
		FollowersCount: u.FollowersCount.Int32,
		FollowingCount: u.FollowingCount.Int32,
		CreatedAt:      u.CreatedAt.Time,
	}
	if u.Name.Valid {
		resp.Name = &u.Name.String
	}
	if u.AvatarUrl.Valid {
		resp.AvatarURL = &u.AvatarUrl.String
	}
	if u.Bio.Valid {
		resp.Bio = &u.Bio.String
	}
	if u.Pronouns.Valid {
		resp.Pronouns = &u.Pronouns.String
	}
	return resp
}
