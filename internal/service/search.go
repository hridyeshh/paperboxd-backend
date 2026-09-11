package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	"github.com/pgvector/pgvector-go"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// Personalised search.
//
// The principle is "search should retrieve, taste should rank". Retrieval is
// the embedding plus any hard constraints the parser found; ranking is the
// same taste machinery the home feed uses, so "books about grief" returns the
// same candidate set to everyone and a different order to each of them.
//
// This supersedes VibeSearch for the search surface. VibeSearch is kept as the
// Ask Jazy path because its contract (Claude writes a per-book reason, the
// response is cached whole) is different: Jazy is a deck of five, search is a
// list that reorders as the reader refines it.

const (
	searchCandidates   = 120
	searchDefaultLimit = 10
	searchMaxLimit     = 30
)

// SearchRequest is one turn of search.
type SearchRequest struct {
	Query     string
	SessionID string
	UserID    string
	AnonID    string
	Limit     int
}

// SearchResponse is the result of one turn.
type SearchResponse struct {
	Kind         string                 `json:"kind"`
	SessionID    string                 `json:"sessionId"`
	Query        string                 `json:"query"`
	Understood   string                 `json:"understood"`
	Intent       SearchIntent           `json:"intent"`
	Refined      bool                   `json:"refined"`
	Personalised bool                   `json:"personalised"`
	TotalItems   int                    `json:"totalItems"`
	Items        []types.VibeBookResult `json:"items"`
	// SimilarToResolved is what the parser's SimilarTo resolved to, when it
	// did, so the client can show "like Sally Rooney (author)" rather than
	// echoing the raw text.
	SimilarToResolved string `json:"similarToResolved,omitempty"`
}

// Search runs one turn: advance the session, retrieve, filter, rank, explain.
func (s *RecommendationService) Search(ctx context.Context, queries *db.Queries, req SearchRequest) (SearchResponse, error) {
	sess, refined := s.AdvanceSearchSession(ctx, req.SessionID, req.UserID, req.AnonID, req.Query)
	return s.searchWithSession(ctx, queries, req, sess, refined)
}

// searchWithSession runs retrieval, filtering, ranking and explanation for an
// already-advanced session. Split from Search so a caller that built the
// parse itself (context presets) can run it without the text parser
// overwriting its constraints.
func (s *RecommendationService) searchWithSession(ctx context.Context, queries *db.Queries, req SearchRequest, sess *SearchSession, refined bool) (SearchResponse, error) {
	if req.Limit <= 0 {
		req.Limit = searchDefaultLimit
	}
	if req.Limit > searchMaxLimit {
		req.Limit = searchMaxLimit
	}
	pq := sess.Current

	// 1. Resolve the query vector. A similarity target that resolves to a
	// known author or book anchors on that book's own embedding; the reader
	// said "like Murakami", and Murakami's actual books are a far better
	// centre than the embedding of the word "Murakami".
	queryVec, resolved, err := s.resolveQueryVector(ctx, queries, pq)
	if err != nil {
		return SearchResponse{}, err
	}

	// 2. Retrieve. Over-fetch so hard filters and taste re-ranking have room.
	rows, err := queries.VibeSearchBooks(ctx, db.VibeSearchBooksParams{
		QueryVec: pgvector.NewVector(queryVec),
		Lim:      searchCandidates,
	})
	if err != nil {
		return SearchResponse{}, fmt.Errorf("search ann: %w", err)
	}

	// 3. Hard filters.
	candidates := make([]Candidate, 0, len(rows))
	byID := make(map[string]db.VibeSearchBooksRow, len(rows))
	for _, r := range rows {
		if !passesHardFilters(r, pq.Constraints) {
			continue
		}
		c := Candidate{
			BookID:          r.ID.String(),
			Title:           r.Title,
			Authors:         r.Authors,
			Categories:      r.Categories,
			VectorScore:     float32(r.SimilarityScore),
			SimilarityScore: float32(r.SimilarityScore),
			Embedding:       r.Embedding.Slice(),
			Source:          SourceVector,
		}
		if r.CoverUrl.Valid {
			c.CoverURL = r.CoverUrl.String
		}
		if r.AverageRating.Valid {
			c.AverageRating = r.AverageRating.Float64
		}
		if r.RatingsCount.Valid {
			c.RatingsCount = int(r.RatingsCount.Int32)
		}
		if r.TotalReadsCount.Valid {
			c.TotalReads = int(r.TotalReadsCount.Int32)
		}
		byID[c.BookID] = r
		candidates = append(candidates, c)
	}

	// 4. Taste. Search uses the same profile the home feed does, so the
	// reader's negative signals apply here too — a book they said "not for me"
	// to should not top a search either.
	var profile *UserSignalProfile
	personalised := false
	if req.UserID != "" {
		if p, err := s.GetOrComputeSignalProfile(ctx, req.UserID); err == nil {
			profile = &p
			personalised = len(p.GenreWeights) > 0 || p.Traits.HasSignal() || p.DiaryEmbedding != nil
		}
		candidates = s.filterSuppressed(ctx, req.UserID, candidates)
	}
	if err := s.fetchCandidateTraits(ctx, candidates); err != nil {
		slog.Warn("search: candidate traits", "error", err)
	}

	// 5. Rank. Query similarity dominates — this is search, the reader asked
	// for something specific — but taste breaks ties and constraint axes pull.
	for i := range candidates {
		c := &candidates[i]
		c.FinalScore = searchScore(c, pq.Constraints, profile)
		if profile != nil {
			c.Confidence = RecConfidence(*c, profile)
			c.ConfidenceLabel = ConfidenceLabel(c.Confidence)
		}
	}
	sortByScore(candidates)

	// 6. On a refinement, push books already shown to the tail. The reader
	// asked for something different; the same deck in a new order is not it.
	if refined && len(sess.Shown) > 0 {
		shown := make(map[string]bool, len(sess.Shown))
		for _, id := range sess.Shown {
			shown[id] = true
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			return !shown[candidates[i].BookID] && shown[candidates[j].BookID]
		})
	}

	// 7. Dedupe editions, cut to size.
	seen := make(map[string]bool, len(candidates))
	final := make([]Candidate, 0, req.Limit)
	for _, c := range candidates {
		key := vibeDedupeKey(c.Title, c.Authors)
		if seen[key] {
			continue
		}
		seen[key] = true
		final = append(final, c)
		if len(final) == req.Limit {
			break
		}
	}

	// 8. Explain. Same reason engine as the feed, so a book gets the same
	// sentence wherever it appears; plus a query-specific line when the
	// constraint axes are what put it here.
	re := &ReasonEngine{}
	items := make([]types.VibeBookResult, len(final))
	ids := make([]string, len(final))
	for i, c := range final {
		reason := ReasonResult{}
		if profile != nil {
			reason = re.Build(c, profile, "")
			// The generic fallbacks say nothing useful under a search result.
			if reason.Type == "favorites" || reason.Type == "cold" {
				reason = ReasonResult{}
			}
		}
		if reason.Text == "" {
			reason = constraintReason(c, pq.Constraints)
		}
		items[i] = vibeBookRowToResult(byID[c.BookID], float64(c.FinalScore), reason)
		items[i].MatchPercent = min(100, max(0, int(c.FinalScore*100+0.5)))
		ids[i] = c.BookID
	}

	sess.RecordShown(ids)
	s.SaveSearchSession(ctx, sess)

	return SearchResponse{
		Kind:              "search#personalised",
		SessionID:         sess.ID,
		Query:             req.Query,
		Understood:        sess.Describe(),
		Intent:            pq.Intent,
		Refined:           refined,
		Personalised:      personalised,
		TotalItems:        len(items),
		Items:             items,
		SimilarToResolved: resolved,
	}, nil
}

