package service

import (
	"math"
	"sort"
	"strings"

	"github.com/hridyesh/paperboxd-backend/internal/util"
)

// Confidence, diversity and hidden gems.
//
// Three things the pipeline could not previously express:
//
//   - How sure it is. FinalScore is a blend, not a probability: 0.7 from a
//     reader with 200 rated books and 0.7 from one with three mean completely
//     different things, and showing both with the same conviction is how a
//     recommender loses trust on the second one.
//   - Whether a set of twenty books is actually varied. The previous MMR
//     measured "similarity" as 1-|scoreA-scoreB|, which is a statement about
//     two numbers rather than two books — the highest-scoring candidates always
//     look most similar under it, so it penalised the best picks and never
//     diversified content at all.
//   - Obscurity. Nothing distinguished a book everyone has read from one almost
//     nobody has, so "a hidden gem that feels unusually you" was unsayable.

// ── Confidence ────────────────────────────────────────────────────────────────

// Confidence tiers, as fractions of the maximum.
const (
	ConfidenceVeryHigh = 0.95
	ConfidenceHigh     = 0.85
	ConfidenceMedium   = 0.70
	ConfidenceLow      = 0.50
)

// RecConfidence estimates how much evidence stands behind a recommendation.
//
// Deliberately not the score itself. Score answers "how well does this match
// what we believe about you"; confidence answers "how much do we actually
// believe about you". A perfect match against an almost-empty profile is a
// confident-looking guess, and the roadmap's rule is that a wild card should
// be labelled as one rather than dressed up.
func RecConfidence(c Candidate, profile *UserSignalProfile) float64 {
	if profile == nil {
		return float64(c.FinalScore) * 0.5
	}

	// Evidence: how many independent signals contributed anything at all.
	// Agreement across several weak signals beats one strong one.
	signals := 0.0
	present := 0.0
	for _, has := range []bool{
		len(profile.GenreWeights) > 0,
		len(profile.AuthorWeights) > 0,
		profile.ThoughtEmbedding != nil,
		profile.FastFinishEmbedding != nil,
		profile.Traits.HasSignal(),
	} {
		present++
		if has {
			signals++
		}
	}
	breadth := signals / present

	// Corroboration from this specific candidate rather than from the profile.
	corroboration := 0.0
	if c.SocialScore > 0 || c.TwinCount > 0 {
		corroboration += 0.4
	}
	if c.AnchorKind == AnchorRated5 || c.AnchorKind == AnchorLoved {
		corroboration += 0.2
	}
	if c.HasTraitFit && c.TraitFitScore > 0.7 {
		corroboration += 0.4
	}
	if c.VectorScore > 0.7 {
		corroboration += 0.2
	}
	corroboration = math.Min(corroboration, 1.0)

	// A known clash caps confidence no matter how well everything else scores:
	// the honest reading of "matches your taste and also looks like what you
	// rejected" is uncertainty, not enthusiasm.
	conf := float64(c.FinalScore)*0.5 + breadth*0.25 + corroboration*0.25
	if c.TraitClashScore > 0.6 {
		conf = math.Min(conf, ConfidenceMedium)
	}
	return math.Max(0, math.Min(1, conf))
}

// ConfidenceLabel translates a confidence into the sentence a reader sees.
//
// The number itself is never exposed. "81% fit" invites arithmetic the engine
// cannot back up; "this feels very you" makes a claim it either earns or does
// not.
func ConfidenceLabel(conf float64) string {
	switch {
	case conf >= ConfidenceVeryHigh:
		return "This feels very you"
	case conf >= ConfidenceHigh:
		return "I think you'll love this"
	case conf >= ConfidenceMedium:
		return "You might be surprised by this one"
	case conf >= ConfidenceLow:
		return "Wild card"
	default:
		return ""
	}
}

// ── Diversity ─────────────────────────────────────────────────────────────────

