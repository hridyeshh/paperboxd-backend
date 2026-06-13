package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/hridyesh/paperboxd-backend/internal/cache"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5/pgxpool"
)

type AnalyticsHandler struct {
	Pool  *pgxpool.Pool
	Cache *cache.Client
}

func NewAnalyticsHandler(pool *pgxpool.Pool, c *cache.Client) *AnalyticsHandler {
	return &AnalyticsHandler{Pool: pool, Cache: c}
}

// ── Overview ──────────────────────────────────────────────────────────────────

type overviewResponse struct {
	TotalUsers  int64 `json:"total_users"`
	NewUsers7d  int64 `json:"new_users_7d"`
	DAU         int64 `json:"dau"`
	WAU         int64 `json:"wau"`
	MAU         int64 `json:"mau"`
	EventsToday int64 `json:"events_today"`
	LiveUsers5m int64 `json:"live_users_5m"`
}

// Overview handles GET /api/v1/analytics/overview.
func (h *AnalyticsHandler) Overview(w http.ResponseWriter, r *http.Request) {
	const cacheKey = "analytics:overview"

	var resp overviewResponse
	if err := h.Cache.GetJSON(r.Context(), cacheKey, &resp); err == nil {
		types.WriteJSON(w, http.StatusOK, resp)
		return
	}

	err := h.Pool.QueryRow(r.Context(), `
		SELECT
		  (SELECT COUNT(*) FROM users WHERE deleted_at IS NULL),
		  (SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at > NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND last_activity_date = CURRENT_DATE),
		  (SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND last_activity_date > (NOW() - INTERVAL '7 days')::date),
		  (SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND last_activity_date > (NOW() - INTERVAL '30 days')::date),
		  (SELECT COUNT(*) FROM events WHERE created_at >= CURRENT_DATE),
		  (SELECT COUNT(DISTINCT user_id) FROM events WHERE created_at > NOW() - INTERVAL '5 minutes')
	`).Scan(
		&resp.TotalUsers, &resp.NewUsers7d,
		&resp.DAU, &resp.WAU, &resp.MAU,
		&resp.EventsToday, &resp.LiveUsers5m,
	)
	if err != nil {
		slog.Error("analytics overview query", "error", err)
		types.WriteInternalError(w)
		return
	}

	_ = h.Cache.SetJSON(r.Context(), cacheKey, resp, 5*time.Minute)
	types.WriteJSON(w, http.StatusOK, resp)
}

// ── Users ─────────────────────────────────────────────────────────────────────

type dayCount struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}

type recentUser struct {
	Username      string    `json:"username"`
	JoinedAt      time.Time `json:"joined_at"`
	LastActive    *string   `json:"last_active"`
	TotalXP       int64     `json:"total_xp"`
	CurrentStreak int64     `json:"current_streak"`
	BooksAdded    int64     `json:"books_added"`
}

type usersResponse struct {
	SignupsByDay []dayCount   `json:"signups_by_day"`
	ActiveByDay  []dayCount   `json:"active_by_day"`
	RecentUsers  []recentUser `json:"recent_users"`
}

