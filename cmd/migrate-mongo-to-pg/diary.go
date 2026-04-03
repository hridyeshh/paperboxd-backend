package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hridyesh/paperboxd-backend/cmd/migrate-mongo-to-pg/idmap"
	"go.mongodb.org/mongo-driver/v2/bson"
	
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func migrateDiaryEntries(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "diary_entries"}
	defer stats.log()

	col := conn.Mongo.Collection("users")
	cur, err := col.Find(ctx, bson.D{}, options.Find().
		SetBatchSize(100).
		SetProjection(bson.M{"_id": 1, "username": 1, "diaryEntries": 1}))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var u struct {
			ID           bson.ObjectID  `bson:"_id"`
			Username     string              `bson:"username"`
			DiaryEntries []mongoDiaryEntry   `bson:"diaryEntries"`
		}
		if err := cur.Decode(&u); err != nil {
			stats.errors++
			continue
		}
		if u.Username == "" {
			continue
		}

		userUUID := idmap.FromObjectID(u.ID)
		entryCount := 0

		for _, de := range u.DiaryEntries {
			entryUUID := idmap.FromObjectID(de.ID)
			content := strings.TrimSpace(de.Content)
			if content == "" {
				stats.skipped++
				continue
			}

			// MongoDB uses "subject" for the title field
			title := strings.TrimSpace(de.Subject)
			if len(title) > 100 {
				title = title[:100]
			}

			createdAt := time.Now()
			updatedAt := time.Now()
			if de.CreatedAt != nil {
				createdAt = *de.CreatedAt
			}
			if de.UpdatedAt != nil {
				updatedAt = *de.UpdatedAt
			}

			// Book reference (nullable)
			var bookPgID interface{}
			if de.BookID != nil && !de.BookID.IsZero() {
				bookUUID := idmap.FromObjectID(*de.BookID)
				bookPgID = pgUUID(bookUUID)
			}

			slog.Debug("diary_entry", "id", entryUUID, "user", u.Username, "dry_run", dryRun)

			if !dryRun {
				_, err := conn.PG.Exec(ctx, `
					INSERT INTO diary_entries (
						id, user_id, book_id, title, content, is_private,
						created_at, updated_at
					) VALUES ($1, $2, $3, $4, $5, false, $6, $7)
					ON CONFLICT (id) DO NOTHING
				`,
					pgUUID(entryUUID),
					pgUUID(userUUID),
					bookPgID,
					nullStr(title),
					content,
					createdAt.UTC(),
					updatedAt.UTC(),
				)
				if err != nil {
					logErr("diary_entries", "insert", err, de.ID.Hex())
					stats.errors++
					continue
				}
			}
			stats.inserted++
			entryCount++

			// Migrate diary entry likes (array of User ObjectIds in the entry)
			for _, likerOID := range de.Likes {
				likerUUID := idmap.FromObjectID(likerOID)
				if !dryRun {
					_, err := conn.PG.Exec(ctx, `
						INSERT INTO diary_entry_likes (user_id, entry_id, created_at)
						VALUES ($1, $2, NOW())
						ON CONFLICT DO NOTHING
					`, pgUUID(likerUUID), pgUUID(entryUUID))
					if err != nil {
						logErr("diary_entry_likes", "insert", err,
							fmt.Sprintf("entry=%s liker=%s", de.ID.Hex(), likerOID.Hex()))
						stats.errors++
					}
				}
			}
		}

		// Update diary_entries_count
		if !dryRun && entryCount > 0 {
			_, _ = conn.PG.Exec(ctx, `
				UPDATE users SET diary_entries_count = (
					SELECT COUNT(*) FROM diary_entries WHERE user_id = $1
				) WHERE id = $1
			`, pgUUID(userUUID))
		}
	}

	return cur.Err()
}
