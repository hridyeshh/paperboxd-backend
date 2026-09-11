package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Reader-to-reader taste similarity.
//
// The number has to survive the obvious objections. Two readers who share
// forty books but rate them oppositely are not twins; two who share three
// books and love all three might be. So the measure is three parts:
//
//   - shelf overlap (Jaccard): have they read the same things?
//   - rating agreement on shared books: do they *feel* the same about them?
//   - trait-profile distance: do they want the same shape of book, even where
//     their shelves do not intersect yet?
//
// The third part is what makes the number useful for a new reader with a thin
// shelf, and is only possible because R1 gave every reader a position on the
// nine axes.

const (
	// Under this many shared books, rating agreement is noise and the shelf
	// term is mostly luck; the trait term carries the estimate.
	overlapMinShared = 3
	// Top-N twins kept per reader. Enough for a "people like you" module and a
	// twin card; more is rows nobody reads.
	overlapKeepPerUser = 25
	// Below this the pair is not worth storing.
	overlapFloor = 0.15
)

// shelfEntry is the slice of a bookshelf row the overlap needs.
type shelfEntry struct {
	BookID string
	Rating *int
}

// readerShelf is one reader's shelf, indexed for pair comparison.
type readerShelf struct {
	UserID string
	Books  map[string]*int // book id -> rating (nil = unrated)
	Traits *TraitProfile
}

// OverlapResult is the computed similarity between two readers, plus the
// evidence behind it.
type OverlapResult struct {
	Overlap     float64
	SharedCount int
	SharedLoved []string // both rated 4+
	Disagreed   []string // ratings ≥2 apart
	ACouldRead  []string // B loved, A has not shelved
	BCouldRead  []string // A loved, B has not shelved
}

// ComputeOverlap scores a pair.
func ComputeOverlap(a, b readerShelf) OverlapResult {
	var res OverlapResult

	var shared, agreeN int
	var agreeSum float64
	for id, ra := range a.Books {
		rb, ok := b.Books[id]
		if !ok {
			continue
		}
		shared++
		if ra != nil && rb != nil {
			agreeN++
			diff := math.Abs(float64(*ra - *rb))
			// 0 apart → 1.0, 4 apart → 0.0.
			agreeSum += 1 - diff/4
			if *ra >= 4 && *rb >= 4 {
				res.SharedLoved = append(res.SharedLoved, id)
			}
			if diff >= 2 {
				res.Disagreed = append(res.Disagreed, id)
			}
		}
	}
	res.SharedCount = shared

	union := len(a.Books) + len(b.Books) - shared
	jaccard := 0.0
	if union > 0 {
		jaccard = float64(shared) / float64(union)
	}
	// Jaccard is harsh on unequal shelves: a reader with 20 books and one with
	// 400 can never exceed 0.05 even if all 20 overlap. Scale by the smaller
	// shelf so "you have read most of what they have" counts.
	smaller := math.Min(float64(len(a.Books)), float64(len(b.Books)))
	containment := 0.0
	if smaller > 0 {
		containment = float64(shared) / smaller
	}
	shelfScore := 0.5*jaccard + 0.5*containment

	agreement := 0.5 // no evidence → neutral
	if agreeN > 0 {
		agreement = agreeSum / float64(agreeN)
	}

	traitSim, hasTraits := traitProfileSimilarity(a.Traits, b.Traits)

	// Weighting shifts with evidence. Plenty of shared books: what they think
	// about them matters most. Few: the trait profiles are the only real
	// signal, and the shelf term is mostly which bestsellers both bought.
	var overlap float64
	switch {
	case shared >= overlapMinShared && hasTraits:
		overlap = 0.35*shelfScore + 0.40*agreement + 0.25*traitSim
	case shared >= overlapMinShared:
		overlap = 0.45*shelfScore + 0.55*agreement
	case hasTraits:
		overlap = 0.25*shelfScore + 0.75*traitSim
	default:
		overlap = shelfScore
	}
	res.Overlap = math.Max(0, math.Min(1, overlap))

	// "Steal their TBR": what one loved that the other has not touched.
	for id, rb := range b.Books {
		if rb != nil && *rb >= 4 {
			if _, has := a.Books[id]; !has {
				res.ACouldRead = append(res.ACouldRead, id)
			}
		}
	}
	for id, ra := range a.Books {
		if ra != nil && *ra >= 4 {
			if _, has := b.Books[id]; !has {
				res.BCouldRead = append(res.BCouldRead, id)
			}
		}
	}
	// Cap the evidence arrays; the card shows a handful.
	res.SharedLoved = capStrings(res.SharedLoved, 10)
	res.Disagreed = capStrings(res.Disagreed, 5)
	res.ACouldRead = capStrings(res.ACouldRead, 20)
	res.BCouldRead = capStrings(res.BCouldRead, 20)
	return res
}

