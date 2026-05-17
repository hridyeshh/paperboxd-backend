// backfill-embeddings generates and stores Cohere embeddings for all books
// that currently have a NULL embedding column.
//
// Run once after enabling pgvector:
//
//	go run cmd/backfill-embeddings/main.go
//
// After this completes, create the ivfflat index:
//
//	CREATE INDEX books_embedding_idx
//	  ON books USING ivfflat (embedding vector_cosine_ops)
//	  WITH (lists = 100);
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/hridyesh/paperboxd-backend/internal/service"
)

func main() {
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
	svc := service.NewRecommendationService(pool, embedder)

	books, err := svc.GetBooksWithoutEmbeddings(ctx)
	if err != nil {
		slog.Error("fetch books without embeddings", "error", err)
		os.Exit(1)
	}
	total := len(books)
	slog.Info("books to embed", "count", total)
	if total == 0 {
		slog.Info("all books already have embeddings — nothing to do")
		return
	}

	const batchSize = 96
	succeeded := 0
	failed := 0

	for start := 0; start < total; start += batchSize {
		end := start + batchSize
		if end > total {
			end = total
		}
		batch := books[start:end]

		texts := make([]string, len(batch))
		for i, b := range batch {
			primaryAuthor := ""
			if len(b.Authors) > 0 {
				primaryAuthor = b.Authors[0]
			}
			texts[i] = service.BookEmbedText(b.Title, b.Subtitle, primaryAuthor, b.Categories, b.Description)
		}

		vecs, err := embedder.EmbedTexts(texts, "search_document")
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
			if err := svc.SaveBookEmbedding(ctx, book.ID, vecs[i]); err != nil {
				slog.Error("save embedding", "book_id", book.ID, "error", err)
				failed++
				continue
			}
			succeeded++
		}

		fmt.Printf("Embedded %d/%d books...\n", min(start+batchSize, total), total)

		if end < total {
			time.Sleep(650 * time.Millisecond)
		}
	}

	slog.Info("backfill complete", "succeeded", succeeded, "failed", failed)

	fmt.Println("\n\033[1mNext step — create the ivfflat index:\033[0m")
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
