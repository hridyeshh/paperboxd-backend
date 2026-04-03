package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/hridyesh/paperboxd-backend/cmd/migrate-mongo-to-pg/idmap"
	"go.mongodb.org/mongo-driver/v2/bson"
	
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoUser mirrors the MongoDB User document structure.
type mongoUser struct {
	ID        bson.ObjectID `bson:"_id"`
	Email     string             `bson:"email"`
	Password  string             `bson:"password"`
	Username  string             `bson:"username"`
	Name      string             `bson:"name"`
	Avatar    string             `bson:"avatar"`
	Bio       string             `bson:"bio"`
	Birthday  *time.Time         `bson:"birthday"`
	Gender    string             `bson:"gender"`
	Pronouns  []string           `bson:"pronouns"`
	Links     []string           `bson:"links"`
	IsPublic  bool               `bson:"isPublic"`

	// Social
	Followers []bson.ObjectID `bson:"followers"`
	Following []bson.ObjectID `bson:"following"`

	// Books
	TopBooks         []mongoBookRef          `bson:"topBooks"`
	FavoriteBooks    []mongoBookRef          `bson:"favoriteBooks"`
	Bookshelf        []mongoBookshelfEntry   `bson:"bookshelf"`
	LikedBooks       []mongoLikedBook        `bson:"likedBooks"`
	TbrBooks         []mongoTbrBook          `bson:"tbrBooks"`
	CurrentlyReading []mongoBookRef          `bson:"currentlyReading"`
	ReadingProgress  []mongoReadingProgress  `bson:"readingProgress"`
	ReadingLists     []mongoReadingList      `bson:"readingLists"`
	DiaryEntries     []mongoDiaryEntry       `bson:"diaryEntries"`
	Activities       []mongoActivity         `bson:"activities"`

	// Stats
	TotalBooksRead int32 `bson:"totalBooksRead"`
	TotalPagesRead int32 `bson:"totalPagesRead"`

	// Timestamps
	CreatedAt  time.Time `bson:"createdAt"`
	UpdatedAt  time.Time `bson:"updatedAt"`
	LastActive time.Time `bson:"lastActive"`
}

type mongoBookshelfEntry struct {
	BookID     bson.ObjectID `bson:"bookId"`
	Title      string             `bson:"title"`
	Author     string             `bson:"author"`
	Cover      string             `bson:"cover"`
	FinishedOn *time.Time         `bson:"finishedOn"`
	Format     string             `bson:"format"`
	Rating     *float64           `bson:"rating"`
	Thoughts   string             `bson:"thoughts"`
}

type mongoLikedBook struct {
	BookID  bson.ObjectID `bson:"bookId"`
	Title   string             `bson:"title"`
	Author  string             `bson:"author"`
	Cover   string             `bson:"cover"`
	LikedOn *time.Time         `bson:"likedOn"`
}

type mongoTbrBook struct {
	BookID  bson.ObjectID `bson:"bookId"`
	Title   string             `bson:"title"`
	Author  string             `bson:"author"`
	Cover   string             `bson:"cover"`
	AddedOn *time.Time         `bson:"addedOn"`
	Urgency string             `bson:"urgency"`
	WhyNow  string             `bson:"whyNow"`
}

type mongoReadingProgress struct {
	BookID    bson.ObjectID `bson:"bookId"`
	PagesRead int32              `bson:"pagesRead"`
	UpdatedAt *time.Time         `bson:"updatedAt"`
}

type mongoReadingList struct {
	ID           bson.ObjectID   `bson:"_id"`
	Title        string               `bson:"title"`
	Description  string               `bson:"description"`
	Books        []bson.ObjectID `bson:"books"`
	IsPublic     bool                 `bson:"isPublic"`
	AllowedUsers []string             `bson:"allowedUsers"`
	CreatedAt    *time.Time           `bson:"createdAt"`
	UpdatedAt    *time.Time           `bson:"updatedAt"`
}

type mongoDiaryEntry struct {
	ID        bson.ObjectID   `bson:"_id"`
	BookID    *bson.ObjectID  `bson:"bookId"`
	Subject   string               `bson:"subject"`
	Content   string               `bson:"content"`
	Likes     []bson.ObjectID `bson:"likes"`
	CreatedAt *time.Time           `bson:"createdAt"`
	UpdatedAt *time.Time           `bson:"updatedAt"`
}