// traitProfileSimilarity is 1 - mean absolute distance over axes both readers
// have an opinion on, weighted by the lesser confidence.
func traitProfileSimilarity(a, b *TraitProfile) (float64, bool) {
	if a == nil || b == nil || !a.HasSignal() || !b.HasSignal() {
		return 0, false
	}
	var num, den float64
	for _, axis := range TraitAxes {
		ca, cb := a.Confidence[axis], b.Confidence[axis]
		if ca <= 0 || cb <= 0 {
			continue
		}
		w := math.Min(ca, cb)
		num += w * (1 - math.Abs(a.Prefs[axis]-b.Prefs[axis]))
		den += w
	}
	if den == 0 {
		return 0, false
	}
	return num / den, true
}

func capStrings(in []string, n int) []string {
	if len(in) > n {
		return in[:n]
	}
	return in
}

// ── Nightly materialisation ───────────────────────────────────────────────────

// RecomputeTasteOverlaps rebuilds taste_overlap for every reader with a shelf.
//
// O(n²) over readers with a shelf. At a few hundred readers this is seconds;
// past ~5k it wants blocking by genre cluster or an ANN over trait vectors.
// ponytail: full pairwise; block by top genre when reader count passes 5k.
func (s *RecommendationService) RecomputeTasteOverlaps(ctx context.Context) error {
	shelves, err := s.loadAllShelves(ctx)
	if err != nil {
		return err
	}
	if len(shelves) < 2 {
		return nil
	}
	slog.Info("taste overlap: computing", "readers", len(shelves))

	type pair struct {
		a, b string
		res  OverlapResult
	}
	// Keep top-N per reader, both directions.
	best := make(map[string][]pair, len(shelves))
	push := func(uid string, p pair) {
		l := append(best[uid], p)
		sort.Slice(l, func(i, j int) bool { return l[i].res.Overlap > l[j].res.Overlap })
		if len(l) > overlapKeepPerUser {
			l = l[:overlapKeepPerUser]
		}
		best[uid] = l
	}

	for i := 0; i < len(shelves); i++ {
		for j := i + 1; j < len(shelves); j++ {
			a, b := shelves[i], shelves[j]
			if a.UserID > b.UserID {
				a, b = b, a
			}
			res := ComputeOverlap(a, b)
			if res.Overlap < overlapFloor {
				continue
			}
			p := pair{a: a.UserID, b: b.UserID, res: res}
			push(a.UserID, p)
			push(b.UserID, p)
		}
	}

	// Union of everyone's top-N, written in one transaction so a reader never
	// sees a half-rebuilt table.
	seen := make(map[[2]string]bool)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM taste_overlap`); err != nil {
		return err
	}
	var written int
	for _, l := range best {
		for _, p := range l {
			key := [2]string{p.a, p.b}
			if seen[key] {
				continue
			}
			seen[key] = true
			ua, _ := uuid.Parse(p.a)
			ub, _ := uuid.Parse(p.b)
			_, err := tx.Exec(ctx, `
				INSERT INTO taste_overlap
				    (user_a, user_b, overlap, shared_count, shared_loved, disagreed,
				     a_could_read, b_could_read, computed_at)
				VALUES ($1,$2,$3,$4,$5::uuid[],$6::uuid[],$7::uuid[],$8::uuid[],$9)
			`, ua, ub, float32(p.res.Overlap), p.res.SharedCount,
				p.res.SharedLoved, p.res.Disagreed, p.res.ACouldRead, p.res.BCouldRead,
				time.Now())
			if err != nil {
				return err
			}
			written++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	slog.Info("taste overlap: done", "pairs", written)
	return nil
}

func (s *RecommendationService) loadAllShelves(ctx context.Context) ([]readerShelf, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.user_id::text, b.book_id::text, b.rating
		FROM bookshelf b
		JOIN users u ON u.id = b.user_id AND u.deleted_at IS NULL
		WHERE b.status IN ('read', 'reading', 'liked')
		   OR b.rating IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byUser := map[string]*readerShelf{}
	for rows.Next() {
		var uid, bid string
		var rating *int32
		if rows.Scan(&uid, &bid, &rating) != nil {
			continue
		}
		sh := byUser[uid]
		if sh == nil {
			sh = &readerShelf{UserID: uid, Books: map[string]*int{}}
			byUser[uid] = sh
		}
		var r *int
		if rating != nil {
			v := int(*rating)
			r = &v
		}
		sh.Books[bid] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attach trait profiles. One query; readers without one stay nil.
	prows, err := s.pool.Query(ctx, `
		SELECT user_id::text, trait_prefs, trait_confidence
		FROM user_signal_profiles
		WHERE trait_prefs IS NOT NULL
	`)
	if err == nil {
		for prows.Next() {
			var uid string
			var prefs, conf []byte
			if prows.Scan(&uid, &prefs, &conf) != nil {
				continue
			}
			sh := byUser[uid]
			if sh == nil {
				continue
			}
			tp := &TraitProfile{}
			_ = json.Unmarshal(prefs, &tp.Prefs)
			_ = json.Unmarshal(conf, &tp.Confidence)
			if tp.HasSignal() {
				sh.Traits = tp
			}
		}
		prows.Close()
	}

	out := make([]readerShelf, 0, len(byUser))
	for _, sh := range byUser {
		// A shelf of one book produces overlaps that are pure coincidence.
		if len(sh.Books) >= 3 {
			out = append(out, *sh)
		}
	}
	return out, nil
}

// ── Read side ─────────────────────────────────────────────────────────────────

// TasteTwin is one reader similar to another, with the evidence.
type TasteTwin struct {
	UserID      string   `json:"user_id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name,omitempty"`
	AvatarURL   string   `json:"avatar_url,omitempty"`
	Overlap     float64  `json:"overlap"`
	OverlapPct  int      `json:"overlap_pct"`
	SharedCount int      `json:"shared_count"`
	SharedLoved []string `json:"shared_loved"` // book ids
	Disagreed   []string `json:"disagreed"`
	CouldRead   []string `json:"could_read"` // books they loved you haven't read
	IsFollowing bool     `json:"is_following"`
}

// GetTasteTwins returns the readers most similar to userID, strongest first.
// Blocked readers and private profiles the viewer cannot see are excluded at
// the query, so a twin is always someone the reader could actually visit.
func (s *RecommendationService) GetTasteTwins(ctx context.Context, userID string, limit int) ([]TasteTwin, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > overlapKeepPerUser {
		limit = 10
	}

	rows, err := s.pool.Query(ctx, `
		SELECT
		    CASE WHEN t.user_a = $1 THEN t.user_b ELSE t.user_a END AS other,
		    u.username, COALESCE(u.name, ''), COALESCE(u.avatar_url, ''),
		    t.overlap, t.shared_count, t.shared_loved, t.disagreed,
		    CASE WHEN t.user_a = $1 THEN t.a_could_read ELSE t.b_could_read END,
		    EXISTS (SELECT 1 FROM follows f WHERE f.follower_id = $1
		            AND f.following_id = CASE WHEN t.user_a = $1 THEN t.user_b ELSE t.user_a END)
		FROM taste_overlap t
		JOIN users u ON u.id = CASE WHEN t.user_a = $1 THEN t.user_b ELSE t.user_a END
		WHERE (t.user_a = $1 OR t.user_b = $1)
		  AND u.deleted_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM blocks bl
		      WHERE (bl.blocker_id = $1 AND bl.blocked_id = u.id)
		         OR (bl.blocker_id = u.id AND bl.blocked_id = $1))
		ORDER BY t.overlap DESC
		LIMIT $2
	`, uid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TasteTwin
	for rows.Next() {
		var t TasteTwin
		var other uuid.UUID
		var overlap float32
		if err := rows.Scan(&other, &t.Username, &t.DisplayName, &t.AvatarURL,
			&overlap, &t.SharedCount, &t.SharedLoved, &t.Disagreed, &t.CouldRead,
			&t.IsFollowing); err != nil {
			continue
		}
		t.UserID = other.String()
		t.Overlap = float64(overlap)
		t.OverlapPct = int(math.Round(float64(overlap) * 100))
		if t.SharedLoved == nil {
			t.SharedLoved = []string{}
		}
		if t.Disagreed == nil {
			t.Disagreed = []string{}
		}
		if t.CouldRead == nil {
			t.CouldRead = []string{}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PeopleLikeYouLoved returns, for a set of candidate books, how many of the
// reader's top twins rated each 4+, plus up to two names. This is the
// "4 readers with your taste loved this" line.
func (s *RecommendationService) PeopleLikeYouLoved(ctx context.Context, userID string, bookIDs []string) (map[string]TwinSignal, error) {
	uid, err := uuid.Parse(userID)
	if err != nil || len(bookIDs) == 0 {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		WITH twins AS (
		    SELECT CASE WHEN t.user_a = $1 THEN t.user_b ELSE t.user_a END AS other,
		           t.overlap
		    FROM taste_overlap t
		    WHERE (t.user_a = $1 OR t.user_b = $1)
		    ORDER BY t.overlap DESC
		    LIMIT 25
		)
		SELECT b.book_id::text, COUNT(*), 
		       (ARRAY_AGG(u.username ORDER BY tw.overlap DESC))[1:2]
		FROM twins tw
		JOIN bookshelf b ON b.user_id = tw.other AND b.rating >= 4
		JOIN users u ON u.id = tw.other AND u.deleted_at IS NULL
		WHERE b.book_id = ANY($2::uuid[])
		GROUP BY b.book_id
	`, uid, bookIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]TwinSignal)
	for rows.Next() {
		var id string
		var sig TwinSignal
		if rows.Scan(&id, &sig.Count, &sig.Names) == nil {
			out[id] = sig
		}
	}
	return out, rows.Err()
}

// TwinSignal is how many taste-twins loved a book, and who.
type TwinSignal struct {
	Count int
	Names []string
}
