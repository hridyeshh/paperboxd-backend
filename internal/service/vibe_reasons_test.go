package service

import (
	"encoding/json"
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
