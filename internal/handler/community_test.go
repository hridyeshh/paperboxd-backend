package handler

import (
	"testing"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCollapsePublicActivity(t *testing.T) {
	alice, bob := uuid.New(), uuid.New()
	dune, emma := uuid.New(), uuid.New()
	note := uuid.New()
	pg := func(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
	row := func(user uuid.UUID, typ string, book, entry pgtype.UUID) db.GetPublicActivitiesRow {
		return db.GetPublicActivitiesRow{ID: uuid.New(), UserID: user, ActivityType: typ, BookID: book, ThoughtID: entry, Username: "u"}
	}

	rows := []db.GetPublicActivitiesRow{
		row(alice, "finished_reading", pg(dune), pgtype.UUID{}),
		row(alice, "started_reading", pg(dune), pgtype.UUID{}),     // same object → dropped
		row(alice, "created_thought", pg(dune), pg(note)),          // entry key → kept
		row(alice, "wants_to_read", pg(emma), pgtype.UUID{}),       // third for alice → kept
		row(alice, "wants_to_read", pg(uuid.New()), pgtype.UUID{}), // fourth → dropped
		row(bob, "finished_reading", pg(dune), pgtype.UUID{}),
	}

	out := collapsePublicActivity(rows, 10)
	if len(out) != 4 {
		t.Fatalf("want 4 rows, got %d", len(out))
	}
	if out[0].ActivityType != "finished_reading" || out[1].ActivityType != "created_thought" || out[3].Username != "u" {
		t.Fatalf("unexpected order: %+v", out)
	}
	if got := collapsePublicActivity(rows, 2); len(got) != 2 {
		t.Fatalf("limit not applied: %d", len(got))
	}
}
