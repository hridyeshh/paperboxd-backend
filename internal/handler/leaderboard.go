package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/cache"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const leaderboardCacheTTL = 5 * time.Minute

// LeaderboardHandler holds dependencies for leaderboard endpoints.
type LeaderboardHandler struct {
	Queries *db.Queries
	Cache   *cache.Client
}

func NewLeaderboardHandler(queries *db.Queries, cache *cache.Client) *LeaderboardHandler {
	return &LeaderboardHandler{Queries: queries, Cache: cache}
}

// validLeaderboardDimensions lists the accepted dimension strings.
var validLeaderboardDimensions = map[string]bool{
	"books":  true,
	"pages":  true,
	"diary":  true,
	"genres": true,
	"xp":     true,
	"streak": true,
}

// ── helpers ───────────────────────────────────────────────────────────────────

func int4ToPtr(v pgtype.Int4) *int32 {
	if !v.Valid {
		return nil
	}
	return &v.Int32
}

func statToMap(stat db.LeaderboardStat, levelName, levelBadge string) map[string]any {
	return map[string]any{
		"user_id":         stat.UserID,
		"username":        stat.Username,
		"books_read":      stat.BooksRead.Int32,
		"pages_read":      stat.PagesRead.Int32,
		"diary_entries":   stat.DiaryEntries.Int32,
		"genres_explored": stat.GenresExplored.Int32,
		"total_xp":        stat.TotalXp.Int32,
		"level":           stat.Level.Int32,
		"current_streak":  stat.CurrentStreak.Int32,
		"books_rank":      int4ToPtr(stat.BooksRank),
		"pages_rank":      int4ToPtr(stat.PagesRank),
		"diary_rank":      int4ToPtr(stat.DiaryRank),
		"genres_rank":     int4ToPtr(stat.GenresRank),
		"xp_rank":         int4ToPtr(stat.XpRank),
		"streak_rank":     int4ToPtr(stat.StreakRank),
		"level_name":      levelName,
		"level_badge":     levelBadge,
		"updated_at":      stat.UpdatedAt,
	}
}

func parseLimit(r *http.Request, defaultVal, maxVal int32) int32 {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return defaultVal
	}
	l, err := strconv.Atoi(s)
	if err != nil || l <= 0 || int32(l) > maxVal {
		return defaultVal
	}
	return int32(l)
}

// ── handlers ──────────────────────────────────────────────────────────────────

// RebuildLeaderboard rebuilds all leaderboard stats and rankings.
// POST /api/v1/admin/leaderboard/rebuild
func (h *LeaderboardHandler) RebuildLeaderboard(w http.ResponseWriter, r *http.Request) {
	if err := h.Queries.RebuildAllLeaderboardStats(r.Context()); err != nil {
		slog.Error("rebuild leaderboard stats", "error", err)
		types.WriteInternalError(w)
		return
	}
	if err := h.Queries.UpdateLeaderboardRankings(r.Context()); err != nil {
		slog.Error("update leaderboard rankings", "error", err)
		types.WriteInternalError(w)
		return
	}
	// Invalidate cached leaderboards so next read gets fresh data.
	if h.Cache != nil {
		keys := []string{
			"leaderboard:global:100",
			"leaderboard:dim:books:100", "leaderboard:dim:pages:100",
			"leaderboard:dim:diary:100", "leaderboard:dim:genres:100",
			"leaderboard:dim:xp:100", "leaderboard:dim:streak:100",
		}
		if err := h.Cache.Del(r.Context(), keys...); err != nil {
			slog.Warn("invalidate leaderboard cache", "error", err)
		}
	}
	types.WriteJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Leaderboard rebuilt successfully",
	})
}

// GetMyLeaderboardStats returns the current user's leaderboard stats and rankings.
// GET /api/v1/users/me/leaderboard-stats
func (h *LeaderboardHandler) GetMyLeaderboardStats(w http.ResponseWriter, r *http.Request) {
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

	cacheKey := fmt.Sprintf("user:stats:%s", userIDStr)
	if h.Cache != nil {
		var cached map[string]any
		if err := h.Cache.GetJSON(r.Context(), cacheKey, &cached); err == nil {
			types.WriteJSON(w, http.StatusOK, cached)
			return
		}
	}

	stat, err := h.Queries.GetUserLeaderboardStats(r.Context(), userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// First time — compute on the fly
			stat, err = h.Queries.RebuildUserLeaderboardStats(r.Context(), userID)
			if err != nil {
				slog.Error("rebuild user leaderboard stats", "error", err, "user_id", userID)
				types.WriteInternalError(w)
				return
			}
		} else {
			slog.Error("get user leaderboard stats", "error", err, "user_id", userID)
			types.WriteInternalError(w)
			return
		}
	}

	levelName := service.GetLevelName(int(stat.Level.Int32))
	levelBadge := service.GetLevelBadge(int(stat.Level.Int32))
	resp := statToMap(stat, levelName, levelBadge)

	if h.Cache != nil {
		if err := h.Cache.SetJSON(r.Context(), cacheKey, resp, leaderboardCacheTTL); err != nil {
			slog.Warn("set user stats cache", "error", err)
		}
	}

	types.WriteJSON(w, http.StatusOK, resp)
}

