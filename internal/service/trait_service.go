package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
)

// Storage and taste maths for book_traits.
//
// The point of the nine axes is that the same numbers do three jobs: they rank
// candidates, they produce a sentence a reader recognises as true about
// themselves, and they draw the taste dashboard. Anything computed here has to
// stay interpretable -- if a number cannot be said out loud it belongs in the
// embedding, not here.

// traitPrefMass is how much accumulated evidence an axis needs before it is
// trusted completely. Five confidently-extracted, highly-rated books is the
// point at which "you like character-driven fiction" stops being one book's
// accident. Below it the axis is used at proportionally reduced weight rather
// than being ignored, so a new reader still gets some personalisation.
const traitPrefMass = 5.0

// TraitProfile is the reader's position on each axis, learned from their shelf.
type TraitProfile struct {
	// Prefs is the weighted mean trait value over books the reader loved.
	Prefs map[string]float64 `json:"prefs"`
	// Dislikes is the same over books they rated 1-2 or abandoned. Kept apart
	// from Prefs rather than folded in as a negative: "dislikes dense
	// worldbuilding" and "has no opinion on worldbuilding" are different
	// states, and averaging them together destroys the distinction the
	// roadmap's whole negative-taste phase depends on.
	Dislikes map[string]float64 `json:"dislikes"`
	// Confidence is 0..1 per axis: how much evidence Prefs rests on.
	Confidence map[string]float64 `json:"confidence"`
}

// HasSignal reports whether the profile knows anything worth ranking with.
func (t *TraitProfile) HasSignal() bool {
	if t == nil || len(t.Prefs) == 0 {
		return false
	}
	for _, c := range t.Confidence {
		if c > 0 {
			return true
		}
	}
	return false
}

// TraitedBook is a shelf entry joined to its extracted traits.
type TraitedBook struct {
	BookID     string
	Rating     *int
	Status     string
	Scalars    map[string]float64
	Confidence float64
	Abandoned  bool
	// When is when the opinion was formed: finished_at, else updated_at.
	// Zero for feedback rows, which are always "now".
	When time.Time
}

// traitSignalWeight scores a shelf entry for trait learning.
//
// Deliberately not SignalWeight: that function is tuned for genre and author
// counting, where a merely-read book is mild evidence. Trait axes are learned
// from opinions, not from exposure, so only a rating or an explicit like
// counts, and a plain "read" contributes nothing. Returns (positive, negative)
// mass; exactly one is non-zero.
func traitSignalWeight(rating *int, status string, abandoned bool) (pos, neg float64) {
	if rating != nil {
		switch *rating {
		case 5:
			return 1.0, 0
		case 4:
			return 0.6, 0
		case 3:
			return 0, 0 // genuinely neutral — teaches nothing either way
		case 2:
			return 0, 0.6
		case 1:
			return 0, 1.0
		}
	}
	if status == "liked" {
		return 0.6, 0
	}
	if abandoned {
		// Weak: an abandoned book may say more about the reader's month than
		// about the book. Enough to nudge, not enough to define an axis.
		return 0, 0.4
	}
	switch status {
	case "read":
		// Finished, unrated. People do not finish what they hate; a completed
		// book is mild evidence for its shape even when they never scored it.
		return 0.3, 0
	case "pending":
		// Saved to read. The roadmap's "recommended → opened → TBR" is an
		// intent signal, weaker than a finish and far weaker than a rating —
		// a TBR pile is aspiration, and aspiration still describes a shape
		// the reader is drawn to. Kept low so a hundred saved doorstops
		// cannot outvote five loved novellas.
		return 0.2, 0
	}
	return 0, 0
}

