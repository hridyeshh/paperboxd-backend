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
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

	blocked, err := h.Queries.CheckBlockedEither(r.Context(), db.CheckBlockedEitherParams{
		BlockerID: followerID,
		BlockedID: target.ID,
	})
	if err != nil {
		slog.Error("check blocked for follow", "error", err)
		types.WriteInternalError(w)
		return
	}
	if blocked {
		types.WriteError(w, http.StatusForbidden, types.ErrCodeForbidden, "You cannot follow this user")
		return
	}

	// A private account is not followed directly — the follow becomes a request
	// the owner approves, and nothing about their profile opens up until then.
	if !target.IsPublic {
		if _, err := h.Queries.CreateFollowRequest(r.Context(), db.CreateFollowRequestParams{
			RequesterID: followerID,
			TargetID:    target.ID,
		}); err != nil {
			slog.Error("create follow request", "error", err)
			types.WriteInternalError(w)
			return
		}
		followersCount, _ := h.Queries.CountFollowers(r.Context(), target.ID)
		followingCount, _ := h.Queries.CountFollowing(r.Context(), target.ID)
		types.WriteJSON(w, http.StatusOK, types.FollowResponse{
			Message:        "Requested to follow " + username,
			IsFollowing:    false,
			HasRequested:   true,
			FollowersCount: int32(followersCount),
			FollowingCount: int32(followingCount),
		})
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

	go func() {
		h.EventSvc.Emit(context.Background(), service.EmitParams{
			UserID:    followerID,
			EventType: "user.followed",
			Source:    "server",
		})
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

	// Same button cancels a pending request on a private account.
	if err := h.Queries.DeleteFollowRequest(r.Context(), db.DeleteFollowRequestParams{
		RequesterID: followerID,
		TargetID:    target.ID,
	}); err != nil {
		slog.Error("delete follow request on unfollow", "error", err)
	}

	go func() {
		h.EventSvc.Emit(context.Background(), service.EmitParams{
			UserID:    followerID,
			EventType: "user.unfollowed",
			Source:    "server",
		})
	}()

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
	}.WithPagination())
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
	}.WithPagination())
}

// Block handles POST /api/v1/users/:username/block
func (h *UserHandler) Block(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	blockerID, err := uuid.Parse(userIDStr)
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
		slog.Error("get user for block", "error", err)
		types.WriteInternalError(w)
		return
	}

	if target.ID == blockerID {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Cannot block yourself")
		return
	}

	if err := h.Queries.BlockUser(r.Context(), db.BlockUserParams{
		BlockerID: blockerID,
		BlockedID: target.ID,
	}); err != nil {
		slog.Error("block user", "error", err)
		types.WriteInternalError(w)
		return
	}

	// Sever the follow edge both ways so feeds stop carrying their activity.
	if err := h.Queries.UnfollowUser(r.Context(), db.UnfollowUserParams{
		FollowerID:  blockerID,
		FollowingID: target.ID,
	}); err != nil {
		slog.Error("unfollow on block", "error", err)
	}
	if err := h.Queries.UnfollowUser(r.Context(), db.UnfollowUserParams{
		FollowerID:  target.ID,
		FollowingID: blockerID,
	}); err != nil {
		slog.Error("unfollow on block (reverse)", "error", err)
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Blocked " + username})
}

// Unblock handles DELETE /api/v1/users/:username/block
func (h *UserHandler) Unblock(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	blockerID, err := uuid.Parse(userIDStr)
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
		slog.Error("get user for unblock", "error", err)
		types.WriteInternalError(w)
		return
	}

	if err := h.Queries.UnblockUser(r.Context(), db.UnblockUserParams{
		BlockerID: blockerID,
		BlockedID: target.ID,
	}); err != nil {
		slog.Error("unblock user", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, types.SuccessResponse{Message: "Unblocked " + username})
}

var validReportContentTypes = map[string]bool{
	"review":      true,
	"diary_entry": true,
	"list":        true,
	"user":        true,
	"book":        true,
}

// CreateReport handles POST /api/v1/reports
func (h *UserHandler) CreateReport(w http.ResponseWriter, r *http.Request) {
	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	reporterID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	var req struct {
		ContentType string `json:"content_type"`
		ContentID   string `json:"content_id"`
		Reason      string `json:"reason"`
		Details     string `json:"details"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid request body")
		return
	}

	if !validReportContentTypes[req.ContentType] {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid content_type")
		return
	}
	if req.ContentID == "" || len(req.ContentID) > 200 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid content_id")
		return
	}
	if req.Reason == "" || len(req.Reason) > 100 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Reason is required (max 100 chars)")
		return
	}
	if len(req.Details) > 2000 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Details too long (max 2000 chars)")
		return
	}

	details := pgtype.Text{}
	if req.Details != "" {
		details = pgtype.Text{String: req.Details, Valid: true}
	}

	if _, err := h.Queries.CreateReport(r.Context(), db.CreateReportParams{
		ReporterID:  reporterID,
		ContentType: req.ContentType,
		ContentID:   req.ContentID,
		Reason:      req.Reason,
		Details:     details,
	}); err != nil {
		slog.Error("create report", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusCreated, types.SuccessResponse{Message: "Report received. We review reports within 24 hours."})
}
