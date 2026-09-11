package service

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/hridyesh/paperboxd-backend/internal/util"
)

// Candidate sources beyond vector + social, and the anchors that let a
// recommendation name the specific shelf book it came from.
//
// The roadmap's candidate stage lists eight sources. Vector similarity and the
// social graph were the original two; hidden gems and exploration came with
// R3. These are the rest: unread books by authors the reader already rates
// highly, neighbours of the TBR pile, what the community shelved this week,
// and what taste twins loved. Each is one cheap query returning a few dozen
// rows — retrieval only decides what competes, ranking decides what surfaces.

const (
	SourceAuthor   CandidateSource = "author"
	SourceTBR      CandidateSource = "tbr"
	SourceTrending CandidateSource = "trending"
	SourceTwins    CandidateSource = "twins"
)

// candidateRow is the column list every source query selects, so the scan is
// written once.
const candidateRow = `b.id::text, b.title, b.authors, COALESCE(b.cover_url, ''), b.categories`

func scanCandidates(ctx context.Context, s *RecommendationService, src CandidateSource, query string, args ...any) []Candidate {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		slog.Warn("candidate source", "source", src, "error", err)
		return nil
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.BookID, &c.Title, &c.Authors, &c.CoverURL, &c.Categories); err != nil {
			continue
		}
		c.Source = src
		out = append(out, c)
	}
	return out
}

// getAuthorCandidates returns unread books by the authors the reader has
// engaged with most. Author affinity was already a ranking term, but a term
// can only reorder what retrieval brought in — a reader who loves an author
// with a thin embedding footprint never saw their back catalogue.
func (s *RecommendationService) getAuthorCandidates(ctx context.Context, userID uuid.UUID, profile UserSignalProfile, limit int) []Candidate {
	authors := make([]string, 0, 5)
	for a, w := range profile.AuthorWeights {
		if w >= 0.3 {
			authors = append(authors, a)
		}
	}
	if len(authors) == 0 {
		return nil
	}
	return scanCandidates(ctx, s, SourceAuthor, `
		SELECT `+candidateRow+`
		FROM books b
		WHERE b.authors && $2::text[]
		  AND b.embedding IS NOT NULL
		  AND b.id NOT IN (SELECT book_id FROM bookshelf WHERE user_id = $1)
		ORDER BY COALESCE(b.ratings_count, 0) DESC
		LIMIT $3
	`, userID, authors, limit)
}

