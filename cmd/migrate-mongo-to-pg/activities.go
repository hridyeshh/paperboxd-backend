package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/cmd/migrate-mongo-to-pg/idmap"
	"github.com/jackc/pgx/v5/pgtype"
	"go.mongodb.org/mongo-driver/v2/bson"
	
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var uuidNil = uuid.Nil

func migrateActivities(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "activities"}
	defer stats.log()

	col := conn.Mongo.Collection("users")
	cur, err := col.Find(ctx, bson.D{}, options.Find().
		SetBatchSize(100).
		SetProjection(bson.M{"_id": 1, "username": 1, "activities": 1}))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var u struct {
			ID         bson.ObjectID `bson:"_id"`
			Username   string             `bson:"username"`
			Activities []mongoActivity    `bson:"activities"`
		}
		if err := cur.Decode(&u); err != nil {
			stats.errors++
			continue
		}
		if u.Username == "" {
			continue
		}

		userUUID := idmap.FromObjectID(u.ID)

		for _, act := range u.Activities {
			pgType := mapActivityType(act.Type)
			if pgType == "" {
				stats.skipped++
				continue
			}

			ts := time.Now()
			if act.Timestamp != nil {
				ts = *act.Timestamp
			}

			// Book FK
			bookPgID := pgtype_nil_uuid()
			if act.BookID != nil && !act.BookID.IsZero() {
				bookPgID = pgUUID(idmap.FromObjectID(*act.BookID))
			}

			// List FK — listId is stored as string in MongoDB activities
			listPgID := pgtype_nil_uuid()
			if act.ListID != nil {
				hexStr := extractObjectIDStr(act.ListID)
				if hexStr != "" {
					listUUID := idmap.FromString(hexStr)
					if listUUID != uuidNil {
						listPgID = pgUUID(listUUID)
					}
				}
			}

			// Diary entry FK
			entryPgID := pgtype_nil_uuid()
			if act.DiaryEntryID != nil {
				hexStr := extractObjectIDStr(act.DiaryEntryID)
				if hexStr != "" {
					entryUUID := idmap.FromString(hexStr)
					if entryUUID != uuidNil {
						entryPgID = pgUUID(entryUUID)
					}
				}
			}

			// Target user (sharedBy = the user who initiated the action toward this user)
			targetUserPgID := pgtype_nil_uuid()
			if act.SharedBy != nil && !act.SharedBy.IsZero() {
				targetUserPgID = pgUUID(idmap.FromObjectID(*act.SharedBy))
			}

			// Actor: for shared_list/granted_access, the actor is sharedBy;
			// for everything else, actor is this user.
			actorUUID := userUUID
			if strings.Contains(act.Type, "shared") || act.Type == "granted_access" || act.Type == "liked_diary_entry" {
				if act.SharedBy != nil && !act.SharedBy.IsZero() {
					actorUUID = idmap.FromObjectID(*act.SharedBy)
					targetUserPgID = pgUUID(userUUID) // this user is the target
				}
			}

			slog.Debug("activity", "type", pgType, "user", u.Username, "dry_run", dryRun)

			if dryRun {
				stats.inserted++
				continue
			}

			_, err := conn.PG.Exec(ctx, `
				INSERT INTO activities (
					user_id, activity_type, book_id, list_id, entry_id,
					target_user_id, created_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7)
			`,
				pgUUID(actorUUID), pgType,
				bookPgID, listPgID, entryPgID,
				targetUserPgID,
				ts.UTC(),
			)
			if err != nil {
				logErr("activities", "insert", err, u.ID.Hex())
				stats.errors++
			} else {
				stats.inserted++
			}
		}
	}

	return cur.Err()
}

// ── Newsletter migration ──────────────────────────────────────────────────────

type mongoNewsletter struct {
	ID             bson.ObjectID `bson:"_id"`
	Email          string             `bson:"email"`
	IsActive       bool               `bson:"isActive"`
	Source         string             `bson:"source"`
	SubscribedAt   *time.Time         `bson:"subscribedAt"`
	UnsubscribedAt *time.Time         `bson:"unsubscribedAt"`
}

func migrateNewsletters(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "newsletters"}
	defer stats.log()

	col := conn.Mongo.Collection("newsletters")
	cur, err := col.Find(ctx, bson.D{}, options.Find().SetBatchSize(500))
	if err != nil {
		// Newsletter collection might not exist
		slog.Warn("newsletters collection not found or error", "error", err)
		return nil
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var n mongoNewsletter
		if err := cur.Decode(&n); err != nil {
			stats.errors++
			continue
		}

		if dryRun {
			stats.inserted++
			continue
		}

		subscribedAt := time.Now()
		if n.SubscribedAt != nil {
			subscribedAt = *n.SubscribedAt
		}
		source := n.Source
		if source == "" {
			source = "footer"
		}

		_, err := conn.PG.Exec(ctx, `
			INSERT INTO newsletters (email, is_active, source, subscribed_at, unsubscribed_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)
			ON CONFLICT (email) DO UPDATE SET
				is_active       = EXCLUDED.is_active,
				unsubscribed_at = EXCLUDED.unsubscribed_at
		`,
			strings.ToLower(n.Email),
			n.IsActive,
			source,
			subscribedAt.UTC(),
			n.UnsubscribedAt,
			subscribedAt.UTC(),
		)
		if err != nil {
			logErr("newsletters", "insert", err, n.Email)
			stats.errors++
		} else {
			stats.inserted++
		}
	}

	return cur.Err()
}

// ── Account deletions migration ───────────────────────────────────────────────

type mongoAccountDeletion struct {
	ID        bson.ObjectID `bson:"_id"`
	Email     string             `bson:"email"`
	Username  string             `bson:"username"`
	Reasons   []string           `bson:"reasons"`
	DeletedAt *time.Time         `bson:"deletedAt"`
}

func migrateAccountDeletions(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "account_deletions"}
	defer stats.log()

	col := conn.Mongo.Collection("accountdeletions")
	cur, err := col.Find(ctx, bson.D{}, options.Find().SetBatchSize(500))
	if err != nil {
		slog.Warn("accountdeletions collection not found or error", "error", err)
		return nil
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var ad mongoAccountDeletion
		if err := cur.Decode(&ad); err != nil {
			stats.errors++
			continue
		}

		if dryRun {
			stats.inserted++
			continue
		}

		deletedAt := time.Now()
		if ad.DeletedAt != nil {
			deletedAt = *ad.DeletedAt
		}
		reasons := ad.Reasons
		if reasons == nil {
			reasons = []string{}
		}

		_, err := conn.PG.Exec(ctx, `
			INSERT INTO account_deletions (email, username, reasons, deleted_at)
			VALUES ($1, $2, $3, $4)
		`,
			ad.Email,
			nullStr(ad.Username),
			reasons,
			deletedAt.UTC(),
		)
		if err != nil {
			logErr("account_deletions", "insert", err, ad.Email)
			stats.errors++
		} else {
			stats.inserted++
		}
	}

	return cur.Err()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func pgtype_nil_uuid() pgtype.UUID {
	return pgtype.UUID{}
}
