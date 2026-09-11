package service

import (
	"regexp"
	"strconv"
	"strings"
)

// Search intent and constraints.
//
// "Search should retrieve. Taste should rank." — but before either, the query
// has to be read. "something like Sally Rooney, under 300 pages, no romance"
// is three different instructions: a similarity target, a hard filter and an
// axis exclusion. Feeding the whole string to the embedder gives a vector that
// is vaguely near all three and precisely at none of them, and the page-count
// constraint in particular is invisible to a vector search entirely.
//
// This is a deterministic parser rather than an LLM call. It runs on every
// keystroke-debounced query, it has to be fast, and the patterns readers use
// for constraints are a small, closed set. The LLM's job (R5) is the
// open-ended half: turning "something that will destroy me" into axes.

// SearchIntent is the coarse type of a query.
type SearchIntent string

const (
	IntentTopic      SearchIntent = "topic"      // "books about grief"
	IntentSimilarity SearchIntent = "similarity" // "something like Sally Rooney"
	IntentMood       SearchIntent = "mood"       // "something comforting"
	IntentConstraint SearchIntent = "constraint" // "under 250 pages"
	IntentContext    SearchIntent = "context"    // "something for my flight"
	IntentEmotional  SearchIntent = "emotional"  // "something that will destroy me"
	IntentMulti      SearchIntent = "multi"      // two or more of the above
)

// SearchConstraints are the hard and soft filters extracted from a query.
type SearchConstraints struct {
	MaxPages int `json:"max_pages,omitempty"`
	MinPages int `json:"min_pages,omitempty"`

	// SimilarTo is a name or title the reader wants more of. Left as text
	// because resolving it (author vs. title vs. nothing) needs the database.
	SimilarTo string `json:"similar_to,omitempty"`

	// ExcludeAxes maps a trait axis to a ceiling: "no romance" becomes
	// {romance_centrality: 0.3}. Soft because the extractor is not perfect and
	// a hard cut would remove books where the axis is wrong by one notch.
	ExcludeAxes map[string]float64 `json:"exclude_axes,omitempty"`

	// PreferAxes maps a trait axis to a target value the reader asked for:
	// "fast-paced" -> {pacing: 0.9}, "gentle" -> {darkness: 0.1}.
	PreferAxes map[string]float64 `json:"prefer_axes,omitempty"`

	// Standalone is set when the reader asked for no series.
	Standalone bool `json:"standalone,omitempty"`

	// Context is a situation keyword when the query is about circumstance
	// rather than content: "flight", "book_club", "returning", "recovery".
	Context string `json:"context,omitempty"`
}

// HasHardFilter reports whether any constraint must be applied as a filter
// rather than a ranking nudge.
func (c SearchConstraints) HasHardFilter() bool {
	return c.MaxPages > 0 || c.MinPages > 0 || c.Standalone
}

// ParsedQuery is the reading of a search string.
type ParsedQuery struct {
	Original    string            `json:"original"`
	Intent      SearchIntent      `json:"intent"`
	Constraints SearchConstraints `json:"constraints"`
	// EmbedText is what actually goes to the embedder: the original with the
	// constraint phrases removed, so "under 300 pages" does not pull the vector
	// toward books whose blurbs mention page counts.
	EmbedText string `json:"embed_text"`
}

var (
	rePagesUnder = regexp.MustCompile(`(?i)\b(?:under|less than|fewer than|below|max(?:imum)?|no more than|at most)\s+(\d{2,4})\s*(?:pages|pp|p)?\b`)
	rePagesOver  = regexp.MustCompile(`(?i)\b(?:over|more than|at least|longer than|min(?:imum)?)\s+(\d{2,4})\s*(?:pages|pp|p)?\b`)
	rePagesShort = regexp.MustCompile(`(?i)\b(short|quick|brief|novella|slim)\b`)
	rePagesLong  = regexp.MustCompile(`(?i)\b(long|epic|doorstop|chunky|big)\b`)

	reSimilar = regexp.MustCompile(`(?i)\b(?:like|similar to|in the vein of|reminiscent of|if i liked|if you liked|fans of|more)\s+(.{3,60}?)(?:[,.;]|\s+(?:but|and|with|without|under|over|that)\b|$)`)

	reNo = regexp.MustCompile(`(?i)\b(?:no|without|not|zero|minus|skip the|hold the)\s+(romance|love story|fantasy|worldbuilding|world-building|magic|sci-fi|violence|gore|darkness|sad|series|sequels?)\b`)

	reStandalone = regexp.MustCompile(`(?i)\b(standalone|stand-alone|not a series|no series|single volume|one-off)\b`)
)