// GetFriendsLeaderboard returns the leaderboard for users the caller follows.
// GET /api/v1/leaderboard/friends?limit=50
func (h *LeaderboardHandler) GetFriendsLeaderboard(w http.ResponseWriter, r *http.Request) {
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

	limit := parseLimit(r, 50, 100)
	stats, err := h.Queries.GetFriendsLeaderboard(r.Context(), db.GetFriendsLeaderboardParams{
		FollowerID: userID,
		Limit:      limit,
	})
	if err != nil {
		slog.Error("get friends leaderboard", "error", err)
		types.WriteInternalError(w)
		return
	}

	results := make([]map[string]any, len(stats))
	for i, s := range stats {
		results[i] = statToMap(s, service.GetLevelName(int(s.Level.Int32)), service.GetLevelBadge(int(s.Level.Int32)))
	}
	types.WriteJSON(w, http.StatusOK, map[string]any{
		"leaderboard": results,
		"count":       len(results),
	})
}

// GetGlobalLeaderboard returns the global leaderboard (opted-in users, sorted by XP).
// GET /api/v1/leaderboard/global?limit=100
func (h *LeaderboardHandler) GetGlobalLeaderboard(w http.ResponseWriter, r *http.Request) {
	limit := parseLimit(r, 100, 100)
	cacheKey := fmt.Sprintf("leaderboard:global:%d", limit)

	if h.Cache != nil {
		var cached map[string]any
		if err := h.Cache.GetJSON(r.Context(), cacheKey, &cached); err == nil {
			types.WriteJSON(w, http.StatusOK, cached)
			return
		}
	}

	stats, err := h.Queries.GetGlobalLeaderboard(r.Context(), limit)
	if err != nil {
		slog.Error("get global leaderboard", "error", err)
		types.WriteInternalError(w)
		return
	}

	results := make([]map[string]any, len(stats))
	for i, s := range stats {
		results[i] = statToMap(s, service.GetLevelName(int(s.Level.Int32)), service.GetLevelBadge(int(s.Level.Int32)))
	}
	resp := map[string]any{"leaderboard": results, "count": len(results)}

	if h.Cache != nil {
		if err := h.Cache.SetJSON(r.Context(), cacheKey, resp, leaderboardCacheTTL); err != nil {
			slog.Warn("set global leaderboard cache", "error", err)
		}
	}

	types.WriteJSON(w, http.StatusOK, resp)
}

// GetLeaderboardByDimension returns the leaderboard sorted by a specific stat dimension.
// GET /api/v1/leaderboard/dimension/:dimension?limit=100
func (h *LeaderboardHandler) GetLeaderboardByDimension(w http.ResponseWriter, r *http.Request) {
	dimension := chi.URLParam(r, "dimension")
	if !validLeaderboardDimensions[dimension] {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeInvalidRequest, "invalid dimension; valid values: books, pages, diary, genres, xp, streak")
		return
	}

	limit := parseLimit(r, 100, 100)
	cacheKey := fmt.Sprintf("leaderboard:dim:%s:%d", dimension, limit)

	if h.Cache != nil {
		var cached map[string]any
		if err := h.Cache.GetJSON(r.Context(), cacheKey, &cached); err == nil {
			types.WriteJSON(w, http.StatusOK, cached)
			return
		}
	}

	stats, err := h.Queries.GetLeaderboardByDimension(r.Context(), db.GetLeaderboardByDimensionParams{
		Column1: dimension,
		Limit:   limit,
	})
	if err != nil {
		slog.Error("get leaderboard by dimension", "error", err, "dimension", dimension)
		types.WriteInternalError(w)
		return
	}

	results := make([]map[string]any, len(stats))
	for i, s := range stats {
		results[i] = statToMap(s, service.GetLevelName(int(s.Level.Int32)), service.GetLevelBadge(int(s.Level.Int32)))
	}
	resp := map[string]any{"dimension": dimension, "leaderboard": results, "count": len(results)}

	if h.Cache != nil {
		if err := h.Cache.SetJSON(r.Context(), cacheKey, resp, leaderboardCacheTTL); err != nil {
			slog.Warn("set dimension leaderboard cache", "error", err)
		}
	}

	types.WriteJSON(w, http.StatusOK, resp)
}
