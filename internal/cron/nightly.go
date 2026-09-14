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
	recomputeStaleThoughtCentroids(pool, recSvc)
	recomputeStaleTraitProfiles(pool, recSvc)
	recomputeTasteOverlaps(recSvc)
	purgeSoftDeletedUsers(pool)
	slog.Info("nightly jobs: done")
}

// recomputeTasteOverlaps rebuilds the reader-to-reader similarity table.
// Runs after trait profiles so the trait term sees tonight's numbers.
func recomputeTasteOverlaps(recSvc *service.RecommendationService) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := recSvc.RecomputeTasteOverlaps(ctx); err != nil {
		slog.Error("nightly: taste overlaps", "error", err)
	}
}

// recomputeStaleTraitProfiles refreshes the interpretable trait axes.
//
// Kept separate from recomputeStaleProfiles for the same reason the thought job
// is: that one rebuilds genre, author and velocity signals from `bookshelf` and
// never touches trait_prefs. Trait preferences also move for a second reason
// the shelf cannot show — the nightly trait backfill gives previously
// unextracted books a vocabulary, which changes what a reader's existing
// ratings imply without any new rating being made.
func recomputeStaleTraitProfiles(pool *pgxpool.Pool, recSvc *service.RecommendationService) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rows, err := pool.Query(ctx, `
		SELECT DISTINCT b.user_id::text
		FROM bookshelf b
		JOIN book_traits t ON t.book_id = b.book_id
		LEFT JOIN user_signal_profiles usp ON usp.user_id = b.user_id
		WHERE usp.user_id IS NULL
		   OR usp.trait_prefs IS NULL
		   OR usp.computed_at < NOW() - INTERVAL '24 hours'
		LIMIT 100
	`)
	if err != nil {
		slog.Error("nightly: query stale trait profiles", "error", err)
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
		slog.Error("nightly: iterate stale trait profiles", "error", err)
		return
	}

	slog.Info("nightly: recomputing trait profiles", "count", len(userIDs))
	for _, uid := range userIDs {
		// ComputeAndSaveNegativeSignals is a superset of ComputeAndSaveTraitProfile:
		// it builds the same trait preferences and additionally folds in explicit
		// verdicts, coded rejections and early abandonment. Calling the narrower
		// one here would overwrite the negative half with every nightly run.
		if err := recSvc.ComputeAndSaveNegativeSignals(ctx, uid); err != nil {
			slog.Warn("nightly: trait profile", "user_id", uid, "error", err)
		}
	}
}

// recomputeStaleThoughtCentroids refreshes user_signal_profiles.thought_embedding.
//
// recomputeStaleProfiles cannot cover this: it enumerates from `bookshelf` and
// calls GetOrComputeSignalProfile, which rebuilds bookshelf and fast-finish
// signals but never touches the thought centroid. Before this job existed the
// only caller of ComputeAndSaveThoughtCentroid was cmd/backfill-embeddings, a
// manual one-off — so a reader's centroid was whatever the last backfill left,
// and a reader who kept a thought but no shelf was never enumerated at all.
//
// Enumerating from thoughts fixes both: shelf-less diarists are included,
// and readers whose entries have all been made private (no rows with a
// non-NULL embedding) still get picked up, so ComputeAndSaveThoughtCentroid's
// zero-embeddings branch can null out a centroid derived from since-retracted
// text.
func recomputeStaleThoughtCentroids(pool *pgxpool.Pool, recSvc *service.RecommendationService) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rows, err := pool.Query(ctx, `
		SELECT DISTINCT de.user_id::text
		FROM thoughts de
		LEFT JOIN user_signal_profiles usp ON usp.user_id = de.user_id
		WHERE usp.user_id IS NULL
		   OR usp.computed_at < NOW() - INTERVAL '24 hours'
		LIMIT 100
	`)
	if err != nil {
		slog.Error("nightly: query stale thought centroids", "error", err)
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
		slog.Error("nightly: iterate stale thought centroids", "error", err)
		return
	}

	slog.Info("nightly: recomputing thought centroids", "count", len(userIDs))
	for _, uid := range userIDs {
		if err := recSvc.ComputeAndSaveThoughtCentroid(ctx, uid); err != nil {
			slog.Warn("nightly: recompute thought centroid", "user_id", uid, "error", err)
		}
	}
}

// softDeleteRetention is how long a soft-deleted account survives before it is
// permanently erased. Backs the privacy-policy commitment to hard-delete within
// 30 days of an account-deletion request.
const softDeleteRetention = 30 * 24 * time.Hour

// purgeSoftDeletedUsers hard-deletes users whose deleted_at is older than the
// retention window. Every user-owned table FKs users(id) ON DELETE CASCADE, so
// this one DELETE removes bookshelf, thought, reviews, lists, events, tokens, and
// signal profiles along with the row; the few referring columns (referred_by,
// activity target_user_id) are ON DELETE SET NULL. The account_deletions audit
// row is not FK-linked and is intentionally retained for retention analysis.
func purgeSoftDeletedUsers(pool *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// NULL deleted_at (live users) never matches `< cutoff`, so live accounts are
	// never touched; the explicit IS NOT NULL keeps that guarantee obvious.
	cutoff := time.Now().Add(-softDeleteRetention)
	tag, err := pool.Exec(ctx, `
		DELETE FROM users
		WHERE deleted_at IS NOT NULL
		  AND deleted_at < $1
	`, cutoff)
	if err != nil {
		slog.Error("nightly: purge soft-deleted users", "error", err)
		return
	}
	slog.Info("nightly: purged soft-deleted users", "count", tag.RowsAffected())
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
