package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/hridyesh/paperboxd-backend/internal/util"
)

const (
	vibeSearchCandidates = 50
	vibeCacheTTL         = 30 * time.Minute
)

// VibeSearch embeds the query, runs ANN, optionally personalises with the
// user's diary centroid, caches the result, and returns a VibeSearchResponse.
//
// Test unauthenticated vibe search:
//
//	curl -X POST http://localhost:8080/api/v1/search/vibe \
//	  -H "Content-Type: application/json" \
//	  -d '{"query": "person who fails at self improvement but never gives up", "limit": 5}'
//
// Expected: 5 books, personalised: false, each has matchReason.
//
// Test cache hit (run twice — second should be faster):
//
//	redis-cli keys "vibe:anon:*"
//	redis-cli TTL "vibe:anon:<hash>"
func (s *RecommendationService) VibeSearch(ctx context.Context, queries *db.Queries, query string, limit int, userID string) (types.VibeSearchResponse, error) {
	cacheKey := vibeCacheKey(userID, query)

	// 1. Redis cache check
	if s.redisClient != nil {
		if data, err := s.redisClient.Get(ctx, cacheKey).Bytes(); err == nil {
			var cached types.VibeSearchResponse
			if json.Unmarshal(data, &cached) == nil {
				return cached, nil
			}
		}
	}

	// 2. Embed query — "search_query" is the Cohere input type for queries (not documents)
	vecs, err := s.embedder.EmbedTexts([]string{query}, "search_query")
	if err != nil || len(vecs) == 0 {
		return types.VibeSearchResponse{}, fmt.Errorf("embed query: %w", err)
	}
	queryVec := vecs[0]

	// 3. ANN — top 50 candidates via pgvector cosine distance
	candidates, err := queries.VibeSearchBooks(ctx, db.VibeSearchBooksParams{
		QueryVec: pgvector.NewVector(queryVec),
		Lim:      vibeSearchCandidates,
	})
	if err != nil {
		return types.VibeSearchResponse{}, fmt.Errorf("vibe ann: %w", err)
	}

	// 4. Optional personalised re-ranking via diary + fast-finish centroids
	personalised := false
	var vibeProfile *UserSignalProfile
	useV2 := s.flags.Bool(ctx, "ranking_v2")

	if userID != "" {
		p, err := s.GetOrComputeSignalProfile(ctx, userID)
		if err == nil && (p.DiaryEmbedding != nil || p.FastFinishEmbedding != nil) {
			vibeProfile = &p
			personalised = true
		}
	}

	type scored struct {
		row   db.VibeSearchBooksRow
		score float64
	}

	items := make([]scored, len(candidates))
	for i, c := range candidates {
		items[i] = scored{
			row:   c,
			score: vibeScore(c.SimilarityScore, c.Embedding.Slice(), vibeProfile, useV2),
		}
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].score > items[j].score
	})

	// Deduplicate by normalised title — keep highest-scoring edition.
	seen := make(map[string]bool, len(items))
	deduped := items[:0]
	for _, it := range items {
		key := vibeDedupeKey(it.row.Title, it.row.Authors)
		if !seen[key] {
			seen[key] = true
			deduped = append(deduped, it)
		}
	}
	items = deduped

	if len(items) > limit {
		items = items[:limit]
	}

	// 5. Build response
	re := &ReasonEngine{}
	results := make([]types.VibeBookResult, len(items))
	for i, sc := range items {
		vibeCandidate := Candidate{FinalScore: float32(sc.score)}
		reason := re.Build(vibeCandidate, vibeProfile, query)
		results[i] = vibeBookRowToResult(sc.row, sc.score, reason)
	}

	resp := types.VibeSearchResponse{
		Kind:         "vibe#search",
		Query:        query,
		TotalItems:   len(results),
		Personalised: personalised,
		Items:        results,
	}

	// 6. Cache result (empty results cached too — avoids hammering Cohere on bad queries)
	if s.redisClient != nil {
		if data, err := json.Marshal(resp); err == nil {
			s.redisClient.Set(ctx, cacheKey, data, vibeCacheTTL)
		}
	}

	return resp, nil
}

