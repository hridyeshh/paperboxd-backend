// cmd/migrate-mongo-to-pg migrates all data from MongoDB (Next.js backend)
// to PostgreSQL (Go backend).
//
// Environment variables:
//
//	MONGO_URI      - MongoDB connection string (required)
//	MONGO_DB       - MongoDB database name (default: "paperboxd")
//	POSTGRES_URL   - PostgreSQL DSN (required)
//	DRY_RUN        - Set to "true" to log without writing (default: false)
//
// Usage:
//
//	DRY_RUN=true MONGO_URI=... POSTGRES_URL=... go run ./cmd/migrate-mongo-to-pg
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"
)

func main() {
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg := configFromEnv()
	if cfg.DryRun {
		slog.Info("DRY RUN mode — no data will be written")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	conn, err := connect(ctx, cfg)
	if err != nil {
		slog.Error("failed to connect", "error", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	// ── Prerequisites: ensure all PG tables exist ─────────────────────────────
	// Run all migrations before this script:
	//   migrate -path ./migrations -database $POSTGRES_URL up
	//
	// Plus the additional tables from MONGO_MIGRATION_ANALYSIS.md:
	//   migration 000007 (bookshelf.notes/format, user reading_goal, newsletters,
	//                      account_deletions, otps, user_preferences)
	// ────────────────────────────────────────────────────────────────────────────

	steps := []struct {
		name string
		fn   func(context.Context, *Connections, bool) error
	}{
		{"books", migrateBooks},
		{"users_core", migrateUsers},
		{"follows", migrateFollows},
		{"likes", migrateLikes},
		{"bookshelf", migrateBookshelf},
		{"favorites", migrateFavorites},
		{"lists", migrateLists},
		{"diary_entries", migrateDiaryEntries},
		{"activities", migrateActivities},
		{"newsletters", migrateNewsletters},
		{"account_deletions", migrateAccountDeletions},
	}

	for _, step := range steps {
		slog.Info("starting step", "step", step.name)
		start := time.Now()
		if err := step.fn(ctx, conn, cfg.DryRun); err != nil {
			slog.Error("step failed", "step", step.name, "error", err)
			os.Exit(1)
		}
		slog.Info("step complete", "step", step.name, "elapsed", time.Since(start).Round(time.Millisecond))
	}

	// Recount denormalized counters
	if !cfg.DryRun {
		slog.Info("recounting user stats")
		recountUserStats(ctx, conn)
	}

	// Verify
	slog.Info("running verification checks")
	verify(ctx, conn)

	slog.Info("migration complete")
}

// recountUserStats recalculates all denormalized COUNT columns on users.
func recountUserStats(ctx context.Context, conn *Connections) {
	queries := []string{
		`UPDATE users u SET books_read_count = (
			SELECT COUNT(*) FROM bookshelf WHERE user_id = u.id AND status = 'read'
		)`,
		`UPDATE users u SET favorites_count = (
			SELECT COUNT(*) FROM favorites WHERE user_id = u.id
		)`,
		`UPDATE users u SET lists_count = (
			SELECT COUNT(*) FROM lists WHERE user_id = u.id
		)`,
		`UPDATE users u SET diary_entries_count = (
			SELECT COUNT(*) FROM diary_entries WHERE user_id = u.id
		)`,
		`UPDATE users u SET followers_count = (
			SELECT COUNT(*) FROM follows WHERE following_id = u.id
		)`,
		`UPDATE users u SET following_count = (
			SELECT COUNT(*) FROM follows WHERE follower_id = u.id
		)`,
	}
	for _, q := range queries {
		if _, err := conn.PG.Exec(ctx, q); err != nil {
			slog.Error("recount failed", "query", q[:40], "error", err)
		}
	}
	slog.Info("user stats recounted")
}