// axisWords maps descriptive words to (axis, target). These are the reader's
// own vocabulary for the nine axes; anything not here falls through to the
// embedding, which handles paraphrase far better than a word list can.
//
// Adjectives only, never subject nouns. "sad" is a request about how the book
// should feel; "grief" is a topic the book should be about. The first is an
// axis instruction, the second belongs to the embedder — "books about grief"
// is a topic search and must not be reclassified as an emotional one.
var axisWords = []struct {
	pattern *regexp.Regexp
	axis    string
	target  float64
}{
	{regexp.MustCompile(`(?i)\b(fast[- ]paced|page[- ]turner|propulsive|gripping|unputdownable|tear through|can't put down)\b`), "pacing", 0.9},
	{regexp.MustCompile(`(?i)\b(slow[- ]burn|slow|unhurried|meditative|leisurely|quiet)\b`), "pacing", 0.15},
	{regexp.MustCompile(`(?i)\b(character[- ]driven|interior|introspective|psychological|intimate)\b`), "character_driven", 0.9},
	{regexp.MustCompile(`(?i)\b(plot[- ]driven|twisty|thriller|action[- ]packed|high[- ]stakes)\b`), "plot_intensity", 0.9},
	{regexp.MustCompile(`(?i)\b(devastat\w*|destroy me|wreck me|gut[- ]wrench\w*|heartbreaking|emotional|make me cry|tearjerker|sad|melancholy)\b`), "emotional_intensity", 0.95},
	{regexp.MustCompile(`(?i)\b(light|fun|funny|humou?rous|hilarious|witty|breezy|easy read|palate cleanser|low[- ]stakes)\b`), "emotional_intensity", 0.2},
	{regexp.MustCompile(`(?i)\b(dark|bleak|grim|brutal|disturbing|unsettling)\b`), "darkness", 0.9},
	{regexp.MustCompile(`(?i)\b(cosy|cozy|comforting|warm|gentle|wholesome|hopeful|uplifting|feel[- ]good)\b`), "darkness", 0.1},
	{regexp.MustCompile(`(?i)\b(literary|lyrical|beautiful prose|beautifully written|rich prose)\b`), "prose_density", 0.85},
	{regexp.MustCompile(`(?i)\b(accessible|easy|plain|straightforward|simple)\b`), "prose_density", 0.2},
	{regexp.MustCompile(`(?i)\b(romance|romantic|love story|enemies to lovers|slow burn romance)\b`), "romance_centrality", 0.85},
	{regexp.MustCompile(`(?i)\b(experimental|nonlinear|non-linear|structurally|ambitious|challenging|dense)\b`), "narrative_complexity", 0.85},
	{regexp.MustCompile(`(?i)\b(worldbuilding|world-building|immersive world|epic fantasy|space opera)\b`), "worldbuilding", 0.9},
}

// exclusionAxis maps a "no X" word to the axis it caps.
var exclusionAxis = map[string]string{
	"romance":        "romance_centrality",
	"love story":     "romance_centrality",
	"fantasy":        "worldbuilding",
	"worldbuilding":  "worldbuilding",
	"world-building": "worldbuilding",
	"magic":          "worldbuilding",
	"sci-fi":         "worldbuilding",
	"violence":       "darkness",
	"gore":           "darkness",
	"darkness":       "darkness",
	"sad":            "emotional_intensity",
}

var contextWords = []struct {
	pattern *regexp.Regexp
	context string
	prefer  map[string]float64
}{
	{regexp.MustCompile(`(?i)\b(flight|plane|train|commute|holiday|vacation|beach)\b`), "travel", map[string]float64{"pacing": 0.75}},
	{regexp.MustCompile(`(?i)\b(book club|bookclub|discuss\w*|reading group)\b`), "book_club", map[string]float64{"narrative_complexity": 0.6, "emotional_intensity": 0.65}},
	{regexp.MustCompile(`(?i)\b(haven't read in|get back into reading|reading slump|out of practice|first book in)\b`), "returning", map[string]float64{"pacing": 0.8, "prose_density": 0.3, "narrative_complexity": 0.2}},
	{regexp.MustCompile(`(?i)\b(just finished|after a devastating|recover\w* from|need something (?:light|gentle) after|palate cleanser)\b`), "recovery", map[string]float64{"darkness": 0.2, "emotional_intensity": 0.3}},
	{regexp.MustCompile(`(?i)\b(challenge me|challenge myself|stretch|something harder|more demanding)\b`), "stretch", map[string]float64{"narrative_complexity": 0.8, "prose_density": 0.8}},
}