type mongoActivity struct {
	Type            string              `bson:"type"`
	BookID          *bson.ObjectID `bson:"bookId"`
	ListID          interface{}         `bson:"listId"` // string or ObjectId
	DiaryEntryID    interface{}         `bson:"diaryEntryId"` // string or ObjectId
	Rating          *float64            `bson:"rating"`
	Review          string              `bson:"review"`
	Timestamp       *time.Time          `bson:"timestamp"`
	SharedBy        *bson.ObjectID `bson:"sharedBy"`
	SharedByUsername string             `bson:"sharedByUsername"`
}

// ── User migration ────────────────────────────────────────────────────────────

func migrateUsers(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "users (core)"}
	defer stats.log()

	col := conn.Mongo.Collection("users")
	cur, err := col.Find(ctx, bson.D{}, options.Find().SetBatchSize(200))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var u mongoUser
		if err := cur.Decode(&u); err != nil {
			logErr("users", "decode", err, nil)
			stats.errors++
			continue
		}

		// Skip users without a username (PG requires it for auth)
		if strings.TrimSpace(u.Username) == "" {
			slog.Warn("skipping user without username", "email", u.Email)
			stats.skipped++
			continue
		}

		pgID := idmap.FromObjectID(u.ID)
		if err := upsertUser(ctx, conn, u, pgID, dryRun); err != nil {
			logErr("users", "upsert", err, u.ID.Hex())
			stats.errors++
		} else {
			stats.inserted++
		}
	}

	return cur.Err()
}

func upsertUser(ctx context.Context, conn *Connections, u mongoUser, pgID uuid.UUID, dryRun bool) error {
	avatarURL := cleanAvatar(u.Avatar)
	pronouns := u.Pronouns
	if pronouns == nil {
		pronouns = []string{}
	}
	links := u.Links
	if links == nil {
		links = []string{}
	}

	slog.Debug("user", "id", pgID, "username", u.Username, "dry_run", dryRun)
	if dryRun {
		return nil
	}

	_, err := conn.PG.Exec(ctx, `
		INSERT INTO users (
			id, email, password_hash, username, name, avatar_url, bio,
			birthday, gender, pronouns, links, is_public,
			books_read_count, total_pages_read, last_active,
			created_at, updated_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17
		)
		ON CONFLICT (id) DO UPDATE SET
			email           = EXCLUDED.email,
			books_read_count = EXCLUDED.books_read_count,
			total_pages_read = EXCLUDED.total_pages_read,
			last_active     = EXCLUDED.last_active
	`,
		pgUUID(pgID),
		u.Email,
		u.Password,
		u.Username,
		u.Name,
		avatarURL,
		nullStr(u.Bio),
		u.Birthday,
		nullStr(u.Gender),
		pronouns,
		links,
		u.IsPublic,
		u.TotalBooksRead,
		u.TotalPagesRead,
		u.LastActive.UTC(),
		u.CreatedAt.UTC(),
		u.UpdatedAt.UTC(),
	)
	return err
}

// ── Follows migration ─────────────────────────────────────────────────────────

func migrateFollows(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "follows"}
	defer stats.log()

	col := conn.Mongo.Collection("users")
	cur, err := col.Find(ctx, bson.D{}, options.Find().
		SetBatchSize(200).
		SetProjection(bson.M{"_id": 1, "following": 1}))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var u struct {
			ID        bson.ObjectID   `bson:"_id"`
			Following []bson.ObjectID `bson:"following"`
		}
		if err := cur.Decode(&u); err != nil {
			logErr("follows", "decode", err, nil)
			stats.errors++
			continue
		}

		followerUUID := idmap.FromObjectID(u.ID)
		for _, followedOID := range u.Following {
			followedUUID := idmap.FromObjectID(followedOID)

			if dryRun {
				stats.inserted++
				continue
			}

			_, err := conn.PG.Exec(ctx, `
				INSERT INTO follows (follower_id, following_id, created_at)
				VALUES ($1, $2, NOW())
				ON CONFLICT DO NOTHING
			`, pgUUID(followerUUID), pgUUID(followedUUID))
			if err != nil {
				logErr("follows", "insert", err, fmt.Sprintf("%s→%s", u.ID.Hex(), followedOID.Hex()))
				stats.errors++
			} else {
				stats.inserted++
			}
		}
	}

	return cur.Err()
}

