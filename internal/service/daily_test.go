package service

import "testing"

func TestPickAtom(t *testing.T) {
	shelf := []DailyAtom{
		{Slug: "narrators", Topics: []string{"mystery"}},
		{Slug: "nostalgia", Topics: []string{"literary"}},
		{Slug: "short-books", Topics: nil},
	}

	if _, ok := pickAtom(nil, nil, 3); ok {
		t.Fatal("empty shelf should pick nothing")
	}

	// Same seed, same atom: a reload must not reshuffle the page.
	a, _ := pickAtom(shelf, nil, 7)
	b, _ := pickAtom(shelf, nil, 7)
	if a.Slug != b.Slug {
		t.Fatalf("not deterministic: %s vs %s", a.Slug, b.Slug)
	}

	// Different days rotate through the shelf.
	seen := map[string]bool{}
	for day := uint64(0); day < 3; day++ {
		got, _ := pickAtom(shelf, nil, day)
		seen[got.Slug] = true
	}
	if len(seen) != 3 {
		t.Fatalf("three days should show three atoms, saw %v", seen)
	}

	// A topic that touches the reader's genres wins on every seed.
	genres := map[string]float64{"Fiction / Mystery & Detective": 0.8, "Fiction / Literary": 0}
	for seed := uint64(0); seed < 6; seed++ {
		got, _ := pickAtom(shelf, genres, seed)
		if got.Slug != "narrators" {
			t.Fatalf("seed %d: want narrators, got %s", seed, got.Slug)
		}
	}

	// No match falls back to the whole shelf instead of nothing.
	if _, ok := pickAtom(shelf, map[string]float64{"Cookbooks": 1}, 1); !ok {
		t.Fatal("unmatched taste should still get an atom")
	}
}

func TestReadSecondsAndFirstWords(t *testing.T) {
	if got := readSeconds("<p>short</p>"); got != 15 {
		t.Fatalf("floor: want 15, got %d", got)
	}
	long := ""
	for i := 0; i < 460; i++ {
		long += "word "
	}
	if got := readSeconds("<p>" + long + "</p>"); got != 120 {
		t.Fatalf("460 words: want 120s, got %d", got)
	}
	if got := firstWords("<p>one two</p><p>three four</p>", 3); got != "one two three…" {
		t.Fatalf("firstWords: got %q", got)
	}
}
