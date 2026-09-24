package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestBuildNow(t *testing.T) {
	bookA := db.Book{ID: uuid.New(), Title: "Piranesi"}
	bookB := db.Book{ID: uuid.New(), Title: "Pachinko"}
	started := func(b db.Book, n int32) *db.GetMostStartedTodayRow {
		return &db.GetMostStartedTodayRow{Book: b, Started: n}
	}
	rising := func(b db.Book, n int32) *db.GetRisingBooksRow {
		return &db.GetRisingBooksRow{Book: b, Recent: n}
	}
	list := &db.GetRisingListRow{ID: uuid.New(), Title: "Dream books", Username: "maya", Saves7d: 4}
	thought := &db.GetHotThoughtRow{ID: uuid.New(), Content: "<p>I can't stop thinking about the ending.</p>", Username: "arjun", Likes48h: 5}
	milestone := &db.GetPublicActivitiesRow{
		ActivityType: "milestone", Username: "sarah",
		Name:      pgtype.Text{String: "Sarah", Valid: true},
		Metadata:  []byte(`{"books_read":100}`),
		CreatedAt: pgtype.Timestamp{Time: time.Now(), Valid: true},
	}

	cases := []struct {
		name  string
		in    nowSignals
		kinds []string
	}{
		{"nothing happening", nowSignals{}, nil},
		{"below floors reads as empty", nowSignals{
			started: started(bookA, 2), rising: rising(bookB, 4),
			list: &db.GetRisingListRow{Saves7d: 1}, thought: &db.GetHotThoughtRow{Likes48h: 2},
		}, nil},
		{"two cards is still too quiet", nowSignals{started: started(bookA, 5), list: list}, nil},
		{"all qualify", nowSignals{
			started: started(bookA, 5), rising: rising(bookB, 9), list: list, thought: thought, milestone: milestone,
		}, []string{"started_today", "surging", "list_rising", "thought_hot", "milestone"}},
		{"same book is one card", nowSignals{
			started: started(bookA, 5), rising: rising(bookA, 9), list: list, thought: thought,
		}, []string{"started_today", "list_rising", "thought_hot"}},
		{"surging keeps the book when started is below floor", nowSignals{
			started: started(bookA, 1), rising: rising(bookA, 9), list: list, thought: thought,
		}, []string{"surging", "list_rising", "thought_hot"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildNow(tc.in)
			if len(got) != len(tc.kinds) {
				t.Fatalf("got %d cards %+v, want %v", len(got), got, tc.kinds)
			}
			for i, k := range tc.kinds {
				if got[i].Kind != k {
					t.Errorf("card %d: got %s, want %s", i, got[i].Kind, k)
				}
			}
		})
	}

	all := buildNow(nowSignals{started: started(bookA, 5), list: list, thought: thought, milestone: milestone})
	if h := all[len(all)-1].Headline; h != "Sarah just finished their 100th book" {
		t.Errorf("milestone headline: %q", h)
	}
	if e := *all[2].Excerpt; e != "I can't stop thinking about the ending." {
		t.Errorf("thought excerpt: %q", e)
	}
}

func TestOrdinal(t *testing.T) {
	for n, want := range map[int32]string{1: "1st", 2: "2nd", 3: "3rd", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 100: "100th", 101: "101st"} {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %s, want %s", n, got, want)
		}
	}
}
