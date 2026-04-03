package main

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/hridyesh/paperboxd-backend/cmd/migrate-mongo-to-pg/idmap"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoBook mirrors the MongoDB Book document structure.
type mongoBook struct {
	ID             bson.ObjectID `bson:"_id"`
	IsbndbID       string             `bson:"isbndbId"`
	ISBN           string             `bson:"isbn"`
	ISBN13         string             `bson:"isbn13"`
	GoogleBooksID  string             `bson:"googleBooksId"`
	OpenLibraryID  string             `bson:"openLibraryId"`
	OpenLibraryKey string             `bson:"openLibraryKey"`
	VolumeInfo     struct {
		Title       string   `bson:"title"`
		Subtitle    string   `bson:"subtitle"`
		Authors     []string `bson:"authors"`
		Publisher   string   `bson:"publisher"`
		PublishedDate string `bson:"publishedDate"`
		Description string   `bson:"description"`
		PageCount   int32    `bson:"pageCount"`
		Categories  []string `bson:"categories"`
		Language    string   `bson:"language"`
		PreviewLink string   `bson:"previewLink"`
		AverageRating float64 `bson:"averageRating"`
		RatingsCount  int32   `bson:"ratingsCount"`
		ImageLinks  struct {
			Thumbnail string `bson:"thumbnail"`
			Small     string `bson:"small"`
			Medium    string `bson:"medium"`
		} `bson:"imageLinks"`
	} `bson:"volumeInfo"`
	PaperboxdRating       float64 `bson:"paperboxdRating"`
	PaperboxdRatingsCount int32   `bson:"paperboxdRatingsCount"`
	TotalReads            int32   `bson:"totalReads"`
	TotalLikes            int32   `bson:"totalLikes"`
	TotalTBR              int32   `bson:"totalTBR"`
	CreatedAt             bson.DateTime `bson:"createdAt"`
}

func migrateBooks(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "books"}
	defer stats.log()

	col := conn.Mongo.Collection("books")
	cur, err := col.Find(ctx, bson.D{}, options.Find().SetBatchSize(500))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var b mongoBook
		if err := cur.Decode(&b); err != nil {
			logErr("books", "decode", err, nil)
			stats.errors++
			continue
		}

		pgID := idmap.FromObjectID(b.ID)
		if err := upsertBook(ctx, conn, b, pgID, dryRun); err != nil {
			logErr("books", "upsert", err, b.ID.Hex())
			stats.errors++
		} else {
			stats.inserted++
		}
	}

	return cur.Err()
}