// ParseQuery reads a search string into intent, constraints and embed text.
func ParseQuery(q string) ParsedQuery {
	original := strings.TrimSpace(q)
	pq := ParsedQuery{Original: original, EmbedText: original}
	if original == "" {
		pq.Intent = IntentTopic
		return pq
	}

	c := &pq.Constraints
	text := original
	var kinds []SearchIntent

	// Hard page limits. Strip the phrase from the embed text — "under 300
	// pages" contributes nothing to what the book is about.
	if m := rePagesUnder.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 20 {
			c.MaxPages = n
		}
		text = rePagesUnder.ReplaceAllString(text, " ")
		kinds = append(kinds, IntentConstraint)
	}
	if m := rePagesOver.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 20 {
			c.MinPages = n
		}
		text = rePagesOver.ReplaceAllString(text, " ")
		kinds = append(kinds, IntentConstraint)
	}
	// Vague length words become a soft limit rather than a hard one: "short"
	// means different things to different readers, and 250 is a defensible
	// middle that filters doorstops without excluding every novel.
	if c.MaxPages == 0 && rePagesShort.MatchString(text) {
		c.MaxPages = 250
		kinds = append(kinds, IntentConstraint)
	}
	if c.MinPages == 0 && rePagesLong.MatchString(text) {
		c.MinPages = 450
		kinds = append(kinds, IntentConstraint)
	}

	if reStandalone.MatchString(text) {
		c.Standalone = true
		text = reStandalone.ReplaceAllString(text, " ")
		kinds = append(kinds, IntentConstraint)
	}

	// Exclusions before similarity, because "like X but no romance" must not
	// have "no romance" swallowed into the similarity target.
	for _, m := range reNo.FindAllStringSubmatch(text, -1) {
		word := strings.ToLower(m[1])
		switch {
		case strings.HasPrefix(word, "series") || strings.HasPrefix(word, "sequel"):
			c.Standalone = true
		default:
			if axis, ok := exclusionAxis[word]; ok {
				if c.ExcludeAxes == nil {
					c.ExcludeAxes = map[string]float64{}
				}
				c.ExcludeAxes[axis] = 0.3
			}
		}
		kinds = append(kinds, IntentConstraint)
	}
	text = reNo.ReplaceAllString(text, " ")

	if m := reSimilar.FindStringSubmatch(text); m != nil {
		target := strings.TrimSpace(strings.Trim(m[1], `"'“”`))
		// "more emotional" is an axis instruction, not a similarity target.
		if !looksLikeAxisWord(target) {
			c.SimilarTo = target
			kinds = append(kinds, IntentSimilarity)
		}
	}

	for _, cw := range contextWords {
		if cw.pattern.MatchString(text) {
			c.Context = cw.context
			if c.PreferAxes == nil {
				c.PreferAxes = map[string]float64{}
			}
			for axis, v := range cw.prefer {
				if _, set := c.PreferAxes[axis]; !set {
					c.PreferAxes[axis] = v
				}
			}
			kinds = append(kinds, IntentContext)
			break
		}
	}

	var sawMood, sawEmotional bool
	for _, aw := range axisWords {
		if !aw.pattern.MatchString(text) {
			continue
		}
		if c.PreferAxes == nil {
			c.PreferAxes = map[string]float64{}
		}
		// An explicit word wins over a context default: "a light read for my
		// flight" should be light, not the travel default of pacey.
		c.PreferAxes[aw.axis] = aw.target
		if aw.axis == "emotional_intensity" && aw.target > 0.5 {
			sawEmotional = true
		} else {
			sawMood = true
		}
	}
	// Both at once ("sad but hopeful") is the roadmap's multi-constraint
	// discovery, not merely an emotional search.
	if sawEmotional {
		kinds = append(kinds, IntentEmotional)
	}
	if sawMood {
		kinds = append(kinds, IntentMood)
	}

	// An exclusion should also keep the axis low at ranking time, not just
	// filter it. Prefer wins if both were set, since it is the more specific
	// instruction.
	for axis, ceiling := range c.ExcludeAxes {
		if c.PreferAxes == nil {
			c.PreferAxes = map[string]float64{}
		}
		if _, set := c.PreferAxes[axis]; !set {
			c.PreferAxes[axis] = ceiling / 2
		}
	}

	pq.EmbedText = collapseSpaces(text)
	if pq.EmbedText == "" {
		// Everything was a constraint ("under 300 pages"). Embed the original
		// so the vector is at least about books rather than about nothing.
		pq.EmbedText = original
	}

	pq.Intent = classify(kinds)
	return pq
}

