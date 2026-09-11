package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Explicit and implicit negative taste.
//
// The roadmap's target statement is "you don't dislike fantasy, you dislike
// high-worldbuilding fantasy". Reaching it needs three things this file
// provides: a verdict the reader can give before reading, a reason code that
// points at one axis rather than the whole book, and an implicit read on how
// far they actually got.

// Feedback verdicts.
const (
	VerdictLoved       = "loved"
	VerdictMaybe       = "maybe"
	VerdictNotForMe    = "not_for_me"
	VerdictAlreadyRead = "already_read"
	VerdictNotNow      = "not_now"
)

var validVerdicts = map[string]struct{}{
	VerdictLoved: {}, VerdictMaybe: {}, VerdictNotForMe: {},
	VerdictAlreadyRead: {}, VerdictNotNow: {},
}

// ValidVerdict reports whether v is a known feedback verdict.
func ValidVerdict(v string) bool {
	_, ok := validVerdicts[v]
	return ok
}

// Reason codes for a negative verdict.
const (
	ReasonTooLong        = "too_long"
	ReasonTooSlow        = "too_slow"
	ReasonWrongGenre     = "wrong_genre"
	ReasonWrongMood      = "wrong_mood"
	ReasonDislikeAuthor  = "dislike_author"
	ReasonDislikePremise = "dislike_premise"
	ReasonTooDark        = "too_dark"
	ReasonTooRomance     = "too_romance"
	ReasonTooComplex     = "too_complex"
	ReasonTooMuchWorld   = "too_much_worldbuilding"
	ReasonNotInterested  = "not_interested"
	ReasonAlreadyKnowIt  = "already_know_it"
)

// reasonCodeAxis maps a reason code onto the trait axis it is a measurement of,
// and the direction the reader is pushing.
//
// This is the whole point of collecting codes. "Not for me" moves every axis a
// little and therefore says almost nothing; "not for me, too slow" is a precise
// statement about `pacing` and should move only that. target is the value the
// reader is implicitly asking for on that axis.
var reasonCodeAxis = map[string]struct {
	axis   string
	target float64
}{
	ReasonTooSlow:      {"pacing", 1.0},               // wants faster
	ReasonTooDark:      {"darkness", 0.0},             // wants lighter
	ReasonTooRomance:   {"romance_centrality", 0.0},   // wants less romance
	ReasonTooComplex:   {"narrative_complexity", 0.0}, // wants simpler
	ReasonTooMuchWorld: {"worldbuilding", 0.0},        // wants less invented world
}

var validReasonCodes = map[string]struct{}{
	ReasonTooLong: {}, ReasonTooSlow: {}, ReasonWrongGenre: {}, ReasonWrongMood: {},
	ReasonDislikeAuthor: {}, ReasonDislikePremise: {}, ReasonTooDark: {},
	ReasonTooRomance: {}, ReasonTooComplex: {}, ReasonTooMuchWorld: {},
	ReasonNotInterested: {}, ReasonAlreadyKnowIt: {},
}

// FeedbackReasonCodes returns every valid reason code, for clients to render.
func FeedbackReasonCodes() []string {
	out := make([]string, 0, len(validReasonCodes))
	for k := range validReasonCodes {
		out = append(out, k)
	}
	return out
}

// SanitizeReasonCodes drops anything unrecognised and caps the list.
// A client sending noise should lose the noise, not the whole verdict: the
// verdict is the part that matters and it is already known to be valid.
func SanitizeReasonCodes(in []string) []string {
	out := make([]string, 0, 4)
	seen := make(map[string]bool, len(in))
	for _, c := range in {
		if _, ok := validReasonCodes[c]; !ok || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
		if len(out) == 4 {
			break
		}
	}
	return out
}

// suppressionFor returns how long a verdict removes a book from rotation.
//
// The distinction the roadmap draws is between "no" and "not yet". A reader who
// says "not now" has expressed interest and should see the book again in a
// couple of months; one who says "not for me" has not, and re-showing it is how
// a recommender teaches people that its feedback controls do nothing.
func suppressionFor(verdict string) (until *time.Time, forever bool) {
	switch verdict {
	case VerdictNotForMe:
		return nil, true
	case VerdictAlreadyRead:
		// They have read it. It should never be recommended again, but this is
		// not a statement of dislike and must not feed negative taste.
		return nil, true
	case VerdictNotNow:
		t := time.Now().Add(90 * 24 * time.Hour)
		return &t, false
	case VerdictMaybe:
		// Acknowledged but not acted on. A short rest stops the card becoming
		// wallpaper without discarding a book they were open to.
		t := time.Now().Add(14 * 24 * time.Hour)
		return &t, false
	default:
		return nil, false
	}
}