// ── Bookshelf migration ───────────────────────────────────────────────────────
// Merges bookshelf (read), currentlyReading (reading), tbrBooks (to-read).
// Priority on conflict: read > reading > to-read.

func migrateBookshelf(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "bookshelf"}
	defer stats.log()

	col := conn.Mongo.Collection("users")
	cur, err := col.Find(ctx, bson.D{}, options.Find().SetBatchSize(100))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var u mongoUser
		if err := cur.Decode(&u); err != nil {
			stats.errors++
			continue
		}
		if strings.TrimSpace(u.Username) == "" {
			continue
		}

		userUUID := idmap.FromObjectID(u.ID)

		// Track which book UUIDs have been inserted for this user (dedup)
		// Priority: read > reading > to-read
		type bookEntry struct {
			status     string
			finishedAt pgtype.Timestamp
			startedAt  pgtype.Timestamp
			rating     pgtype.Int4
			tbrAddedAt pgtype.Timestamp
			tbrPriority pgtype.Text
			tbrNotes   pgtype.Text
			currentPage pgtype.Int4
		}
		bookMap := make(map[uuid.UUID]bookEntry)

		// 1. TBR (lowest priority)
		for _, tb := range u.TbrBooks {
			bookUUID, err := findOrCreateBookFromRef(ctx, conn, mongoBookRef{
				BookID: tb.BookID, Title: tb.Title, Author: tb.Author, Cover: tb.Cover,
			}, dryRun)
			if err != nil {
				stats.errors++
				continue
			}
			bookMap[bookUUID] = bookEntry{
				status:      "to-read",
				tbrAddedAt:  pgTimestampPtr(tb.AddedOn),
				tbrPriority: mapTBRUrgency(tb.Urgency),
				tbrNotes:    pgTextPtr(strPtr(tb.WhyNow)),
			}
		}

		// 2. Currently reading (overrides TBR)
		for _, cr := range u.CurrentlyReading {
			bookUUID, err := findOrCreateBookFromRef(ctx, conn, cr, dryRun)
			if err != nil {
				stats.errors++
				continue
			}
			// Apply reading progress if available
			currentPage := pgtype.Int4{}
			for _, rp := range u.ReadingProgress {
				if rp.BookID == cr.BookID {
					currentPage = pgInt4(rp.PagesRead)
					break
				}
			}
			bookMap[bookUUID] = bookEntry{
				status:      "reading",
				currentPage: currentPage,
			}
		}

		// 3. Finished books (highest priority — overrides everything)
		for _, bs := range u.Bookshelf {
			bookUUID, err := findOrCreateBookFromRef(ctx, conn, mongoBookRef{
				BookID: bs.BookID, Title: bs.Title, Author: bs.Author, Cover: bs.Cover,
			}, dryRun)
			if err != nil {
				stats.errors++
				continue
			}
			entry := bookEntry{
				status:     "read",
				finishedAt: pgTimestampPtr(bs.FinishedOn),
			}
			if bs.Rating != nil {
				entry.rating = pgInt4(int32(*bs.Rating))
			}
			bookMap[bookUUID] = entry
		}

		if dryRun {
			stats.inserted += len(bookMap)
			continue
		}

		for bookUUID, entry := range bookMap {
			_, err := conn.PG.Exec(ctx, `
				INSERT INTO bookshelf (
					user_id, book_id, status, rating, finished_at,
					tbr_added_at, tbr_priority, tbr_notes, current_page,
					created_at, updated_at
				) VALUES (
					$1,$2,$3,$4,$5,$6,$7,$8,$9,NOW(),NOW()
				)
				ON CONFLICT (user_id, book_id) DO UPDATE SET
					status       = CASE WHEN bookshelf.status = 'read' THEN bookshelf.status ELSE EXCLUDED.status END,
					rating       = COALESCE(bookshelf.rating, EXCLUDED.rating),
					finished_at  = COALESCE(bookshelf.finished_at, EXCLUDED.finished_at)
			`,
				pgUUID(userUUID), pgUUID(bookUUID),
				entry.status,
				entry.rating,
				entry.finishedAt,
				entry.tbrAddedAt,
				entry.tbrPriority,
				entry.tbrNotes,
				entry.currentPage,
			)
			if err != nil {
				logErr("bookshelf", "insert", err, fmt.Sprintf("user=%s book=%s", userUUID, bookUUID))
				stats.errors++
			} else {
				stats.inserted++
			}
		}
	}

	return cur.Err()
}