// vibeCacheKey returns "vibe:{userID}:{sha256(query)[0:8]}" or "vibe:anon:{hash}".
func vibeCacheKey(userID, query string) string {
	h := sha256.Sum256([]byte(query))
	hash := fmt.Sprintf("%x", h[:8])
	if userID == "" {
		return "vibe:anon:" + hash
	}
	return fmt.Sprintf("vibe:%s:%s", userID, hash)
}

func vibeBookRowToResult(r db.VibeSearchBooksRow, score float64, reason ReasonResult) types.VibeBookResult {
	vi := types.VolumeInfo{
		Title:      r.Title,
		Authors:    r.Authors,
		Categories: r.Categories,
	}
	if vi.Authors == nil {
		vi.Authors = []string{}
	}
	if vi.Categories == nil {
		vi.Categories = []string{}
	}
	if r.Description.Valid {
		vi.Description = r.Description.String
	}
	if r.Subtitle.Valid {
		vi.Subtitle = r.Subtitle.String
	}
	if r.Publisher.Valid {
		vi.Publisher = r.Publisher.String
	}
	if r.PublishedDate.Valid {
		vi.PublishedDate = r.PublishedDate.Time.Format("2006-01-02")
	}
	if r.PageCount.Valid {
		vi.PageCount = int(r.PageCount.Int32)
	}
	if r.Language.Valid {
		vi.Language = r.Language.String
	}
	if r.CoverUrl.Valid {
		vi.ImageLinks = types.ImageLinks{
			Thumbnail: r.CoverUrl.String,
			Small:     r.CoverUrl.String,
			Medium:    r.CoverUrl.String,
		}
	}
	if r.AverageRating.Valid {
		v := r.AverageRating.Float64
		vi.AverageRating = &v
	}
	if r.RatingsCount.Valid {
		v := int(r.RatingsCount.Int32)
		vi.RatingsCount = &v
	}

	stats := types.PaperboxdStats{}
	if r.TotalReadsCount.Valid {
		v := int(r.TotalReadsCount.Int32)
		stats.TotalReads = &v
	}
	if r.TotalTbrCount.Valid {
		v := int(r.TotalTbrCount.Int32)
		stats.TotalTBR = &v
	}

	return types.VibeBookResult{
		ID:              r.ID.String(),
		Slug:            r.Slug,
		APISource:       "paperboxd",
		FromCache:       true,
		VolumeInfo:      vi,
		PaperboxdStats:  stats,
		SimilarityScore: score,
		MatchReason:     reason.Text,
		ReasonType:      reason.Type,
	}
}

// vibeScore blends query similarity with diary and fast-finish centroids.
// useV2=false: legacy 0.6/0.4 split (query + diary). useV2=true: 0.60/0.25/0.15.
func vibeScore(querySim float64, bookEmb []float32, profile *UserSignalProfile, useV2 bool) float64 {
	if profile == nil {
		return querySim
	}
	if !useV2 {
		if profile.DiaryEmbedding != nil && len(bookEmb) > 0 {
			return 0.6*querySim + 0.4*util.CosineSimilarity(bookEmb, profile.DiaryEmbedding)
		}
		return querySim
	}
	score := querySim * 0.60
	if profile.DiaryEmbedding != nil && len(bookEmb) > 0 {
		score += util.CosineSimilarity(bookEmb, profile.DiaryEmbedding) * 0.25
	}
	if profile.FastFinishEmbedding != nil && len(bookEmb) > 0 {
		score += util.CosineSimilarity(bookEmb, profile.FastFinishEmbedding) * 0.15
	}
	if score > 1.0 {
		score = 1.0
	}
	return score
}

// vibeDedupeKey normalises title+first-author into a deduplication key.
// Strips subtitles after ":" and "(", lowercases. Two editions of the same
// book by the same author produce the same key.
func vibeDedupeKey(title string, authors []string) string {
	t := strings.ToLower(title)
	if i := strings.Index(t, ":"); i != -1 {
		t = t[:i]
	}
	if i := strings.Index(t, "("); i != -1 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	author := ""
	if len(authors) > 0 {
		author = strings.ToLower(authors[0])
	}
	return t + "|" + author
}