// classify collapses the observed intent kinds into one label.
func classify(kinds []SearchIntent) SearchIntent {
	if len(kinds) == 0 {
		return IntentTopic
	}
	seen := map[SearchIntent]bool{}
	for _, k := range kinds {
		seen[k] = true
	}
	if len(seen) > 1 {
		// A constraint on top of anything else is still fundamentally that
		// thing; "multi" is reserved for two content intents at once.
		delete(seen, IntentConstraint)
		if len(seen) == 1 {
			for k := range seen {
				return k
			}
		}
		if len(seen) == 0 {
			return IntentConstraint
		}
		return IntentMulti
	}
	for k := range seen {
		return k
	}
	return IntentTopic
}

// isRefinementOnly reports whether a parse carried nothing but constraints —
// "under 300 pages, no romance" — and so should inherit the previous topic.
func isRefinementOnly(pq ParsedQuery) bool {
	if pq.Constraints.SimilarTo != "" {
		return false
	}
	if !pq.Constraints.HasHardFilter() && len(pq.Constraints.ExcludeAxes) == 0 && len(pq.Constraints.PreferAxes) == 0 {
		return false
	}
	// If stripping constraints left fewer than three words, there was no topic.
	return len(strings.Fields(pq.EmbedText)) < 3 || pq.EmbedText == pq.Original
}

func looksLikeAxisWord(s string) bool {
	for _, aw := range axisWords {
		if aw.pattern.MatchString(s) && len(strings.Fields(s)) <= 2 {
			return true
		}
	}
	return false
}

var (
	reSpaces = regexp.MustCompile(`\s+`)
	// Stripping a constraint phrase leaves its separators behind: "grief ,
	// , no romance" → "grief , ,". Orphaned commas and a trailing "and"/"but"
	// are noise to the embedder.
	reOrphanPunct  = regexp.MustCompile(`\s*[,;]\s*(?:[,;]\s*)*`)
	reDanglingConj = regexp.MustCompile(`(?i)\s+(?:and|but|with|or)\s*$`)
	reLeadingConj  = regexp.MustCompile(`(?i)^\s*(?:and|but|with|or)\s+`)
)