// ── Likes migration ───────────────────────────────────────────────────────────

func migrateLikes(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "likes"}
	defer stats.log()

	col := conn.Mongo.Collection("users")
	cur, err := col.Find(ctx, bson.D{}, options.Find().
		SetBatchSize(200).
		SetProjection(bson.M{"_id": 1, "likedBooks": 1, "username": 1}))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var u struct {
			ID         bson.ObjectID `bson:"_id"`
			Username   string             `bson:"username"`
			LikedBooks []mongoLikedBook   `bson:"likedBooks"`
		}
		if err := cur.Decode(&u); err != nil {
			stats.errors++
			continue
		}
		if u.Username == "" {
			continue
		}

		userUUID := idmap.FromObjectID(u.ID)
		for _, lb := range u.LikedBooks {
			bookUUID, err := findOrCreateBookFromRef(ctx, conn, mongoBookRef{
				BookID: lb.BookID, Title: lb.Title, Author: lb.Author, Cover: lb.Cover,
			}, dryRun)
			if err != nil {
				stats.errors++
				continue
			}

			if dryRun {
				stats.inserted++
				continue
			}

			createdAt := time.Now()
			if lb.LikedOn != nil {
				createdAt = *lb.LikedOn
			}
			_, err = conn.PG.Exec(ctx, `
				INSERT INTO likes (user_id, book_id, created_at)
				VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING
			`, pgUUID(userUUID), pgUUID(bookUUID), createdAt.UTC())
			if err != nil {
				logErr("likes", "insert", err, u.ID.Hex())
				stats.errors++
			} else {
				stats.inserted++
			}
		}
	}

	return cur.Err()
}

// ── Favorites migration ───────────────────────────────────────────────────────

func migrateFavorites(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "favorites"}
	defer stats.log()

	col := conn.Mongo.Collection("users")
	cur, err := col.Find(ctx, bson.D{}, options.Find().
		SetBatchSize(200).
		SetProjection(bson.M{"_id": 1, "username": 1, "topBooks": 1}))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var u struct {
			ID       bson.ObjectID `bson:"_id"`
			Username string             `bson:"username"`
			TopBooks []mongoBookRef     `bson:"topBooks"`
		}
		if err := cur.Decode(&u); err != nil {
			stats.errors++
			continue
		}
		if u.Username == "" {
			continue
		}

		userUUID := idmap.FromObjectID(u.ID)
		for i, tb := range u.TopBooks {
			if i >= 4 {
				break // PG favorites is max 4
			}
			displayOrder := int32(i + 1)

			bookUUID, err := findOrCreateBookFromRef(ctx, conn, tb, dryRun)
			if err != nil {
				stats.errors++
				continue
			}

			if dryRun {
				stats.inserted++
				continue
			}

			_, err = conn.PG.Exec(ctx, `
				INSERT INTO favorites (user_id, book_id, display_order, created_at, updated_at)
				VALUES ($1, $2, $3, NOW(), NOW())
				ON CONFLICT (user_id, display_order) DO UPDATE SET
					book_id = EXCLUDED.book_id
			`, pgUUID(userUUID), pgUUID(bookUUID), displayOrder)
			if err != nil {
				logErr("favorites", "insert", err, fmt.Sprintf("user=%s book=%s", userUUID, bookUUID))
				stats.errors++
			} else {
				stats.inserted++
			}
		}
	}

	return cur.Err()
}

// strPtr is a helper to get a pointer from a string (empty → nil pointer).
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
