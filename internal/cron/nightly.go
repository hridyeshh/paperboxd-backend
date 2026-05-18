package cron

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hridyesh/paperboxd-backend/internal/service"
)

// StartNightlyCron runs nightly jobs once at startup and then every 24 hours.
// It is non-blocking: the ticker loop runs in a background goroutine.
func StartNightlyCron(pool *pgxpool.Pool, recSvc *service.RecommendationService) {
	go func() {
		runNightlyJobs(pool, recSvc)

		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			runNightlyJobs(pool, recSvc)
		}
	}()
}

func runNightlyJobs(pool *pgxpool.Pool, recSvc *service.RecommendationService) {
	slog.Info("nightly jobs: starting")
	recomputeStaleProfiles(pool, recSvc)
	slog.Info("nightly jobs: done")
}

// recomputeStaleProfiles refreshes signal profiles for users whose profiles are
// missing or older than 24 hours, processing up to 100 users per run.
func recomputeStaleProfiles(pool *pgxpool.Pool, recSvc *service.RecommendationService) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rows, err := pool.Query(ctx, `
		SELECT DISTINCT bs.user_id::text
		FROM bookshelf bs
		LEFT JOIN user_signal_profiles usp ON usp.user_id = bs.user_id
		WHERE usp.user_id IS NULL
		   OR usp.computed_at < NOW() - INTERVAL '24 hours'
		LIMIT 100
	`)
	if err != nil {
		slog.Error("nightly: query stale profiles", "error", err)
		return
	}
	defer rows.Close()

	var userIDs []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err == nil {
			userIDs = append(userIDs, uid)
		}
	}
	if err := rows.Err(); err != nil {
		slog.Error("nightly: iterate stale profiles", "error", err)
		return
	}

	slog.Info("nightly: recomputing signal profiles", "count", len(userIDs))
	for _, uid := range userIDs {
		if _, err := recSvc.GetOrComputeSignalProfile(ctx, uid); err != nil {
			slog.Warn("nightly: recompute profile", "user_id", uid, "error", err)
		}
	}
}
