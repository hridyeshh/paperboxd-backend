package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Surprise Me, the taste dashboard, and longitudinal insights.
//
// Everything here reads from what R1–R6 already compute. The point of this
// file is the roadmap's last claim: the recommendation system is itself a
// product. A reader who can see "you tend to love slow-burn, character-driven
// books, and lately darker than usual" has been handed a mirror, and a mirror
// people recognise themselves in is the moat.

// ── Surprise Me ───────────────────────────────────────────────────────────────

// Surprise modes. Each is a different slice of the same ranked pool, framed
// honestly: the reader asked to be surprised, and "why this surprise" is the
// part that keeps it from feeling random.
const (
	SurpriseSafe       = "safe"       // highest confidence, closest to taste
	SurpriseUnexpected = "unexpected" // exploration slot with a trait bridge
	SurpriseWild       = "wild"       // lowest-confidence pick still above floor
	SurpriseGem        = "gem"        // hidden gem
	SurpriseObsession  = "obsession"  // high fit + high community love
)

var surpriseModes = []string{SurpriseSafe, SurpriseUnexpected, SurpriseWild, SurpriseGem, SurpriseObsession}

// SurpriseResult is one pick with its framing.
type SurpriseResult struct {
	Mode    string        `json:"mode"`
	Label   string        `json:"label"`
	Book    BookCandidate `json:"book"`
	Because string        `json:"because"`
}

// SurpriseMe picks one book for a mode. An empty mode picks a mode at random,
// weighted toward the confident end so the button is fun before it is risky.
func (s *RecommendationService) SurpriseMe(ctx context.Context, userID, mode string) (*SurpriseResult, error) {
	pool, _, err := s.GetHomeRecommendations(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		return nil, nil
	}

	if mode == "" {
		// 40% safe, 25% unexpected, 15% gem, 12% obsession, 8% wild.
		r := rand.Float64()
		switch {
		case r < 0.40:
			mode = SurpriseSafe
		case r < 0.65:
			mode = SurpriseUnexpected
		case r < 0.80:
			mode = SurpriseGem
		case r < 0.92:
			mode = SurpriseObsession
		default:
			mode = SurpriseWild
		}
	}

	pick := func(pred func(BookCandidate) bool) *BookCandidate {
		var matches []BookCandidate
		for _, b := range pool {
			if pred(b) {
				matches = append(matches, b)
			}
		}
		if len(matches) == 0 {
			return nil
		}
		// Random among the top few so the button does not always return the
		// same book for the same mode.
		n := min(3, len(matches))
		return &matches[rand.Intn(n)]
	}

	var book *BookCandidate
	var label, because string
	switch mode {
	case SurpriseSafe:
		book = pick(func(b BookCandidate) bool {
			return b.Confidence == "This feels very you" || b.Confidence == "I think you'll love this"
		})
		label, because = "Safe choice", "Closest thing in the pool to what you already love"
	case SurpriseUnexpected:
		book = pick(func(b BookCandidate) bool { return b.ReasonType == "explore" })
		label, because = "Unexpected", "Outside your usual genres, inside your usual shape"
	case SurpriseWild:
		book = pick(func(b BookCandidate) bool { return b.Confidence == "Wild card" })
		label, because = "Wild card", "Honestly, a guess — but a considered one"
	case SurpriseGem:
		book = pick(func(b BookCandidate) bool { return b.IsHiddenGem })
		label, because = "Hidden gem", "Barely read, well loved, and very you"
	case SurpriseObsession:
		book = pick(func(b BookCandidate) bool {
			return b.ReasonType == "social" || b.ReasonType == "people_like_you"
		})
		label, because = "Your next obsession", "Readers who share your taste could not put it down"
	default:
		return nil, fmt.Errorf("unknown surprise mode %q", mode)
	}

	// Fall through to the best remaining pick rather than returning nothing:
	// the button was pressed, it has to do something.
	if book == nil {
		book = &pool[0]
		label, because = "Safe choice", "Closest thing in the pool to what you already love"
		mode = SurpriseSafe
	}
	if book.Reason != "" {
		because = book.Reason + ". " + because
	}
	return &SurpriseResult{Mode: mode, Label: label, Book: *book, Because: because}, nil
}

