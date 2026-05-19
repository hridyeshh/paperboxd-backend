package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
)

// Follow handles POST /api/v1/users/:username/follow
func (h *UserHandler) Follow(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	followerID, err := uuid.Parse(userIDStr)
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
		slog.Error("get user for follow", "error", err)
		types.WriteInternalError(w)
		return
	}

	if target.ID == followerID {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Cannot follow yourself")
		return
	}

	_, err = h.Queries.FollowUser(r.Context(), db.FollowUserParams{
		FollowerID:  followerID,
		FollowingID: target.ID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("follow user", "error", err)
		types.WriteInternalError(w)
		return
	}

	followedID := target.ID
	go func() {
		xpSvc := service.NewXPService(h.Queries)
		_ = xpSvc.AwardXP(context.Background(), followedID, "follow_gained", service.XPFollowGained, nil)
	}()

	followersCount, _ := h.Queries.CountFollowers(r.Context(), target.ID)
	followingCount, _ := h.Queries.CountFollowing(r.Context(), target.ID)

	types.WriteJSON(w, http.StatusOK, types.FollowResponse{
		Message:        "Following " + username,
		IsFollowing:    true,
		FollowersCount: int32(followersCount),
		FollowingCount: int32(followingCount),
	})
}

// Unfollow handles DELETE /api/v1/users/:username/follow
func (h *UserHandler) Unfollow(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	followerID, err := uuid.Parse(userIDStr)
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
		slog.Error("get user for unfollow", "error", err)
		types.WriteInternalError(w)
		return
	}

	if err := h.Queries.UnfollowUser(r.Context(), db.UnfollowUserParams{
		FollowerID:  followerID,
		FollowingID: target.ID,
	}); err != nil {
		slog.Error("unfollow user", "error", err)
		types.WriteInternalError(w)
		return
	}

	followersCount, _ := h.Queries.CountFollowers(r.Context(), target.ID)
	followingCount, _ := h.Queries.CountFollowing(r.Context(), target.ID)

	types.WriteJSON(w, http.StatusOK, types.FollowResponse{
		Message:        "Unfollowed " + username,
		IsFollowing:    false,
		FollowersCount: int32(followersCount),
		FollowingCount: int32(followingCount),
	})
}

// GetFollowers handles GET /api/v1/users/:username/followers
func (h *UserHandler) GetFollowers(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for followers", "error", err)
		types.WriteInternalError(w)
		return
	}

	page, pageSize := parsePagination(r)
	offset := int32((page - 1) * pageSize)
	limit := int32(pageSize)

	users, err := h.Queries.GetFollowers(r.Context(), db.GetFollowersParams{
		FollowingID: target.ID,
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		slog.Error("get followers", "error", err)
		types.WriteInternalError(w)
		return
	}

	total, err := h.Queries.CountFollowers(r.Context(), target.ID)
	if err != nil {
		slog.Error("count followers", "error", err)
		types.WriteInternalError(w)
		return
	}

	resp := make([]types.UserResponse, len(users))
	for i, u := range users {
		resp[i] = userToResponse(u)
	}

	types.WriteJSON(w, http.StatusOK, types.UserListResponse{
		Users:      resp,
		TotalCount: total,
		Page:       page,
		PageSize:   pageSize,
	})
}

// GetFollowing handles GET /api/v1/users/:username/following
func (h *UserHandler) GetFollowing(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	target, err := h.Queries.GetUserByUsername(r.Context(), strings.ToLower(username))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			types.WriteError(w, http.StatusNotFound, types.ErrCodeNotFound, "User not found")
			return
		}
		slog.Error("get user for following", "error", err)
		types.WriteInternalError(w)
		return
	}

	page, pageSize := parsePagination(r)
	offset := int32((page - 1) * pageSize)
	limit := int32(pageSize)

	users, err := h.Queries.GetFollowing(r.Context(), db.GetFollowingParams{
		FollowerID: target.ID,
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		slog.Error("get following", "error", err)
		types.WriteInternalError(w)
		return
	}

	total, err := h.Queries.CountFollowing(r.Context(), target.ID)
	if err != nil {
		slog.Error("count following", "error", err)
		types.WriteInternalError(w)
		return
	}

	resp := make([]types.UserResponse, len(users))
	for i, u := range users {
		resp[i] = userToResponse(u)
	}

	types.WriteJSON(w, http.StatusOK, types.UserListResponse{
		Users:      resp,
		TotalCount: total,
		Page:       page,
		PageSize:   pageSize,
	})
}
