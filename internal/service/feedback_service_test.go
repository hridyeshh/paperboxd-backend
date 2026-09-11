package service

import (
	"testing"
	"time"
)

// Negative signals are the easiest thing in a recommender to get subtly wrong:
// each of these covers a case where the wrong behaviour still looks plausible.

func TestSuppressionDistinguishesNoFromNotYet(t *testing.T) {
	if _, forever := suppressionFor(VerdictNotForMe); !forever {
		t.Error("not_for_me must suppress permanently")
	}
	if _, forever := suppressionFor(VerdictAlreadyRead); !forever {
		t.Error("already_read must suppress permanently")
	}

	until, forever := suppressionFor(VerdictNotNow)
	if forever || until == nil {
		t.Fatal("not_now must suppress temporarily, not permanently")
	}
	if until.Before(time.Now().Add(60 * 24 * time.Hour)) {
		t.Errorf("not_now suppression ends %v, too soon to feel like 'later'", until)
	}

	if until, forever := suppressionFor(VerdictLoved); forever || until != nil {
		t.Error("loved must not suppress the book at all")
	}
}

// already_read means "I know this one", not "I disliked it". Letting it into
// the trait profile would teach the engine to avoid exactly the books a reader
// has most enjoyed.
func TestAlreadyReadNeverTeachesDislike(t *testing.T) {
	rows := []FeedbackRow{{
		BookID: "b1", Verdict: VerdictAlreadyRead,
		Scalars: allAxes(0.9), Confidence: 1,
	}}
	if got := feedbackAsTraitedBooks(rows); len(got) != 0 {
		t.Errorf("already_read produced %d training rows, want 0", len(got))
	}
}

// "maybe" and "not now" are scheduling, not opinion.
func TestSoftVerdictsDoNotTrainTaste(t *testing.T) {
	for _, v := range []string{VerdictMaybe, VerdictNotNow} {
		rows := []FeedbackRow{{BookID: "b", Verdict: v, Scalars: allAxes(0.9), Confidence: 1}}
		if got := feedbackAsTraitedBooks(rows); len(got) != 0 {
			t.Errorf("verdict %q produced %d training rows, want 0", v, len(got))
		}
	}
}

// A card verdict is an opinion about a cover and a blurb; a rating is an
// opinion about a book that was read. They must not carry equal weight.
func TestCardVerdictsWeighLessThanRatings(t *testing.T) {
	rows := []FeedbackRow{{BookID: "b", Verdict: VerdictLoved, Scalars: allAxes(0.9), Confidence: 1.0}}
	got := feedbackAsTraitedBooks(rows)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Confidence >= 1.0 {
		t.Errorf("verdict confidence = %v, want discounted below the extractor's 1.0", got[0].Confidence)
	}
}

// The core claim of coded feedback: "too slow" is a measurement of pacing and
// must not move darkness, romance or worldbuilding with it.
func TestReasonCodesMoveOnlyTheirOwnAxis(t *testing.T) {
	book := allAxes(0.5)
	book["pacing"] = 0.1
	book["darkness"] = 0.95

	p := TraitProfile{Prefs: map[string]float64{}, Dislikes: map[string]float64{}, Confidence: map[string]float64{}}
	applyReasonCodes(&p, []FeedbackRow{{
		BookID: "b", Verdict: VerdictNotForMe,
		ReasonCodes: []string{ReasonTooSlow},
		Scalars:     book, Confidence: 1,
	}})

	got, ok := p.Dislikes["pacing"]
	if !ok {
		t.Fatal("too_slow did not record a pacing dislike")
	}
	if got > 0.2 {
		t.Errorf("pacing dislike = %v, want the book's slow value (~0.1)", got)
	}
	if _, ok := p.Dislikes["darkness"]; ok {
		t.Error("too_slow moved the darkness axis; a code must not leak across axes")
	}
}

func TestEveryCodedReasonMapsToARealAxis(t *testing.T) {
	known := make(map[string]bool, len(TraitAxes))
	for _, a := range TraitAxes {
		known[a] = true
	}
	for code, m := range reasonCodeAxis {
		if _, ok := validReasonCodes[code]; !ok {
			t.Errorf("reasonCodeAxis maps %q, which is not a valid reason code", code)
		}
		if !known[m.axis] {
			t.Errorf("reason code %q maps to unknown axis %q", code, m.axis)
		}
	}
}

func TestSanitizeReasonCodesDropsNoiseNotSignal(t *testing.T) {
	got := SanitizeReasonCodes([]string{ReasonTooSlow, "made_up", ReasonTooSlow, ReasonTooDark})
	if len(got) != 2 {
		t.Fatalf("got %v, want the two valid codes de-duplicated", got)
	}
	if got[0] != ReasonTooSlow || got[1] != ReasonTooDark {
		t.Errorf("got %v, want order preserved", got)
	}
	if len(SanitizeReasonCodes(nil)) != 0 {
		t.Error("nil input produced codes")
	}
}