// ComputeTraitProfile derives per-axis preferences from a reader's shelf.
func ComputeTraitProfile(books []TraitedBook) TraitProfile {
	p := TraitProfile{
		Prefs:      make(map[string]float64, len(TraitAxes)),
		Dislikes:   make(map[string]float64, len(TraitAxes)),
		Confidence: make(map[string]float64, len(TraitAxes)),
	}

	posNum := make(map[string]float64, len(TraitAxes))
	posDen := make(map[string]float64, len(TraitAxes))
	negNum := make(map[string]float64, len(TraitAxes))
	negDen := make(map[string]float64, len(TraitAxes))

	for _, b := range books {
		pos, neg := traitSignalWeight(b.Rating, b.Status, b.Abandoned)
		if pos == 0 && neg == 0 {
			continue
		}
		// A book the extractor was unsure about contributes proportionally
		// less, so a corpus of thin blurbs cannot fabricate a confident taste.
		conf := b.Confidence
		if conf <= 0 {
			continue
		}

		for _, axis := range TraitAxes {
			v, ok := b.Scalars[axis]
			if !ok {
				continue
			}
			if pos > 0 {
				posNum[axis] += pos * conf * v
				posDen[axis] += pos * conf
			} else {
				negNum[axis] += neg * conf * v
				negDen[axis] += neg * conf
			}
		}
	}

	for _, axis := range TraitAxes {
		if posDen[axis] > 0 {
			p.Prefs[axis] = posNum[axis] / posDen[axis]
			p.Confidence[axis] = math.Min(1.0, posDen[axis]/traitPrefMass)
		}
		if negDen[axis] > 0 {
			p.Dislikes[axis] = negNum[axis] / negDen[axis]
		}
	}

	return p
}

// TraitFit scores how closely a book sits to what the reader has loved.
//
// Returns 0..1 where 1 is an exact match on every axis the reader has an
// opinion about. Axes are weighted by confidence, so a reader with a strong
// read on pacing and nothing on worldbuilding is judged mostly on pacing.
// Returns (0, false) when there is nothing to compare, so callers can leave
// the term out of the score entirely rather than inject a neutral 0.5 that
// would quietly compress every candidate towards the middle.
func TraitFit(book map[string]float64, p *TraitProfile) (float64, bool) {
	if p == nil || book == nil {
		return 0, false
	}
	var num, den float64
	for _, axis := range TraitAxes {
		conf := p.Confidence[axis]
		if conf <= 0 {
			continue
		}
		v, ok := book[axis]
		if !ok {
			continue
		}
		num += conf * (1 - math.Abs(v-p.Prefs[axis]))
		den += conf
	}
	if den == 0 {
		return 0, false
	}
	return num / den, true
}

// TraitClash scores how closely a book resembles what the reader has rejected.
//
// The roadmap's example is the whole reason this is separate from TraitFit:
// a reader who loves character-driven historical fiction but bounces off
// plot-dense historical fiction has a *positive* genre signal and a *negative*
// trait signal for the same book. Only measuring distance-to-liked would rank
// that book highly and keep doing so.
//
// Only axes where liked and disliked books actually diverge count -- if both
// sets sit at 0.6 on darkness, darkness says nothing about the rejection.
func TraitClash(book map[string]float64, p *TraitProfile) (float64, bool) {
	if p == nil || book == nil || len(p.Dislikes) == 0 {
		return 0, false
	}
	const minDivergence = 0.2

	var num, den float64
	for _, axis := range TraitAxes {
		disliked, ok := p.Dislikes[axis]
		if !ok {
			continue
		}
		liked, hasPref := p.Prefs[axis]
		if hasPref && math.Abs(disliked-liked) < minDivergence {
			continue
		}
		v, ok := book[axis]
		if !ok {
			continue
		}
		w := 1.0
		if hasPref {
			w = math.Abs(disliked - liked)
		}
		num += w * (1 - math.Abs(v-disliked))
		den += w
	}
	if den == 0 {
		return 0, false
	}
	return num / den, true
}

