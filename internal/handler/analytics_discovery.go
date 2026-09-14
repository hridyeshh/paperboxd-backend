package handler

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// Retention and discovery-quality reporting.
//
// The dashboard previously rendered D1/D7/D30 and activation as literal "—"
// because no endpoint produced them, and its MAU card summed active_by_day over
// 30 days -- counting one user up to 30 times. Both numbers are computed here
// instead, from `events` rather than users.last_activity_date: that column only
// moves on XP-earning actions and web daily-open, so mobile readers who only
// browse never register at all.

// ── Retention ─────────────────────────────────────────────────────────────────

type retentionCohort struct {
	CohortWeek string  `json:"cohort_week"`
	Size       int64   `json:"size"`
	D1         int64   `json:"d1"`
	D7         int64   `json:"d7"`
	D14        int64   `json:"d14"`
	D30        int64   `json:"d30"`
	D1Rate     float64 `json:"d1_rate"`
	D7Rate     float64 `json:"d7_rate"`
	D14Rate    float64 `json:"d14_rate"`
	D30Rate    float64 `json:"d30_rate"`
}

type retentionResponse struct {
	// WindowDays is echoed back so the dashboard never has to assume it.
	WindowDays int               `json:"window_days"`
	Cohorts    []retentionCohort `json:"cohorts"`
	// Overall rates across every cohort old enough to have been measured.
	D1Rate  float64 `json:"d1_rate"`
	D7Rate  float64 `json:"d7_rate"`
	D30Rate float64 `json:"d30_rate"`
	// ActivationRate = signed up and put at least one book on a shelf within
	// 7 days. The single number worth watching on the acquisition side.
	ActivationRate float64 `json:"activation_rate"`
	// Stickiness = DAU/MAU, the conventional habit proxy.
	Stickiness float64 `json:"stickiness"`
	// DormantUsers signed up more than 30 days ago and have done nothing in 30.
	DormantUsers int64 `json:"dormant_users"`
}

