package service

import (
	"math"
	"strings"
	"testing"
)

// These cover the places trait handling degrades silently rather than loudly:
// a model response that parses but is subtly wrong, a taste profile built from
// too little evidence, and a reason that is plausible but not actually true of
// the book.

func TestParseTraitResponseHandlesFenceAndClamps(t *testing.T) {
	raw := "```json\n[{\"scalars\":{\"character_driven\":1.4,\"pacing\":-0.2},\"confidence\":2.0,\"moods\":[\"Sad\",\"sad\",\" tender \"]}]\n```"
	out, err := ParseTraitResponse(raw, 1)
	if err != nil {
		t.Fatalf("ParseTraitResponse: %v", err)
	}
	if got := out[0].Scalars["character_driven"]; got != 1.0 {
		t.Errorf("character_driven = %v, want clamped to 1.0", got)
	}
	if got := out[0].Scalars["pacing"]; got != 0 {
		t.Errorf("pacing = %v, want clamped to 0", got)
	}
	if out[0].Confidence != 1.0 {
		t.Errorf("confidence = %v, want clamped to 1.0", out[0].Confidence)
	}
	if len(out[0].Moods) != 2 || out[0].Moods[0] != "sad" || out[0].Moods[1] != "tender" {
		t.Errorf("moods = %v, want lowercased and de-duplicated", out[0].Moods)
	}
}

// A missing axis must read as "no opinion" (0.5), never as a confident 0.0.
// On character_driven, 0.0 is the claim "this is pure plot machinery" — a very
// different thing from "the blurb didn't say".
func TestParseTraitResponseFillsMissingAxesWithNeutral(t *testing.T) {
	out, err := ParseTraitResponse(`[{"scalars":{"pacing":0.9},"confidence":0.5}]`, 1)
	if err != nil {
		t.Fatalf("ParseTraitResponse: %v", err)
	}
	for _, axis := range TraitAxes {
		v, ok := out[0].Scalars[axis]
		if !ok {
			t.Errorf("axis %q missing after normalise", axis)
			continue
		}
		if axis != "pacing" && v != 0.5 {
			t.Errorf("axis %q = %v, want 0.5 (no opinion)", axis, v)
		}
	}
}

// Misalignment would attach one book's traits to another. That is worse than
// no traits: it produces a confident, specific, false reason.
func TestParseTraitResponseRejectsWrongCount(t *testing.T) {
	if _, err := ParseTraitResponse(`[{"confidence":0.5}]`, 3); err == nil {
		t.Error("ParseTraitResponse accepted 1 result for 3 books")
	}
	if _, err := ParseTraitResponse(`not json`, 1); err == nil {
		t.Error("ParseTraitResponse accepted non-JSON")
	}
}

func TestParseTraitResponseDropsUnknownAxes(t *testing.T) {
	out, err := ParseTraitResponse(`[{"scalars":{"vibes":0.9,"pacing":0.3},"confidence":0.6}]`, 1)
	if err != nil {
		t.Fatalf("ParseTraitResponse: %v", err)
	}
	if _, ok := out[0].Scalars["vibes"]; ok {
		t.Error("invented axis 'vibes' survived normalise")
	}
}

func shelfBook(rating int, scalars map[string]float64) TraitedBook {
	r := rating
	return TraitedBook{Rating: &r, Status: "read", Scalars: scalars, Confidence: 1.0}
}

func allAxes(v float64) map[string]float64 {
	m := make(map[string]float64, len(TraitAxes))
	for _, a := range TraitAxes {
		m[a] = v
	}
	return m
}

func TestComputeTraitProfileLearnsFromLovedBooks(t *testing.T) {
	shelf := []TraitedBook{
		shelfBook(5, allAxes(0.9)),
		shelfBook(5, allAxes(0.9)),
		shelfBook(4, allAxes(0.8)),
	}
	p := ComputeTraitProfile(shelf)
	got := p.Prefs["character_driven"]
	if got < 0.85 || got > 0.92 {
		t.Errorf("pref = %v, want ~0.87 (weighted toward the two 5-star books)", got)
	}
	if p.Confidence["character_driven"] <= 0 {
		t.Error("confidence = 0 after three rated books")
	}
}

// Three stars is genuinely neutral: it should teach nothing in either
// direction, or a shelf of shrugs would look like a preference.
func TestComputeTraitProfileIgnoresNeutralRatings(t *testing.T) {
	p := ComputeTraitProfile([]TraitedBook{shelfBook(3, allAxes(1.0))})
	if len(p.Prefs) != 0 {
		t.Errorf("prefs = %v, want empty from 3-star books alone", p.Prefs)
	}
}