// RecordFeedback stores a verdict and applies its suppression.
func (s *RecommendationService) RecordFeedback(
	ctx context.Context, userID, bookID, verdict string, reasonCodes []string, reasonType string,
) error {
	if !ValidVerdict(verdict) {
		return fmt.Errorf("unknown verdict %q", verdict)
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("invalid user id: %w", err)
	}
	bid, err := uuid.Parse(bookID)
	if err != nil {
		return fmt.Errorf("invalid book id: %w", err)
	}

	codes := SanitizeReasonCodes(reasonCodes)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO recommendation_feedback
		    (user_id, book_id, verdict, reason_codes, reason_type, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
		ON CONFLICT (user_id, book_id) DO UPDATE SET
		    verdict      = EXCLUDED.verdict,
		    reason_codes = EXCLUDED.reason_codes,
		    reason_type  = COALESCE(EXCLUDED.reason_type, recommendation_feedback.reason_type),
		    updated_at   = NOW()
	`, uid, bid, verdict, codes, nullableText(reasonType)); err != nil {
		return err
	}

	until, forever := suppressionFor(verdict)
	if until == nil && !forever {
		return nil
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO recommendation_impressions
		    (user_id, book_id, seen_count, last_seen, dismissed_until, dismissed_forever)
		VALUES ($1, $2, 1, NOW(), $3, $4)
		ON CONFLICT (user_id, book_id) DO UPDATE SET
		    dismissed_until   = EXCLUDED.dismissed_until,
		    dismissed_forever = recommendation_impressions.dismissed_forever OR EXCLUDED.dismissed_forever,
		    last_seen         = NOW()
	`, uid, bid, until, forever)
	return err
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// FeedbackRow is one stored verdict, joined to the book's traits.
type FeedbackRow struct {
	BookID      string
	Verdict     string
	ReasonCodes []string
	Scalars     map[string]float64
	Confidence  float64
}

// getFeedbackWithTraits loads the reader's verdicts alongside book traits.
// already_read is excluded at the query level: it is a statement about
// coverage, not taste, and letting it into the profile would teach the engine
// to dislike exactly the books the reader has most enjoyed.
func (s *RecommendationService) getFeedbackWithTraits(ctx context.Context, userID string) ([]FeedbackRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT f.book_id::text, f.verdict, f.reason_codes,
		       t.character_driven, t.emotional_intensity, t.plot_intensity,
		       t.pacing, t.prose_density, t.narrative_complexity, t.darkness,
		       t.romance_centrality, t.worldbuilding, t.confidence
		FROM recommendation_feedback f
		JOIN book_traits t ON t.book_id = f.book_id
		WHERE f.user_id = $1
		  AND f.verdict <> 'already_read'
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FeedbackRow
	for rows.Next() {
		var r FeedbackRow
		vals := make([]float64, len(TraitAxes))
		dest := []any{&r.BookID, &r.Verdict, &r.ReasonCodes}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		dest = append(dest, &r.Confidence)
		if err := rows.Scan(dest...); err != nil {
			continue
		}
		r.Scalars = make(map[string]float64, len(TraitAxes))
		for i, axis := range TraitAxes {
			r.Scalars[axis] = vals[i]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// feedbackAsTraitedBooks converts verdicts into the shape ComputeTraitProfile
// learns from, so explicit feedback and ratings train the same profile.
//
// "loved" is mapped to a 5-star equivalent and "not_for_me" to a 1-star one,
// but both at reduced weight: a verdict given from a recommendation card is an
// opinion about a cover and a blurb, while a rating is an opinion about a book
// that was actually read. Treating them as equal would let a few seconds of
// swiping outweigh a year of reading.
func feedbackAsTraitedBooks(rows []FeedbackRow) []TraitedBook {
	out := make([]TraitedBook, 0, len(rows))
	for _, r := range rows {
		var rating *int
		switch r.Verdict {
		case VerdictLoved:
			v := 4
			rating = &v
		case VerdictNotForMe:
			v := 2
			rating = &v
		default:
			// maybe / not_now are not opinions about the book's shape.
			continue
		}
		out = append(out, TraitedBook{
			BookID:     r.BookID,
			Rating:     rating,
			Status:     "feedback",
			Scalars:    r.Scalars,
			Confidence: r.Confidence * 0.5,
		})
	}
	return out
}

// applyReasonCodes folds coded rejections into a trait profile's Dislikes.
//
// Handled apart from feedbackAsTraitedBooks because a code is a claim about one
// axis, not about the book as a whole. A reader who says "too slow" about a
// dark, romantic, densely-built book is telling us about pacing and nothing
// else; averaging the whole book into Dislikes would teach four wrong lessons
// to learn one right one.
func applyReasonCodes(p *TraitProfile, rows []FeedbackRow) {
	if p.Dislikes == nil {
		p.Dislikes = map[string]float64{}
	}
	type acc struct{ num, den float64 }
	byAxis := map[string]*acc{}

	for _, r := range rows {
		for _, code := range r.ReasonCodes {
			m, ok := reasonCodeAxis[code]
			if !ok {
				continue
			}
			v, ok := r.Scalars[m.axis]
			if !ok {
				continue
			}
			// The rejected value is what the book actually was on that axis;
			// the target only tells us which side of it they want to be on.
			a := byAxis[m.axis]
			if a == nil {
				a = &acc{}
				byAxis[m.axis] = a
			}
			a.num += v
			a.den++
		}
	}

	for axis, a := range byAxis {
		if a.den == 0 {
			continue
		}
		coded := a.num / a.den
		// A coded rejection is direct evidence and outweighs whatever the
		// book-level average said about this axis.
		if existing, ok := p.Dislikes[axis]; ok {
			p.Dislikes[axis] = (existing + coded*2) / 3
		} else {
			p.Dislikes[axis] = coded
		}
	}
}

// DepthSignal is the implicit half of negative taste.
//
// The roadmap's distinction is between "recommended -> ignored", "recommended
// -> opened -> saved" and "recommended -> started -> abandoned at 12%". They
// are three different statements and only the last one is strongly negative.
type DepthSignal struct {
	// AbandonedEarly are books started and dropped under earlyAbandonFraction.
	// A book abandoned at 12% was rejected; one abandoned at 80% was probably
	// just interrupted by life, and treating the two alike would punish the
	// books a reader nearly finished.
	AbandonedEarly []string `json:"abandoned_early,omitempty"`
	AbandonedLate  []string `json:"abandoned_late,omitempty"`
	// SavedNeverStarted is the roadmap's "you keep saving books like this but
	// never starting them" moment — aspiration rather than appetite.
	SavedNeverStarted int       `json:"saved_never_started"`
	StartedCount      int       `json:"started_count"`
	FinishRate        float64   `json:"finish_rate"`
	ComputedAt        time.Time `json:"computed_at"`
}

const earlyAbandonFraction = 0.25

// ComputeDepthSignal derives engagement depth from the shelf and page counts.
func (s *RecommendationService) ComputeDepthSignal(ctx context.Context, userID string) (*DepthSignal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.book_id::text,
		       b.status,
		       COALESCE(b.current_page, 0),
		       COALESCE(bk.page_count, 0),
		       b.started_at IS NOT NULL AS started,
		       b.finished_at IS NOT NULL AS finished,
		       b.updated_at
		FROM bookshelf b
		JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = $1
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sig := &DepthSignal{ComputedAt: time.Now()}
	var finished int

	for rows.Next() {
		var bookID, status string
		var currentPage, pageCount int
		var started, isFinished bool
		var updatedAt time.Time
		if err := rows.Scan(&bookID, &status, &currentPage, &pageCount,
			&started, &isFinished, &updatedAt); err != nil {
			continue
		}

		if isFinished {
			finished++
			sig.StartedCount++
			continue
		}

		if !started {
			if status == "pending" {
				sig.SavedNeverStarted++
			}
			continue
		}

		sig.StartedCount++

		// Still counts as in-progress until it has been untouched for 60 days;
		// before that "abandoned" is just "reading slowly".
		if time.Since(updatedAt) < 60*24*time.Hour {
			continue
		}
		// With no page count there is no depth to measure, so the book tells
		// us it was dropped but not how decisively. Treat as late (weak).
		if pageCount <= 0 {
			sig.AbandonedLate = append(sig.AbandonedLate, bookID)
			continue
		}
		if float64(currentPage)/float64(pageCount) < earlyAbandonFraction {
			sig.AbandonedEarly = append(sig.AbandonedEarly, bookID)
		} else {
			sig.AbandonedLate = append(sig.AbandonedLate, bookID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if sig.StartedCount > 0 {
		sig.FinishRate = float64(finished) / float64(sig.StartedCount)
	}
	return sig, nil
}

// ComputeAndSaveNegativeSignals recomputes the trait profile including explicit
// feedback and coded rejections, plus the depth signal, and persists both.
func (s *RecommendationService) ComputeAndSaveNegativeSignals(ctx context.Context, userID string) error {
	shelf, err := s.getTraitedShelf(ctx, userID)
	if err != nil {
		return err
	}
	feedback, err := s.getFeedbackWithTraits(ctx, userID)
	if err != nil {
		return err
	}

	// Early abandonment is a rejection the reader never wrote down. Folding it
	// in here rather than in getTraitedShelf keeps the depth rule in one place.
	depth, err := s.ComputeDepthSignal(ctx, userID)
	if err != nil {
		return err
	}
	early := make(map[string]bool, len(depth.AbandonedEarly))
	for _, id := range depth.AbandonedEarly {
		early[id] = true
	}
	for i := range shelf {
		if early[shelf[i].BookID] {
			shelf[i].Abandoned = true
		}
	}

	combined := append(shelf, feedbackAsTraitedBooks(feedback)...)
	if len(combined) == 0 {
		return nil
	}

	p := ComputeTraitProfile(combined)
	applyReasonCodes(&p, feedback)

	// The second clock. Same maths over the last 90 days of shelf opinions
	// (feedback rows are always recent, so they belong in both). Confidence
	// is naturally lower — fewer books — which is exactly the point: recent
	// taste should pull hard when there is a lot of it and barely at all when
	// the reader has rated two books this quarter.
	recent := RecentTraitProfile(combined, feedback, time.Now())

	prefs, _ := json.Marshal(p.Prefs)
	dislikes, _ := json.Marshal(p.Dislikes)
	conf, _ := json.Marshal(p.Confidence)
	depthJSON, _ := json.Marshal(depth)
	var recentPrefs, recentConf []byte
	if recent.HasSignal() {
		recentPrefs, _ = json.Marshal(recent.Prefs)
		recentConf, _ = json.Marshal(recent.Confidence)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO user_signal_profiles
		    (user_id, genre_weights, author_weights,
		     trait_prefs, trait_dislikes, trait_confidence, depth_signal,
		     trait_recent, trait_recent_confidence, signal_version)
		VALUES ($1, '{}', '{}', $2, $3, $4, $5, $6, $7, 1)
		ON CONFLICT (user_id) DO UPDATE SET
		    trait_prefs             = EXCLUDED.trait_prefs,
		    trait_dislikes          = EXCLUDED.trait_dislikes,
		    trait_confidence        = EXCLUDED.trait_confidence,
		    depth_signal            = EXCLUDED.depth_signal,
		    trait_recent            = EXCLUDED.trait_recent,
		    trait_recent_confidence = EXCLUDED.trait_recent_confidence,
		    signal_version          = user_signal_profiles.signal_version + 1
	`, userID, prefs, dislikes, conf, depthJSON, recentPrefs, recentConf)
	return err
}

// recentTasteWindow is how far back "lately" reaches.
const recentTasteWindow = 90 * 24 * time.Hour

// RecentTraitProfile computes the trait profile over opinions formed inside
// recentTasteWindow. Feedback rows carry no timestamp on the TraitedBook and
// are always included: a verdict given from a card is by definition recent.
func RecentTraitProfile(shelf []TraitedBook, feedback []FeedbackRow, now time.Time) TraitProfile {
	cutoff := now.Add(-recentTasteWindow)
	recent := make([]TraitedBook, 0, len(shelf))
	for _, b := range shelf {
		if b.When.IsZero() || b.When.After(cutoff) {
			recent = append(recent, b)
		}
	}
	return ComputeTraitProfile(recent)
}
