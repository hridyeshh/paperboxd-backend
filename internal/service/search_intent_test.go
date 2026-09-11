package service

import "testing"

// The parser is where a search silently becomes a different search. Every
// case below is a query a reader actually types, and each asserts the one
// thing that would go wrong if the pattern regressed.

func TestParseQueryPageConstraints(t *testing.T) {
	cases := map[string]struct{ max, min int }{
		"books about grief under 300 pages": {300, 0},
		"something less than 250 pp":        {250, 0},
		"at least 500 pages of fantasy":     {0, 500},
		"a short book about the sea":        {250, 0},
		"a long epic":                       {0, 450},
		"books about grief":                 {0, 0},
	}
	for q, want := range cases {
		got := ParseQuery(q).Constraints
		if got.MaxPages != want.max || got.MinPages != want.min {
			t.Errorf("%q: max=%d min=%d, want max=%d min=%d", q, got.MaxPages, got.MinPages, want.max, want.min)
		}
	}
}

// The constraint phrase must leave the embed text: "under 300 pages" pulls the
// vector toward blurbs that mention page counts, which is nothing.
func TestParseQueryStripsConstraintsFromEmbedText(t *testing.T) {
	pq := ParseQuery("books about grief under 300 pages, no romance")
	if pq.EmbedText != "books about grief" {
		t.Errorf("EmbedText = %q, want %q", pq.EmbedText, "books about grief")
	}
}

func TestParseQueryAllConstraintsStillEmbedsSomething(t *testing.T) {
	pq := ParseQuery("under 300 pages")
	if pq.EmbedText == "" {
		t.Error("EmbedText empty; the vector would be about nothing")
	}
}

func TestParseQuerySimilarity(t *testing.T) {
	cases := map[string]string{
		"something like Sally Rooney":     "Sally Rooney",
		"books like Murakami but shorter": "Murakami",
		"in the vein of Piranesi":         "Piranesi",
		"more Ishiguro, under 300 pages":  "Ishiguro",
		"books about grief":               "",
		"something more emotional":        "", // an axis word, not a target
	}
	for q, want := range cases {
		got := ParseQuery(q).Constraints.SimilarTo
		if got != want {
			t.Errorf("%q: SimilarTo = %q, want %q", q, got, want)
		}
	}
}

func TestParseQueryExclusionsBecomeAxisCeilings(t *testing.T) {
	pq := ParseQuery("sad but hopeful, no romance, no fantasy")
	if v, ok := pq.Constraints.ExcludeAxes["romance_centrality"]; !ok || v > 0.5 {
		t.Errorf("no romance → romance_centrality ceiling = %v, %v", v, ok)
	}
	if v, ok := pq.Constraints.ExcludeAxes["worldbuilding"]; !ok || v > 0.5 {
		t.Errorf("no fantasy → worldbuilding ceiling = %v, %v", v, ok)
	}
	if pq.Constraints.PreferAxes["romance_centrality"] > 0.5 {
		t.Error("an exclusion should also keep the axis low at ranking time")
	}
}

// "like X but no romance" must not swallow "no romance" into X.
func TestParseQueryExclusionDoesNotPolluteSimilarTo(t *testing.T) {
	pq := ParseQuery("something like Sally Rooney without romance")
	if pq.Constraints.SimilarTo != "Sally Rooney" {
		t.Errorf("SimilarTo = %q, want %q", pq.Constraints.SimilarTo, "Sally Rooney")
	}
	if _, ok := pq.Constraints.ExcludeAxes["romance_centrality"]; !ok {
		t.Error("exclusion was lost")
	}
}

func TestParseQueryAxisWords(t *testing.T) {
	pq := ParseQuery("something fast-paced and dark")
	if pq.Constraints.PreferAxes["pacing"] < 0.8 {
		t.Errorf("fast-paced → pacing = %v", pq.Constraints.PreferAxes["pacing"])
	}
	if pq.Constraints.PreferAxes["darkness"] < 0.8 {
		t.Errorf("dark → darkness = %v", pq.Constraints.PreferAxes["darkness"])
	}
	if pq.Intent != IntentMood {
		t.Errorf("intent = %q, want mood", pq.Intent)
	}
}

func TestParseQueryEmotionalIntent(t *testing.T) {
	pq := ParseQuery("I want something that will destroy me")
	if pq.Intent != IntentEmotional {
		t.Errorf("intent = %q, want emotional", pq.Intent)
	}
	if pq.Constraints.PreferAxes["emotional_intensity"] < 0.9 {
		t.Errorf("emotional_intensity = %v, want ~0.95", pq.Constraints.PreferAxes["emotional_intensity"])
	}
}

func TestParseQueryContext(t *testing.T) {
	cases := map[string]string{
		"something for my 10-hour flight tomorrow": "travel",
		"I need something for book club":           "book_club",
		"I haven't read in months":                 "returning",
		"I just finished a devastating book":       "recovery",
		"I want to challenge myself":               "stretch",
	}
	for q, want := range cases {
		pq := ParseQuery(q)
		if pq.Constraints.Context != want {
			t.Errorf("%q: context = %q, want %q", q, pq.Constraints.Context, want)
		}
		if pq.Intent != IntentContext && pq.Intent != IntentMulti {
			t.Errorf("%q: intent = %q, want context", q, pq.Intent)
		}
	}
}

// An explicit axis word must beat a context default: "a light read for my
// flight" is light, not the travel default of pacey-and-neutral.
func TestParseQueryExplicitWordBeatsContextDefault(t *testing.T) {
	pq := ParseQuery("a light read for my flight")
	if pq.Constraints.PreferAxes["emotional_intensity"] > 0.3 {
		t.Errorf("emotional_intensity = %v; 'light' should have won", pq.Constraints.PreferAxes["emotional_intensity"])
	}
}