// Evidence has to accumulate. One loved book should personalise a little, not
// assert a taste — otherwise the first rating a reader makes defines them.
func TestComputeTraitProfileConfidenceGrowsWithEvidence(t *testing.T) {
	one := ComputeTraitProfile([]TraitedBook{shelfBook(5, allAxes(0.9))})
	many := ComputeTraitProfile([]TraitedBook{
		shelfBook(5, allAxes(0.9)), shelfBook(5, allAxes(0.9)),
		shelfBook(5, allAxes(0.9)), shelfBook(5, allAxes(0.9)),
		shelfBook(5, allAxes(0.9)), shelfBook(5, allAxes(0.9)),
	})
	if one.Confidence["pacing"] >= many.Confidence["pacing"] {
		t.Errorf("confidence did not grow: one=%v many=%v",
			one.Confidence["pacing"], many.Confidence["pacing"])
	}
	if many.Confidence["pacing"] != 1.0 {
		t.Errorf("confidence = %v after six loved books, want capped at 1.0",
			many.Confidence["pacing"])
	}
}

// A low-confidence extraction must not be able to define a taste on its own.
func TestComputeTraitProfileDiscountsLowExtractorConfidence(t *testing.T) {
	weak := TraitedBook{Rating: ptrInt(5), Status: "read", Scalars: allAxes(0.9), Confidence: 0.1}
	strong := TraitedBook{Rating: ptrInt(5), Status: "read", Scalars: allAxes(0.9), Confidence: 1.0}
	if ComputeTraitProfile([]TraitedBook{weak}).Confidence["darkness"] >=
		ComputeTraitProfile([]TraitedBook{strong}).Confidence["darkness"] {
		t.Error("a thin-description extraction carried as much weight as a confident one")
	}
}

func TestTraitFitRewardsCloseness(t *testing.T) {
	p := ComputeTraitProfile([]TraitedBook{
		shelfBook(5, allAxes(0.9)), shelfBook(5, allAxes(0.9)),
		shelfBook(5, allAxes(0.9)), shelfBook(5, allAxes(0.9)),
		shelfBook(5, allAxes(0.9)),
	})
	near, ok := TraitFit(allAxes(0.85), &p)
	if !ok {
		t.Fatal("TraitFit reported no signal on a confident profile")
	}
	far, _ := TraitFit(allAxes(0.1), &p)
	if near <= far {
		t.Errorf("near=%v far=%v; a closer book must score higher", near, far)
	}
}

// Without this, a book with no extracted traits and a book that perfectly
// matches would be indistinguishable from a neutral one.
func TestTraitFitReportsNoSignalWhenNothingToCompare(t *testing.T) {
	empty := TraitProfile{}
	if _, ok := TraitFit(allAxes(0.5), &empty); ok {
		t.Error("TraitFit claimed a signal from an empty profile")
	}
	p := ComputeTraitProfile([]TraitedBook{shelfBook(5, allAxes(0.9))})
	if _, ok := TraitFit(nil, &p); ok {
		t.Error("TraitFit claimed a signal for a book with no traits")
	}
}

// The roadmap's central example: a reader who loves character-driven historical
// fiction but abandons plot-dense historical fiction. Genre says "yes" to both;
// only the trait axes can separate them.
func TestTraitClashSeparatesSameGenreOppositeShape(t *testing.T) {
	loved := allAxes(0.5)
	loved["character_driven"] = 0.9
	loved["plot_intensity"] = 0.15

	hated := allAxes(0.5)
	hated["character_driven"] = 0.15
	hated["plot_intensity"] = 0.9

	shelf := []TraitedBook{
		{Rating: ptrInt(5), Status: "read", Scalars: loved, Confidence: 1},
		{Rating: ptrInt(5), Status: "read", Scalars: loved, Confidence: 1},
		{Rating: ptrInt(5), Status: "read", Scalars: loved, Confidence: 1},
		{Rating: ptrInt(1), Status: "read", Scalars: hated, Confidence: 1},
		{Rating: ptrInt(1), Status: "read", Scalars: hated, Confidence: 1},
	}
	p := ComputeTraitProfile(shelf)

	clashHigh, ok := TraitClash(hated, &p)
	if !ok {
		t.Fatal("TraitClash reported no signal despite two disliked books")
	}
	clashLow, _ := TraitClash(loved, &p)
	if clashHigh <= clashLow {
		t.Errorf("clash(plot-dense)=%v clash(character-driven)=%v; the rejected shape must clash more",
			clashHigh, clashLow)
	}

	fitHigh, _ := TraitFit(loved, &p)
	fitLow, _ := TraitFit(hated, &p)
	if fitHigh <= fitLow {
		t.Errorf("fit(loved shape)=%v fit(hated shape)=%v", fitHigh, fitLow)
	}
}

