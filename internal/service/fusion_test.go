package service

import (
	"strings"
	"testing"
	"time"
)

// The Fusion story must only say things the two shelves prove, and must read
// correctly from both sides.

func fusionFixture() fusionInput {
	a := shelf("a", map[string]int{"s1": 5, "s2": 5, "s3": 4, "s4": 2, "mine": 5})
	b := shelf("b", map[string]int{"s1": 5, "s2": 4, "s3": 5, "s4": 5, "theirs": 5, "meh": 3})
	books := map[string]fusionBookMeta{}
	for _, id := range []string{"s1", "s2", "s3", "s4", "mine", "theirs", "meh", "r1", "r2", "r3", "wild"} {
		books[id] = fusionBookMeta{Title: "Book " + id, Authors: []string{"Author"}}
	}
	return fusionInput{
		A: fusionSide{
			Person: FusionPerson{UserID: "a", First: "Hridyesh"},
			Shelf:  a,
			AllIDs: map[string]bool{"s1": true, "s2": true, "s3": true, "s4": true, "mine": true, "tbr-a": true},
			Genres: map[string]float64{"Literary Fiction": 6, "Horror": 4},
			Recs: []BookCandidate{
				{ID: "r1", Title: "Book r1"},
				{ID: "theirs", Title: "Book theirs"}, // already on B's shelf: never a pick
				{ID: "r2", Title: "Book r2"},
			},
			MedianPages: 250,
		},
		B: fusionSide{
			Person: FusionPerson{UserID: "b", First: "Alex"},
			Shelf:  b,
			AllIDs: map[string]bool{"s1": true, "s2": true, "s3": true, "s4": true, "theirs": true, "meh": true},
			Genres: map[string]float64{"Literary Fiction": 5, "History": 5},
			Recs: []BookCandidate{
				{ID: "r2", Title: "Book r2"},
				{ID: "wild", Title: "Book wild", Confidence: "Wild card", Categories: []string{"Westerns"}},
				{ID: "r3", Title: "Book r3"},
			},
			MedianPages: 520,
		},
		Books: books,
		Now:   time.Unix(0, 0),
	}
}

func TestFusionSharedShelf(t *testing.T) {
	v := buildFusionView(fusionFixture())
	if v.SharedCount != 4 || v.LovedCount != 3 {
		t.Fatalf("shared=%d loved=%d, want 4 and 3", v.SharedCount, v.LovedCount)
	}
	// s1, s2, s3 within a star; s4 is 2 vs 5.
	if v.Agreement != 75 {
		t.Errorf("Agreement = %d, want 75", v.Agreement)
	}
	if v.SharedLoved[0].ID != "s1" { // 5+5 ranks first
		t.Errorf("first shared loved = %s, want s1", v.SharedLoved[0].ID)
	}
	if v.Disagreement == nil || v.Disagreement.ID != "s4" || *v.Disagreement.You != 2 || *v.Disagreement.Them != 5 {
		t.Errorf("Disagreement = %+v, want s4 you 2 them 5", v.Disagreement)
	}
}

func TestFusionShelvesPointTheRightWay(t *testing.T) {
	v := buildFusionView(fusionFixture())
	if len(v.ForYou) != 1 || v.ForYou[0].ID != "theirs" || v.ForYou[0].Them == nil || v.ForYou[0].You != nil {
		t.Fatalf("ForYou = %+v, want only 'theirs' rated by them", v.ForYou)
	}
	if !strings.HasPrefix(v.ForYou[0].Reason, "Alex ") {
		t.Errorf("ForYou reason %q should name Alex", v.ForYou[0].Reason)
	}
	if len(v.ForThem) != 1 || v.ForThem[0].ID != "mine" || v.ForThem[0].You == nil {
		t.Fatalf("ForThem = %+v, want only 'mine' rated by you", v.ForThem)
	}
	// "meh" is a 3: liked-ish is not loved.
	for _, bk := range v.ForYou {
		if bk.ID == "meh" {
			t.Error("a 3-star book was offered as something Alex loved")
		}
	}

	flip := buildFusionView(fusionFixture().flipped())
	if flip.Me.First != "Alex" || len(flip.ForYou) != 1 || flip.ForYou[0].ID != "mine" {
		t.Errorf("flipped view ForYou = %+v, want 'mine' for Alex", flip.ForYou)
	}
	if flip.Score != v.Score {
		t.Errorf("score differs by side: %d vs %d", flip.Score, v.Score)
	}
}

