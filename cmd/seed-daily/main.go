// seed-daily loads Paperboxd Daily atoms from a JSON file into daily_atoms.
//
// Atoms are upserted by slug, so the file is the source of truth: edit it and
// rerun. Books are named by "ref" — a Postgres UUID, Google Books id or ISBN —
// and resolved the same way the API resolves a book id, caching a book that is
// not in the catalogue yet. Every resolution is logged with the title it
// landed on: read them, because a wrong ISBN puts the wrong book on the card.
//
// Flags:
//
//	--file PATH   JSON file (default content/daily/atoms.json)
//	--dry-run     validate and resolve books, write no atoms
//
// Publishing is a field in the file ("status": "published"); the home page
// also needs the flag: UPDATE feature_flags SET value = 'true' WHERE key = 'daily';
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/handler"
)

type atomFile struct {
	Slug        string   `json:"slug"`
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	Dek         string   `json:"dek"`
	Body        string   `json:"body"`
	ReadSeconds int      `json:"read_seconds"`
	SourceKind  string   `json:"source_kind"`
	SourceNote  string   `json:"source_note"`
	SourceURL   string   `json:"source_url"`
	Topics      []string `json:"topics"`
	Status      string   `json:"status"`
	Books       []struct {
		Ref  string `json:"ref"`
		Note string `json:"note"`
	} `json:"books"`
}

type storedBook struct {
	ID   string `json:"id"`
	Note string `json:"note,omitempty"`
}

func main() {
	path := flag.String("file", "content/daily/atoms.json", "atoms JSON file")
	dryRun := flag.Bool("dry-run", false, "validate and resolve, write nothing")
	flag.Parse()

	_ = godotenv.Load()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	raw, err := os.ReadFile(*path)
	if err != nil {
		slog.Error("read atoms file", "error", err)
		os.Exit(1)
	}
	var atoms []atomFile
	if err := json.Unmarshal(raw, &atoms); err != nil {
		slog.Error("parse atoms file", "error", err)
		os.Exit(1)
	}

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
	q := db.New(pool)
	gb := external.NewGoogleBooksClient(os.Getenv("GOOGLE_BOOKS_API_KEY"))
	isbndb := external.NewISBNdbClient(os.Getenv("ISBNDB_API_KEY"))

	failed := 0
	for _, a := range atoms {
		if err := validate(a); err != nil {
			slog.Error("daily seed: invalid atom", "slug", a.Slug, "error", err)
			failed++
			continue
		}
		books := make([]storedBook, 0, len(a.Books))
		ok := true
		for _, b := range a.Books {
			id, err := handler.ResolveBookID(ctx, q, gb, isbndb, b.Ref)
			if err != nil {
				slog.Error("daily seed: unresolved book", "slug", a.Slug, "ref", b.Ref, "error", err)
				ok = false
				continue
			}
			book, _ := q.GetBookByID(ctx, id)
			slog.Info("daily seed: book", "slug", a.Slug, "ref", b.Ref, "title", book.Title)
			books = append(books, storedBook{ID: id.String(), Note: b.Note})
		}
		if !ok {
			failed++
			continue
		}
		if *dryRun {
			slog.Info("daily seed: valid", "slug", a.Slug, "status", a.Status)
			continue
		}
		booksJSON, _ := json.Marshal(books)
		if a.Topics == nil {
			a.Topics = []string{}
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO daily_atoms (slug, kind, title, dek, body, read_seconds, source_kind,
			                         source_note, source_url, books, topics, status)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), $10, $11, $12)
			ON CONFLICT (slug) DO UPDATE SET
			    kind = EXCLUDED.kind, title = EXCLUDED.title, dek = EXCLUDED.dek, body = EXCLUDED.body,
			    read_seconds = EXCLUDED.read_seconds, source_kind = EXCLUDED.source_kind,
			    source_note = EXCLUDED.source_note, source_url = EXCLUDED.source_url,
			    books = EXCLUDED.books, topics = EXCLUDED.topics, status = EXCLUDED.status,
			    updated_at = NOW()
		`, a.Slug, a.Kind, a.Title, a.Dek, a.Body, a.ReadSeconds, a.SourceKind,
			a.SourceNote, a.SourceURL, booksJSON, a.Topics, a.Status)
		if err != nil {
			slog.Error("daily seed: upsert", "slug", a.Slug, "error", err)
			failed++
			continue
		}
		slog.Info("daily seed: upserted", "slug", a.Slug, "status", a.Status)
	}

	slog.Info("daily seed: done", "atoms", len(atoms), "failed", failed, "dry_run", *dryRun)
	if failed > 0 {
		os.Exit(1)
	}
}

// validate mirrors the table's CHECKs so a bad file fails before any write,
// plus the one rule the table cannot see: a passage must be public domain
// with a Project Gutenberg source.
func validate(a atomFile) error {
	switch {
	case a.Slug == "" || strings.ContainsAny(a.Slug, " /?#"):
		return fmt.Errorf("slug must be a non-empty url segment")
	case a.Title == "" || a.Body == "":
		return fmt.Errorf("title and body are required")
	case a.ReadSeconds <= 0:
		return fmt.Errorf("read_seconds must be positive")
	case len(a.Books) == 0:
		return fmt.Errorf("an atom must name at least one book")
	}
	switch a.Kind {
	case "question", "idea", "rabbit_hole", "passage":
	default:
		return fmt.Errorf("unknown kind %q", a.Kind)
	}
	switch a.SourceKind {
	case "editorial", "public_domain", "author_interview", "readers":
	default:
		return fmt.Errorf("unknown source_kind %q", a.SourceKind)
	}
	switch a.Status {
	case "draft", "published":
	default:
		return fmt.Errorf("status must be draft or published")
	}
	if a.Kind == "passage" {
		u, err := url.Parse(a.SourceURL)
		if a.SourceKind != "public_domain" || err != nil || u.Scheme != "https" ||
			(u.Host != "www.gutenberg.org" && u.Host != "gutenberg.org") {
			return fmt.Errorf("a passage must be public_domain with an https gutenberg.org source_url")
		}
	}
	if a.Kind == "rabbit_hole" && len(a.Books) < 2 {
		return fmt.Errorf("a rabbit hole needs at least two books")
	}
	return nil
}
