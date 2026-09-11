package service

import (
	"strings"
	"testing"
)

// The previous diversity pass measured "similarity" as 1-|scoreA-scoreB|, which
// describes two numbers rather than two books — under it the highest-scoring
// candidates always looked most alike, so it penalised the best picks and never
// diversified content. These pin the replacement's actual behaviour.

func cand(id string, score float32, author, category string) Candidate {
	return Candidate{
		BookID:     id,
		Title:      id,
		FinalScore: score,
		Authors:    []string{author},
		Categories: []string{category},
	}
}

func ids(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.BookID
	}
	return out
}

func TestDiversifyCapsOneAuthorDominating(t *testing.T) {
	pool := []Candidate{
		cand("a1", 0.99, "Same Author", "Fiction"),
		cand("a2", 0.98, "Same Author", "Fiction"),
		cand("a3", 0.97, "Same Author", "Fiction"),
		cand("a4", 0.96, "Same Author", "Fiction"),
		cand("b1", 0.50, "Other Author", "Fiction"),
		cand("c1", 0.40, "Third Author", "Fiction"),
	}
	got := diversify(pool, 4, diversityLambda)

	var sameAuthor int
	for _, c := range got {
		if c.Authors[0] == "Same Author" {
			sameAuthor++
		}
	}
	if sameAuthor > maxPerAuthor {
		t.Errorf("got %d books by one author in %v, cap is %d", sameAuthor, ids(got), maxPerAuthor)
	}
	if len(got) != 4 {
		t.Errorf("got %d results, want 4", len(got))
	}
}

// A pool that is genuinely all one author must still fill the page. Returning
// three books because the cap could not be satisfied is worse than bending it.
func TestDiversifyFillsPageEvenWhenCapsCannotBeMet(t *testing.T) {
	pool := []Candidate{
		cand("a1", 0.9, "Only Author", "Fiction"),
		cand("a2", 0.8, "Only Author", "Fiction"),
		cand("a3", 0.7, "Only Author", "Fiction"),
		cand("a4", 0.6, "Only Author", "Fiction"),
	}
	if got := diversify(pool, 4, diversityLambda); len(got) != 4 {
		t.Errorf("got %d results from a 4-book pool, want 4", len(got))
	}
}

// The top of the page still has to be the best book, or the whole rail reads
// as noise however varied it is.
func TestDiversifyKeepsTheBestPickFirst(t *testing.T) {
	pool := []Candidate{
		cand("best", 0.99, "A", "Fiction"),
		cand("mid", 0.60, "B", "History"),
		cand("low", 0.30, "C", "Poetry"),
	}
	got := diversify(pool, 2, diversityLambda)
	if got[0].BookID != "best" {
		t.Errorf("first pick = %q, want the highest-scoring book", got[0].BookID)
	}
}

func TestDiversifyReturnsEverythingWhenPoolIsSmall(t *testing.T) {
	pool := []Candidate{cand("a", 0.9, "A", "F"), cand("b", 0.8, "B", "H")}
	if got := diversify(pool, 5, diversityLambda); len(got) != 2 {
		t.Errorf("got %d, want the whole 2-book pool", len(got))
	}
}

// Embedding cosine is the only measure that notices two different authors
// writing the same book; the categorical fallback must still work without it.
func TestCandidateSimilarityUsesEmbeddingsThenFallsBack(t *testing.T) {
	a := cand("a", 0.9, "X", "Fiction")
	b := cand("b", 0.9, "Y", "History")
	a.Embedding = []float32{1, 0, 0}
	b.Embedding = []float32{1, 0, 0}
	if sim := candidateSimilarity(a, b); sim < 0.99 {
		t.Errorf("identical embeddings gave similarity %v, want ~1", sim)
	}

	c := cand("c", 0.9, "X", "Fiction")
	d := cand("d", 0.9, "X", "Fiction")
	if sim := candidateSimilarity(c, d); sim < 0.99 {
		t.Errorf("identical author+category gave %v, want ~1 from the fallback", sim)
	}

	e := cand("e", 0.9, "X", "Fiction")
	f := cand("f", 0.9, "Y", "Poetry")
	if sim := candidateSimilarity(e, f); sim != 0 {
		t.Errorf("disjoint author+category gave %v, want 0", sim)
	}
}