// Caps on how much of one thing a single page of recommendations may contain.
// A reader who loves literary fiction should not be shown twenty literary
// novels by six authors — relevance alone converges on that, which is the
// echo chamber the roadmap warns about.
const (
	maxPerAuthor   = 2
	maxPerCategory = 4
	// Phase 17 axes beyond genre and author. Caps are per page of 20; each
	// stops one shape of book from being the whole page, none forces any in.
	maxPerLength    = 10 // short (≤250) / mid / long (≥450)
	maxPopular      = 8  // TotalReads ≥ popularShelfCount
	maxKnownAuthors = 8  // authors already on the reader's shelf
)

// lengthBucket groups a page count for the length cap. Unknown counts are
// their own bucket so they are not mistaken for short.
func lengthBucket(pages int) string {
	switch {
	case pages <= 0:
		return "unknown"
	case pages <= 250:
		return "short"
	case pages >= 450:
		return "long"
	default:
		return "mid"
	}
}

// candidateSimilarity measures how alike two books are.
//
// Embedding cosine when both have one, because that is the only measure that
// notices two different authors writing the same book. Otherwise a Jaccard
// overlap of categories and authors, which is coarse but at least describes the
// books rather than their scores.
func candidateSimilarity(a, b Candidate) float64 {
	if len(a.Embedding) > 0 && len(a.Embedding) == len(b.Embedding) {
		sim := util.CosineSimilarity(a.Embedding, b.Embedding)
		// Cosine over text embeddings rarely goes negative and negative values
		// are not "extra different" in any useful sense.
		return math.Max(0, sim)
	}

	tokens := func(c Candidate) map[string]bool {
		m := make(map[string]bool, len(c.Categories)+len(c.Authors))
		for _, x := range c.Categories {
			m["c:"+strings.ToLower(x)] = true
		}
		for _, x := range c.Authors {
			m["a:"+strings.ToLower(x)] = true
		}
		return m
	}
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	var inter int
	for k := range ta {
		if tb[k] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// diversify selects k candidates trading relevance against variety.
//
// lambda is the weight on relevance: 1.0 is pure ranking, 0.0 pure novelty.
// 0.72 keeps the first few picks essentially in score order — the top of the
// list has to be the best books or the whole page reads as noise — while
// letting the tail spread out. knownAuthors (lower-cased) are the authors
// already on the reader's shelf; nil when unknown.
func diversify(candidates []Candidate, k int, lambda float64, knownAuthors map[string]bool) []Candidate {
	if len(candidates) <= k {
		return candidates
	}

	selected := make([]Candidate, 0, k)
	remaining := make([]Candidate, len(candidates))
	copy(remaining, candidates)

	authorCount := map[string]int{}
	categoryCount := map[string]int{}
	lengthCount := map[string]int{}
	popular, known := 0, 0

	isKnown := func(c Candidate) bool {
		for _, a := range c.Authors {
			if knownAuthors[strings.ToLower(a)] {
				return true
			}
		}
		return false
	}
	isPopular := func(c Candidate) bool { return c.TotalReads >= popularShelfCount }

	overCap := func(c Candidate) bool {
		for _, a := range c.Authors {
			if authorCount[strings.ToLower(a)] >= maxPerAuthor {
				return true
			}
		}
		if len(c.Categories) > 0 {
			if categoryCount[strings.ToLower(c.Categories[0])] >= maxPerCategory {
				return true
			}
		}
		if lengthCount[lengthBucket(c.PageCount)] >= maxPerLength {
			return true
		}
		if isPopular(c) && popular >= maxPopular {
			return true
		}
		if isKnown(c) && known >= maxKnownAuthors {
			return true
		}
		return false
	}

	take := func(idx int) {
		c := remaining[idx]
		selected = append(selected, c)
		for _, a := range c.Authors {
			authorCount[strings.ToLower(a)]++
		}
		if len(c.Categories) > 0 {
			categoryCount[strings.ToLower(c.Categories[0])]++
		}
		lengthCount[lengthBucket(c.PageCount)]++
		if isPopular(c) {
			popular++
		}
		if isKnown(c) {
			known++
		}
		remaining = append(remaining[:idx], remaining[idx+1:]...)
	}

	for len(selected) < k && len(remaining) > 0 {
		bestIdx, bestScore := -1, math.Inf(-1)
		// Fallback tracks the best candidate ignoring caps, so a pool that is
		// entirely one author still fills the page rather than returning short.
		fallbackIdx, fallbackScore := 0, math.Inf(-1)

		for i, c := range remaining {
			maxSim := 0.0
			for _, sel := range selected {
				if sim := candidateSimilarity(sel, c); sim > maxSim {
					maxSim = sim
				}
			}
			score := lambda*float64(c.FinalScore) - (1-lambda)*maxSim

			if score > fallbackScore {
				fallbackScore, fallbackIdx = score, i
			}
			if overCap(c) {
				continue
			}
			if score > bestScore {
				bestScore, bestIdx = score, i
			}
		}

		if bestIdx == -1 {
			bestIdx = fallbackIdx
		}
		take(bestIdx)
	}

	return selected
}

// ── Hidden gems ───────────────────────────────────────────────────────────────

// A hidden gem is high personal fit + low awareness + decent community signal.
// Awareness is measured against the corpus rather than in absolute terms, so
// the definition survives the catalogue growing.
const (
	hiddenGemMaxReads  = 25
	hiddenGemMinRating = 3.8
	hiddenGemMinFit    = 0.55
)

// IsHiddenGem reports whether a candidate qualifies, given its global counts.
//
// The rating floor is what separates "a hidden gem" from "a book nobody read
// for a reason". Without it obscurity alone would be treated as a virtue.
// Awareness is checked twice: Paperboxd shelves (live) and external ratings
// count, because while the community is small every book clears the first.
func IsHiddenGem(c Candidate, averageRating float64, ratingsCount int) bool {
	if c.TotalReads > hiddenGemMaxReads || ratingsCount > hiddenGemMaxGlobalRatings {
		return false
	}
	if averageRating < hiddenGemMinRating || ratingsCount < 5 {
		return false
	}
	if c.HasTraitFit {
		return c.TraitFitScore >= hiddenGemMinFit
	}
	return float64(c.VectorScore) >= 0.6
}

// ── Exploration ───────────────────────────────────────────────────────────────

// explorationFraction is the roadmap's 80/20 split. The previous pipeline gave
// exploration 2 of 20 slots; at 10% a reader can go a week without seeing
// anything outside their existing shape, which is the definition of the echo
// chamber the split exists to prevent.
const explorationFraction = 0.20

// ExplorationSlots returns how many of n slots should be exploratory.
// Always at least one when there is room: a page with zero exploration is a
// page that can only ever confirm what is already known.
func ExplorationSlots(n int) int {
	if n <= 2 {
		return 0
	}
	slots := int(math.Round(float64(n) * explorationFraction))
	if slots < 1 {
		slots = 1
	}
	return slots
}

// InterleaveExploration places exploratory picks through the list instead of
// appending them.
//
// Position carries meaning: everything after slot ~12 is below the fold on a
// phone, so appending exploration to the end is functionally the same as not
// doing it. Spacing them out also stops the stretch picks reading as one odd
// clump at the bottom of the page.
func InterleaveExploration(ranked, exploration []Candidate, slots int) []Candidate {
	if len(exploration) == 0 || slots <= 0 {
		return ranked
	}
	if slots > len(exploration) {
		slots = len(exploration)
	}

	out := make([]Candidate, 0, len(ranked))
	total := len(ranked)
	if total == 0 {
		return exploration[:slots]
	}

	// Never the first slot: the top of the page has to earn trust before it
	// spends any.
	step := total / (slots + 1)
	if step < 1 {
		step = 1
	}
	positions := make(map[int]int, slots)
	for i := 0; i < slots; i++ {
		pos := (i + 1) * step
		if pos >= total {
			pos = total - 1
		}
		positions[pos] = i
	}

	used := 0
	for i, c := range ranked {
		if idx, ok := positions[i]; ok && idx < len(exploration) {
			out = append(out, exploration[idx])
			used++
			continue
		}
		out = append(out, c)
	}
	_ = used
	return out
}

// sortByScore orders candidates by final score, highest first.
func sortByScore(candidates []Candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].FinalScore > candidates[j].FinalScore
	})
}