// Users handles GET /api/v1/analytics/users.
func (h *AnalyticsHandler) Users(w http.ResponseWriter, r *http.Request) {
	const cacheKey = "analytics:users"

	var resp usersResponse
	if err := h.Cache.GetJSON(r.Context(), cacheKey, &resp); err == nil {
		types.WriteJSON(w, http.StatusOK, resp)
		return
	}

	ctx := r.Context()

	// signups by day — last 30 days
	rows, err := h.Pool.Query(ctx, `
		SELECT DATE(created_at)::text, COUNT(*)
		FROM users
		WHERE deleted_at IS NULL AND created_at > NOW() - INTERVAL '30 days'
		GROUP BY 1 ORDER BY 1
	`)
	if err != nil {
		slog.Error("analytics users signups query", "error", err)
		types.WriteInternalError(w)
		return
	}
	resp.SignupsByDay = []dayCount{}
	for rows.Next() {
		var d dayCount
		if err := rows.Scan(&d.Date, &d.Count); err == nil {
			resp.SignupsByDay = append(resp.SignupsByDay, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("analytics users signups rows", "error", err)
		types.WriteInternalError(w)
		return
	}

	// active by day — distinct users from events, last 30 days
	rows, err = h.Pool.Query(ctx, `
		SELECT DATE(created_at)::text, COUNT(DISTINCT user_id)
		FROM events
		WHERE created_at > NOW() - INTERVAL '30 days'
		GROUP BY 1 ORDER BY 1
	`)
	if err != nil {
		slog.Error("analytics users active query", "error", err)
		types.WriteInternalError(w)
		return
	}
	resp.ActiveByDay = []dayCount{}
	for rows.Next() {
		var d dayCount
		if err := rows.Scan(&d.Date, &d.Count); err == nil {
			resp.ActiveByDay = append(resp.ActiveByDay, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("analytics users active rows", "error", err)
		types.WriteInternalError(w)
		return
	}

	// 50 most recently active users with lifetime book count from events
	rows, err = h.Pool.Query(ctx, `
		SELECT u.username, u.created_at, u.last_activity_date::text,
		       u.total_xp, u.current_streak, COALESCE(ba.cnt, 0)
		FROM users u
		LEFT JOIN (
		    SELECT user_id, COUNT(*) AS cnt
		    FROM events
		    WHERE event_type = 'book.added_to_shelf'
		    GROUP BY user_id
		) ba ON ba.user_id = u.id
		WHERE u.deleted_at IS NULL
		ORDER BY u.last_activity_date DESC NULLS LAST
		LIMIT 50
	`)
	if err != nil {
		slog.Error("analytics recent users query", "error", err)
		types.WriteInternalError(w)
		return
	}
	resp.RecentUsers = []recentUser{}
	for rows.Next() {
		var u recentUser
		if err := rows.Scan(&u.Username, &u.JoinedAt, &u.LastActive,
			&u.TotalXP, &u.CurrentStreak, &u.BooksAdded); err == nil {
			resp.RecentUsers = append(resp.RecentUsers, u)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("analytics recent users rows", "error", err)
		types.WriteInternalError(w)
		return
	}

	_ = h.Cache.SetJSON(ctx, cacheKey, resp, 5*time.Minute)
	types.WriteJSON(w, http.StatusOK, resp)
}

// ── Features ──────────────────────────────────────────────────────────────────

type featureUsage struct {
	EventType   string `json:"event_type"`
	Events      int64  `json:"events"`
	UniqueUsers int64  `json:"unique_users"`
}

type powerUser struct {
	Username      string `json:"username"`
	Actions30d    int64  `json:"actions_30d"`
	TotalXP       int64  `json:"total_xp"`
	CurrentStreak int64  `json:"current_streak"`
}

type featuresResponse struct {
	FeatureUsage30d []featureUsage `json:"feature_usage_30d"`
	PowerUsers      []powerUser    `json:"power_users"`
}

// Features handles GET /api/v1/analytics/features.
func (h *AnalyticsHandler) Features(w http.ResponseWriter, r *http.Request) {
	const cacheKey = "analytics:features"

	var resp featuresResponse
	if err := h.Cache.GetJSON(r.Context(), cacheKey, &resp); err == nil {
		types.WriteJSON(w, http.StatusOK, resp)
		return
	}

	ctx := r.Context()

	// event type breakdown — last 30 days
	rows, err := h.Pool.Query(ctx, `
		SELECT event_type, COUNT(*), COUNT(DISTINCT user_id)
		FROM events
		WHERE created_at > NOW() - INTERVAL '30 days'
		GROUP BY event_type
		ORDER BY COUNT(DISTINCT user_id) DESC
	`)
	if err != nil {
		slog.Error("analytics feature usage query", "error", err)
		types.WriteInternalError(w)
		return
	}
	resp.FeatureUsage30d = []featureUsage{}
	for rows.Next() {
		var f featureUsage
		if err := rows.Scan(&f.EventType, &f.Events, &f.UniqueUsers); err == nil {
			resp.FeatureUsage30d = append(resp.FeatureUsage30d, f)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("analytics feature usage rows", "error", err)
		types.WriteInternalError(w)
		return
	}

	// top 50 users by xp transaction count in last 30 days
	rows, err = h.Pool.Query(ctx, `
		SELECT u.username, COUNT(xt.id) AS actions_30d, u.total_xp, u.current_streak
		FROM users u
		JOIN xp_transactions xt ON xt.user_id = u.id
		    AND xt.created_at > NOW() - INTERVAL '30 days'
		WHERE u.deleted_at IS NULL
		GROUP BY u.id, u.username, u.total_xp, u.current_streak
		ORDER BY actions_30d DESC
		LIMIT 50
	`)
	if err != nil {
		slog.Error("analytics power users query", "error", err)
		types.WriteInternalError(w)
		return
	}
	resp.PowerUsers = []powerUser{}
	for rows.Next() {
		var p powerUser
		if err := rows.Scan(&p.Username, &p.Actions30d, &p.TotalXP, &p.CurrentStreak); err == nil {
			resp.PowerUsers = append(resp.PowerUsers, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("analytics power users rows", "error", err)
		types.WriteInternalError(w)
		return
	}

	_ = h.Cache.SetJSON(ctx, cacheKey, resp, 10*time.Minute)
	types.WriteJSON(w, http.StatusOK, resp)
}