// resolveQueryVector picks the embedding to search with.
//
// Returns the vector, a human description of what SimilarTo resolved to (or
// ""), and an error only when nothing at all could be embedded.
func (s *RecommendationService) resolveQueryVector(ctx context.Context, queries *db.Queries, pq ParsedQuery) ([]float32, string, error) {
	if target := pq.Constraints.SimilarTo; target != "" {
		if vec, label := s.anchorVector(ctx, target); vec != nil {
			// Blend a little of the free text back in so "like Murakami but
			// about grief" is not purely Murakami.
			if pq.EmbedText != "" && !strings.EqualFold(pq.EmbedText, target) {
				if qv, err := s.embedder.EmbedTexts([]string{pq.EmbedText}, "search_query"); err == nil && len(qv) == 1 && len(qv[0]) == len(vec) {
					for i := range vec {
						vec[i] = vec[i]*0.75 + qv[0][i]*0.25
					}
				}
			}
			return vec, label, nil
		}
	}

	vecs, err := s.embedder.EmbedTexts([]string{pq.EmbedText}, "search_query")
	if err != nil || len(vecs) == 0 {
		return nil, "", fmt.Errorf("embed query: %w", err)
	}
	return vecs[0], "", nil
}

// anchorVector finds an author or title matching target and returns the
// centroid of their book embeddings. Author first: "like Sally Rooney" is a
// far more common request than "like Normal People", and an author's centroid
// is a better description of "like them" than any one book.
func (s *RecommendationService) anchorVector(ctx context.Context, target string) ([]float32, string) {
	pattern := "%" + strings.ToLower(target) + "%"

	rows, err := s.pool.Query(ctx, `
		SELECT embedding::text
		FROM books
		WHERE embedding IS NOT NULL
		  AND EXISTS (SELECT 1 FROM unnest(authors) a WHERE LOWER(a) LIKE $1)
		ORDER BY COALESCE(total_reads_count, 0) DESC
		LIMIT 8
	`, pattern)
	if err == nil {
		if vec := centroidFromRows(rows); vec != nil {
			return vec, target + " (author)"
		}
	}

	rows, err = s.pool.Query(ctx, `
		SELECT embedding::text
		FROM books
		WHERE embedding IS NOT NULL
		  AND LOWER(title) LIKE $1
		ORDER BY COALESCE(total_reads_count, 0) DESC
		LIMIT 3
	`, pattern)
	if err == nil {
		if vec := centroidFromRows(rows); vec != nil {
			return vec, target + " (book)"
		}
	}
	return nil, ""
}

