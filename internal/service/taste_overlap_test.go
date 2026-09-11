package service

import (
	"testing"
	"time"
)

// The overlap number has to survive the obvious objections: shared books with
// opposite opinions are not twinship, unequal shelf sizes should not make
// twinship impossible, and thin shelves should lean on trait profiles.

func shelf(id string, books map[string]int) readerShelf {
	sh := readerShelf{UserID: id, Books: map[string]*int{}}
	for b, r := range books {
		if r == 0 {
			sh.Books[b] = nil
		} else {
			v := r
			sh.Books[b] = &v
		}
	}
	return sh
}

func TestOverlapSharedBooksOppositeOpinionsAreNotTwins(t *testing.T) {
	agree := ComputeOverlap(
		shelf("a", map[string]int{"1": 5, "2": 5, "3": 4, "4": 5}),
		shelf("b", map[string]int{"1": 5, "2": 4, "3": 5, "4": 5}),
	)
	disagree := ComputeOverlap(
		shelf("a", map[string]int{"1": 5, "2": 5, "3": 4, "4": 5}),
		shelf("b", map[string]int{"1": 1, "2": 1, "3": 2, "4": 1}),
	)
	if disagree.Overlap >= agree.Overlap {
		t.Errorf("same shelf, opposite ratings scored %v vs agreeing %v", disagree.Overlap, agree.Overlap)
	}
	if len(disagree.Disagreed) != 4 {
		t.Errorf("Disagreed = %v, want all four", disagree.Disagreed)
	}
	if len(agree.SharedLoved) != 4 {
		t.Errorf("SharedLoved = %v, want all four", agree.SharedLoved)
	}
}

// A 20-book shelf fully contained in a 400-book shelf is a strong signal,
// even though Jaccard alone would report 0.05.
func TestOverlapUnequalShelvesCanStillMatch(t *testing.T) {
	small := map[string]int{}
	big := map[string]int{}
	for i := 0; i < 20; i++ {
		id := string(rune('a' + i))
		small[id] = 5
		big[id] = 5
	}
	for i := 0; i < 380; i++ {
		big[string(rune(1000+i))] = 4
	}
	res := ComputeOverlap(shelf("s", small), shelf("b", big))
	if res.Overlap < 0.5 {
		t.Errorf("overlap = %v for a fully-contained shelf with agreeing ratings; want > 0.5", res.Overlap)
	}
}

func TestOverlapThinShelvesLeanOnTraits(t *testing.T) {
	close := &TraitProfile{
		Prefs:      map[string]float64{"pacing": 0.2, "character_driven": 0.9},
		Confidence: map[string]float64{"pacing": 1, "character_driven": 1},
	}
	far := &TraitProfile{
		Prefs:      map[string]float64{"pacing": 0.9, "character_driven": 0.1},
		Confidence: map[string]float64{"pacing": 1, "character_driven": 1},
	}
	a := shelf("a", map[string]int{"1": 5, "2": 4})
	a.Traits = close
	b := shelf("b", map[string]int{"3": 5, "4": 4})
	b.Traits = close
	c := shelf("c", map[string]int{"3": 5, "4": 4})
	c.Traits = far

	ab := ComputeOverlap(a, b)
	ac := ComputeOverlap(a, c)
	if ab.Overlap <= ac.Overlap {
		t.Errorf("no shared books: similar traits %v should beat opposite traits %v", ab.Overlap, ac.Overlap)
	}
}

func TestOverlapCouldReadIsDirectional(t *testing.T) {
	res := ComputeOverlap(
		shelf("a", map[string]int{"1": 5, "2": 5}),
		shelf("b", map[string]int{"1": 5, "9": 5, "8": 2}),
	)
	// b loved 9 (a hasn't read it); b rated 8 low so it is not a suggestion.
	if len(res.ACouldRead) != 1 || res.ACouldRead[0] != "9" {
		t.Errorf("ACouldRead = %v, want [9]", res.ACouldRead)
	}
	if len(res.BCouldRead) != 1 || res.BCouldRead[0] != "2" {
		t.Errorf("BCouldRead = %v, want [2]", res.BCouldRead)
	}
}

func TestOverlapIsBounded(t *testing.T) {
	res := ComputeOverlap(
		shelf("a", map[string]int{"1": 5, "2": 5, "3": 5}),
		shelf("b", map[string]int{"1": 5, "2": 5, "3": 5}),
	)
	if res.Overlap > 1 || res.Overlap < 0 {
		t.Errorf("overlap = %v out of 0..1", res.Overlap)
	}
	if res.Overlap < 0.9 {
		t.Errorf("identical shelves and ratings scored %v, want ~1", res.Overlap)
	}
}

func TestGreetingByHour(t *testing.T) {
	cases := map[int]string{3: "Still up?", 8: "Good morning.", 14: "Good afternoon.", 20: "Good evening."}
	for h, want := range cases {
		at := time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC)
		if got := greeting(at); got != want {
			t.Errorf("hour %d: %q, want %q", h, got, want)
		}
	}
}
