package service

import (
	"math"
	"strings"
)

// Human phrasing for the trait axes.
//
// The roadmap's benchmark line is "You tend to love quiet, character-driven
// stories with understated emotional arcs" — a sentence about the *reader*,
// not about the book. That only works if the phrase comes from an axis the
// reader demonstrably has an opinion on AND the book actually sits there, so
// everything below is gated on both. A reason that is merely plausible is the
// failure mode the "never fabricate reasons" rule exists to prevent: it costs
// more trust when it is wrong than it earns when it is right.

// traitPhrases maps an axis to how to describe a high and a low preference.
var traitPhrases = map[string][2]string{
	// [low, high]
	"character_driven":     {"stories with real forward momentum", "quiet, character-driven stories"},
	"emotional_intensity":  {"books that stay light on their feet", "books that put you through something"},
	"plot_intensity":       {"books where very little happens, beautifully", "tightly plotted stories"},
	"pacing":               {"slow-burn books you live inside for a while", "books you can tear through"},
	"prose_density":        {"clean, unfussy writing", "prose you want to reread"},
	"narrative_complexity": {"a single clear thread", "structurally ambitious novels"},
	"darkness":             {"warm, comforting books", "bleak, unsparing books"},
	"romance_centrality":   {"stories where romance stays off to the side", "romance-led stories"},
	"worldbuilding":        {"stories rooted in the world as it is", "deeply built invented worlds"},
}

// traitReasonMinStrength is how far from neutral, after confidence weighting, a
// preference has to be before it is worth saying out loud. Below this the
// sentence would be technically true and useless ("you tend to love books that
// are slightly more character-driven than average").
const traitReasonMinStrength = 0.30

// traitReasonMaxDistance is how close the book has to sit to the reader's
// preference on that axis for the claim to be honest.
const traitReasonMaxDistance = 0.25

// matchedTraitPhrases returns up to n phrases for axes the reader has a
// confident opinion on AND this book actually sits on, strongest first.
// Empty when the evidence is thin, so callers stay silent rather than vague.
func matchedTraitPhrases(c Candidate, p *TraitProfile, n int) []string {
	if p == nil || c.Traits == nil {
		return nil
	}
	matched := make([]string, 0, n)
	for _, t := range StrongestTraits(p, len(TraitAxes)) {
		if t.Strength < traitReasonMinStrength {
			break // StrongestTraits is ordered, so nothing after this qualifies
		}
		v, ok := c.Traits[t.Axis]
		if !ok || math.Abs(v-t.Value) > traitReasonMaxDistance {
			continue
		}
		phrases, ok := traitPhrases[t.Axis]
		if !ok {
			continue
		}
		idx := 0
		if t.Value > 0.5 {
			idx = 1
		}
		matched = append(matched, phrases[idx])
		if len(matched) == n {
			break
		}
	}
	return matched
}

// buildTraitReason returns a "you tend to love X" line for a candidate, or ""
// when no axis both matters to the reader and matches this book.
//
// Uses at most two axes: one is thin, three reads like a horoscope.
func buildTraitReason(c Candidate, p *TraitProfile) string {
	matched := matchedTraitPhrases(c, p, 2)
	switch len(matched) {
	case 0:
		return ""
	case 1:
		return "You tend to love " + matched[0]
	default:
		return "You tend to love " + matched[0] + ", and this is one"
	}
}

// buildExploreTraitReason explains a deliberate stretch pick.
//
// The roadmap is explicit that exploration should not read as a random draw:
// "You don't usually read historical fiction, but you consistently love
// intimate character-driven stories, and this one strongly matches that side of
// your taste." That requires naming the axis that *does* match, which is only
// possible because the axes are separate from genre.
func buildExploreTraitReason(c Candidate, p *TraitProfile) string {
	m := matchedTraitPhrases(c, p, 1)
	if len(m) != 1 {
		return ""
	}
	// Exploration candidates come from genres the reader has never shelved,
	// so the first category is honestly "not your usual".
	if len(c.Categories) > 0 && c.Categories[0] != "" {
		return "You don't usually read " + c.Categories[0] + ", but you love " + m[0]
	}
	return "Not your usual thing, but you love " + m[0]
}

// DescribeTaste renders a reader's strongest axes as short human lines, for the
// taste dashboard and for Jazy's context. Returns nil when nothing is confident
// enough to claim.
func DescribeTaste(p *TraitProfile, n int) []string {
	out := make([]string, 0, n)
	for _, t := range StrongestTraits(p, n) {
		if t.Strength < traitReasonMinStrength {
			break
		}
		phrases, ok := traitPhrases[t.Axis]
		if !ok {
			continue
		}
		idx := 0
		if t.Value > 0.5 {
			idx = 1
		}
		out = append(out, strings.ToUpper(phrases[idx][:1])+phrases[idx][1:])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildRecentReason explains a pick by the reader's recent drift when that is
// the stronger story. "You've been reading darker books lately, and this is
// one" only when the axis has actually moved — otherwise the long-term line
// is the honest one and this returns "".
func buildRecentReason(c Candidate, longTerm, recent *TraitProfile) string {
	if recent == nil || longTerm == nil || c.Traits == nil {
		return ""
	}
	for _, t := range StrongestTraits(recent, len(TraitAxes)) {
		if t.Strength < traitReasonMinStrength {
			break
		}
		lt, ok := longTerm.Prefs[t.Axis]
		if !ok || math.Abs(t.Value-lt) < 0.2 {
			continue // not a drift, just taste
		}
		v, ok := c.Traits[t.Axis]
		if !ok || math.Abs(v-t.Value) > traitReasonMaxDistance {
			continue
		}
		phrases, ok := traitPhrases[t.Axis]
		if !ok {
			continue
		}
		idx := 0
		if t.Value > 0.5 {
			idx = 1
		}
		return "You've been drawn to " + phrases[idx] + " lately"
	}
	return ""
}