func TestParseQueryIntentClassification(t *testing.T) {
	cases := map[string]SearchIntent{
		"books about grief":                            IntentTopic,
		"something like Sally Rooney":                  IntentSimilarity,
		"something comforting":                         IntentMood,
		"under 250 pages":                              IntentConstraint,
		"something for my flight":                      IntentContext,
		"something that will destroy me":               IntentEmotional,
		"sad but hopeful, under 300 pages, no romance": IntentEmotional, // constraint on top of emotional is still emotional
		"like Murakami, something comforting":          IntentMulti,
	}
	for q, want := range cases {
		if got := ParseQuery(q).Intent; got != want {
			t.Errorf("%q: intent = %q, want %q", q, got, want)
		}
	}
}

func TestParseQueryStandalone(t *testing.T) {
	for _, q := range []string{"a standalone fantasy", "fantasy, not a series", "no sequels please"} {
		if !ParseQuery(q).Constraints.Standalone {
			t.Errorf("%q: Standalone = false", q)
		}
	}
}

// The conversation the roadmap describes, turn by turn.
func TestRefinementConversation(t *testing.T) {
	sess := ParseQuery("books like Murakami")
	if sess.Constraints.SimilarTo != "Murakami" {
		t.Fatalf("turn 1: SimilarTo = %q", sess.Constraints.SimilarTo)
	}

	sess, ok := ParseRefinement(sess, "shorter")
	if !ok {
		t.Fatal("turn 2: 'shorter' was not read as a refinement")
	}
	if sess.Constraints.MaxPages == 0 || sess.Constraints.MaxPages > 300 {
		t.Errorf("turn 2: MaxPages = %d, want a real ceiling", sess.Constraints.MaxPages)
	}
	if sess.Constraints.SimilarTo != "Murakami" {
		t.Error("turn 2: lost the similarity target")
	}

	sess, ok = ParseRefinement(sess, "more emotional")
	if !ok {
		t.Fatal("turn 3: 'more emotional' was not read as a refinement")
	}
	if sess.Constraints.PreferAxes["emotional_intensity"] <= 0.5 {
		t.Errorf("turn 3: emotional_intensity = %v, want raised", sess.Constraints.PreferAxes["emotional_intensity"])
	}
	if sess.Constraints.MaxPages == 0 {
		t.Error("turn 3: lost the page ceiling")
	}

	sess, ok = ParseRefinement(sess, "something less weird")
	if !ok {
		t.Fatal("turn 4: 'something less weird' was not read as a refinement")
	}
	if sess.Constraints.PreferAxes["narrative_complexity"] >= 0.5 {
		t.Errorf("turn 4: narrative_complexity = %v, want lowered", sess.Constraints.PreferAxes["narrative_complexity"])
	}
	// Everything from before survives.
	if sess.Constraints.SimilarTo != "Murakami" || sess.Constraints.MaxPages == 0 ||
		sess.Constraints.PreferAxes["emotional_intensity"] <= 0.5 {
		t.Errorf("turn 4: earlier constraints lost: %+v", sess.Constraints)
	}
}

// "shorter" twice must tighten twice, not reset.
func TestRefinementCompounds(t *testing.T) {
	sess := ParseQuery("books about the sea")
	sess, _ = ParseRefinement(sess, "shorter")
	first := sess.Constraints.MaxPages
	sess, _ = ParseRefinement(sess, "shorter")
	if sess.Constraints.MaxPages >= first {
		t.Errorf("second 'shorter' gave %d, first gave %d; must tighten", sess.Constraints.MaxPages, first)
	}
}

// Refinement must not mutate the previous turn's map in place, or the session
// history becomes unreliable.
func TestRefinementDoesNotMutatePrevious(t *testing.T) {
	prev := ParseQuery("something dark")
	before := prev.Constraints.PreferAxes["darkness"]
	_, _ = ParseRefinement(prev, "lighter")
	if prev.Constraints.PreferAxes["darkness"] != before {
		t.Error("ParseRefinement mutated the previous parse")
	}
}

func TestRefinementRejectsFullQueries(t *testing.T) {
	prev := ParseQuery("books like Murakami")
	if _, ok := ParseRefinement(prev, "books about grief"); ok {
		t.Error("a fresh topic was read as a refinement")
	}
}

// A constraints-only follow-up ("under 300 pages, no romance") inherits the
// previous topic; a new topic replaces it.
func TestMergeInheritsTopicForConstraintOnlyFollowUps(t *testing.T) {
	prev := ParseQuery("books about grief")
	next := ParseQuery("under 300 pages, no romance")
	if !isRefinementOnly(next) {
		t.Fatal("constraint-only follow-up not recognised")
	}
	merged := prev.Merge(next)
	if merged.EmbedText != "books about grief" {
		t.Errorf("EmbedText = %q, want the previous topic kept", merged.EmbedText)
	}
	if merged.Constraints.MaxPages != 300 {
		t.Errorf("MaxPages = %d, want 300", merged.Constraints.MaxPages)
	}

	fresh := ParseQuery("something like Ishiguro")
	merged = prev.Merge(fresh)
	if merged.Constraints.SimilarTo != "Ishiguro" {
		t.Error("a new similarity target did not replace the search")
	}
}

func TestSearchSessionDescribe(t *testing.T) {
	sess := &SearchSession{Current: ParseQuery("books like Murakami under 300 pages")}
	got := sess.Describe()
	for _, want := range []string{"like Murakami", "under 300 pages"} {
		if !contains(got, want) {
			t.Errorf("Describe() = %q, want it to mention %q", got, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
