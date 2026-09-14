package handler

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

// The private case is the one that matters: before this guard existed, thoughts
// marked private were still sent to Cohere and their text stored in cleartext.
func TestShouldEmbedThought(t *testing.T) {
	tests := []struct {
		name      string
		hasBook   bool
		isPrivate bool
		want      bool
	}{
		{"public thought on a book", true, false, true},
		{"private thought on a book", true, true, false},
		{"public thought with no book", false, false, false},
		{"private thought with no book", false, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldEmbedThought(tt.hasBook, tt.isPrivate); got != tt.want {
				t.Fatalf("shouldEmbedThought(%v, %v) = %v, want %v",
					tt.hasBook, tt.isPrivate, got, tt.want)
			}
		})
	}
}

// Threads are flat: a follow-up to a follow-up still joins the first thought,
// otherwise the thread query (root OR thread_root_id = root) would miss it.
func TestThreadRootOf(t *testing.T) {
	root := uuid.New()
	if got := threadRootOf(db.Thought{ID: root}); got != root {
		t.Errorf("a first thought is its own root: got %v", got)
	}
	followUp := db.Thought{ID: uuid.New(), ThreadRootID: pgtype.UUID{Bytes: root, Valid: true}}
	if got := threadRootOf(followUp); got != root {
		t.Errorf("a follow-up resolves to the first thought: got %v, want %v", got, root)
	}
}

func TestRepostDenial(t *testing.T) {
	tests := []struct {
		name                                string
		own, private, authorPublic, blocked bool
		want                                int
	}{
		{"someone else's public thought", false, false, true, false, 0},
		{"own thought", true, false, true, false, http.StatusBadRequest},
		{"private thought is indistinguishable from missing", false, true, true, false, http.StatusNotFound},
		{"block is indistinguishable from missing", false, false, true, true, http.StatusNotFound},
		{"private account", false, false, false, false, http.StatusForbidden},
		{"private thought on a private account still 404s", false, true, false, false, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := repostDenial(tt.own, tt.private, tt.authorPublic, tt.blocked); got != tt.want {
				t.Fatalf("repostDenial = %d, want %d", got, tt.want)
			}
		})
	}
}