func upsertBook(ctx context.Context, conn *Connections, b mongoBook, pgID uuid.UUID, dryRun bool) error {
	vi := b.VolumeInfo

	// Resolve cover URL
	coverURL := vi.ImageLinks.Thumbnail
	if coverURL == "" {
		coverURL = vi.ImageLinks.Small
	}
	if coverURL == "" {
		coverURL = vi.ImageLinks.Medium
	}

	// Authors
	if vi.Authors == nil {
		vi.Authors = []string{}
	}
	if vi.Categories == nil {
		vi.Categories = []string{}
	}

	// Primary author for slug
	primaryAuthor := ""
	if len(vi.Authors) > 0 {
		primaryAuthor = vi.Authors[0]
	}
	slug := generateSlug(vi.Title, primaryAuthor)

	// Published date
	var publishedDate pgtype.Date
	if d := parseMongoBooksDate(vi.PublishedDate); d != nil {
		publishedDate = pgDate(*d)
	}

	// Ratings: prefer paperboxd-specific if set
	avgRating := pgtype.Float8{}
	ratingsCount := pgtype.Int4{}
	if b.PaperboxdRatingsCount > 0 && b.PaperboxdRating > 0 {
		avgRating = pgtype.Float8{Float64: b.PaperboxdRating, Valid: true}
		ratingsCount = pgInt4(b.PaperboxdRatingsCount)
	} else if vi.RatingsCount > 0 {
		avgRating = pgtype.Float8{Float64: vi.AverageRating, Valid: true}
		ratingsCount = pgInt4(vi.RatingsCount)
	}

	slog.Debug("book", "id", pgID, "title", vi.Title, "dry_run", dryRun)
	if dryRun {
		return nil
	}

	_, err := conn.PG.Exec(ctx, `
		INSERT INTO books (
			id, title, slug, subtitle, authors, publisher, published_date,
			description, page_count, categories, language, cover_url, preview_link,
			isbn_13, isbndb_id, google_books_id, open_library_id,
			average_rating, ratings_count, total_reads_count, total_tbr_count,
			created_at, updated_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23
		)
		ON CONFLICT (id) DO UPDATE SET
			average_rating   = EXCLUDED.average_rating,
			ratings_count    = EXCLUDED.ratings_count,
			total_reads_count = EXCLUDED.total_reads_count,
			total_tbr_count  = EXCLUDED.total_tbr_count
	`,
		pgUUID(pgID),
		vi.Title,
		slug,
		nullStr(vi.Subtitle),
		vi.Authors,
		nullStr(vi.Publisher),
		publishedDate,
		nullStr(vi.Description),
		nullInt32(vi.PageCount),
		vi.Categories,
		nullStr(vi.Language),
		nullStr(coverURL),
		nullStr(vi.PreviewLink),
		nullStr(b.ISBN13),
		nullStr(b.IsbndbID),
		nullStr(b.GoogleBooksID),
		nullStr(b.OpenLibraryID),
		avgRating,
		ratingsCount,
		pgInt4(b.TotalReads),
		pgInt4(b.TotalTBR),
		b.CreatedAt.Time().UTC(),
		b.CreatedAt.Time().UTC(),
	)
	return err
}

// ── null helpers for raw SQL ──────────────────────────────────────────────────

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullInt32(n int32) interface{} {
	if n == 0 {
		return nil
	}
	return n
}

// findBookByMongoID looks up the PG UUID for a mongo ObjectId.
// Tries the deterministic mapping first; falls back to a DB lookup by external IDs.
func findBookUUID(ctx context.Context, conn *Connections, oid bson.ObjectID) (uuid.UUID, bool) {
	// Try deterministic UUID first
	pgID := idmap.FromObjectID(oid)
	var exists bool
	err := conn.PG.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM books WHERE id = $1)`, pgUUID(pgID)).Scan(&exists)
	if err == nil && exists {
		return pgID, true
	}
	slog.Warn("book not found by deterministic ID, book may not exist yet", "oid", oid.Hex())
	return uuid.Nil, false
}

// findOrCreateBookFromRef creates a minimal book record from a BookReference if the book
// doesn't exist yet. Returns the UUID to use as foreign key.
func findOrCreateBookFromRef(ctx context.Context, conn *Connections, ref mongoBookRef, dryRun bool) (uuid.UUID, error) {
	pgID := idmap.FromObjectID(ref.BookID)

	if dryRun {
		return pgID, nil
	}

	// Check existence
	var exists bool
	_ = conn.PG.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM books WHERE id = $1)`, pgUUID(pgID)).Scan(&exists)
	if exists {
		return pgID, nil
	}

	// Create minimal book record from the denormalized reference
	title := strings.TrimSpace(ref.Title)
	if title == "" {
		title = "Unknown Title"
	}
	author := strings.TrimSpace(ref.Author)
	slug := generateSlug(title, author)
	authors := []string{}
	if author != "" {
		authors = append(authors, author)
	}

	_, err := conn.PG.Exec(ctx, `
		INSERT INTO books (id, title, slug, authors, cover_url, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
		ON CONFLICT (id) DO NOTHING
	`, pgUUID(pgID), title, slug, authors, nullStr(ref.Cover))
	if err != nil {
		return uuid.Nil, err
	}
	return pgID, nil
}

// mongoBookRef is a denormalized book reference embedded in User documents.
type mongoBookRef struct {
	BookID bson.ObjectID `bson:"bookId"`
	Title  string             `bson:"title"`
	Author string             `bson:"author"`
	Cover  string             `bson:"cover"`
	IsbndbID string           `bson:"isbndbId"`
}
