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

func migrateLists(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "lists"}
	defer stats.log()

	col := conn.Mongo.Collection("users")
	cur, err := col.Find(ctx, bson.D{}, options.Find().
		SetBatchSize(100).
		SetProjection(bson.M{"_id": 1, "username": 1, "readingLists": 1}))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var u struct {
			ID           bson.ObjectID `bson:"_id"`
			Username     string             `bson:"username"`
			ReadingLists []mongoReadingList `bson:"readingLists"`
		}
		if err := cur.Decode(&u); err != nil {
			stats.errors++
			continue
		}
		if u.Username == "" {
			continue
		}

		userUUID := idmap.FromObjectID(u.ID)

		for _, rl := range u.ReadingLists {
			listUUID := idmap.FromObjectID(rl.ID)
			isPrivate := !rl.IsPublic // MongoDB isPublic → PG is_private (inverted)

			createdAt := time.Now()
			updatedAt := time.Now()
			if rl.CreatedAt != nil {
				createdAt = *rl.CreatedAt
			}
			if rl.UpdatedAt != nil {
				updatedAt = *rl.UpdatedAt
			}

			title := strings.TrimSpace(rl.Title)
			if title == "" {
				title = "Untitled List"
			}
			if len(title) > 50 {
				title = title[:50]
			}

			desc := strings.TrimSpace(rl.Description)
			if len(desc) > 200 {
				desc = desc[:200]
			}

			slog.Debug("list", "id", listUUID, "title", title, "dry_run", dryRun)

			if !dryRun {
				_, err := conn.PG.Exec(ctx, `
					INSERT INTO lists (id, user_id, title, description, is_private, created_at, updated_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
					ON CONFLICT (id) DO NOTHING
				`, pgUUID(listUUID), pgUUID(userUUID),
					title, nullStr(desc), isPrivate,
					createdAt.UTC(), updatedAt.UTC())
				if err != nil {
					logErr("lists", "insert", err, rl.ID.Hex())
					stats.errors++
					continue
				}
			}
			stats.inserted++

			// list_books
			for i, bookOID := range rl.Books {
				bookUUID := idmap.FromObjectID(bookOID)
				if !dryRun {
					_, err := conn.PG.Exec(ctx, `
						INSERT INTO list_books (list_id, book_id, display_order, added_at)
						VALUES ($1, $2, $3, NOW())
						ON CONFLICT (list_id, book_id) DO NOTHING
					`, pgUUID(listUUID), pgUUID(bookUUID), int32(i))
					if err != nil {
						logErr("list_books", "insert", err, fmt.Sprintf("%s[%d]", rl.ID.Hex(), i))
						stats.errors++
					}
				}
			}

			// list_access (allowedUsers → look up user by username)
			for _, allowedUsername := range rl.AllowedUsers {
				allowedUsername = strings.ToLower(strings.TrimSpace(allowedUsername))
				if allowedUsername == "" {
					continue
				}
				if !dryRun {
					var allowedUserUUID [16]byte
					err := conn.PG.QueryRow(ctx,
						`SELECT id FROM users WHERE username = $1`, allowedUsername).Scan(&allowedUserUUID)
					if err != nil {
						slog.Warn("list_access: user not found", "username", allowedUsername)
						continue
					}
					_, err = conn.PG.Exec(ctx, `
						INSERT INTO list_access (list_id, user_id, granted_by, granted_at)
						VALUES ($1, $2, $3, NOW())
						ON CONFLICT DO NOTHING
					`, pgUUID(listUUID), allowedUserUUID, pgUUID(userUUID))
					if err != nil {
						logErr("list_access", "insert", err, allowedUsername)
						stats.errors++
					}
				}
			}
		}

		// Update user lists_count
		if !dryRun {
			_, _ = conn.PG.Exec(ctx, `
				UPDATE users SET lists_count = (
					SELECT COUNT(*) FROM lists WHERE user_id = $1
				) WHERE id = $1
			`, pgUUID(userUUID))
		}
	}

	return cur.Err()
}
