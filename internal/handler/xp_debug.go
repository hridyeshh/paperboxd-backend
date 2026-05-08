package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// XPHandler holds dependencies for XP debug endpoints.
// TEMPORARY - DELETE BEFORE PRODUCTION
type XPHandler struct {
	Queries *db.Queries
}

func NewXPHandler(queries *db.Queries) *XPHandler {
	return &XPHandler{Queries: queries}
}

// TestAwardXP is a temporary endpoint for testing XP awarding.
func (h *XPHandler) TestAwardXP(w http.ResponseWriter, r *http.Request) {
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

	var req struct {
		ActionType  string `json:"action_type"`
		ReferenceID string `json:"reference_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid request body")
		return
	}

	xpSvc := service.NewXPService(h.Queries)
	xpAmount := xpSvc.GetXPForAction(req.ActionType, nil)
	if xpAmount == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid action type")
		return
	}

	var refID *uuid.UUID
	if req.ReferenceID != "" {
		parsed, err := uuid.Parse(req.ReferenceID)
		if err != nil {
			types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "Invalid reference_id")
			return
		}
		refID = &parsed
	}

	if err := xpSvc.AwardXP(r.Context(), userID, req.ActionType, xpAmount, refID); err != nil {
		slog.Error("award xp", "error", err, "user_id", userID, "action", req.ActionType)
		types.WriteInternalError(w)
		return
	}

	info, err := xpSvc.GetUserXPInfo(r.Context(), userID)
	if err != nil {
		slog.Error("get user xp info", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"message":        "XP awarded successfully",
		"action_type":    req.ActionType,
		"xp_awarded":     xpAmount,
		"total_xp":       info.TotalXp.Int32,
		"level":          info.Level.Int32,
		"current_streak": info.CurrentStreak.Int32,
		"level_name":     service.GetLevelName(int(info.Level.Int32)),
		"level_badge":    service.GetLevelBadge(int(info.Level.Int32)),
	})
}

// TestGetXPInfo returns the authenticated user's XP information.
func (h *XPHandler) TestGetXPInfo(w http.ResponseWriter, r *http.Request) {
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

	xpSvc := service.NewXPService(h.Queries)

	info, err := xpSvc.GetUserXPInfo(r.Context(), userID)
	if err != nil {
		slog.Error("get user xp info", "error", err)
		types.WriteInternalError(w)
		return
	}

	todayXP, err := xpSvc.GetUserXPToday(r.Context(), userID)
	if err != nil {
		slog.Error("get user xp today", "error", err)
		types.WriteInternalError(w)
		return
	}

	history, err := xpSvc.GetUserXPHistory(r.Context(), userID, 10)
	if err != nil {
		slog.Error("get user xp history", "error", err)
		types.WriteInternalError(w)
		return
	}

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"total_xp":       info.TotalXp.Int32,
		"level":          info.Level.Int32,
		"current_streak": info.CurrentStreak.Int32,
		"longest_streak": info.LongestStreak.Int32,
		"today_xp":       todayXP,
		"daily_cap":      service.MaxDailyXP,
		"remaining_xp":   service.MaxDailyXP - todayXP,
		"level_name":     service.GetLevelName(int(info.Level.Int32)),
		"level_badge":    service.GetLevelBadge(int(info.Level.Int32)),
		"xp_to_next":     service.GetXPNeededForNextLevel(int(info.TotalXp.Int32), int(info.Level.Int32)),
		"recent_actions": history,
	})
}
