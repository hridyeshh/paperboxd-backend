// backfill-traits extracts book characteristics (the nine axes in
// service.TraitAxes) for every book that has a usable description.
//
// Why this exists: a book's embedding can say "close to that one" but never
// "character-driven and slow-burn", and `categories` is too coarse to split a
// genre the reader likes from the same genre they bounce off. The extracted
// axes are what both trait ranking and the "you tend to love…" reasons read.
//
// Flags:
//
//	--dry-run      Print what would be extracted, call nothing, write nothing
//	--limit N      Process at most N books (0 = every book that needs traits)
//	--batch N      Books per Claude call (default service.TraitBatchSize)
//	--profiles     After extraction, recompute trait profiles for all readers
//	--sample N     Print the full extracted traits for the first N books
//
// Recommended first run — look before you spend:
//
//	go run ./cmd/backfill-traits --limit 24 --sample 8
//
// Check a few well-known titles against the printed axes before running the
// whole corpus. Wrong traits are worse than none: they produce a confident,
// specific, false reason.
//
// Coverage audit:
//
//	SELECT COUNT(*) AS books,
//	       COUNT(t.book_id) AS with_traits,
//	       AVG(t.confidence) AS mean_confidence
//	FROM books b LEFT JOIN book_traits t ON t.book_id = b.id;
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/hridyesh/paperboxd-backend/internal/service"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "log what would be processed without calling Claude or writing")
	limit := flag.Int("limit", 0, "process at most N books (0 = no limit)")
	batch := flag.Int("batch", service.TraitBatchSize, "books per Claude call")
	profiles := flag.Bool("profiles", false, "recompute trait profiles for all readers after extraction")
	sample := flag.Int("sample", 0, "print full extracted traits for the first N books")
	flag.Parse()

	_ = godotenv.Load()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		slog.Error("DATABASE_URL not set")
		os.Exit(1)
	}
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	if anthropicKey == "" && !*dryRun {
		slog.Error("ANTHROPIC_API_KEY not set (use --dry-run to inspect the queue without it)")
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

	svc := service.NewRecommendationService(pool, service.NoopEmbedder{}, nil, nil, anthropicKey)
	extractor := service.NewTraitExtractor(anthropicKey)

	queueLimit := *limit
	if queueLimit == 0 {
		queueLimit = 100000
	}
	books, err := svc.GetBooksNeedingTraits(ctx, queueLimit)
	if err != nil {
		slog.Error("load trait queue", "error", err)
		os.Exit(1)
	}
	slog.Info("trait backfill", "books", len(books), "batch_size", *batch,
		"extractor_version", service.TraitExtractorVersion, "dry_run", *dryRun)

	if *dryRun {
		for i, b := range books {
			if i >= 10 {
				slog.Info("… and more", "remaining", len(books)-10)
				break
			}
			slog.Info("would extract", "title", b.Title, "desc_len", len(b.Description))
		}
		return
	}

	var done, failed, printed int
	start := time.Now()

	for i := 0; i < len(books); i += *batch {
		end := min(i+*batch, len(books))
		chunk := books[i:end]

		traits, err := extractor.Extract(ctx, chunk)
		if err != nil {
			// A batch fails as a unit because the index-alignment check is what
			// stops one book's traits landing on another. Skip and continue;
			// the queue query picks these up on the next run.
			slog.Warn("extract batch failed", "start", i, "size", len(chunk), "error", err)
			failed += len(chunk)
			continue
		}

		for j, t := range traits {
			if err := svc.SaveBookTraits(ctx, chunk[j].BookID, t); err != nil {
				slog.Warn("save traits", "book", chunk[j].Title, "error", err)
				failed++
				continue
			}
			done++
			if printed < *sample {
				printed++
				out, _ := json.MarshalIndent(t, "", "  ")
				slog.Info("sample", "title", chunk[j].Title, "traits", string(out))
			}
		}

		slog.Info("progress", "done", done, "failed", failed,
			"total", len(books), "elapsed", time.Since(start).Round(time.Second))
	}

	slog.Info("extraction complete", "saved", done, "failed", failed,
		"elapsed", time.Since(start).Round(time.Second))

	if !*profiles {
		return
	}

	// Trait preferences are derived from book traits, so every reader whose
	// shelf was just given a vocabulary needs recomputing before ranking can
	// use any of this.
	rows, err := pool.Query(ctx, `SELECT DISTINCT user_id::text FROM bookshelf`)
	if err != nil {
		slog.Error("list readers", "error", err)
		return
	}
	var userIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			userIDs = append(userIDs, id)
		}
	}
	rows.Close()

	var profileCount int
	for _, id := range userIDs {
		if err := svc.ComputeAndSaveTraitProfile(ctx, id); err != nil {
			slog.Warn("trait profile", "user", id, "error", err)
			continue
		}
		profileCount++
	}
	slog.Info("trait profiles recomputed", "readers", profileCount, "of", len(userIDs))
}