// SurpriseModes lists the modes for clients to render.
func SurpriseModes() []string { return surpriseModes }

// ── Taste dashboard ───────────────────────────────────────────────────────────

// TasteBar is one axis on the dashboard.
type TasteBar struct {
	Axis  string  `json:"axis"`
	Label string  `json:"label"` // "Character-driven"
	Value float64 `json:"value"` // 0..1, the reader's preference on the axis
	// Strength is how confidently we can say it; bars are sorted by it.
	Strength float64 `json:"strength"`
}

// TasteShift is one axis moving over the last ~90 days.
type TasteShift struct {
	Axis      string  `json:"axis"`
	Label     string  `json:"label"`
	Direction string  `json:"direction"` // "up" | "down"
	Delta     float64 `json:"delta"`
}

// TasteInsight is a longitudinal observation about the reader.
type TasteInsight struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// TasteDashboard is the reader-facing view of their own profile.
type TasteDashboard struct {
	Bars        []TasteBar     `json:"bars"`
	Shifts      []TasteShift   `json:"shifts"`
	CurrentMood []string       `json:"current_mood"`
	Insights    []TasteInsight `json:"insights"`
	TopGenres   []string       `json:"top_genres"`
	TopAuthors  []string       `json:"top_authors"`
	Dislikes    []string       `json:"dislikes"` // phrased, e.g. "dense worldbuilding"
	BooksRated  int            `json:"books_rated"`
	// Enough is false while the profile is too thin to show honestly. The
	// client renders an invitation to rate more instead of empty bars.
	Enough bool `json:"enough"`
}

// axisLabel is the dashboard's short name for an axis.
var axisLabel = map[string]string{
	"character_driven":     "Character-driven",
	"emotional_intensity":  "Emotionally intense",
	"plot_intensity":       "Plot-driven",
	"pacing":               "Fast-paced",
	"prose_density":        "Literary",
	"narrative_complexity": "Structurally ambitious",
	"darkness":             "Dark",
	"romance_centrality":   "Romance-led",
	"worldbuilding":        "Big worlds",
}

