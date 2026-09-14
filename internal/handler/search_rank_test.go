package handler

import (
	"testing"

	"github.com/hridyesh/paperboxd-backend/internal/types"
)

func book(title, isbn string, authors ...string) types.BookResponse {
	return types.BookResponse{
		VolumeInfo: types.VolumeInfo{
			Title:               title,
			Authors:             authors,
			IndustryIdentifiers: []types.IndustryIdentifier{{Type: "ISBN_13", Identifier: isbn}},
		},
	}
}

func titles(items []types.BookResponse) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.VolumeInfo.Title
	}
	return out
}

func TestMergeSearchResults(t *testing.T) {
	spidey := book("Spider-Man Comics Vol 1", "1")
	daredevil := book("Daredevil: Man Without Fear", "2")
	cachedIron := book("Iron Man: Extremis", "3", "Warren Ellis")

	cases := []struct {
		name  string
		query string
		db    []types.BookResponse
		ext   []types.BookResponse
		want  []string
	}{
		{
			name:  "external iron man beats cached fuzzy hits",
			query: "iron man comics",
			db:    []types.BookResponse{spidey, daredevil},
			ext:   []types.BookResponse{book("Invincible Iron Man", "4"), book("Some Other Book", "5")},
			want:  []string{"Invincible Iron Man", "Spider-Man Comics Vol 1", "Daredevil: Man Without Fear", "Some Other Book"},
		},
		{
			name:  "cached match first, duplicate external dropped",
			query: "iron man",
			db:    []types.BookResponse{spidey, cachedIron},
			ext:   []types.BookResponse{book("Iron Man: Extremis", "3"), book("Iron Man: Armor Wars", "6")},
			want:  []string{"Iron Man: Extremis", "Iron Man: Armor Wars", "Spider-Man Comics Vol 1"},
		},
		{
			name:  "typo keeps fuzzy db hit above external noise",
			query: "deapool",
			db:    []types.BookResponse{book("Deadpool: Bad Blood", "7")},
			ext:   []types.BookResponse{book("Random Result", "8")},
			want:  []string{"Deadpool: Bad Blood", "Random Result"},
		},
		{
			name:  "isbn and author queries count as covering",
			query: "ellis 3",
			db:    []types.BookResponse{spidey, cachedIron},
			want:  []string{"Iron Man: Extremis", "Spider-Man Comics Vol 1"},
		},
	}
	for _, c := range cases {
		got := titles(mergeSearchResults(queryWords(c.query), c.db, c.ext, 20))
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}

	if got := len(mergeSearchResults(queryWords("x"), []types.BookResponse{spidey, daredevil}, nil, 1)); got != 1 {
		t.Fatalf("page size not applied: %d", got)
	}
	if w := queryWords("the comics"); len(w) != 2 {
		t.Fatalf("all-filler query should keep its words, got %v", w)
	}
}
