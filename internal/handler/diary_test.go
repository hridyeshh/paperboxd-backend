package handler

import "testing"

// The private case is the one that matters: before this guard existed, entries
// marked private were still sent to Cohere and their text stored in cleartext.
func TestShouldEmbedDiaryEntry(t *testing.T) {
	tests := []struct {
		name      string
		hasBook   bool
		isPrivate bool
		want      bool
	}{
		{"public entry on a book", true, false, true},
		{"private entry on a book", true, true, false},
		{"public entry with no book", false, false, false},
		{"private entry with no book", false, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldEmbedDiaryEntry(tt.hasBook, tt.isPrivate); got != tt.want {
				t.Fatalf("shouldEmbedDiaryEntry(%v, %v) = %v, want %v",
					tt.hasBook, tt.isPrivate, got, tt.want)
			}
		})
	}
}