// StrongestTraits returns the axes the reader feels most strongly about,
// as (axis, value) pairs ordered by how far the preference sits from neutral
// and how much evidence backs it. Drives both the "why you" sentence and the
// taste dashboard bars.
func StrongestTraits(p *TraitProfile, n int) []TraitStrength {
	if p == nil {
		return nil
	}
	out := make([]TraitStrength, 0, len(TraitAxes))
	for _, axis := range TraitAxes {
		conf := p.Confidence[axis]
		if conf <= 0 {
			continue
		}
		v := p.Prefs[axis]
		out = append(out, TraitStrength{
			Axis:     axis,
			Value:    v,
			Strength: math.Abs(v-0.5) * 2 * conf,
		})
	}
	// Small, fixed-size list; insertion sort keeps it dependency-free.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Strength > out[j-1].Strength; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// TraitStrength is one axis the reader has a real opinion about.
type TraitStrength struct {
	Axis     string  `json:"axis"`
	Value    float64 `json:"value"`
	Strength float64 `json:"strength"`
}

// ── Persistence ───────────────────────────────────────────────────────────────

// SaveBookTraits upserts one book's extracted traits.
func (s *RecommendationService) SaveBookTraits(ctx context.Context, bookID string, t BookTraits) error {
	bid, err := uuid.Parse(bookID)
	if err != nil {
		return fmt.Errorf("invalid book id: %w", err)
	}

	catJSON, err := json.Marshal(map[string]any{
		"moods":       t.Moods,
		"themes":      t.Themes,
		"tone":        t.Tone,
		"pov":         t.POV,
		"setting":     t.Setting,
		"time_period": t.TimePeriod,
		"audience":    t.Audience,
		"is_series":   t.IsSeries,
	})
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO book_traits (
		    book_id, character_driven, emotional_intensity, plot_intensity,
		    pacing, prose_density, narrative_complexity, darkness,
		    romance_centrality, worldbuilding, traits, confidence,
		    extractor_version, extracted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NOW())
		ON CONFLICT (book_id) DO UPDATE SET
		    character_driven     = EXCLUDED.character_driven,
		    emotional_intensity  = EXCLUDED.emotional_intensity,
		    plot_intensity       = EXCLUDED.plot_intensity,
		    pacing               = EXCLUDED.pacing,
		    prose_density        = EXCLUDED.prose_density,
		    narrative_complexity = EXCLUDED.narrative_complexity,
		    darkness             = EXCLUDED.darkness,
		    romance_centrality   = EXCLUDED.romance_centrality,
		    worldbuilding        = EXCLUDED.worldbuilding,
		    traits               = EXCLUDED.traits,
		    confidence           = EXCLUDED.confidence,
		    extractor_version    = EXCLUDED.extractor_version,
		    extracted_at         = NOW()
	`,
		bid,
		t.Scalars["character_driven"], t.Scalars["emotional_intensity"],
		t.Scalars["plot_intensity"], t.Scalars["pacing"],
		t.Scalars["prose_density"], t.Scalars["narrative_complexity"],
		t.Scalars["darkness"], t.Scalars["romance_centrality"],
		t.Scalars["worldbuilding"],
		catJSON, t.Confidence, TraitExtractorVersion,
	)
	return err
}

// GetBooksNeedingTraits returns books that have no traits at the current
// extractor version. Books with no description are skipped: the model would be
// guessing from a title alone, which produces exactly the confident-and-wrong
// output the confidence field exists to avoid.
func (s *RecommendationService) GetBooksNeedingTraits(ctx context.Context, limit int) ([]TraitInput, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.id::text, b.title, b.authors, b.categories,
		       COALESCE(b.description, ''), COALESCE(b.page_count, 0)
		FROM books b
		LEFT JOIN book_traits t
		       ON t.book_id = b.id AND t.extractor_version = $1
		WHERE t.book_id IS NULL
		  AND b.description IS NOT NULL
		  AND LENGTH(b.description) >= 80
		ORDER BY COALESCE(b.total_reads_count, 0) DESC, b.created_at DESC
		LIMIT $2
	`, TraitExtractorVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TraitInput
	for rows.Next() {
		var in TraitInput
		if err := rows.Scan(&in.BookID, &in.Title, &in.Authors,
			&in.Categories, &in.Description, &in.PageCount); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// fetchCandidateTraits bulk-loads trait scalars for a candidate pool and sets
// Candidate.Traits. Missing books are left nil — ranking handles that by
// omitting the trait term for them rather than assuming a neutral book.
func (s *RecommendationService) fetchCandidateTraits(ctx context.Context, candidates []Candidate) error {
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.BookID)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT book_id::text, character_driven, emotional_intensity, plot_intensity,
		       pacing, prose_density, narrative_complexity, darkness,
		       romance_centrality, worldbuilding, confidence,
		       COALESCE((traits->>'is_series')::boolean, false)
		FROM book_traits
		WHERE book_id = ANY($1::uuid[])
	`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()

	type loaded struct {
		scalars  map[string]float64
		conf     float64
		isSeries bool
	}
	byID := make(map[string]loaded, len(candidates))
	for rows.Next() {
		var id string
		vals := make([]float64, len(TraitAxes))
		var l loaded
		dest := []any{&id}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		dest = append(dest, &l.conf, &l.isSeries)
		if err := rows.Scan(dest...); err != nil {
			continue
		}
		l.scalars = make(map[string]float64, len(TraitAxes))
		for i, axis := range TraitAxes {
			l.scalars[axis] = vals[i]
		}
		byID[id] = l
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range candidates {
		if l, ok := byID[candidates[i].BookID]; ok {
			candidates[i].Traits = l.scalars
			candidates[i].TraitConfidence = l.conf
			candidates[i].IsSeries = l.isSeries
		}
	}
	return nil
}

// getTraitedShelf returns the reader's shelf joined to extracted traits, which
// is the input ComputeTraitProfile learns from.
func (s *RecommendationService) getTraitedShelf(ctx context.Context, userID string) ([]TraitedBook, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.book_id::text, b.rating, b.status,
		       t.character_driven, t.emotional_intensity, t.plot_intensity,
		       t.pacing, t.prose_density, t.narrative_complexity, t.darkness,
		       t.romance_centrality, t.worldbuilding, t.confidence,
		       (b.status = 'reading'
		        AND b.finished_at IS NULL
		        AND b.created_at < NOW() - INTERVAL '60 days') AS abandoned,
		       COALESCE(b.finished_at, b.updated_at) AS opinion_at
		FROM bookshelf b
		JOIN book_traits t ON t.book_id = b.book_id
		WHERE b.user_id = $1
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TraitedBook
	for rows.Next() {
		var tb TraitedBook
		var rating *int32
		vals := make([]float64, len(TraitAxes))
		dest := []any{&tb.BookID, &rating, &tb.Status}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		dest = append(dest, &tb.Confidence, &tb.Abandoned, &tb.When)
		if err := rows.Scan(dest...); err != nil {
			continue
		}
		if rating != nil {
			r := int(*rating)
			tb.Rating = &r
		}
		tb.Scalars = make(map[string]float64, len(TraitAxes))
		for i, axis := range TraitAxes {
			tb.Scalars[axis] = vals[i]
		}
		out = append(out, tb)
	}
	return out, rows.Err()
}

// ComputeAndSaveTraitProfile recomputes a reader's trait preferences and
// persists them onto user_signal_profiles. Called from the nightly job and
// whenever a rating changes the picture materially.
func (s *RecommendationService) ComputeAndSaveTraitProfile(ctx context.Context, userID string) error {
	shelf, err := s.getTraitedShelf(ctx, userID)
	if err != nil {
		return err
	}
	if len(shelf) == 0 {
		return nil
	}

	p := ComputeTraitProfile(shelf)
	prefs, _ := json.Marshal(p.Prefs)
	dislikes, _ := json.Marshal(p.Dislikes)
	conf, _ := json.Marshal(p.Confidence)

	_, err = s.pool.Exec(ctx, `
		INSERT INTO user_signal_profiles
		    (user_id, genre_weights, author_weights, trait_prefs, trait_dislikes, trait_confidence, signal_version)
		VALUES ($1, '{}', '{}', $2, $3, $4, 1)
		ON CONFLICT (user_id) DO UPDATE SET
		    trait_prefs      = EXCLUDED.trait_prefs,
		    trait_dislikes   = EXCLUDED.trait_dislikes,
		    trait_confidence = EXCLUDED.trait_confidence,
		    signal_version   = user_signal_profiles.signal_version + 1
	`, userID, prefs, dislikes, conf)
	if err != nil {
		slog.Error("save trait profile", "error", err, "user_id", userID)
	}
	return err
}
