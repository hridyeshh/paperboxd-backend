package service

import (
	"strings"
	"testing"
)

// Phase 4's one rule is "never fabricate reasons". These pin that every
// sentence the engine produces is backed by a signal that is actually set on
// the candidate, and that the more specific signal wins.

func TestSocialReasonSaysLovedOnlyWhenEveryFriendLikedIt(t *testing.T) {
	re := &ReasonEngine{}
	c := Candidate{SocialScore: 3, FriendNames: []string{"maya", "sam"}, FriendLovedCount: 1}
	if got := re.Build(c, nil, "").Text; got != "maya and sam read this" {
		t.Errorf("got %q, want the honest verb when only one of two liked it", got)
	}
	c.FriendLovedCount = 2
	if got := re.Build(c, nil, "").Text; got != "maya and sam loved this" {
		t.Errorf("got %q, want loved when both liked it", got)
	}
}

func TestTwinReasonOutranksAnchorAndUsesNamesUpToTwo(t *testing.T) {
	re := &ReasonEngine{}
	c := Candidate{TwinCount: 1, TwinNames: []string{"ana"}, AnchorKind: AnchorLoved, AnchorTitle: "Stoner"}
	r := re.Build(c, nil, "")
	if r.Type != "people_like_you" || r.Text != "@ana loved this" {
		t.Errorf("got %+v", r)
	}
	c.TwinCount, c.TwinNames = 4, []string{"ana", "bo"}
	if got := re.Build(c, nil, "").Text; got != "4 readers with your taste loved this" {
		t.Errorf("got %q", got)
	}
}

func TestAnchorReasonNamesTheRealBook(t *testing.T) {
	re := &ReasonEngine{}
	cases := map[string]string{
		AnchorRated5: "Because you rated Stoner 5★",
		AnchorLoved:  "Because you loved Stoner",
		AnchorTBR:    "Similar to Stoner on your TBR",
	}
	for kind, want := range cases {
		got := re.Build(Candidate{AnchorKind: kind, AnchorTitle: "Stoner"}, nil, "").Text
		if got != want {
			t.Errorf("%s: got %q want %q", kind, got, want)
		}
	}
	// No anchor, no social, no traits: falls through to the neutral line
	// rather than inventing something.
	if got := re.Build(Candidate{}, nil, "").Text; got != "Picked for you" {
		t.Errorf("got %q, want the neutral fallback", got)
	}
}

func TestSocialReasonGetsTraitTailOnlyWhenBookMatchesReader(t *testing.T) {
	re := &ReasonEngine{}
	profile := &UserSignalProfile{Traits: &TraitProfile{
		Prefs:      map[string]float64{"character_driven": 0.9},
		Confidence: map[string]float64{"character_driven": 1},
	}}
	c := Candidate{SocialScore: 2, FriendNames: []string{"maya"}, FriendLovedCount: 1,
		Traits: map[string]float64{"character_driven": 0.85}}
	got := re.Build(c, profile, "").Text
	if !strings.HasPrefix(got, "maya loved this — and you tend to love quiet, character-driven") {
		t.Errorf("got %q", got)
	}
	c.Traits["character_driven"] = 0.2 // book is the opposite of the reader
	if got := re.Build(c, profile, "").Text; got != "maya loved this" {
		t.Errorf("got %q, want no tail when the book does not match", got)
	}
}

func TestNearestAnchorPrefersLovedOverTBRAndRespectsThreshold(t *testing.T) {
	anchors := []Anchor{
		{Title: "TBR Twin", Kind: AnchorTBR, Embedding: []float32{1, 0, 0}},
		{Title: "Loved Cousin", Kind: AnchorRated5, Embedding: []float32{0.9, 0.436, 0}}, // cos ≈ 0.9
	}
	c := Candidate{Embedding: []float32{1, 0, 0}}
	nearestAnchor(&c, anchors)
	if c.AnchorKind != AnchorRated5 || c.AnchorTitle != "Loved Cousin" {
		t.Errorf("got %s/%s, want the loved book even though the TBR one is closer", c.AnchorKind, c.AnchorTitle)
	}

	far := Candidate{Embedding: []float32{0, 0, 1}}
	nearestAnchor(&far, anchors)
	if far.AnchorKind != "" {
		t.Errorf("got anchor %q for an orthogonal book; threshold is %v", far.AnchorTitle, anchorMinSim)
	}
}

func TestMergeCandidatePoolsAbsorbsEvidenceAndCaps(t *testing.T) {
	vec := []Candidate{{BookID: "a", VectorScore: 0.8}, {BookID: "b", VectorScore: 0.7}}
	soc := []Candidate{{BookID: "a", SocialScore: 3, FriendNames: []string{"maya"}}, {BookID: "c"}}
	tr := []Candidate{{BookID: "b", Source: SourceTrending, IsTrending: true}}

	got := mergeCandidatePools(2, vec, soc, tr)
	if len(got) != 2 {
		t.Fatalf("got %d, want cap of 2", len(got))
	}
	if got[0].VectorScore != 0.8 || got[0].SocialScore != 3 || got[0].FriendNames[0] != "maya" {
		t.Errorf("a lost evidence on merge: %+v", got[0])
	}
	if !got[1].IsTrending || got[1].Source != SourceVector && got[1].Source != "" {
		t.Errorf("b should keep its first source and pick up trending: %+v", got[1])
	}
}

func TestQualityScoreDiscountsThinEvidence(t *testing.T) {
	if q := qualityScore(3.0, 100); q != 0 {
		t.Errorf("3.0 should be neutral, got %v", q)
	}
	if q := qualityScore(5.0, 100); q != 1 {
		t.Errorf("5.0 with plenty of ratings should be 1, got %v", q)
	}
	if strong, thin := qualityScore(4.5, 100), qualityScore(4.5, 2); thin >= strong {
		t.Errorf("2 ratings (%v) should count for less than 100 (%v)", thin, strong)
	}
}