func TestSanitizeReasonCodesCaps(t *testing.T) {
	in := []string{ReasonTooLong, ReasonTooSlow, ReasonWrongGenre, ReasonWrongMood, ReasonTooDark, ReasonTooRomance}
	if got := SanitizeReasonCodes(in); len(got) != 4 {
		t.Errorf("got %d codes, want capped at 4", len(got))
	}
}

func TestValidVerdictRejectsUnknown(t *testing.T) {
	for _, v := range []string{VerdictLoved, VerdictMaybe, VerdictNotForMe, VerdictAlreadyRead, VerdictNotNow} {
		if !ValidVerdict(v) {
			t.Errorf("ValidVerdict(%q) = false", v)
		}
	}
	for _, v := range []string{"", "hate", "LOVED", "not-for-me"} {
		if ValidVerdict(v) {
			t.Errorf("ValidVerdict(%q) = true", v)
		}
	}
}

// Explicit feedback and ratings must train one profile, not two.
func TestFeedbackAndRatingsTrainTheSameProfile(t *testing.T) {
	liked := allAxes(0.5)
	liked["pacing"] = 0.9

	shelf := []TraitedBook{
		{Rating: ptrInt(5), Status: "read", Scalars: liked, Confidence: 1},
		{Rating: ptrInt(5), Status: "read", Scalars: liked, Confidence: 1},
	}
	feedback := feedbackAsTraitedBooks([]FeedbackRow{
		{BookID: "b", Verdict: VerdictLoved, Scalars: liked, Confidence: 1},
	})

	withFeedback := ComputeTraitProfile(append(shelf, feedback...))
	shelfOnly := ComputeTraitProfile(shelf)

	if withFeedback.Confidence["pacing"] <= shelfOnly.Confidence["pacing"] {
		t.Error("adding explicit feedback did not increase confidence")
	}
}

// Phase 0's two clocks. The recent profile must be computed from recent
// opinions only, and be less confident than the long-term one when the reader
// has rated less lately — that lower confidence is what stops two books from
// this month overriding five years of taste.
func TestRecentTraitProfileUsesOnlyRecentOpinions(t *testing.T) {
	now := time.Now()
	old := TraitedBook{Rating: ptrInt(5), Status: "read", Scalars: allAxes(0.1), Confidence: 1, When: now.Add(-400 * 24 * time.Hour)}
	recent := TraitedBook{Rating: ptrInt(5), Status: "read", Scalars: allAxes(0.9), Confidence: 1, When: now.Add(-10 * 24 * time.Hour)}

	rp := RecentTraitProfile([]TraitedBook{old, old, old, old, recent}, nil, now)
	if got := rp.Prefs["pacing"]; got < 0.85 {
		t.Errorf("recent pacing = %v; old books leaked into the recent window", got)
	}

	lt := ComputeTraitProfile([]TraitedBook{old, old, old, old, recent})
	if rp.Confidence["pacing"] >= lt.Confidence["pacing"] {
		t.Errorf("recent confidence %v >= long-term %v; one recent book must not be as sure as five",
			rp.Confidence["pacing"], lt.Confidence["pacing"])
	}
}

// Feedback rows have no When; they are always recent.
func TestRecentTraitProfileIncludesFeedback(t *testing.T) {
	now := time.Now()
	fb := feedbackAsTraitedBooks([]FeedbackRow{{BookID: "b", Verdict: VerdictLoved, Scalars: allAxes(0.9), Confidence: 1}})
	rp := RecentTraitProfile(fb, nil, now)
	if !rp.HasSignal() {
		t.Error("a loved verdict produced no recent signal")
	}
}

// TBR and finished-unrated now teach — but far less than a rating.
func TestImplicitShelfSignalsAreWeakPositives(t *testing.T) {
	pTBR, _ := traitSignalWeight(nil, "pending", false)
	pRead, _ := traitSignalWeight(nil, "read", false)
	pFive, _ := traitSignalWeight(ptrInt(5), "read", false)
	if pTBR <= 0 || pRead <= 0 {
		t.Errorf("tbr=%v read=%v; implicit signals must be positive", pTBR, pRead)
	}
	if pTBR >= pRead || pRead >= pFive {
		t.Errorf("tbr=%v read=%v five-star=%v; must be strictly ordered", pTBR, pRead, pFive)
	}
	// Ten saved doorstops must not outvote three loved novellas.
	if pTBR*10 >= pFive*3 {
		t.Errorf("tbr weight %v is high enough for a big pile to outvote ratings", pTBR)
	}
}