// GetTasteDashboard assembles the reader's mirror.
func (s *RecommendationService) GetTasteDashboard(ctx context.Context, userID string) (TasteDashboard, error) {
	var d TasteDashboard
	profile, err := s.GetOrComputeSignalProfile(ctx, userID)
	if err != nil {
		return d, err
	}

	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bookshelf WHERE user_id = $1 AND rating IS NOT NULL
	`, userID).Scan(&d.BooksRated)

	d.TopGenres = topWeighted(profile.GenreWeights, 5)
	d.TopAuthors = topWeighted(profile.AuthorWeights, 5)

	// Bars: every axis with evidence, strongest conviction first.
	if profile.Traits != nil {
		for _, t := range StrongestTraits(profile.Traits, len(TraitAxes)) {
			d.Bars = append(d.Bars, TasteBar{
				Axis:     t.Axis,
				Label:    axisLabel[t.Axis],
				Value:    t.Value,
				Strength: t.Strength,
			})
		}
		for axis, v := range profile.Traits.Dislikes {
			liked, has := profile.Traits.Prefs[axis]
			if !has || math.Abs(v-liked) < 0.25 {
				continue
			}
			if p, ok := traitPhrases[axis]; ok {
				idx := 0
				if v > 0.5 {
					idx = 1
				}
				d.Dislikes = append(d.Dislikes, p[idx])
			}
		}
	}

	// Shifts: recent vs long-term, from the same computation Jazy uses.
	d.Shifts = s.tasteShifts(ctx, userID, &profile)

	// Current mood: what the last few finished books looked like, as axes.
	d.CurrentMood = s.currentMood(ctx, userID)

	d.Insights = s.readingInsights(ctx, userID, &profile)

	d.Enough = d.BooksRated >= 5 && len(d.Bars) >= 3
	if d.Bars == nil {
		d.Bars = []TasteBar{}
	}
	if d.Shifts == nil {
		d.Shifts = []TasteShift{}
	}
	if d.Insights == nil {
		d.Insights = []TasteInsight{}
	}
	if d.CurrentMood == nil {
		d.CurrentMood = []string{}
	}
	if d.Dislikes == nil {
		d.Dislikes = []string{}
	}
	return d, nil
}

func (s *RecommendationService) tasteShifts(ctx context.Context, userID string, profile *UserSignalProfile) []TasteShift {
	if profile.Traits == nil || !profile.Traits.HasSignal() {
		return nil
	}
	recent := s.recentAxisMeans(ctx, userID, 90, true)
	if recent == nil {
		return nil
	}
	var out []TasteShift
	for _, axis := range TraitAxes {
		lt, ok := profile.Traits.Prefs[axis]
		if !ok || profile.Traits.Confidence[axis] < 0.4 {
			continue
		}
		r, ok := recent[axis]
		if !ok {
			continue
		}
		delta := r - lt
		if math.Abs(delta) < 0.15 {
			continue
		}
		dir := "up"
		if delta < 0 {
			dir = "down"
		}
		out = append(out, TasteShift{Axis: axis, Label: axisLabel[axis], Direction: dir, Delta: delta})
	}
	return out
}

// currentMood names the 2–3 strongest axes of the last few finished books,
// regardless of rating: it is what they have been reading, not what they loved.
func (s *RecommendationService) currentMood(ctx context.Context, userID string) []string {
	means := s.recentAxisMeans(ctx, userID, 45, false)
	if means == nil {
		return nil
	}
	type kv struct {
		axis string
		dist float64
		val  float64
	}
	var ranked []kv
	for axis, v := range means {
		ranked = append(ranked, kv{axis, math.Abs(v - 0.5), v})
	}
	for i := 1; i < len(ranked); i++ {
		for j := i; j > 0 && ranked[j].dist > ranked[j-1].dist; j-- {
			ranked[j], ranked[j-1] = ranked[j-1], ranked[j]
		}
	}
	var out []string
	for _, r := range ranked {
		if r.dist < 0.2 || len(out) == 3 {
			break
		}
		p, ok := traitPhrases[r.axis]
		if !ok {
			continue
		}
		idx := 0
		if r.val > 0.5 {
			idx = 1
		}
		out = append(out, strings.ToUpper(p[idx][:1])+p[idx][1:])
	}
	return out
}

// recentAxisMeans averages trait axes over books finished in the last N days.
// lovedOnly restricts to 4★+. Returns nil under three books.
func (s *RecommendationService) recentAxisMeans(ctx context.Context, userID string, days int, lovedOnly bool) map[string]float64 {
	ratingClause := ""
	if lovedOnly {
		ratingClause = "AND b.rating >= 4"
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT t.character_driven, t.emotional_intensity, t.plot_intensity,
		       t.pacing, t.prose_density, t.narrative_complexity, t.darkness,
		       t.romance_centrality, t.worldbuilding
		FROM bookshelf b
		JOIN book_traits t ON t.book_id = b.book_id
		WHERE b.user_id = $1
		  AND COALESCE(b.finished_at, b.updated_at) > NOW() - make_interval(days => $2)
		  %s
	`, ratingClause), userID, days)
	if err != nil {
		return nil
	}
	defer rows.Close()

	sums := make([]float64, len(TraitAxes))
	var n int
	for rows.Next() {
		vals := make([]float64, len(TraitAxes))
		dest := make([]any, len(vals))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if rows.Scan(dest...) != nil {
			continue
		}
		for i, v := range vals {
			sums[i] += v
		}
		n++
	}
	if n < 3 {
		return nil
	}
	out := make(map[string]float64, len(TraitAxes))
	for i, axis := range TraitAxes {
		out[axis] = sums[i] / float64(n)
	}
	return out
}

