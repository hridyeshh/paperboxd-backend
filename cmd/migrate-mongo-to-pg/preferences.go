package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hridyesh/paperboxd-backend/cmd/migrate-mongo-to-pg/idmap"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// migrateUserPreferences copies the entire `userpreferences` collection into
// user_preferences_legacy.payload (JSONB) so no Mongo fields are dropped.
func migrateUserPreferences(ctx context.Context, conn *Connections, dryRun bool) error {
	stats := &migrationStats{collection: "user_preferences_legacy"}
	defer stats.log()

	col := conn.Mongo.Collection("userpreferences")
	cur, err := col.Find(ctx, bson.D{}, options.Find().SetBatchSize(200))
	if err != nil {
		slog.Warn("userpreferences collection missing or unreadable", "error", err)
		return nil
	}
	defer cur.Close(ctx)

	stepStart := time.Now()
	nDocs := 0
	const prefProgressEvery = 25

	for cur.Next(ctx) {
		nDocs++
		logMigrationProgress("user_preferences_legacy", nDocs, prefProgressEvery, stepStart)

		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			stats.errors++
			continue
		}

		rawUID, ok := doc["userId"]
		if !ok {
			stats.skipped++
			continue
		}
		oid, ok := rawUID.(bson.ObjectID)
		if !ok || oid.IsZero() {
			stats.skipped++
			continue
		}
		userUUID := idmap.FromObjectID(oid)

		safe := bsonMToJSONSafe(doc)
		payload, err := json.Marshal(safe)
		if err != nil {
			logErr("user_preferences_legacy", "json", err, oid.Hex())
			stats.errors++
			continue
		}

		var mongoUpdated *time.Time
		if v, ok := doc["updatedAt"]; ok {
			if t, ok := v.(time.Time); ok {
				mongoUpdated = &t
			} else if t, ok := v.(bson.DateTime); ok {
				tt := t.Time()
				mongoUpdated = &tt
			}
		}

		if dryRun {
			stats.inserted++
			continue
		}

		_, err = conn.PG.Exec(ctx, `
			INSERT INTO user_preferences_legacy (user_id, payload, mongo_updated_at)
			VALUES ($1, $2::jsonb, $3)
			ON CONFLICT (user_id) DO UPDATE SET
				payload = EXCLUDED.payload,
				mongo_updated_at = EXCLUDED.mongo_updated_at,
				migrated_at = NOW()
		`, pgUUID(userUUID), string(payload), mongoUpdated)
		if err != nil {
			if isFKViolation(err) {
				stats.skipped++
				continue
			}
			logErr("user_preferences_legacy", "insert", err, fmt.Sprintf("user=%s", userUUID))
			stats.errors++
		} else {
			stats.inserted++
		}
	}

	return cur.Err()
}

func isFKViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "23503")
}

// bsonMToJSONSafe converts bson.M values to JSON-serializable values (ObjectId → hex).
func bsonMToJSONSafe(m bson.M) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = bsonValueToJSONSafe(v)
	}
	return out
}

func bsonValueToJSONSafe(v interface{}) interface{} {
	switch t := v.(type) {
	case nil:
		return nil
	case bson.ObjectID:
		return t.Hex()
	case bson.M:
		return bsonMToJSONSafe(t)
	case bson.A:
		arr := make([]interface{}, len(t))
		for i, x := range t {
			arr[i] = bsonValueToJSONSafe(x)
		}
		return arr
	case []interface{}:
		arr := make([]interface{}, len(t))
		for i, x := range t {
			arr[i] = bsonValueToJSONSafe(x)
		}
		return arr
	case map[string]interface{}:
		return bsonMToJSONSafe(bson.M(t))
	case bson.DateTime:
		return t.Time().UTC().Format(time.RFC3339Nano)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case bson.Binary:
		return map[string]string{
			"encoding": "base64",
			"data":     base64.StdEncoding.EncodeToString(t.Data),
			"subtype":  fmt.Sprint(t.Subtype),
		}
	default:
		return t
	}
}
