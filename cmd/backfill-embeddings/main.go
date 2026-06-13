// backfill-embeddings generates and stores Cohere embeddings for books and diary entries.
//
// Default mode: embed books where embedding IS NULL.
//
// Enrichment mode (--enrich): target books with NULL/thin descriptions (<200 chars)
// or missing embeddings. Fetches descriptions from Open Library, ISBNdb, and Google Books
// before embedding. Requires ISBNDB_API_KEY and optionally GOOGLE_BOOKS_API_KEY.
//
// Diary mode (--diary): embed diary_entries where embedding IS NULL.
// Add --recompute-profiles to also recompute diary centroids for all affected users.
//
// Flags:
//
//	--enrich              Enrich thin descriptions from external sources before embedding
//	--diary               Embed diary entries instead of books
//	--recompute-profiles  After --diary, recompute diary centroid for each affected user
//	--dry-run             Log what would be processed without writing to DB
//	--limit N             Process at most N records (0 = no limit)
//
// After book backfill completes, create the ivfflat index if not already done:
//
//	CREATE INDEX books_embedding_idx
//	  ON books USING ivfflat (embedding vector_cosine_ops)
//	  WITH (lists = 100);
//
// Audit queries — run after backfill to verify coverage:
//
//	-- Books
//	SELECT
//	  COUNT(*) AS total_books,
//	  COUNT(CASE WHEN embedding IS NOT NULL THEN 1 END) AS has_embedding,
//	  COUNT(CASE WHEN embedding_text IS NOT NULL THEN 1 END) AS has_embedding_text,
//	  COUNT(CASE WHEN LENGTH(description) > 200 THEN 1 END) AS rich_description,
//	  COUNT(CASE WHEN LENGTH(description) BETWEEN 1 AND 200 THEN 1 END) AS thin_description,
//	  COUNT(CASE WHEN description IS NULL OR description = '' THEN 1 END) AS no_description,
//	  COUNT(CASE WHEN description_source = 'open_library' THEN 1 END) AS from_open_library,
//	  COUNT(CASE WHEN description_source = 'isbndb' THEN 1 END) AS from_isbndb,
//	  COUNT(CASE WHEN description_source = 'google_books' THEN 1 END) AS from_google_books,
//	  COUNT(CASE WHEN description_source = 'title_only' THEN 1 END) AS title_only
//	FROM books;
//
//	-- Diary entries
//	SELECT
//	  COUNT(*) AS total_entries,
//	  COUNT(CASE WHEN embedding IS NOT NULL THEN 1 END) AS has_embedding,
//	  COUNT(CASE WHEN embedding IS NULL THEN 1 END) AS missing_embedding
//	FROM diary_entries;
//
//	-- Signal profiles with diary centroid
//	SELECT COUNT(*) AS with_centroid FROM user_signal_profiles WHERE diary_embedding IS NOT NULL;
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"

	"github.com/hridyesh/paperboxd-backend/internal/service"
)

func main() {
	enrich            := flag.Bool("enrich", false, "enrich thin descriptions from Open Library/ISBNdb/Google Books before embedding")
	diary             := flag.Bool("diary", false, "embed diary entries instead of books")
	recomputeProfiles := flag.Bool("recompute-profiles", false, "after --diary, recompute diary centroid for each affected user")
	fastFinish        := flag.Bool("fast-finish", false, "compute and store fast_finish_embedding for all users with velocity signals")
	bustCache         := flag.Bool("bust-cache", false, "delete all rec:pool:* keys from Redis (run after Phase 5 deploy for clean reasonType data)")
	dryRun            := flag.Bool("dry-run", false, "log what would be processed without writing to the database")
	limit             := flag.Int("limit", 0, "process at most N records (0 = no limit)")
	flag.Parse()

	_ = godotenv.Load()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		slog.Error("DATABASE_URL not set")
		os.Exit(1)
	}
	cohereKey := os.Getenv("COHERE_API_KEY")
	if cohereKey == "" {
		slog.Error("COHERE_API_KEY not set")
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
	slog.Info("connected to postgres")

	embedder := service.NewCohereEmbedder(cohereKey)
	svc := service.NewRecommendationService(pool, embedder, nil, nil)

	var enricher *service.Enricher
	if *enrich {
		isbndbKey := os.Getenv("ISBNDB_API_KEY")
		gbKey := os.Getenv("GOOGLE_BOOKS_API_KEY")
		enricher = service.NewEnricher(isbndbKey, gbKey)
		slog.Info("enrichment mode enabled", "isbndb_key_set", isbndbKey != "", "google_key_set", gbKey != "")
	}

	switch {
	case *bustCache:
		redisClient := connectRedis()
		if redisClient == nil {
			slog.Error("REDIS_URL not set — cannot bust cache")
			os.Exit(1)
		}
		defer redisClient.Close()
		runBustCacheMode(ctx, redisClient, *dryRun)
		return
	case *fastFinish:
		runFastFinishMode(ctx, pool, svc, *dryRun)
	case *diary:
		runDiaryMode(ctx, pool, svc, *limit, *dryRun, *recomputeProfiles)
	case *enrich:
		runEnrichMode(ctx, pool, svc, enricher, *limit, *dryRun)
	default:
		runDefaultMode(ctx, pool, svc, *limit, *dryRun)
	}
}