// getTBRCandidates returns books near the centroid of the reader's TBR pile.
// The pile is intent the reader has stated but not acted on, and nothing else
// in the pipeline reads it — the taste vector is built from finished books
// only. Which TBR book each candidate resembles is worked out later by
// nearestAnchor, so the reason can name it.
func (s *RecommendationService) getTBRCandidates(ctx context.Context, userID uuid.UUID, anchors []Anchor, limit int) []Candidate {
	var rows []vectorRow
	for _, a := range anchors {
		if a.Kind == AnchorTBR {
			rows = append(rows, vectorRow{Embedding: a.Embedding, InteractionTime: a.At})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	centroid := weightedAverageVectors(rows)
	bcs, err := s.getVectorCandidates(ctx, userID, centroid, limit)
	if err != nil {
		slog.Warn("tbr candidates", "error", err)
		return nil
	}
	out := make([]Candidate, len(bcs))
	for i, bc := range bcs {
		out[i] = Candidate{
			BookID: bc.ID, Title: bc.Title, Authors: bc.Authors, Categories: bc.Categories,
			CoverURL: bc.CoverURL, Source: SourceTBR,
		}
	}
	return out
}

// getTrendingCandidates returns what live accounts shelved most in the last
// seven days that the reader has not. Same definition as the community page's
// trending rail, so a book cannot be "trending" in one place and not another.
func (s *RecommendationService) getTrendingCandidates(ctx context.Context, userID uuid.UUID, limit int) []Candidate {
	out := scanCandidates(ctx, s, SourceTrending, `
		SELECT `+candidateRow+`
		FROM books b
		JOIN bookshelf bs ON bs.book_id = b.id AND bs.created_at > NOW() - INTERVAL '7 days'
		JOIN users u ON u.id = bs.user_id AND u.deleted_at IS NULL
		WHERE b.embedding IS NOT NULL
		  AND b.id NOT IN (SELECT book_id FROM bookshelf WHERE user_id = $1)
		GROUP BY b.id
		HAVING COUNT(*) >= 2
		ORDER BY COUNT(*) DESC
		LIMIT $2
	`, userID, limit)
	for i := range out {
		out[i].IsTrending = true
	}
	return out
}

// getTwinCandidates returns books the reader's closest taste twins rated 4+
// that the reader has not shelved. Empty until the nightly overlap job has
// run, which is fine: the source contributes nothing rather than guessing.
func (s *RecommendationService) getTwinCandidates(ctx context.Context, userID uuid.UUID, limit int) []Candidate {
	return scanCandidates(ctx, s, SourceTwins, `
		WITH twins AS (
		    SELECT CASE WHEN t.user_a = $1 THEN t.user_b ELSE t.user_a END AS other, t.overlap
		    FROM taste_overlap t
		    WHERE t.user_a = $1 OR t.user_b = $1
		    ORDER BY t.overlap DESC
		    LIMIT 25
		)
		SELECT `+candidateRow+`
		FROM twins tw
		JOIN bookshelf bs ON bs.user_id = tw.other AND bs.rating >= 4
		JOIN users u ON u.id = tw.other AND u.deleted_at IS NULL
		JOIN books b ON b.id = bs.book_id
		WHERE b.embedding IS NOT NULL
		  AND b.id NOT IN (SELECT book_id FROM bookshelf WHERE user_id = $1)
		GROUP BY b.id
		ORDER BY SUM(tw.overlap) DESC
		LIMIT $2
	`, userID, limit)
}

// mergeCandidatePools unions several sources into one deduplicated pool,
// capped at maxSize. A book found by more than one source keeps the first
// occurrence and absorbs the others' per-source evidence, because "a friend
// read it AND a twin loved it AND it is by an author you rate" are three
// separate facts the ranker and the reason both want.
func mergeCandidatePools(maxSize int, pools ...[]Candidate) []Candidate {
	seen := make(map[string]int)
	result := make([]Candidate, 0, maxSize)
	for _, pool := range pools {
		for _, c := range pool {
			if idx, ok := seen[c.BookID]; ok {
				result[idx].absorb(c)
				continue
			}
			if len(result) >= maxSize {
				continue
			}
			seen[c.BookID] = len(result)
			result = append(result, c)
		}
	}
	return result
}

// absorb copies the evidence another source attached to the same book.
func (c *Candidate) absorb(o Candidate) {
	if o.VectorScore > c.VectorScore {
		c.VectorScore, c.SimilarityScore = o.VectorScore, o.SimilarityScore
	}
	if o.SocialScore > 0 {
		c.SocialScore, c.FriendNames, c.FriendLovedCount = o.SocialScore, o.FriendNames, o.FriendLovedCount
	}
	if o.Source == SourceTrending {
		c.IsTrending = true
	}
	if c.AverageRating == 0 {
		c.AverageRating, c.RatingsCount = o.AverageRating, o.RatingsCount
	}
}

// ── Anchors ───────────────────────────────────────────────────────────────────

// Anchor is one shelf book a recommendation can be explained by.
type Anchor struct {
	Title     string
	Kind      string
	Embedding []float32
	At        time.Time
}

const (
	AnchorRated5 = "rated5" // rated 5★
	AnchorLoved  = "loved"  // liked, or rated 4★
	AnchorTBR    = "tbr"    // on the to-read pile
)

// anchorMinSim is how close a candidate must sit to one specific shelf book
// before "because you loved X" is an honest sentence. Below it the two share
// a neighbourhood, not a resemblance. Cohere embed-v3 cosine: sequels and
// same-author novels land 0.8+, same-genre strangers around 0.6.
// ponytail: calibration knob — eyeball twenty pairs after backfill and move it.
const anchorMinSim = 0.72

// loadAnchors returns the reader's loved and to-read books with embeddings,
// most recent first. Capped so the per-candidate scan in nearestAnchor stays
// a few thousand dot products, not a few hundred thousand.
func (s *RecommendationService) loadAnchors(ctx context.Context, userID uuid.UUID) []Anchor {
	rows, err := s.pool.Query(ctx, `
		(SELECT bk.title, bk.embedding::text,
		        CASE WHEN b.rating = 5 THEN $2::text ELSE $3::text END AS kind,
		        COALESCE(b.finished_at, b.updated_at) AS acted_at
		 FROM bookshelf b JOIN books bk ON bk.id = b.book_id
		 WHERE b.user_id = $1 AND bk.embedding IS NOT NULL
		   AND (b.status = 'liked' OR b.rating >= 4)
		 ORDER BY acted_at DESC LIMIT 40)
		UNION ALL
		(SELECT bk.title, bk.embedding::text, $4::text, COALESCE(b.tbr_added_at, b.updated_at)
		 FROM bookshelf b JOIN books bk ON bk.id = b.book_id
		 WHERE b.user_id = $1 AND bk.embedding IS NOT NULL AND b.status = 'pending'
		 ORDER BY COALESCE(b.tbr_added_at, b.updated_at) DESC LIMIT 20)
	`, userID, AnchorRated5, AnchorLoved, AnchorTBR)
	if err != nil {
		slog.Warn("load anchors", "error", err)
		return nil
	}
	defer rows.Close()

	var out []Anchor
	for rows.Next() {
		var a Anchor
		var lit string
		if err := rows.Scan(&a.Title, &lit, &a.Kind, &a.At); err != nil {
			continue
		}
		vec, err := parsePGVectorLiteral(lit)
		if err != nil {
			continue
		}
		a.Embedding = vec
		out = append(out, a)
	}
	return out
}

// nearestAnchor finds the shelf book this candidate most resembles and, if it
// is close enough, records it on the candidate for the reason engine.
//
// Loved books outrank TBR: "because you loved X" is explicit evidence, "like
// something on your list" is a hunch the reader has not confirmed yet. A TBR
// anchor is used only when no loved book clears the bar.
func nearestAnchor(c *Candidate, anchors []Anchor) {
	if len(c.Embedding) == 0 {
		return
	}
	var bestLoved, bestTBR Anchor
	lovedSim, tbrSim := -1.0, -1.0
	for _, a := range anchors {
		if len(a.Embedding) != len(c.Embedding) {
			continue
		}
		sim := util.CosineSimilarity(c.Embedding, a.Embedding)
		switch a.Kind {
		case AnchorTBR:
			if sim > tbrSim {
				tbrSim, bestTBR = sim, a
			}
		default:
			if sim > lovedSim {
				lovedSim, bestLoved = sim, a
			}
		}
	}
	switch {
	case lovedSim >= anchorMinSim:
		c.AnchorTitle, c.AnchorKind, c.AnchorSim = bestLoved.Title, bestLoved.Kind, lovedSim
	case tbrSim >= anchorMinSim:
		c.AnchorTitle, c.AnchorKind, c.AnchorSim = bestTBR.Title, bestTBR.Kind, tbrSim
	}
}

// ── Live community stats ──────────────────────────────────────────────────────

// popularShelfCount is how many Paperboxd shelves a book needs before it is
// "popular" here. books.like_count / total_reads_count have been 0 since
// migration 000003 and nothing writes them, so awareness is counted live off
// bookshelf. ponytail: absolute number sized for a ~100-reader community —
// make it a percentile of the corpus once the community outgrows it.
const popularShelfCount = 10

// hiddenGemMaxGlobalRatings is the Google ratings ceiling for a hidden gem.
// Paperboxd's own shelf count cannot tell a hidden gem from a bestseller
// while the community is small enough that nothing has 25 shelves.
const hiddenGemMaxGlobalRatings = 200

// qualityScore folds an external average rating into 0..1, discounted by how
// many ratings stand behind it. 3.0 and below is 0, 5.0 is 1.
func qualityScore(avg float64, count int) float64 {
	if count <= 0 || avg <= 0 {
		return 0
	}
	q := (avg - 3.0) / 2.0
	q = math.Max(0, math.Min(1, q))
	return q * math.Min(float64(count)/20.0, 1.0)
}