// readingInsights derives the roadmap's "advanced personal intelligence"
// observations. Each one is gated on enough evidence to be true rather than
// merely plausible — an insight the reader knows is wrong costs more than
// five they never see.
func (s *RecommendationService) readingInsights(ctx context.Context, userID string, profile *UserSignalProfile) []TasteInsight {
	var out []TasteInsight

	// "You tend to give books 5★ when you finish them quickly."
	if profile.VelocitySignal != nil && profile.VelocitySignal.ComputedFromNBooks >= 8 {
		var fastFive, fastAll, slowFive, slowAll int
		_ = s.pool.QueryRow(ctx, `
			SELECT
			  COUNT(*) FILTER (WHERE fast AND rating = 5),
			  COUNT(*) FILTER (WHERE fast),
			  COUNT(*) FILTER (WHERE NOT fast AND rating = 5),
			  COUNT(*) FILTER (WHERE NOT fast)
			FROM (
			  SELECT rating,
			         EXTRACT(EPOCH FROM (finished_at - COALESCE(started_at, created_at)))/86400 < 7 AS fast
			  FROM bookshelf
			  WHERE user_id = $1 AND finished_at IS NOT NULL AND rating IS NOT NULL
			) x
		`, userID).Scan(&fastFive, &fastAll, &slowFive, &slowAll)
		if fastAll >= 4 && slowAll >= 4 {
			fr := float64(fastFive) / float64(fastAll)
			sr := float64(slowFive) / float64(slowAll)
			if fr-sr > 0.25 {
				out = append(out, TasteInsight{Kind: "velocity_rating",
					Text: "You tend to give books 5★ when you finish them quickly."})
			}
		}
	}

	// "Your favourite books average N pages."
	var avgLoved, avgAll float64
	var lovedN int
	_ = s.pool.QueryRow(ctx, `
		SELECT
		  COALESCE(AVG(bk.page_count) FILTER (WHERE b.rating >= 4), 0),
		  COALESCE(AVG(bk.page_count), 0),
		  COUNT(*) FILTER (WHERE b.rating >= 4)
		FROM bookshelf b JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = $1 AND b.finished_at IS NOT NULL AND bk.page_count > 0
	`, userID).Scan(&avgLoved, &avgAll, &lovedN)
	if lovedN >= 5 && avgLoved > 0 {
		out = append(out, TasteInsight{Kind: "loved_length",
			Text: fmt.Sprintf("Your favourite books average %d pages.", int(math.Round(avgLoved/10)*10))})
	}

	// "You frequently abandon books in the first 15%."
	var early, abandoned int
	_ = s.pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE bk.page_count > 0 AND b.current_page::float / bk.page_count < 0.15),
		  COUNT(*)
		FROM bookshelf b JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = $1 AND b.status = 'reading' AND b.finished_at IS NULL
		  AND b.updated_at < NOW() - INTERVAL '60 days'
	`, userID).Scan(&early, &abandoned)
	if abandoned >= 4 && float64(early)/float64(abandoned) > 0.6 {
		out = append(out, TasteInsight{Kind: "early_abandon",
			Text: "When a book doesn't grab you in the first few chapters, you tend not to go back."})
	}

	// "You keep saving books like this but never starting them."
	var saved, started int
	_ = s.pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE status = 'pending'),
		  COUNT(*) FILTER (WHERE started_at IS NOT NULL AND created_at > NOW() - INTERVAL '180 days')
		FROM bookshelf WHERE user_id = $1
	`, userID).Scan(&saved, &started)
	if saved >= 15 && saved > started*3 {
		out = append(out, TasteInsight{Kind: "saver",
			Text: fmt.Sprintf("You've saved %d books and started %d in the last six months. The pile is aspiration, not appetite — shorter picks help.", saved, started)})
	}

	// "You discover your favourites through other readers."
	var socialLoved, allLoved int
	_ = s.pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE e.metadata->>'reason_type' IN ('social','people_like_you')),
		  COUNT(*)
		FROM bookshelf b
		LEFT JOIN LATERAL (
		  SELECT metadata FROM events
		  WHERE user_id = b.user_id AND book_id = b.book_id AND event_type = 'rec_click'
		  ORDER BY created_at ASC LIMIT 1
		) e ON true
		WHERE b.user_id = $1 AND b.rating >= 4
	`, userID).Scan(&socialLoved, &allLoved)
	if allLoved >= 8 && float64(socialLoved)/float64(allLoved) > 0.4 {
		out = append(out, TasteInsight{Kind: "social_discovery",
			Text: "Most of the books you've loved came from other readers, not search."})
	}

	// Recent intensity vs. usual.
	if profile.Traits != nil {
		if recent := s.recentAxisMeans(ctx, userID, 60, false); recent != nil {
			lt, ok := profile.Traits.Prefs["emotional_intensity"]
			if ok && profile.Traits.Confidence["emotional_intensity"] >= 0.4 && recent["emotional_intensity"]-lt > 0.2 {
				out = append(out, TasteInsight{Kind: "heavier_lately",
					Text: "You've been reading heavier books than usual lately."})
			}
		}
	}

	return out
}

// ── Context-aware discovery ───────────────────────────────────────────────────

// ContextPreset is a named situation the reader can pick instead of typing.
// Each maps onto the same constraint vocabulary the search parser produces, so
// "I have a 10-hour flight" and typing "something for my flight" land in the
// same place.
type ContextPreset struct {
	ID       string             `json:"id"`
	Label    string             `json:"label"`
	Hint     string             `json:"hint"`
	Prefer   map[string]float64 `json:"-"`
	MaxPages int                `json:"-"`
	MinPages int                `json:"-"`
}

var contextPresets = []ContextPreset{
	{ID: "flight", Label: "I have a long flight", Hint: "Immersive, hard to put down",
		Prefer: map[string]float64{"pacing": 0.75, "narrative_complexity": 0.4}, MinPages: 250},
	{ID: "slump", Label: "I haven't read in months", Hint: "Fast hook, easy re-entry",
		Prefer: map[string]float64{"pacing": 0.85, "prose_density": 0.3, "narrative_complexity": 0.2}, MaxPages: 320},
	{ID: "recovery", Label: "I just finished something devastating", Hint: "Gentle landing",
		Prefer: map[string]float64{"darkness": 0.15, "emotional_intensity": 0.3}},
	{ID: "book_club", Label: "I need something for book club", Hint: "Worth arguing about",
		Prefer: map[string]float64{"narrative_complexity": 0.6, "emotional_intensity": 0.65, "character_driven": 0.7}},
	{ID: "stretch", Label: "I want to challenge myself", Hint: "A deliberate stretch",
		Prefer: map[string]float64{"narrative_complexity": 0.85, "prose_density": 0.85}},
	{ID: "comfort", Label: "I want comfort", Hint: "Warm and low-stakes",
		Prefer: map[string]float64{"darkness": 0.1, "emotional_intensity": 0.25, "pacing": 0.5}},
}

// ContextPresets lists the situations for clients to render.
func ContextPresets() []ContextPreset { return contextPresets }

// ContextDiscovery runs a search from a preset instead of free text.
func (s *RecommendationService) ContextDiscovery(ctx context.Context, userID, anonID, presetID string, limit int) (SearchResponse, error) {
	var preset *ContextPreset
	for i := range contextPresets {
		if contextPresets[i].ID == presetID {
			preset = &contextPresets[i]
			break
		}
	}
	if preset == nil {
		return SearchResponse{}, fmt.Errorf("unknown context %q", presetID)
	}

	// Build the parse directly rather than round-tripping through text, so
	// the preset's exact constraints are what run. The embed text is the
	// label: it is a reasonable topic vector and keeps the search grounded.
	pq := ParsedQuery{
		Original:  preset.Label,
		Intent:    IntentContext,
		EmbedText: preset.Hint + " " + preset.Label,
		Constraints: SearchConstraints{
			PreferAxes: preset.Prefer,
			MaxPages:   preset.MaxPages,
			MinPages:   preset.MinPages,
			Context:    preset.ID,
		},
	}
	sess := &SearchSession{
		ID: uuid.NewString(), UserID: userID, AnonID: anonID,
		Current: pq, CreatedAt: time.Now(),
	}
	sess.Turns = append(sess.Turns, SearchTurn{Query: preset.Label, Intent: IntentContext, At: time.Now()})

	resp, err := s.searchWithSession(ctx, s.queries, SearchRequest{
		Query: preset.Label, SessionID: sess.ID, UserID: userID, AnonID: anonID, Limit: limit,
	}, sess, false)
	if err != nil {
		slog.Warn("context discovery", "preset", presetID, "error", err)
		return resp, err
	}
	resp.Kind = "search#context"
	return resp, nil
}