func TestFusionPicksAreUnreadByBoth(t *testing.T) {
	v := buildFusionView(fusionFixture())
	if v.Pick == nil || v.Pick.Book.ID != "r2" || v.Pick.Label != "Strong Fusion pick" {
		t.Fatalf("Pick = %+v, want r2 (on both lists)", v.Pick)
	}
	if v.Wildcard == nil || v.Wildcard.Book.ID != "wild" {
		t.Errorf("Wildcard = %+v, want 'wild'", v.Wildcard)
	}
	for _, bk := range v.Both {
		if bk.ID == "theirs" {
			t.Error("a book on Alex's shelf was offered as unread by both")
		}
	}
	seen := map[string]bool{}
	for _, bk := range v.Both {
		if seen[bk.ID] {
			t.Errorf("duplicate %s in Both", bk.ID)
		}
		seen[bk.ID] = true
	}
}

func TestFusionWildcardSkipsUsualGenres(t *testing.T) {
	in := fusionFixture()
	in.B.Recs[1].Categories = []string{"Horror"} // Hridyesh reads plenty of horror
	if v := buildFusionView(in); v.Wildcard != nil {
		t.Errorf("Wildcard = %+v; a genre one of them reads is not a wildcard", v.Wildcard)
	}
}

func TestFusionSplitsWithoutTraits(t *testing.T) {
	v := buildFusionView(fusionFixture())
	labels := map[string]FusionSplit{}
	for _, s := range v.Splits {
		labels[s.Label] = s
	}
	if s, ok := labels["Book length"]; !ok || s.You != "Around 250 pages" || s.Them != "Around 500 pages" {
		t.Errorf("length split = %+v", labels["Book length"])
	}
	if _, ok := labels["History"]; !ok {
		t.Errorf("splits %+v should include History (only Alex reads it)", v.Splits)
	}
	if len(v.Agree) == 0 || v.Agree[0].Kind != "genre" || v.Agree[0].Key != "Literary Fiction" {
		t.Errorf("Agree = %+v, want Literary Fiction genre fallback", v.Agree)
	}
}

func TestFusionAgreeOnAxes(t *testing.T) {
	in := fusionFixture()
	conf := map[string]float64{"character_driven": 1, "darkness": 1}
	in.A.Shelf.Traits = &TraitProfile{Prefs: map[string]float64{"character_driven": 0.9, "darkness": 0.1}, Confidence: conf}
	in.B.Shelf.Traits = &TraitProfile{Prefs: map[string]float64{"character_driven": 0.85, "darkness": 0.9}, Confidence: conf}
	v := buildFusionView(in)
	if len(v.Agree) != 1 || v.Agree[0].Key != "character_driven" || v.Agree[0].You != 90 || v.Agree[0].Them != 85 {
		t.Fatalf("Agree = %+v, want character_driven 90/85", v.Agree)
	}
	if v.Splits[0].Label != axisLabel["darkness"] || v.Splits[0].You != "Warm, comforting books" {
		t.Errorf("first split = %+v, want darkness", v.Splits[0])
	}
}

func TestFusionLowData(t *testing.T) {
	in := fusionFixture()
	in.B.Shelf = shelf("b", map[string]int{"s1": 5})
	if !buildFusionView(in).LowData {
		t.Error("a one-book shelf should be low data")
	}
	if buildFusionView(fusionFixture()).LowData {
		t.Error("four shared books should not be low data")
	}
}

func TestFusionToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tok, err := newFusionToken()
		if err != nil || !ValidFusionToken(tok) || seen[tok] {
			t.Fatalf("token %q invalid or repeated (err %v)", tok, err)
		}
		seen[tok] = true
	}
	for _, bad := range []string{"", "short", "ABCDEFGHJKMN", "abcdefghijk0", "abcdefghijklm", "../../etc/pa"} {
		if ValidFusionToken(bad) {
			t.Errorf("ValidFusionToken(%q) = true", bad)
		}
	}
}
