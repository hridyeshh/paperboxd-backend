// backfill-read-links matches books against Project Gutenberg (via Gutendex)
// so public-domain titles get a "Read free" button. The nightly cron runs the
// same job capped at a few hundred books; this is for a first pass over the
// catalogue and for spot-checking matches.
//
// Flags:
//
//	--limit N   Check at most N books (default 300). Books checked in the last
//	            30 days are skipped, so rerunning continues where it left off.
//
// Every match is logged as "gutenberg backfill: match" with the book's title
// and the Gutenberg title. Read a few before trusting the run: a wrong match
// puts a stranger's book behind "Read free".
//
// Coverage audit:
//
//	SELECT COUNT(*) FILTER (WHERE gutenberg_checked_at IS NOT NULL) AS checked,
//	       COUNT(*) FILTER (WHERE is_public_domain) AS public_domain
//	FROM book_read_links;
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/service"
)

func main() {
	limit := flag.Int("limit", 300, "check at most N books")
	flag.Parse()

	_ = godotenv.Load()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		slog.Error("DATABASE_URL not set")
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		slog.Error("connect to postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		slog.Error("ping postgres", "error", err)
		os.Exit(1)
	}

	svc := service.NewReadLinksService(db.New(pool), nil, nil, external.NewGutenbergClient())
	start := time.Now()
	run, err := svc.BackfillPublicDomain(ctx, int32(*limit))
	if err != nil {
		slog.Error("gutenberg backfill", "error", err, "checked", run.Checked)
		os.Exit(1)
	}
	slog.Info("gutenberg backfill: done", "checked", run.Checked, "matched", run.Matched,
		"no_match", run.NoMatch, "errored", run.Errored, "aborted", run.Aborted,
		"elapsed", time.Since(start).Round(time.Second))
}
