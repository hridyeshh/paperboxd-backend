package main

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// ── UUID / pgtype helpers ─────────────────────────────────────────────────────

func pgUUID(u uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: u, Valid: true}
}

func pgUUIDPtr(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *u, Valid: true}
}

func pgText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

func pgTextPtr(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func pgInt4(n int32) pgtype.Int4 {
	return pgtype.Int4{Int32: n, Valid: true}
}

func pgInt4Ptr(n *int32) pgtype.Int4 {
	if n == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *n, Valid: true}
}

func pgTimestamp(t time.Time) pgtype.Timestamp {
	return pgtype.Timestamp{Time: t.UTC(), Valid: true}
}

func pgTimestampPtr(t *time.Time) pgtype.Timestamp {
	if t == nil {
		return pgtype.Timestamp{}
	}
	return pgtype.Timestamp{Time: t.UTC(), Valid: true}
}

func pgDate(t time.Time) pgtype.Date {
	return pgtype.Date{Time: t.UTC(), Valid: true}
}

// ── Date parsing ──────────────────────────────────────────────────────────────

// parseMongoBooksDate tries multiple formats for publishedDate which may be
// "2023-10-15", "2023-10", "2023", or empty.
func parseMongoBooksDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	formats := []string{
		"2006-01-02",
		"2006-01",
		"2006",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return &t
		}
	}
	slog.Warn("could not parse publishedDate", "value", s)
	return nil
}

// ── Slug generation ───────────────────────────────────────────────────────────

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// generateSlug creates a URL-safe slug from a book title and primary author.
func generateSlug(title, author string) string {
	combined := strings.ToLower(title + " " + author)
	combined = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			return r
		}
		return ' '
	}, combined)
	slug := slugNonAlnum.ReplaceAllString(combined, "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 100 {
		slug = slug[:100]
	}
	return slug
}

// ── TBR priority mapping ──────────────────────────────────────────────────────

// mapTBRUrgency maps MongoDB urgency values to PG priority values.
func mapTBRUrgency(urgency string) pgtype.Text {
	switch urgency {
	case "Soon", "This weekend":
		return pgText("high")
	case "Eventually":
		return pgText("low")
	default:
		return pgtype.Text{}
	}
}

// ── Activity type mapping ─────────────────────────────────────────────────────

// mapActivityType converts a MongoDB activity type to PG activity_type.
// Returns "" to skip the activity.
func mapActivityType(mongoType string) string {
	switch mongoType {
	case "read", "rated", "started_reading":
		return "added_book"
	case "reviewed":
		return "created_diary_entry"
	case "shared_list":
		return "shared_list"
	case "shared_book":
		return "shared_book"
	case "granted_access":
		return "granted_access"
	case "liked_diary_entry":
		return "liked_diary_entry"
	case "liked", "added_to_list", "collaboration_request":
		return "" // skip
	default:
		return ""
	}
}

// ── Avatar helpers ────────────────────────────────────────────────────────────

// cleanAvatar returns nil if the avatar is base64 data (too large / not a URL).
func cleanAvatar(avatar string) *string {
	if strings.HasPrefix(avatar, "data:") {
		// base64 encoded image — not suitable for storage as-is
		return nil
	}
	if avatar == "" {
		return nil
	}
	return &avatar
}

// ── ObjectId string extraction ────────────────────────────────────────────────

// extractObjectIDStr extracts a hex string from either an ObjectId or a plain string.
// This handles MongoDB activities.listId which can be either type.
func extractObjectIDStr(v interface{}) string {
	switch val := v.(type) {
	case bson.ObjectID:
		return val.Hex()
	case string:
		return val
	}
	return ""
}

// ── Counter helpers ───────────────────────────────────────────────────────────

type migrationStats struct {
	collection string
	inserted   int
	skipped    int
	errors     int
}

func (s *migrationStats) log() {
	slog.Info("migration complete",
		"collection", s.collection,
		"inserted", s.inserted,
		"skipped", s.skipped,
		"errors", s.errors,
	)
}

func logErr(collection, op string, err error, id interface{}) {
	slog.Error("migration error",
		"collection", collection,
		"operation", op,
		"id", fmt.Sprintf("%v", id),
		"error", err,
	)
}
