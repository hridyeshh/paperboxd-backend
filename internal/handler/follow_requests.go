package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
)

// viewerID resolves the authenticated caller, writing the 401 itself when there
// isn't one. Every handler in this file is mounted behind Authenticate.
func viewerID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	idStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return uuid.Nil, false
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return uuid.Nil, false
	}
	return id, true
}

// ListFollowRequests handles GET /api/v1/users/me/follow-requests
func (h *UserHandler) ListFollowRequests(w http.ResponseWriter, r *http.Request) {
	me, ok := viewerID(w, r)
	if !ok {
		return
	}

	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 100 {
		limit = v
	}
	page := 1
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}

	rows, err := h.Queries.ListIncomingFollowRequests(r.Context(), db.ListIncomingFollowRequestsParams{
		TargetID: me,
		Limit:    int32(limit),
		Offset:   int32((page - 1) * limit),
	})
	if err != nil {
		slog.Error("list follow requests", "error", err)
		types.WriteInternalError(w)
		return
	}

	total, _ := h.Queries.CountIncomingFollowRequests(r.Context(), me)

	out := make([]types.FollowRequestUser, 0, len(rows))
	for _, row := range rows {
		item := types.FollowRequestUser{
			RequestID: row.RequestID.String(),
			ID:        row.User.ID.String(),
			Username:  row.User.Username,
			Name:      row.User.Name.String,
		}
		if row.RequestedAt.Valid {
			item.RequestedAt = row.RequestedAt.Time.Format("2006-01-02T15:04:05Z07:00")
		}
		if row.User.AvatarUrl.Valid {
			v := row.User.AvatarUrl.String
			item.AvatarURL = &v
		}
		if row.User.Bio.Valid {
			v := row.User.Bio.String
			item.Bio = &v
		}
		out = append(out, item)
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"requests":    out,
		"total_count": total,
	})
}

// AcceptFollowRequest handles POST /api/v1/users/me/follow-requests/{username}
func (h *UserHandler) AcceptFollowRequest(w http.ResponseWriter, r *http.Request) {
	me, ok := viewerID(w, r)
	if !ok {
		return
	}
	requester, ok := h.requesterFromPath(w, r)
	if !ok {
		return
	}

	// Only approve something actually pending — otherwise this endpoint would
	// let anyone mint a follower for themselves.
	pending, err := h.Queries.CheckFollowRequest(r.Context(), db.CheckFollowRequestParams{
		RequesterID: requester.ID,
		TargetID:    me,
	})
	if err != nil {
		slog.Error("check follow request", "error", err)
		types.WriteInternalError(w)
		return
	}
	if !pending {
		types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "No pending request from this user")
		return
	}

	if _, err := h.Queries.FollowUser(r.Context(), db.FollowUserParams{
		FollowerID:  requester.ID,
		FollowingID: me,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("accept follow request", "error", err)
		types.WriteInternalError(w)
		return
	}
	if err := h.Queries.DeleteFollowRequest(r.Context(), db.DeleteFollowRequestParams{
		RequesterID: requester.ID,
		TargetID:    me,
	}); err != nil {
		slog.Error("clear accepted follow request", "error", err)
	}

	go func() {
		xpSvc := service.NewXPService(h.Queries)
		_ = xpSvc.AwardXP(context.Background(), me, "follow_gained", service.XPFollowGained, nil)
	}()

	followersCount, _ := h.Queries.CountFollowers(r.Context(), me)
	followingCount, _ := h.Queries.CountFollowing(r.Context(), me)
	types.WriteJSON(w, http.StatusOK, types.FollowResponse{
		Message:        "Accepted " + requester.Username,
		IsFollowing:    false,
		FollowersCount: int32(followersCount),
		FollowingCount: int32(followingCount),
	})
}

// RejectFollowRequest handles DELETE /api/v1/users/me/follow-requests/{username}
func (h *UserHandler) RejectFollowRequest(w http.ResponseWriter, r *http.Request) {
	me, ok := viewerID(w, r)
	if !ok {
		return
	}
	requester, ok := h.requesterFromPath(w, r)
	if !ok {
		return
	}

	if err := h.Queries.DeleteFollowRequest(r.Context(), db.DeleteFollowRequestParams{
		RequesterID: requester.ID,
		TargetID:    me,
	}); err != nil {
		slog.Error("reject follow request", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Request declined"})
}

func (h *UserHandler) requesterFromPath(w http.ResponseWriter, r *http.Request) (db.User, bool) {
	username := chi.URLParam(r, "username")
	if username == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Username is required")
		return db.User{}, false
	}
	user, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return db.User{}, false
		}
		slog.Error("get requester", "error", err)
		types.WriteInternalError(w)
		return db.User{}, false
	}
	return user, true
}

// UpdateVisibility handles PATCH /api/v1/users/me/visibility
func (h *UserHandler) UpdateVisibility(w http.ResponseWriter, r *http.Request) {
	me, ok := viewerID(w, r)
	if !ok {
		return
	}

	var body struct {
		IsPublic *bool `json:"is_public"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.IsPublic == nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "is_public is required")
		return
	}

	user, err := h.Queries.SetUserVisibility(r.Context(), db.SetUserVisibilityParams{
		ID:       me,
		IsPublic: *body.IsPublic,
	})
	if err != nil {
		slog.Error("set user visibility", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Going public leaves nobody stuck in a queue they can no longer see.
	if *body.IsPublic {
		if err := h.Queries.AcceptAllFollowRequests(r.Context(), me); err != nil {
			slog.Error("accept pending requests on going public", "error", err)
		}
	}

	types.WriteJSON(w, http.StatusOK, userToResponse(user))
}