// runDefaultMode embeds books that currently have embedding IS NULL.
func runDefaultMode(ctx context.Context, pool *pgxpool.Pool, svc *service.RecommendationService, limit int, dryRun bool) {
	books, err := svc.GetBooksWithoutEmbeddings(ctx)
	if err != nil {
		slog.Error("fetch books without embeddings", "error", err)
		os.Exit(1)
	}
	if limit > 0 && len(books) > limit {
		books = books[:limit]
	}
	total := len(books)
	slog.Info("books to embed", "count", total, "dry_run", dryRun)
	if total == 0 {
		slog.Info("all books already have embeddings — nothing to do")
		return
	}

	const batchSize = 96
	succeeded, failed := 0, 0

	for start := 0; start < total; start += batchSize {
		end := start + batchSize
		if end > total {
			end = total
		}
		batch := books[start:end]

		texts := make([]string, len(batch))
		for i, b := range batch {
			texts[i] = service.BookEmbedText(b.Title, b.Subtitle, b.Authors, "", b.Categories, b.Description)
		}

		if dryRun {
			for _, b := range batch {
				slog.Info("dry-run: would embed", "id", b.ID, "title", b.Title)
			}
			succeeded += len(batch)
			continue
		}

		vecs, err := svc.Embedder().EmbedTexts(texts, "search_document")
		if err != nil {
			slog.Error("embed batch", "start", start, "end", end, "error", err)
			failed += len(batch)
			time.Sleep(650 * time.Millisecond)
			continue
		}

		for i, book := range batch {
			if i >= len(vecs) {
				failed++
				continue
			}
			if err := svc.SaveBookEmbeddingWithText(ctx, book.ID, texts[i], vecs[i]); err != nil {
				slog.Error("save embedding", "book_id", book.ID, "error", err)
				failed++
				continue
			}
			succeeded++
		}

		fmt.Printf("embedded %d/%d books...\n", min(start+batchSize, total), total)

		if end < total {
			time.Sleep(650 * time.Millisecond)
		}
	}

	slog.Info("backfill complete", "succeeded", succeeded, "failed", failed)
	printNextSteps()
}

// runEnrichMode enriches thin descriptions then re-embeds.
func runEnrichMode(ctx context.Context, pool *pgxpool.Pool, svc *service.RecommendationService, enricher *service.Enricher, limit int, dryRun bool) {
	books, err := svc.GetBooksForEnrichment(ctx, limit)
	if err != nil {
		slog.Error("fetch books for enrichment", "error", err)
		os.Exit(1)
	}
	total := len(books)
	slog.Info("books to enrich and embed", "count", total, "dry_run", dryRun)
	if total == 0 {
		slog.Info("no books need enrichment — nothing to do")
		return
	}

	succeeded, failed, enriched := 0, 0, 0

	for i, book := range books {
		if dryRun {
			slog.Info("dry-run: would enrich",
				"id", book.ID, "title", book.Title,
				"desc_len", len(book.Description),
				"open_library_id", book.OpenLibraryID,
				"isbndb_id", book.ISBNdbID,
				"google_books_id", book.GoogleBooksID,
			)
			succeeded++
			continue
		}

		desc := book.Description
		descSource := ""

		if enricher != nil && len(desc) < 200 {
			enrichedDesc, src, enrichErr := enricher.EnrichBookDescription(ctx, book)
			if enrichErr == nil && len(enrichedDesc) > len(desc) {
				desc = enrichedDesc
				descSource = src
				enriched++

				if _, dbErr := pool.Exec(ctx,
					`UPDATE books SET description = $1, description_source = $2 WHERE id = $3`,
					desc, descSource, book.ID,
				); dbErr != nil {
					slog.Warn("save enriched description", "book_id", book.ID, "error", dbErr)
				}
			}
		}

		embedText := service.BookEmbedText(book.Title, book.Subtitle, book.Authors, book.Publisher, book.Categories, desc)

		vecs, embedErr := svc.Embedder().EmbedTexts([]string{embedText}, "search_document")
		if embedErr != nil || len(vecs) == 0 {
			slog.Warn("embed failed", "book_id", book.ID, "error", embedErr)
			failed++
			time.Sleep(50 * time.Millisecond)
			continue
		}

		if err := svc.SaveBookEmbeddingWithText(ctx, book.ID, embedText, vecs[0]); err != nil {
			slog.Warn("save embedding", "book_id", book.ID, "error", err)
			failed++
			time.Sleep(50 * time.Millisecond)
			continue
		}

		succeeded++
		time.Sleep(50 * time.Millisecond)

		if (i+1)%50 == 0 {
			slog.Info("progress", "enriched", i+1, "total", total, "descriptions_enriched", enriched)
		}
	}

	slog.Info("enrichment backfill complete",
		"succeeded", succeeded, "failed", failed,
		"descriptions_enriched", enriched,
	)
	printNextSteps()
}