func collapseSpaces(s string) string {
	s = reOrphanPunct.ReplaceAllString(s, " ")
	s = reSpaces.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = strings.Trim(s, ",;.")
	s = reDanglingConj.ReplaceAllString(s, "")
	s = reLeadingConj.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// Merge layers a follow-up query onto a previous parse.
//
// This is what makes "books like Murakami" → "shorter" → "more emotional" →
// "something less weird" work as a conversation. Each refinement carries only
// what changed; everything else is inherited. A follow-up that is itself a
// full query (has its own similarity target or topic text) replaces rather
// than refines.
func (pq ParsedQuery) Merge(next ParsedQuery) ParsedQuery {
	out := pq
	out.Original = next.Original

	// A new similarity target or substantive topic text is a new search.
	if next.Constraints.SimilarTo != "" && next.Constraints.SimilarTo != pq.Constraints.SimilarTo {
		out.Constraints.SimilarTo = next.Constraints.SimilarTo
		out.EmbedText = next.EmbedText
		out.Constraints.PreferAxes = nil
		out.Constraints.ExcludeAxes = nil
	} else if isRefinementOnly(next) {
		// Keep the previous embed text; "shorter" says nothing about topic.
	} else if len(strings.Fields(next.EmbedText)) >= 3 {
		out.EmbedText = next.EmbedText
	}

	// Constraints: later wins, absent inherits.
	if next.Constraints.MaxPages > 0 {
		out.Constraints.MaxPages = next.Constraints.MaxPages
	}
	if next.Constraints.MinPages > 0 {
		out.Constraints.MinPages = next.Constraints.MinPages
	}
	if next.Constraints.Standalone {
		out.Constraints.Standalone = true
	}
	if next.Constraints.Context != "" {
		out.Constraints.Context = next.Constraints.Context
	}
	if len(next.Constraints.ExcludeAxes) > 0 {
		if out.Constraints.ExcludeAxes == nil {
			out.Constraints.ExcludeAxes = map[string]float64{}
		}
		for k, v := range next.Constraints.ExcludeAxes {
			out.Constraints.ExcludeAxes[k] = v
		}
	}
	if len(next.Constraints.PreferAxes) > 0 {
		if out.Constraints.PreferAxes == nil {
			out.Constraints.PreferAxes = map[string]float64{}
		}
		for k, v := range next.Constraints.PreferAxes {
			out.Constraints.PreferAxes[k] = v
		}
	}

	out.Intent = IntentMulti
	if out.Constraints.SimilarTo != "" && len(out.Constraints.PreferAxes) == 0 && !out.Constraints.HasHardFilter() {
		out.Intent = IntentSimilarity
	}
	return out
}

// Comparative refinements. "shorter" is not a query; it is an edit to one.
var reComparative = regexp.MustCompile(`(?i)^\s*(?:something\s+|a bit\s+|a little\s+|way\s+|much\s+)?(shorter|longer|lighter|darker|faster|slower|sadder|happier|weirder|less weird|less strange|more emotional|less emotional|more literary|less literary|more romantic|less romance|less romantic|simpler|more complex|gentler|harder|easier|funnier)\s*[.!]?\s*$`)

// comparativeAxis maps a comparative to (axis, delta). Applied relative to
// whatever the previous preference was — or to neutral when there was none.
var comparativeAxis = map[string]struct {
	axis  string
	delta float64
}{
	"shorter":        {"_pages", -1},
	"longer":         {"_pages", +1},
	"lighter":        {"emotional_intensity", -0.3},
	"darker":         {"darkness", +0.3},
	"faster":         {"pacing", +0.3},
	"slower":         {"pacing", -0.3},
	"sadder":         {"emotional_intensity", +0.3},
	"happier":        {"darkness", -0.3},
	"weirder":        {"narrative_complexity", +0.3},
	"less weird":     {"narrative_complexity", -0.3},
	"less strange":   {"narrative_complexity", -0.3},
	"more emotional": {"emotional_intensity", +0.3},
	"less emotional": {"emotional_intensity", -0.3},
	"more literary":  {"prose_density", +0.3},
	"less literary":  {"prose_density", -0.3},
	"more romantic":  {"romance_centrality", +0.3},
	"less romance":   {"romance_centrality", -0.3},
	"less romantic":  {"romance_centrality", -0.3},
	"simpler":        {"narrative_complexity", -0.3},
	"more complex":   {"narrative_complexity", +0.3},
	"gentler":        {"darkness", -0.3},
	"harder":         {"narrative_complexity", +0.3},
	"easier":         {"prose_density", -0.3},
	"funnier":        {"darkness", -0.3},
}

// ParseRefinement reads a follow-up like "shorter" or "less weird" against a
// previous parse. Returns (result, true) when the input was a comparative;
// (zero, false) when it should be treated as a fresh query.
func ParseRefinement(prev ParsedQuery, q string) (ParsedQuery, bool) {
	m := reComparative.FindStringSubmatch(q)
	if m == nil {
		return ParsedQuery{}, false
	}
	word := strings.ToLower(collapseSpaces(m[1]))
	adj, ok := comparativeAxis[word]
	if !ok {
		return ParsedQuery{}, false
	}

	out := prev
	out.Original = strings.TrimSpace(q)
	out.Intent = IntentMulti

	if adj.axis == "_pages" {
		// Length is the one refinement that edits a hard filter. Step by a
		// third of the current ceiling, or from a sensible default.
		cur := out.Constraints.MaxPages
		if adj.delta < 0 {
			if cur == 0 {
				// First "shorter" with no ceiling: 300 is what most readers
				// mean, and a third off that on each further ask.
				out.Constraints.MaxPages = 300
			} else {
				out.Constraints.MaxPages = max(80, cur*2/3)
			}
		} else {
			out.Constraints.MaxPages = 0
			if out.Constraints.MinPages == 0 {
				out.Constraints.MinPages = 350
			} else {
				out.Constraints.MinPages = out.Constraints.MinPages * 4 / 3
			}
		}
		return out, true
	}

	if out.Constraints.PreferAxes == nil {
		out.Constraints.PreferAxes = map[string]float64{}
	} else {
		// Copy so the previous turn's map is not mutated in place.
		cp := make(map[string]float64, len(out.Constraints.PreferAxes))
		for k, v := range out.Constraints.PreferAxes {
			cp[k] = v
		}
		out.Constraints.PreferAxes = cp
	}
	cur, set := out.Constraints.PreferAxes[adj.axis]
	if !set {
		cur = 0.5
	}
	out.Constraints.PreferAxes[adj.axis] = clamp01(cur + adj.delta)
	return out, true
}