// Retention handles GET /api/v1/analytics/retention?days=90
//
// Day-N retention is classic (active *on* day N), not rolling (active on or
// after day N). Classic is the stricter reading and the one the cohort grid is
// shaped for; with a rolling definition every cell would be monotonic and the
// grid would say much less.
func (h *AnalyticsHandler) Retention(w http.ResponseWriter, r *http.Request) {
	days := 90
	if v := r.URL.Query().Get("days"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 365 {
			days = parsed
		}
	}

	cacheKey := "analytics:retention:" + strconv.Itoa(days)
	var resp retentionResponse
	if err := h.Cache.GetJSON(r.Context(), cacheKey, &resp); err == nil {
		types.WriteJSON(w, http.StatusOK, resp)
		return
	}
	resp.WindowDays = days

	ctx := r.Context()

	rows, err := h.Pool.Query(ctx, `
		WITH cohort AS (
		    SELECT id AS user_id,
		           created_at::date                       AS joined_day,
		           date_trunc('week', created_at)::date   AS cohort_week
		    FROM users
		    WHERE deleted_at IS NULL
		      AND created_at > NOW() - make_interval(days => $1)
		),
		activity AS (
		    SELECT DISTINCT user_id, created_at::date AS active_day
		    FROM events
		    WHERE user_id IS NOT NULL
		)
		SELECT c.cohort_week,
		       COUNT(DISTINCT c.user_id)                                                      AS size,
		       COUNT(DISTINCT c.user_id) FILTER (WHERE a.active_day = c.joined_day + 1)       AS d1,
		       COUNT(DISTINCT c.user_id) FILTER (WHERE a.active_day = c.joined_day + 7)       AS d7,
		       COUNT(DISTINCT c.user_id) FILTER (WHERE a.active_day = c.joined_day + 14)      AS d14,
		       COUNT(DISTINCT c.user_id) FILTER (WHERE a.active_day = c.joined_day + 30)      AS d30
		FROM cohort c
		LEFT JOIN activity a ON a.user_id = c.user_id
		GROUP BY c.cohort_week
		ORDER BY c.cohort_week DESC
	`, days)
	if err != nil {
		slog.Error("analytics retention cohorts", "error", err)
		types.WriteInternalError(w)
		return
	}
	defer rows.Close()

	var totalSize, totalD1, totalD7, totalD30 int64
	for rows.Next() {
		var c retentionCohort
		var week time.Time
		if err := rows.Scan(&week, &c.Size, &c.D1, &c.D7, &c.D14, &c.D30); err != nil {
			continue
		}
		c.CohortWeek = week.Format("2006-01-02")
		c.D1Rate = rate(c.D1, c.Size)
		c.D7Rate = rate(c.D7, c.Size)
		c.D14Rate = rate(c.D14, c.Size)
		c.D30Rate = rate(c.D30, c.Size)
		resp.Cohorts = append(resp.Cohorts, c)

		totalSize += c.Size
		totalD1 += c.D1
		totalD7 += c.D7
		totalD30 += c.D30
	}
	if err := rows.Err(); err != nil {
		slog.Error("analytics retention iterate", "error", err)
		types.WriteInternalError(w)
		return
	}
	if resp.Cohorts == nil {
		resp.Cohorts = []retentionCohort{}
	}

	resp.D1Rate = rate(totalD1, totalSize)
	resp.D7Rate = rate(totalD7, totalSize)
	resp.D30Rate = rate(totalD30, totalSize)

	var activated, eligible, dau, mau int64
	err = h.Pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM users u
		     WHERE u.deleted_at IS NULL
		       AND EXISTS (
		           SELECT 1 FROM bookshelf b
		           WHERE b.user_id = u.id
		             AND b.created_at <= u.created_at + INTERVAL '7 days')),
		  (SELECT COUNT(*) FROM users
		     WHERE deleted_at IS NULL
		       AND created_at < NOW() - INTERVAL '7 days'),
		  (SELECT COUNT(DISTINCT user_id) FROM events
		     WHERE user_id IS NOT NULL AND created_at >= CURRENT_DATE),
		  (SELECT COUNT(DISTINCT user_id) FROM events
		     WHERE user_id IS NOT NULL AND created_at > NOW() - INTERVAL '30 days'),
		  (SELECT COUNT(*) FROM users u
		     WHERE u.deleted_at IS NULL
		       AND u.created_at < NOW() - INTERVAL '30 days'
		       AND NOT EXISTS (
		           SELECT 1 FROM events e
		           WHERE e.user_id = u.id
		             AND e.created_at > NOW() - INTERVAL '30 days'))
	`).Scan(&activated, &eligible, &dau, &mau, &resp.DormantUsers)
	if err != nil {
		slog.Error("analytics retention summary", "error", err)
		types.WriteInternalError(w)
		return
	}

	resp.ActivationRate = rate(activated, eligible)
	resp.Stickiness = rate(dau, mau)

	_ = h.Cache.SetJSON(ctx, cacheKey, resp, 15*time.Minute)
	types.WriteJSON(w, http.StatusOK, resp)
}

// ── Discovery quality ─────────────────────────────────────────────────────────

type discoveryFunnel struct {
	ReasonType  string  `json:"reason_type"`
	Impressions int64   `json:"impressions"`
	Opens       int64   `json:"opens"`
	Saved       int64   `json:"saved"`
	Started     int64   `json:"started"`
	Finished    int64   `json:"finished"`
	Rated       int64   `json:"rated"`
	Rated4Plus  int64   `json:"rated_4_plus"`
	Rated5      int64   `json:"rated_5"`
	Thoughts    int64   `json:"thoughts"`
	Shared      int64   `json:"shared"`
	OpenRate    float64 `json:"open_rate"`
	SaveRate    float64 `json:"save_rate"`
	FinishRate  float64 `json:"finish_rate"`
	// LoveRate is the north star: of everything we put in front of this
	// reader under this reason, how much did they end up rating 4+.
	LoveRate float64 `json:"love_rate"`
}

type discoveryResponse struct {
	WindowDays int               `json:"window_days"`
	ByReason   []discoveryFunnel `json:"by_reason"`
	Overall    discoveryFunnel   `json:"overall"`
}

// Discovery handles GET /api/v1/analytics/discovery?days=30
//
// The funnel is impression -> open -> save -> start -> finish -> rating, split
// by the reason the recommendation carried. A reason with fewer clicks but a
// higher love rate is the better reason, which is exactly the comparison raw
// CTR cannot make.
//
// Terminal states come from `bookshelf`, not from events: shelf rows are the
// durable record and predate any client-side tracking, so the bottom of the
// funnel is measurable from day one instead of from first deploy.
func (h *AnalyticsHandler) Discovery(w http.ResponseWriter, r *http.Request) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 365 {
			days = parsed
		}
	}

	cacheKey := "analytics:discovery:" + strconv.Itoa(days)
	var resp discoveryResponse
	if err := h.Cache.GetJSON(r.Context(), cacheKey, &resp); err == nil {
		types.WriteJSON(w, http.StatusOK, resp)
		return
	}
	resp.WindowDays = days

	rows, err := h.Pool.Query(r.Context(), `
		WITH impressions AS (
		    SELECT DISTINCT ON (e.user_id, e.book_id)
		           e.user_id,
		           e.book_id,
		           COALESCE(NULLIF(e.metadata->>'reason_type', ''), 'unknown') AS reason_type
		    FROM events e
		    WHERE e.event_type = 'rec_impression'
		      AND e.user_id IS NOT NULL
		      AND e.book_id IS NOT NULL
		      AND e.created_at > NOW() - make_interval(days => $1)
		    ORDER BY e.user_id, e.book_id, e.created_at ASC
		)
		SELECT i.reason_type,
		       COUNT(*)                                                    AS impressions,
		       COUNT(*) FILTER (WHERE o.seen)                              AS opens,
		       COUNT(*) FILTER (WHERE b.book_id IS NOT NULL)               AS saved,
		       COUNT(*) FILTER (WHERE b.started_at IS NOT NULL)            AS started,
		       COUNT(*) FILTER (WHERE b.finished_at IS NOT NULL)           AS finished,
		       COUNT(*) FILTER (WHERE b.rating IS NOT NULL)                AS rated,
		       COUNT(*) FILTER (WHERE b.rating >= 4)                       AS rated_4_plus,
		       COUNT(*) FILTER (WHERE b.rating = 5)                        AS rated_5,
		       COUNT(*) FILTER (WHERE d.seen)                              AS thoughts,
		       COUNT(*) FILTER (WHERE sh.seen)                             AS shared
		FROM impressions i
		LEFT JOIN LATERAL (
		    SELECT true AS seen
		    FROM events c
		    WHERE c.user_id = i.user_id
		      AND c.book_id = i.book_id
		      AND c.event_type IN ('rec_click', 'book_viewed')
		    LIMIT 1
		) o ON true
		LEFT JOIN bookshelf b
		       ON b.user_id = i.user_id AND b.book_id = i.book_id
		LEFT JOIN LATERAL (
		    SELECT true AS seen FROM thoughts d
		    WHERE d.user_id = i.user_id AND d.book_id = i.book_id LIMIT 1
		) d ON true
		LEFT JOIN LATERAL (
		    SELECT true AS seen FROM activities a
		    WHERE a.user_id = i.user_id AND a.book_id = i.book_id AND a.activity_type = 'shared_book' LIMIT 1
		) sh ON true
		GROUP BY i.reason_type
		ORDER BY impressions DESC
	`, days)
	if err != nil {
		slog.Error("analytics discovery funnel", "error", err)
		types.WriteInternalError(w)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var f discoveryFunnel
		if err := rows.Scan(
			&f.ReasonType, &f.Impressions, &f.Opens, &f.Saved,
			&f.Started, &f.Finished, &f.Rated, &f.Rated4Plus, &f.Rated5, &f.Thoughts, &f.Shared,
		); err != nil {
			continue
		}
		f.fillRates()
		resp.ByReason = append(resp.ByReason, f)

		resp.Overall.Impressions += f.Impressions
		resp.Overall.Opens += f.Opens
		resp.Overall.Saved += f.Saved
		resp.Overall.Started += f.Started
		resp.Overall.Finished += f.Finished
		resp.Overall.Rated += f.Rated
		resp.Overall.Rated4Plus += f.Rated4Plus
		resp.Overall.Rated5 += f.Rated5
		resp.Overall.Thoughts += f.Thoughts
		resp.Overall.Shared += f.Shared
	}
	if err := rows.Err(); err != nil {
		slog.Error("analytics discovery iterate", "error", err)
		types.WriteInternalError(w)
		return
	}
	if resp.ByReason == nil {
		resp.ByReason = []discoveryFunnel{}
	}
	resp.Overall.ReasonType = "all"
	resp.Overall.fillRates()

	_ = h.Cache.SetJSON(r.Context(), cacheKey, resp, 15*time.Minute)
	types.WriteJSON(w, http.StatusOK, resp)
}

func (f *discoveryFunnel) fillRates() {
	f.OpenRate = rate(f.Opens, f.Impressions)
	f.SaveRate = rate(f.Saved, f.Impressions)
	f.FinishRate = rate(f.Finished, f.Saved)
	f.LoveRate = rate(f.Rated4Plus, f.Impressions)
}

// rate returns n/d as a 0..1 fraction, or 0 when the denominator is zero.
// Every ratio on this page goes through it so an empty cohort reads as 0
// rather than as NaN in the dashboard's JSON.
func rate(n, d int64) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}
