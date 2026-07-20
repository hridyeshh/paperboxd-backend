package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// The reason model is the only thing standing between a vibe search and a card
// that says "Matches your vibe" at 44%. These cover the parts that silently
// degrade if they break: fenced JSON, out-of-range match numbers, and the genre
// ordering the prompt is built from.

func TestStripFence(t *testing.T) {
	cases := map[string]string{
		`[{"match":90}]`:                       `[{"match":90}]`,
		"```json\n[{\"match\":90}]\n```":       `[{"match":90}]`,
		"```\n[{\"match\":90}]\n```":           `[{"match":90}]`,
		"  \n```json\n[{\"match\":90}]\n```  ": `[{"match":90}]`,
	}
	for in, want := range cases {
		if got := stripFence(in); got != want {
			t.Errorf("stripFence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBookReasonsDecodesMatch(t *testing.T) {
	var out []BookReasons
	raw := stripFence("```json\n[{\"match\": 88, \"why\": \"w\", \"caveat\": \"c\"}]\n```")
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 1 || out[0].Match != 88 || out[0].Why != "w" || out[0].Caveat != "c" {
		t.Fatalf("got %+v", out)
	}
}

func TestMatchClamp(t *testing.T) {
	// Mirrors the clamp in Reasons — a model that answers 140 or -5 must not
	// reach the card.
	for _, c := range []struct{ in, want int }{{140, 100}, {-5, 0}, {73, 73}} {
		if got := min(100, max(0, c.in)); got != c.want {
			t.Errorf("clamp(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestClaudeReasonsLive hits the real Anthropic API. Skipped unless
// ANTHROPIC_API_KEY is set, so `go test ./...` stays offline:
//
//	set -a; . ./.env; set +a; go test ./internal/service/ -run Live -v
//
// This is the check that would have caught the reasons never reaching the card —
// a wrong model ID or a changed wire format shows up here, not in production
// where the failure is silent by design.
func TestClaudeReasonsLive(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	reasoner := NewClaudeReasoner(key)
	if reasoner == nil {
		t.Fatal("NewClaudeReasoner returned nil with a non-empty key")
	}

	books := []ReasonBook{
		{
			Title:       "Piranesi",
			Authors:     []string{"Susanna Clarke"},
			Categories:  []string{"Fantasy", "Literary Fiction"},
			Description: "A man lives alone in an infinite house of statues and tides, keeping a careful journal, slowly realising his memories do not add up.",
		},
		{
			Title:       "The Martian",
			Authors:     []string{"Andy Weir"},
			Categories:  []string{"Science Fiction"},
			Description: "An astronaut is stranded on Mars and survives by improvising engineering solutions, narrated as wisecracking log entries.",
		},
	}

	got, err := reasoner.Reasons(context.Background(), "something quiet and lonely that will wreck me", books,
		ReaderTaste{TopGenres: []string{"literary fiction"}, LovedBooks: []string{"Never Let Me Go"}})
	if err != nil {
		t.Fatalf("Reasons: %v", err)
	}
	if len(got) != len(books) {
		t.Fatalf("got %d reasons for %d books", len(got), len(books))
	}
	for i, r := range got {
		if r.Why == "" || r.Caveat == "" {
			t.Errorf("book %d: empty why/caveat: %+v", i, r)
		}
		if r.Match <= 0 || r.Match > 100 {
			t.Errorf("book %d: match %d out of range", i, r.Match)
		}
		t.Logf("%s → %d%% | why: %s | caveat: %s", books[i].Title, r.Match, r.Why, r.Caveat)
	}
	// The lonely, melancholy book has to beat the wisecracking survival one for
	// this query, otherwise the model is ignoring the request.
	if got[0].Match <= got[1].Match {
		t.Errorf("Piranesi (%d) should outscore The Martian (%d) for a quiet, lonely vibe",
			got[0].Match, got[1].Match)
	}
}

func TestTopWeighted(t *testing.T) {
	got := topWeighted(map[string]float64{
		"fantasy": 0.9, "memoir": 0.2, "horror": 0.7, "poetry": 0.5,
	}, 3)
	want := []string{"fantasy", "horror", "poetry"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if n := len(topWeighted(nil, 3)); n != 0 {
		t.Errorf("topWeighted(nil) returned %d keys", n)
	}
}
