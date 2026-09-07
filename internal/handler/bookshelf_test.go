package handler

import (
	"testing"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCanRateEntry(t *testing.T) {
	pages := func(n int32) pgtype.Int4 { return pgtype.Int4{Int32: n, Valid: true} }

	tests := []struct {
		name  string
		entry db.Bookshelf
		want  bool
	}{
		{"fresh shelf, no pages", db.Bookshelf{Status: "reading"}, false},
		{"below threshold", db.Bookshelf{Status: "reading", CurrentPage: pages(19)}, false},
		{"exactly at threshold", db.Bookshelf{Status: "reading", CurrentPage: pages(20)}, true},
		{"well past threshold", db.Bookshelf{Status: "reading", CurrentPage: pages(340)}, true},
		{"finished, page count unknown", db.Bookshelf{Status: "read"}, true},
		{"finished, short book", db.Bookshelf{Status: "read", CurrentPage: pages(12)}, true},
		{"queued with no progress", db.Bookshelf{Status: "to-read"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canRateEntry(tt.entry); got != tt.want {
				t.Fatalf("canRateEntry() = %v, want %v", got, tt.want)
			}
		})
	}
}