func centroidFromRows(rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
}) []float32 {
	defer rows.Close()
	var vecs [][]float32
	for rows.Next() {
		var lit string
		if rows.Scan(&lit) != nil {
			continue
		}
		if v, err := parsePGVectorLiteral(lit); err == nil {
			vecs = append(vecs, v)
		}
	}
	if len(vecs) == 0 {
		return nil
	}
	return weightedAverageVectors(func() []vectorRow {
		out := make([]vectorRow, len(vecs))
		for i, v := range vecs {
			out[i] = vectorRow{Embedding: v}
		}
		return out
	}())
}

// passesHardFilters applies the constraints that are filters, not nudges.
func passesHardFilters(r db.VibeSearchBooksRow, c SearchConstraints) bool {
	if c.MaxPages > 0 || c.MinPages > 0 {
		// A book with no page count cannot be shown to satisfy a length
		// request; excluding it is the honest reading of "under 300 pages".
		if !r.PageCount.Valid || r.PageCount.Int32 <= 0 {
			return false
		}
		n := int(r.PageCount.Int32)
		if c.MaxPages > 0 && n > c.MaxPages {
			return false
		}
		if c.MinPages > 0 && n < c.MinPages {
			return false
		}
	}
	return true
}

// searchScore blends query similarity, constraint axes and taste.
//
// Query similarity is the anchor: this is search, and a book that is not
// about what the reader asked for should not outrank one that is because it
// suits their taste. Constraint axes are next — they are the reader's own
// words about this search — and long-term taste last, as a tiebreak.
func searchScore(c *Candidate, con SearchConstraints, profile *UserSignalProfile) float32 {
	score := float64(c.VectorScore) * 0.60

	// Axis constraints. Only meaningful when the book has traits; a book
	// without them is neither rewarded nor punished for an axis it cannot
	// express, which keeps unextracted books from sinking on every search.
	if c.Traits != nil && len(con.PreferAxes) > 0 {
		var fit, n float64
		for axis, want := range con.PreferAxes {
			v, ok := c.Traits[axis]
			if !ok {
				continue
			}
			fit += 1 - math.Abs(v-want)
			n++
		}
		if n > 0 {
			score += (fit / n) * 0.25
		} else {
			score += 0.5 * 0.25
		}
	} else {
		score += 0.5 * 0.25
	}

	// Exclusion ceilings are soft: over the ceiling costs, proportionally.
	if c.Traits != nil {
		for axis, ceiling := range con.ExcludeAxes {
			if v, ok := c.Traits[axis]; ok && v > ceiling {
				score -= (v - ceiling) * 0.3
			}
		}
	}

	if con.Standalone && c.IsSeries {
		score -= 0.15
	}

	if profile != nil {
		if fit, ok := TraitFit(c.Traits, profile.Traits); ok {
			c.TraitFitScore = fit
			c.HasTraitFit = true
			score += fit * 0.10
		} else {
			score += 0.5 * 0.10
		}
		if clash, ok := TraitClash(c.Traits, profile.Traits); ok && clash > 0.5 {
			c.TraitClashScore = clash
			score -= (clash - 0.5) * 2 * 0.08
		}
		var g float64
		for _, cat := range c.Categories {
			if w, ok := profile.GenreWeights[cat]; ok {
				g += w
			}
		}
		score += math.Min(g, 1.0) * 0.05
	} else {
		score += 0.5*0.10 + 0.5*0.05
	}

	return float32(math.Max(0, math.Min(1, score)))
}

// constraintReason explains a result in terms of what the reader asked for,
// when no taste-based reason applied. "Under 300 pages, and fast-paced" is a
// weaker line than "you tend to love…" but it is true and specific, which
// beats a blank.
func constraintReason(c Candidate, con SearchConstraints) ReasonResult {
	var parts []string
	if con.MaxPages > 0 || con.MinPages > 0 {
		// Page count is on the row, not the candidate; the caller filtered on
		// it, so the constraint is known to hold.
		if con.MaxPages > 0 {
			parts = append(parts, fmt.Sprintf("under %d pages", con.MaxPages))
		} else {
			parts = append(parts, fmt.Sprintf("over %d pages", con.MinPages))
		}
	}
	if c.Traits != nil {
		for axis, want := range con.PreferAxes {
			v, ok := c.Traits[axis]
			if !ok || math.Abs(v-want) > 0.25 {
				continue
			}
			if p, ok := traitPhrases[axis]; ok {
				idx := 0
				if want > 0.5 {
					idx = 1
				}
				parts = append(parts, p[idx])
				break
			}
		}
	}
	if len(parts) == 0 {
		return ReasonResult{}
	}
	text := strings.ToUpper(parts[0][:1]) + parts[0][1:]
	if len(parts) > 1 {
		text += ", and " + parts[1]
	}
	return ReasonResult{Text: text, Type: "constraint"}
}