// Mismatched embedding lengths must fall back rather than silently scoring 0
// similarity, which would read as "maximally different".
func TestCandidateSimilarityFallsBackOnDimensionMismatch(t *testing.T) {
	a := cand("a", 0.9, "X", "Fiction")
	b := cand("b", 0.9, "X", "Fiction")
	a.Embedding = []float32{1, 0, 0}
	b.Embedding = []float32{1, 0}
	if sim := candidateSimilarity(a, b); sim < 0.99 {
		t.Errorf("got %v; want the categorical fallback to report these as alike", sim)
	}
}

func TestExplorationSlotsIsTwentyPercent(t *testing.T) {
	if got := ExplorationSlots(20); got != 4 {
		t.Errorf("ExplorationSlots(20) = %d, want 4 (20%%)", got)
	}
	if got := ExplorationSlots(10); got != 2 {
		t.Errorf("ExplorationSlots(10) = %d, want 2", got)
	}
	// A page with zero exploration can only ever confirm what is already known.
	if got := ExplorationSlots(3); got < 1 {
		t.Errorf("ExplorationSlots(3) = %d, want at least 1", got)
	}
	if got := ExplorationSlots(2); got != 0 {
		t.Errorf("ExplorationSlots(2) = %d, want 0 — too short to spend a slot", got)
	}
}

// Appending exploration to the end of a 20-item list puts it below the fold,
// which is functionally the same as not doing it.
func TestInterleaveExplorationPlacesPicksInsideTheVisibleList(t *testing.T) {
	ranked := make([]Candidate, 20)
	for i := range ranked {
		ranked[i] = cand(string(rune('a'+i)), float32(20-i)/20, "A", "F")
	}
	explore := []Candidate{
		cand("x1", 0.1, "Z", "Poetry"),
		cand("x2", 0.1, "Z", "Poetry"),
		cand("x3", 0.1, "Z", "Poetry"),
		cand("x4", 0.1, "Z", "Poetry"),
	}

	got := InterleaveExploration(ranked, explore, ExplorationSlots(len(ranked)))
	if len(got) != len(ranked) {
		t.Fatalf("length changed: got %d want %d", len(got), len(ranked))
	}

	var firstExploreIdx = -1
	var count int
	for i, c := range got {
		if strings.HasPrefix(c.BookID, "x") {
			count++
			if firstExploreIdx == -1 {
				firstExploreIdx = i
			}
		}
	}
	if count != 4 {
		t.Errorf("placed %d exploration picks, want 4", count)
	}
	if firstExploreIdx == 0 {
		t.Error("exploration took the first slot; the top of the page has to earn trust first")
	}
	if firstExploreIdx > 12 {
		t.Errorf("first exploration pick at index %d — below the fold on a phone", firstExploreIdx)
	}
}

func TestInterleaveExplorationNoOpWithoutPicks(t *testing.T) {
	ranked := []Candidate{cand("a", 0.9, "A", "F")}
	if got := InterleaveExploration(ranked, nil, 4); len(got) != 1 || got[0].BookID != "a" {
		t.Errorf("got %v, want the original list unchanged", ids(got))
	}
}