func printNextSteps() {
	fmt.Println("\n\033[1mNext step — create the ivfflat index if not already done:\033[0m")
	fmt.Println("  CREATE INDEX books_embedding_idx")
	fmt.Println("    ON books USING ivfflat (embedding vector_cosine_ops)")
	fmt.Println("    WITH (lists = 100);")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func connectRedis() *redis.Client {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		return nil
	}
	addr := redisURL
	password := ""
	if strings.HasPrefix(redisURL, "redis://") {
		if u, err := url.Parse(redisURL); err == nil {
			addr = u.Host
			if p, ok := u.User.Password(); ok {
				password = p
			}
		}
	}
	return redis.NewClient(&redis.Options{Addr: addr, Password: password})
}

// runBustCacheMode deletes all rec:pool:* keys from Redis using SCAN+DEL.
// Safe for production Redis — does not use KEYS command.
func runBustCacheMode(ctx context.Context, client *redis.Client, dryRun bool) {
	const pattern = "rec:pool:*"
	var cursor uint64
	var deleted int64

	for {
		keys, nextCursor, err := client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			slog.Error("redis SCAN failed", "error", err)
			return
		}
		if len(keys) > 0 {
			if dryRun {
				slog.Info("dry-run: would delete keys", "count", len(keys))
			} else {
				n, err := client.Del(ctx, keys...).Result()
				if err != nil {
					slog.Warn("redis DEL failed", "error", err)
				} else {
					deleted += n
				}
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	slog.Info("cache bust complete", "keys_deleted", deleted, "dry_run", dryRun)
}

// runFastFinishMode computes fast_finish_embedding for all users where velocity_signal IS NOT NULL.
// Run after --recompute-profiles so velocity_signal.fast_finish_book_ids are populated.
//
// Verification queries:
//
//	SELECT
//	  COUNT(*) AS total_profiles,
//	  COUNT(velocity_signal) AS has_velocity,
//	  COUNT(diary_embedding) AS has_diary_centroid,
//	  COUNT(fast_finish_embedding) AS has_fast_finish,
//	  COUNT(CASE WHEN velocity_signal->>'velocity_bucket' = 'fast' THEN 1 END) AS fast_readers,
//	  COUNT(CASE WHEN velocity_signal->>'velocity_bucket' = 'slow' THEN 1 END) AS slow_readers
//	FROM user_signal_profiles;
//
//	SELECT key, value, updated_at FROM feature_flags;
func runFastFinishMode(ctx context.Context, pool *pgxpool.Pool, svc *service.RecommendationService, dryRun bool) {
	rows, err := pool.Query(ctx, `
		SELECT user_id::text, velocity_signal
		FROM user_signal_profiles
		WHERE velocity_signal IS NOT NULL
		ORDER BY user_id
	`)
	if err != nil {
		slog.Error("fetch velocity signals", "error", err)
		os.Exit(1)
	}

	type userVel struct {
		UserID   string
		Velocity service.VelocitySignal
	}
	var users []userVel
	for rows.Next() {
		var userID string
		var velJSON []byte
		if err := rows.Scan(&userID, &velJSON); err != nil {
			slog.Warn("scan user velocity", "error", err)
			continue
		}
		var vel service.VelocitySignal
		if err := json.Unmarshal(velJSON, &vel); err != nil {
			slog.Warn("unmarshal velocity signal", "user_id", userID, "error", err)
			continue
		}
		users = append(users, userVel{UserID: userID, Velocity: vel})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("read velocity rows", "error", err)
		os.Exit(1)
	}

	total := len(users)
	slog.Info("users with velocity signals", "count", total, "dry_run", dryRun)
	if total == 0 {
		slog.Info("no velocity signals found — run --diary --recompute-profiles first")
		return
	}

	succeeded, skipped, failed := 0, 0, 0
	for i, u := range users {
		if len(u.Velocity.FastFinishBookIDs) == 0 {
			skipped++
			continue
		}
		if dryRun {
			slog.Info("dry-run: would compute fast_finish_embedding",
				"user_id", u.UserID,
				"fast_finish_book_count", len(u.Velocity.FastFinishBookIDs),
			)
			succeeded++
			continue
		}

		centroid, err := svc.ComputeFastFinishEmbedding(ctx, &u.Velocity)
		if err != nil {
			slog.Warn("compute fast_finish_embedding", "user_id", u.UserID, "error", err)
			failed++
			continue
		}
		if centroid == nil {
			skipped++
			continue
		}

		vecLit := service.ExportFloat32Literal(centroid)
		if _, dbErr := pool.Exec(ctx,
			`UPDATE user_signal_profiles SET fast_finish_embedding = $1::vector WHERE user_id = $2`,
			vecLit, u.UserID,
		); dbErr != nil {
			slog.Warn("save fast_finish_embedding", "user_id", u.UserID, "error", dbErr)
			failed++
			continue
		}
		succeeded++

		if (i+1)%10 == 0 {
			slog.Info("progress", "processed", i+1, "total", total,
				"succeeded", succeeded, "skipped", skipped, "failed", failed)
		}
	}

	slog.Info("fast-finish backfill complete",
		"succeeded", succeeded, "skipped", skipped, "failed", failed)
}

// runDiaryMode embeds diary entries that have embedding IS NULL.
// If recomputeProfiles is true, recomputes the diary centroid for every user
// that had at least one entry newly embedded.
func runDiaryMode(ctx context.Context, pool *pgxpool.Pool, svc *service.RecommendationService, limit int, dryRun bool, recomputeProfiles bool) {
	rows, err := pool.Query(ctx, `
		SELECT
		    de.id::text,
		    de.user_id::text,
		    de.content,
		    COALESCE(b.title, '')   AS book_title,
		    COALESCE(b.authors, '{}') AS book_authors
		FROM diary_entries de
		LEFT JOIN books b ON de.book_id = b.id
		WHERE de.embedding IS NULL
		ORDER BY de.created_at DESC
	`)
	if err != nil {
		slog.Error("fetch diary entries without embeddings", "error", err)
		os.Exit(1)
	}

	type diaryRow struct {
		ID          string
		UserID      string
		Content     string
		BookTitle   string
		BookAuthors []string
	}
	var entries []diaryRow
	for rows.Next() {
		var r diaryRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.Content, &r.BookTitle, &r.BookAuthors); err != nil {
			slog.Warn("scan diary row", "error", err)
			continue
		}
		entries = append(entries, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("read diary rows", "error", err)
		os.Exit(1)
	}

	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	total := len(entries)
	slog.Info("diary entries to embed", "count", total, "dry_run", dryRun)
	if total == 0 {
		slog.Info("all diary entries already have embeddings — nothing to do")
		return
	}

	succeeded, failed := 0, 0
	affectedUsers := map[string]struct{}{}

	for i, e := range entries {
		if dryRun {
			slog.Info("dry-run: would embed diary entry",
				"id", e.ID, "user_id", e.UserID,
				"book_title", e.BookTitle, "content_len", len(e.Content),
			)
			succeeded++
			continue
		}

		embedText := service.DiaryEmbedText(e.BookTitle, e.BookAuthors, e.Content)
		if embedText == "" {
			slog.Warn("empty embed text, skipping", "entry_id", e.ID)
			failed++
			continue
		}

		vecs, embedErr := svc.Embedder().EmbedTexts([]string{embedText}, "search_document")
		if embedErr != nil || len(vecs) == 0 {
			slog.Warn("embed failed", "entry_id", e.ID, "error", embedErr)
			failed++
			time.Sleep(50 * time.Millisecond)
			continue
		}

		vec := service.ExportFloat32Literal(vecs[0])
		_, dbErr := pool.Exec(ctx,
			`UPDATE diary_entries SET embedding = $1::vector, embedding_text = $2 WHERE id = $3`,
			vec, embedText, e.ID,
		)
		if dbErr != nil {
			slog.Warn("save diary embedding", "entry_id", e.ID, "error", dbErr)
			failed++
			time.Sleep(50 * time.Millisecond)
			continue
		}

		succeeded++
		affectedUsers[e.UserID] = struct{}{}
		time.Sleep(50 * time.Millisecond)

		if (i+1)%50 == 0 {
			slog.Info("progress", "processed", i+1, "total", total)
		}
	}

	slog.Info("diary backfill complete", "succeeded", succeeded, "failed", failed)

	if recomputeProfiles && !dryRun && len(affectedUsers) > 0 {
		slog.Info("recomputing diary centroids", "user_count", len(affectedUsers))
		for userID := range affectedUsers {
			if err := svc.ComputeAndSaveDiaryCentroid(ctx, userID); err != nil {
				slog.Warn("recompute diary centroid", "user_id", userID, "error", err)
			}
		}
		slog.Info("diary centroid recompute complete")
	}
}
