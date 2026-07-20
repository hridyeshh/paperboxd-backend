package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hridyesh/paperboxd-backend/internal/service"
)

func main() {
	bookID := os.Args[1]
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("db:", err)
	}
	defer pool.Close()

	embedder := service.NewCohereEmbedder(os.Getenv("COHERE_API_KEY"))
	svc := service.NewRecommendationService(pool, embedder, nil, nil, "")

	var title, subtitle, description string
	var authors, categories []string
	err = pool.QueryRow(ctx, `
		SELECT title, COALESCE(subtitle,''), authors, categories, COALESCE(description,'')
		FROM books WHERE id = $1
	`, bookID).Scan(&title, &subtitle, &authors, &categories, &description)
	if err != nil {
		log.Fatal("book not found:", err)
	}

	var publisher string
	pool.QueryRow(ctx, `SELECT COALESCE(publisher,'') FROM books WHERE id = $1`, bookID).Scan(&publisher)

	text := service.BookEmbedText(title, subtitle, authors, publisher, categories, description)
	vecs, err := embedder.EmbedTexts([]string{text}, "search_document")
	if err != nil {
		log.Fatal("embed:", err)
	}
	if err := svc.SaveBookEmbedding(ctx, bookID, vecs[0]); err != nil {
		log.Fatal("save:", err)
	}
	fmt.Println("OK:", title)
}