// Obscurity alone is not a virtue: without the rating floor, "hidden gem"
// would mean "a book nobody read, for a reason".
func TestHiddenGemRequiresQualityNotJustObscurity(t *testing.T) {
	good := Candidate{TotalReads: 5, LikeCount: 1, VectorScore: 0.8}
	if !IsHiddenGem(good, 4.4, 30) {
		t.Error("a well-rated, obscure, well-matched book was not a hidden gem")
	}
	if IsHiddenGem(good, 2.9, 30) {
		t.Error("a poorly-rated obscure book was called a hidden gem")
	}
	if IsHiddenGem(good, 4.4, 2) {
		t.Error("a book with 2 ratings was called a hidden gem; too little evidence")
	}

	popular := Candidate{TotalReads: 500, LikeCount: 100, VectorScore: 0.9}
	if IsHiddenGem(popular, 4.6, 900) {
		t.Error("a widely-read book was called hidden")
	}

	weakMatch := Candidate{TotalReads: 3, VectorScore: 0.2}
	if IsHiddenGem(weakMatch, 4.5, 40) {
		t.Error("an obscure book that does not match the reader was called a gem for them")
	}
}

// Confidence must reflect how much is known about the reader, not only how
// well the book scored — otherwise a lucky match against an empty profile is
// announced with the same certainty as one backed by two hundred ratings.
func TestConfidenceRisesWithEvidenceNotJustScore(t *testing.T) {
	c := Candidate{FinalScore: 0.8, VectorScore: 0.8}

	thin := &UserSignalProfile{}
	rich := &UserSignalProfile{
		GenreWeights:        map[string]float64{"Fiction": 1},
		AuthorWeights:       map[string]float64{"X": 1},
		DiaryEmbedding:      []float32{1},
		FastFinishEmbedding: []float32{1},
		Traits: &TraitProfile{
			Prefs:      map[string]float64{"pacing": 0.8},
			Confidence: map[string]float64{"pacing": 1},
		},
	}

	if RecConfidence(c, thin) >= RecConfidence(c, rich) {
		t.Errorf("same score scored no less confident against an empty profile: thin=%v rich=%v",
			RecConfidence(c, thin), RecConfidence(c, rich))
	}
}

// "Matches your taste, and also looks like what you rejected" is uncertainty.
func TestConfidenceCappedByKnownClash(t *testing.T) {
	rich := &UserSignalProfile{
		GenreWeights:        map[string]float64{"Fiction": 1},
		AuthorWeights:       map[string]float64{"X": 1},
		DiaryEmbedding:      []float32{1},
		FastFinishEmbedding: []float32{1},
		Traits: &TraitProfile{
			Prefs:      map[string]float64{"pacing": 0.8},
			Confidence: map[string]float64{"pacing": 1},
		},
	}
	clean := Candidate{FinalScore: 0.95, VectorScore: 0.9, SocialScore: 2, HasTraitFit: true, TraitFitScore: 0.9}
	clashing := clean
	clashing.TraitClashScore = 0.8

	if RecConfidence(clashing, rich) > ConfidenceMedium {
		t.Error("a candidate clashing with known dislikes kept high confidence")
	}
	if RecConfidence(clean, rich) <= RecConfidence(clashing, rich) {
		t.Error("the clash did not reduce confidence at all")
	}
}

func TestConfidenceLabelsAreOrderedAndSilentWhenUnsure(t *testing.T) {
	if ConfidenceLabel(0.97) != "This feels very you" {
		t.Errorf("0.97 -> %q", ConfidenceLabel(0.97))
	}
	if ConfidenceLabel(0.86) != "I think you'll love this" {
		t.Errorf("0.86 -> %q", ConfidenceLabel(0.86))
	}
	if ConfidenceLabel(0.72) != "You might be surprised by this one" {
		t.Errorf("0.72 -> %q", ConfidenceLabel(0.72))
	}
	if ConfidenceLabel(0.55) != "Wild card" {
		t.Errorf("0.55 -> %q", ConfidenceLabel(0.55))
	}
	// Below the wild-card floor the honest thing is to claim nothing.
	if ConfidenceLabel(0.2) != "" {
		t.Errorf("0.2 -> %q, want no claim at all", ConfidenceLabel(0.2))
	}
}