// An axis where liked and disliked books agree says nothing about why the
// reader rejected anything, and must not contribute to a clash score.
func TestTraitClashIgnoresNonDivergentAxes(t *testing.T) {
	same := allAxes(0.5)
	same["darkness"] = 0.6
	shelf := []TraitedBook{
		{Rating: ptrInt(5), Status: "read", Scalars: same, Confidence: 1},
		{Rating: ptrInt(1), Status: "read", Scalars: same, Confidence: 1},
	}
	p := ComputeTraitProfile(shelf)
	if _, ok := TraitClash(same, &p); ok {
		t.Error("TraitClash found signal where liked and disliked books are identical")
	}
}

// The trust rule: a reason must be true of the book, not merely true of the
// reader. A book sitting at the opposite end of the reader's strongest axis
// must produce no sentence at all.
func TestBuildTraitReasonStaysSilentWhenBookDoesNotMatch(t *testing.T) {
	loved := allAxes(0.5)
	loved["character_driven"] = 0.95
	shelf := make([]TraitedBook, 0, 5)
	for i := 0; i < 5; i++ {
		shelf = append(shelf, TraitedBook{Rating: ptrInt(5), Status: "read", Scalars: loved, Confidence: 1})
	}
	p := ComputeTraitProfile(shelf)

	opposite := allAxes(0.5)
	opposite["character_driven"] = 0.05
	if got := buildTraitReason(Candidate{Traits: opposite}, &p); got != "" {
		t.Errorf("buildTraitReason = %q for a book at the opposite end of the axis; want silence", got)
	}

	matching := allAxes(0.5)
	matching["character_driven"] = 0.9
	got := buildTraitReason(Candidate{Traits: matching}, &p)
	if !strings.Contains(got, "character-driven") {
		t.Errorf("buildTraitReason = %q, want it to name the axis that matched", got)
	}
}

func TestBuildTraitReasonSilentWithoutProfile(t *testing.T) {
	if got := buildTraitReason(Candidate{Traits: allAxes(0.9)}, nil); got != "" {
		t.Errorf("buildTraitReason = %q with no profile; want silence", got)
	}
	empty := TraitProfile{}
	if got := buildTraitReason(Candidate{Traits: allAxes(0.9)}, &empty); got != "" {
		t.Errorf("buildTraitReason = %q with an empty profile; want silence", got)
	}
}

// Every axis must have phrasing for both directions, or a confident preference
// silently produces no reason at all.
func TestEveryAxisHasPhrasing(t *testing.T) {
	for _, axis := range TraitAxes {
		p, ok := traitPhrases[axis]
		if !ok {
			t.Errorf("axis %q has no phrasing", axis)
			continue
		}
		if p[0] == "" || p[1] == "" {
			t.Errorf("axis %q has an empty direction: %v", axis, p)
		}
	}
	if len(traitPhrases) != len(TraitAxes) {
		t.Errorf("traitPhrases has %d entries for %d axes", len(traitPhrases), len(TraitAxes))
	}
	for axis := range traitAxisMeaning {
		if _, ok := traitPhrases[axis]; !ok {
			t.Errorf("axis %q is documented for the prompt but has no phrasing", axis)
		}
	}
}

func TestStrongestTraitsOrdersByConvictionNotValue(t *testing.T) {
	p := TraitProfile{
		Prefs:      map[string]float64{"pacing": 0.95, "darkness": 0.55},
		Confidence: map[string]float64{"pacing": 0.2, "darkness": 1.0},
	}
	got := StrongestTraits(&p, 2)
	if len(got) != 2 {
		t.Fatalf("got %d axes, want 2", len(got))
	}
	// pacing: |0.95-0.5|*2*0.2 = 0.18; darkness: |0.55-0.5|*2*1.0 = 0.10.
	if got[0].Axis != "pacing" {
		t.Errorf("strongest = %q, want pacing", got[0].Axis)
	}
	if math.Abs(got[0].Strength-0.18) > 1e-9 {
		t.Errorf("strength = %v, want 0.18", got[0].Strength)
	}
}

func ptrInt(v int) *int { return &v }
