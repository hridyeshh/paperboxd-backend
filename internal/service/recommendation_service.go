package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// BookCandidate is a recommendation result returned to the handler.
type BookCandidate struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Authors         []string `json:"authors"`
	CoverURL        string   `json:"cover_url"`
	Categories      []string `json:"categories"`
	SimilarityScore float64  `json:"similarity_score"`
	Reason          string   `json:"reason,omitempty"`
}

// CandidateSource identifies which retrieval path surfaced a candidate.
type CandidateSource string

const (
	SourceVector   CandidateSource = "vector"
	SourceSocial   CandidateSource = "social"
	SourceFallback CandidateSource = "fallback"
)

// Candidate is the internal pool entry used during the ranking pipeline.
// BookCandidate is kept as the external API type for backwards compatibility.
type Candidate struct {
	BookID          string
	Title           string
	Authors         []string
	Categories      []string
	CoverURL        string
	VectorScore     float32         // cosine similarity from Path A
	SocialScore     float32         // friend-read score from Path B
	FinalScore      float32         // set during Phase 4 ranking
	Source          CandidateSource
	FriendNames     []string // friends who read/liked this book
	LikeCount       int
	TotalReads      int
	SimilarityScore float32 // kept for backwards-compatible JSON conversion
	Reason          string  // human-readable reason string
}

func candidateToBookCandidate(c Candidate) BookCandidate {
	score := float64(c.VectorScore)
	if c.FinalScore > 0 {
		score = float64(c.FinalScore)
	}
	return BookCandidate{
		ID:              c.BookID,
		Title:           c.Title,
		Authors:         c.Authors,
		CoverURL:        c.CoverURL,
		Categories:      c.Categories,
		SimilarityScore: score,
		Reason:          c.Reason,
	}
}

func toBookCandidates(candidates []Candidate) []BookCandidate {
	result := make([]BookCandidate, len(candidates))
	for i, c := range candidates {
		result[i] = candidateToBookCandidate(c)
	}
	return result
}

// BookRow is used internally when fetching books from the DB.
type BookRow struct {
	ID          string
	Title       string
	Subtitle    string
	Authors     []string
	Categories  []string
	Description string
	CoverURL    string
}

// vectorRow holds an embedding and the timestamp used for time-weighting.
type vectorRow struct {
	Embedding       []float32
	InteractionTime time.Time
}

// RecommendationService provides home and similar-book recommendations.
type RecommendationService struct {
	pool        *pgxpool.Pool
	embedder    Embedder
	redisClient *redis.Client
}

func NewRecommendationService(pool *pgxpool.Pool, embedder Embedder, redisClient *redis.Client) *RecommendationService {
	return &RecommendationService{pool: pool, embedder: embedder, redisClient: redisClient}
}

// ── Public API ────────────────────────────────────────────────────────────────

// GetHomeRecommendations returns up to 20 personalised books for the user via
// a two-path parallel funnel: vector similarity (Path A) + social graph (Path B).
func (s *RecommendationService) GetHomeRecommendations(ctx context.Context, userID string) ([]BookCandidate, string, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return s.fallback(ctx, userID), "fallback", nil
	}

	// 1. Cache hit — apply ranking on the cached raw pool and return.
	if cached, err := s.getCachedPool(ctx, userID); err == nil && len(cached) > 0 {
		profile, _ := s.GetOrComputeSignalProfile(ctx, userID)
		results := rankCandidates(cached, profile)
		results = s.filterSuppressed(ctx, userID, results)
		results = deduplicateCandidates(results)
		ranked := s.applyMMR(results, 20)
		ranked = s.blendExploration(ctx, userID, profile, ranked)
		return toBookCandidates(ranked), "vector", nil
	}

	// 2. Run both retrieval paths in parallel.
	type pathResult struct {
		candidates []Candidate
		err        error
	}
	vectorCh := make(chan pathResult, 1)
	socialCh := make(chan pathResult, 1)

	go func() {
		tasteVector, err := s.getUserTasteVector(ctx, uid)
		if err != nil || tasteVector == nil {
			vectorCh <- pathResult{nil, err}
			return
		}
		bcs, err := s.getVectorCandidates(ctx, uid, tasteVector, 200)
		if err != nil {
			vectorCh <- pathResult{nil, err}
			return
		}
		candidates := make([]Candidate, len(bcs))
		for i, bc := range bcs {
			candidates[i] = Candidate{
				BookID:          bc.ID,
				Title:           bc.Title,
				Authors:         bc.Authors,
				Categories:      bc.Categories,
				CoverURL:        bc.CoverURL,
				VectorScore:     float32(bc.SimilarityScore),
				SimilarityScore: float32(bc.SimilarityScore),
				Source:          SourceVector,
			}
		}
		vectorCh <- pathResult{candidates, nil}
	}()

	go func() {
		candidates, err := s.getSocialCandidates(ctx, userID, 100)
		socialCh <- pathResult{candidates, err}
	}()

	vectorResult := <-vectorCh
	socialResult := <-socialCh

	// 3. Determine source label.
	source := "fallback"
	if vectorResult.err == nil && len(vectorResult.candidates) > 0 {
		source = "vector"
	}
	if len(socialResult.candidates) > 0 {
		if source == "vector" {
			source = "vector+social"
		} else {
			source = "social"
		}
	}

	// 4. Merge into candidate pool (max 300).
	pool := mergeCandidatePools(vectorResult.candidates, socialResult.candidates, 300)

	// 5. Fallback if pool is empty.
	if len(pool) == 0 {
		return s.fallback(ctx, userID), "fallback", nil
	}

	// 6. Cache the raw pool before ranking mutates it.
	rawPool := make([]Candidate, len(pool))
	copy(rawPool, pool)
	go s.setCachedPool(context.Background(), userID, rawPool)

	// 7. Rank → suppress → dedup → MMR → exploration blend.
	profile, _ := s.GetOrComputeSignalProfile(ctx, userID)
	pool = rankCandidates(pool, profile)
	pool = s.filterSuppressed(ctx, userID, pool)
	pool = deduplicateCandidates(pool)
	ranked := s.applyMMR(pool, 20)
	ranked = s.blendExploration(ctx, userID, profile, ranked)

	return toBookCandidates(ranked), source, nil
}

// GetSimilarBooks returns up to 10 books similar to bookID, excluding the book itself.
func (s *RecommendationService) GetSimilarBooks(ctx context.Context, bookID, userID string) ([]BookCandidate, error) {
	bid, err := uuid.Parse(bookID)
	if err != nil {
		return nil, fmt.Errorf("invalid book id: %w", err)
	}

	embedding, err := s.getBookEmbedding(ctx, bid)
	if err != nil || embedding == nil {
		return s.getSameAuthorBooks(ctx, bid, 10)
	}

	uid, _ := uuid.Parse(userID)
	candidates, err := s.getVectorCandidatesForBook(ctx, uid, bid, embedding, 10)
	if err != nil {
		slog.Error("get similar books vector candidates", "error", err)
		return s.getSameAuthorBooks(ctx, bid, 10)
	}

	// Append up to 5 books that friends have read in the same primary category.
	if userID != "" {
		social, _ := s.getSimilarSocialBooks(ctx, userID, bid, 5)
		seen := make(map[string]bool, len(candidates))
		for _, c := range candidates {
			seen[c.ID] = true
		}
		for _, sc := range social {
			if !seen[sc.ID] && len(candidates) < 10 {
				candidates = append(candidates, sc)
				seen[sc.ID] = true
			}
		}
	}

	candidates = deduplicateByTitle(candidates)
	if len(candidates) > 10 {
		candidates = candidates[:10]
	}
	return candidates, nil
}

// TrackEvent records a user interaction event (click, impression, dismiss).
func (s *RecommendationService) TrackEvent(ctx context.Context, userID, bookID, eventType string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO events (user_id, book_id, event_type)
		VALUES ($1, $2, $3)
	`, userID, bookID, eventType)
	return err
}

// ── Social graph retrieval ────────────────────────────────────────────────────

// getSocialCandidates returns books read or liked by followed users, scored
// +1 per friend who read it and +2 per friend who liked it.
func (s *RecommendationService) getSocialCandidates(ctx context.Context, userID string, limit int) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT
			b.id::text,
			b.title,
			b.authors,
			b.categories,
			COALESCE(b.cover_url, ''),
			b.like_count,
			b.total_reads_count,
			SUM(CASE WHEN bs.status = 'liked' THEN 2 ELSE 1 END) AS social_score,
			ARRAY_AGG(DISTINCT u.username) AS friend_names
		FROM follows f
		JOIN bookshelf bs ON bs.user_id = f.following_id
		JOIN books b ON b.id = bs.book_id
		JOIN users u ON u.id = f.following_id
		WHERE f.follower_id = $1
		  AND bs.status IN ('read', 'liked')
		  AND b.id NOT IN (
		      SELECT book_id FROM bookshelf WHERE user_id = $1
		  )
		  AND b.embedding IS NOT NULL
		GROUP BY b.id, b.title, b.authors, b.categories,
		         b.cover_url, b.like_count, b.total_reads_count
		ORDER BY social_score DESC, b.like_count DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []Candidate
	for rows.Next() {
		var c Candidate
		var socialScore float64
		var likeCount, totalReads *int32
		if err := rows.Scan(
			&c.BookID, &c.Title, &c.Authors, &c.Categories,
			&c.CoverURL, &likeCount, &totalReads,
			&socialScore, &c.FriendNames,
		); err != nil {
			slog.Warn("scan social candidate", "error", err)
			continue
		}
		if likeCount != nil {
			c.LikeCount = int(*likeCount)
		}
		if totalReads != nil {
			c.TotalReads = int(*totalReads)
		}
		c.SocialScore = float32(socialScore)
		c.Source = SourceSocial
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

// getSimilarSocialBooks returns up to limit books that friends have read in the
// same primary category as the target book (used by GetSimilarBooks).
func (s *RecommendationService) getSimilarSocialBooks(ctx context.Context, userID string, bid uuid.UUID, limit int) ([]BookCandidate, error) {
	var categories []string
	if err := s.pool.QueryRow(ctx, `SELECT categories FROM books WHERE id = $1`, bid).Scan(&categories); err != nil || len(categories) == 0 {
		return nil, nil
	}
	primaryCategory := categories[0]

	rows, err := s.pool.Query(ctx, `
		SELECT b.id::text, b.title, b.authors, COALESCE(b.cover_url, ''),
		       b.categories, COUNT(*)::float8 AS friend_reads
		FROM follows f
		JOIN bookshelf bs ON bs.user_id = f.following_id
		JOIN books b ON b.id = bs.book_id
		WHERE f.follower_id = $1
		  AND $2 = ANY(b.categories)
		  AND b.id != $3
		  AND b.id NOT IN (
		      SELECT book_id FROM bookshelf WHERE user_id = $1
		  )
		GROUP BY b.id, b.title, b.authors, b.categories, b.cover_url
		ORDER BY friend_reads DESC
		LIMIT $4
	`, userID, primaryCategory, bid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BookCandidate
	for rows.Next() {
		var c BookCandidate
		var score float64
		if err := rows.Scan(&c.ID, &c.Title, &c.Authors, &c.CoverURL, &c.Categories, &score); err != nil {
			continue
		}
		c.SimilarityScore = score * 0.3
		out = append(out, c)
	}
	return out, rows.Err()
}

// ── Candidate pool ────────────────────────────────────────────────────────────

// mergeCandidatePools merges vector and social candidates into a single
// deduplicated pool capped at maxSize. Books in both paths retain both scores.
func mergeCandidatePools(vectorCandidates, socialCandidates []Candidate, maxSize int) []Candidate {
	seen := make(map[string]*Candidate, len(vectorCandidates)+len(socialCandidates))
	ordered := make([]string, 0, len(vectorCandidates)+len(socialCandidates))

	for i := range vectorCandidates {
		c := &vectorCandidates[i]
		seen[c.BookID] = c
		ordered = append(ordered, c.BookID)
	}

	for i := range socialCandidates {
		c := &socialCandidates[i]
		if existing, ok := seen[c.BookID]; ok {
			existing.SocialScore = c.SocialScore
			existing.FriendNames = c.FriendNames
			// Source stays SourceVector — book appeared in both paths
		} else {
			seen[c.BookID] = c
			ordered = append(ordered, c.BookID)
		}
	}

	result := make([]Candidate, 0, min(maxSize, len(ordered)))
	for _, id := range ordered {
		if len(result) >= maxSize {
			break
		}
		result = append(result, *seen[id])
	}
	return result
}

// ── Redis candidate cache ─────────────────────────────────────────────────────

func (s *RecommendationService) getCachedPool(ctx context.Context, userID string) ([]Candidate, error) {
	if s.redisClient == nil {
		return nil, errors.New("redis not available")
	}
	data, err := s.redisClient.Get(ctx, "rec:pool:"+userID).Bytes()
	if err != nil {
		return nil, err
	}
	var candidates []Candidate
	if err := json.Unmarshal(data, &candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (s *RecommendationService) setCachedPool(ctx context.Context, userID string, candidates []Candidate) {
	if s.redisClient == nil {
		return
	}
	data, err := json.Marshal(candidates)
	if err != nil {
		return
	}
	s.redisClient.Set(ctx, "rec:pool:"+userID, data, 30*time.Minute)
}

// InvalidateUserPool clears the candidate cache when the user's bookshelf changes.
func (s *RecommendationService) InvalidateUserPool(ctx context.Context, userID string) {
	if s.redisClient == nil {
		return
	}
	s.redisClient.Del(ctx, "rec:pool:"+userID)
}

// ── Pool-level ranking helpers ────────────────────────────────────────────────

// rankCandidates computes a FinalScore from four weighted signals and sets
// a human-readable Reason string on each candidate, then sorts by FinalScore.
func rankCandidates(candidates []Candidate, profile UserSignalProfile) []Candidate {
	for i := range candidates {
		c := &candidates[i]

		// Genre boost — up to 0.15
		genreBoost := 0.0
		for _, cat := range c.Categories {
			if w, ok := profile.GenreWeights[cat]; ok {
				genreBoost += w * 0.15
			}
		}
		if genreBoost > 0.15 {
			genreBoost = 0.15
		}

		// Author boost — up to 0.10
		authorBoost := 0.0
		for _, author := range c.Authors {
			if w, ok := profile.AuthorWeights[author]; ok {
				authorBoost += w * 0.10
			}
		}
		if authorBoost > 0.10 {
			authorBoost = 0.10
		}

		// Social boost — up to 0.20, normalised over 5 friends
		socialBoost := 0.0
		if c.SocialScore > 0 {
			socialBoost = math.Min(float64(c.SocialScore)/5.0, 1.0) * 0.20
		}

		// Recency/popularity boost — up to 0.05
		recencyBoost := 0.0
		if c.LikeCount+c.TotalReads > 0 {
			recencyBoost = math.Min(float64(c.LikeCount+c.TotalReads)/100.0, 1.0) * 0.05
		}

		c.FinalScore = float32(
			float64(c.VectorScore)*0.50 +
				socialBoost +
				genreBoost +
				authorBoost +
				recencyBoost,
		)

		c.Reason = buildReason(c, profile)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].FinalScore > candidates[j].FinalScore
	})
	return candidates
}

func buildReason(c *Candidate, profile UserSignalProfile) string {
	// Social signal is strongest
	if c.SocialScore > 0 && len(c.FriendNames) > 0 {
		switch len(c.FriendNames) {
		case 1:
			return fmt.Sprintf("%s read this", c.FriendNames[0])
		case 2:
			return fmt.Sprintf("%s & %s read this", c.FriendNames[0], c.FriendNames[1])
		default:
			return fmt.Sprintf("%s & %d others read this", c.FriendNames[0], len(c.FriendNames)-1)
		}
	}

	// Strong author match
	for _, author := range c.Authors {
		if w, ok := profile.AuthorWeights[author]; ok && w > 0.7 {
			return fmt.Sprintf("You read %s", author)
		}
	}

	// Strong genre match — highest-weight matching genre
	bestGenre := ""
	bestWeight := 0.0
	for _, cat := range c.Categories {
		if w, ok := profile.GenreWeights[cat]; ok && w > bestWeight {
			bestWeight = w
			bestGenre = cat
		}
	}
	if bestGenre != "" && bestWeight > 0.6 {
		return fmt.Sprintf("Matches your %s taste", bestGenre)
	}

	// Trending
	if c.LikeCount+c.TotalReads > 50 {
		return "Popular right now"
	}

	return "Picked for you"
}

// deduplicateCandidates removes duplicate editions by normalising titles.
func deduplicateCandidates(candidates []Candidate) []Candidate {
	seen := make(map[string]bool, len(candidates))
	result := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		key := strings.ToLower(c.Title)
		if idx := strings.Index(key, ":"); idx != -1 {
			key = key[:idx]
		}
		if idx := strings.Index(key, "("); idx != -1 {
			key = key[:idx]
		}
		key = strings.TrimSpace(key)
		if !seen[key] {
			seen[key] = true
			result = append(result, c)
		}
	}
	return result
}

// applyMMR applies Maximal Marginal Relevance diversity to a Candidate pool.
func (s *RecommendationService) applyMMR(candidates []Candidate, k int) []Candidate {
	if len(candidates) <= k {
		return candidates
	}
	combinedScore := func(c Candidate) float64 {
		return float64(c.VectorScore) + float64(c.SocialScore)*0.3
	}
	selected := make([]Candidate, 0, k)
	remaining := make([]Candidate, len(candidates))
	copy(remaining, candidates)

	for len(selected) < k && len(remaining) > 0 {
		bestIdx := 0
		bestScore := math.Inf(-1)
		for i, c := range remaining {
			maxSim := 0.0
			for _, sel := range selected {
				if sim := cosineSimilarityByScore(combinedScore(sel), combinedScore(c)); sim > maxSim {
					maxSim = sim
				}
			}
			score := 0.5*combinedScore(c) - 0.5*maxSim
			if score > bestScore {
				bestScore = score
				bestIdx = i
			}
		}
		selected = append(selected, remaining[bestIdx])
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
	}
	return selected
}

// ── Signal profiles ───────────────────────────────────────────────────────────

// GetOrComputeSignalProfile returns a cached profile if fresh (< 24h),
// otherwise recomputes from bookshelf and saves it asynchronously.
func (s *RecommendationService) GetOrComputeSignalProfile(ctx context.Context, userID string) (UserSignalProfile, error) {
	cached, err := s.getSignalProfileFromDB(ctx, userID)
	if err == nil && time.Since(cached.ComputedAt) < 24*time.Hour {
		return cached, nil
	}

	entries, err := s.getBookshelfWithMetadata(ctx, userID)
	if err != nil || len(entries) == 0 {
		return UserSignalProfile{UserID: userID}, nil
	}

	profile := ComputeUserSignalProfile(entries)

	go func() {
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.saveSignalProfile(saveCtx, profile); err != nil {
			slog.Warn("save signal profile", "error", err, "user_id", userID)
		}
	}()

	return profile, nil
}

func (s *RecommendationService) getBookshelfWithMetadata(ctx context.Context, userID string) ([]BookshelfEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT
		    bs.user_id::text,
		    bs.book_id::text,
		    bs.status,
		    bs.rating,
		    bs.finished_at,
		    bs.updated_at,
		    bs.created_at,
		    b.authors,
		    b.categories
		FROM bookshelf bs
		JOIN books b ON b.id = bs.book_id
		WHERE bs.user_id = $1
		  AND bs.status IN ('read', 'liked', 'pending')
		ORDER BY bs.updated_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []BookshelfEntry
	for rows.Next() {
		var e BookshelfEntry
		var rating *int32
		var finishedAt *time.Time
		var updatedAt, createdAt time.Time

		if err := rows.Scan(
			&e.UserID, &e.BookID, &e.Status, &rating,
			&finishedAt, &updatedAt, &createdAt,
			&e.Authors, &e.Categories,
		); err != nil {
			slog.Warn("scan bookshelf entry", "error", err)
			continue
		}
		if rating != nil {
			v := int(*rating)
			e.Rating = &v
		}
		e.FinishedAt = finishedAt
		e.UpdatedAt = updatedAt
		e.CreatedAt = createdAt
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *RecommendationService) getSignalProfileFromDB(ctx context.Context, userID string) (UserSignalProfile, error) {
	var profile UserSignalProfile
	var genreJSON, authorJSON []byte

	err := s.pool.QueryRow(ctx, `
		SELECT user_id::text, genre_weights, author_weights, computed_at
		FROM user_signal_profiles
		WHERE user_id = $1
	`, userID).Scan(&profile.UserID, &genreJSON, &authorJSON, &profile.ComputedAt)
	if err != nil {
		return UserSignalProfile{}, err
	}

	_ = json.Unmarshal(genreJSON, &profile.GenreWeights)
	_ = json.Unmarshal(authorJSON, &profile.AuthorWeights)
	return profile, nil
}

func (s *RecommendationService) saveSignalProfile(ctx context.Context, profile UserSignalProfile) error {
	genreJSON, _ := json.Marshal(profile.GenreWeights)
	authorJSON, _ := json.Marshal(profile.AuthorWeights)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_signal_profiles (user_id, genre_weights, author_weights, computed_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE SET
		    genre_weights  = EXCLUDED.genre_weights,
		    author_weights = EXCLUDED.author_weights,
		    computed_at    = EXCLUDED.computed_at
	`, profile.UserID, genreJSON, authorJSON, profile.ComputedAt)
	return err
}

// ── Embedding operations ──────────────────────────────────────────────────────

// SaveBookEmbedding persists a float32 slice as a pgvector in the books table.
func (s *RecommendationService) SaveBookEmbedding(ctx context.Context, bookID string, embedding []float32) error {
	vec := float32SliceToLiteral(embedding)
	_, err := s.pool.Exec(ctx,
		`UPDATE books SET embedding = $1::vector WHERE id = $2`,
		vec, bookID,
	)
	return err
}

// GetBooksWithoutEmbeddings returns up to 500 books that have no embedding yet.
func (s *RecommendationService) GetBooksWithoutEmbeddings(ctx context.Context) ([]BookRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, title, COALESCE(subtitle, ''), authors, categories,
		       COALESCE(description, ''), COALESCE(cover_url, '')
		FROM books
		WHERE embedding IS NULL
		LIMIT 500
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []BookRow
	for rows.Next() {
		var b BookRow
		if err := rows.Scan(&b.ID, &b.Title, &b.Subtitle, &b.Authors, &b.Categories, &b.Description, &b.CoverURL); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

// filterSuppressed removes books the user has already seen 3+ times recently.
func (s *RecommendationService) filterSuppressed(ctx context.Context, userID string, candidates []Candidate) []Candidate {
	rows, err := s.pool.Query(ctx, `
		SELECT book_id::text
		FROM recommendation_impressions
		WHERE user_id = $1
		  AND seen_count >= 3
		  AND suppress_until > NOW()
	`, userID)
	if err != nil {
		return candidates
	}
	defer rows.Close()

	suppressed := make(map[string]bool)
	for rows.Next() {
		var bookID string
		if err := rows.Scan(&bookID); err == nil {
			suppressed[bookID] = true
		}
	}
	if len(suppressed) == 0 {
		return candidates
	}

	filtered := candidates[:0]
	for _, c := range candidates {
		if !suppressed[c.BookID] {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

// getExplorationCandidates returns popular books from genres the user has never engaged with.
func (s *RecommendationService) getExplorationCandidates(ctx context.Context, userID string, profile UserSignalProfile, limit int) []Candidate {
	knownGenres := make([]string, 0, len(profile.GenreWeights))
	for g := range profile.GenreWeights {
		knownGenres = append(knownGenres, g)
	}
	if len(knownGenres) == 0 {
		return nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT b.id::text, b.title, b.authors, b.categories,
		       COALESCE(b.cover_url, ''),
		       COALESCE(b.like_count, 0), COALESCE(b.total_reads_count, 0)
		FROM books b
		WHERE b.embedding IS NOT NULL
		  AND b.id NOT IN (
		      SELECT book_id FROM bookshelf WHERE user_id = $1
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM unnest(b.categories) AS cat
		      WHERE cat = ANY($2::text[])
		  )
		ORDER BY b.total_reads_count DESC NULLS LAST
		LIMIT $3
	`, userID, knownGenres, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var candidates []Candidate
	for rows.Next() {
		var c Candidate
		var likeCount, totalReads int32
		if err := rows.Scan(
			&c.BookID, &c.Title, &c.Authors, &c.Categories,
			&c.CoverURL, &likeCount, &totalReads,
		); err != nil {
			continue
		}
		c.LikeCount = int(likeCount)
		c.TotalReads = int(totalReads)
		c.Source = SourceFallback
		c.Reason = "Something different"
		candidates = append(candidates, c)
	}
	return candidates
}

// blendExploration replaces the last 2 slots of ranked with exploration picks.
func (s *RecommendationService) blendExploration(ctx context.Context, userID string, profile UserSignalProfile, ranked []Candidate) []Candidate {
	exploration := s.getExplorationCandidates(ctx, userID, profile, 4)
	if len(exploration) == 0 {
		return ranked
	}
	cutoff := len(ranked) - 2
	if cutoff < 0 {
		cutoff = 0
	}
	end := 2
	if len(exploration) < end {
		end = len(exploration)
	}
	return append(ranked[:cutoff], exploration[:end]...)
}

// UpdateImpressions records or increments that a user saw a recommendation.
// When seen_count reaches 3 the book is suppressed for 24 h.
func (s *RecommendationService) UpdateImpressions(ctx context.Context, userID, bookID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO recommendation_impressions (user_id, book_id, seen_count, last_seen, suppress_until)
		VALUES ($1, $2, 1, NOW(), NULL)
		ON CONFLICT (user_id, book_id) DO UPDATE SET
		    seen_count     = recommendation_impressions.seen_count + 1,
		    last_seen      = NOW(),
		    suppress_until = CASE
		        WHEN recommendation_impressions.seen_count + 1 >= 3
		        THEN NOW() + INTERVAL '24 hours'
		        ELSE NULL
		    END
	`, userID, bookID)
	return err
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func (s *RecommendationService) getUserTasteVector(ctx context.Context, userID uuid.UUID) ([]float32, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.embedding::text,
		       COALESCE(bs.finished_at, bs.updated_at) AS interaction_time
		FROM bookshelf bs
		JOIN books b ON b.id = bs.book_id
		WHERE bs.user_id = $1
		  AND bs.status IN ('read', 'liked')
		  AND b.embedding IS NOT NULL
		ORDER BY COALESCE(bs.finished_at, bs.updated_at) DESC
		LIMIT 100
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vrows []vectorRow
	for rows.Next() {
		var lit string
		var interactionTime time.Time
		if err := rows.Scan(&lit, &interactionTime); err != nil {
			continue
		}
		vec, err := parsePGVectorLiteral(lit)
		if err != nil {
			continue
		}
		vrows = append(vrows, vectorRow{Embedding: vec, InteractionTime: interactionTime})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(vrows) == 0 {
		return nil, nil
	}
	return weightedAverageVectors(vrows), nil
}

// weightedAverageVectors computes a time-weighted average embedding.
// Books read in the last 90 days get full weight, last year 50%, older 25%.
func weightedAverageVectors(rows []vectorRow) []float32 {
	if len(rows) == 0 {
		return nil
	}
	now := time.Now()
	result := make([]float32, len(rows[0].Embedding))
	totalWeight := 0.0

	for _, row := range rows {
		ageDays := now.Sub(row.InteractionTime).Hours() / 24
		var weight float64
		switch {
		case ageDays <= 90:
			weight = 1.0
		case ageDays <= 365:
			weight = 0.5
		default:
			weight = 0.25
		}
		for i, v := range row.Embedding {
			result[i] += float32(weight) * v
		}
		totalWeight += weight
	}

	if totalWeight > 0 {
		for i := range result {
			result[i] /= float32(totalWeight)
		}
	}
	return result
}

func (s *RecommendationService) getVectorCandidates(ctx context.Context, userID uuid.UUID, taste []float32, limit int) ([]BookCandidate, error) {
	return s.queryVectorCandidates(ctx, userID, uuid.Nil, taste, limit)
}

func (s *RecommendationService) getVectorCandidatesForBook(ctx context.Context, userID, excludeBookID uuid.UUID, vec []float32, limit int) ([]BookCandidate, error) {
	return s.queryVectorCandidates(ctx, userID, excludeBookID, vec, limit)
}

func (s *RecommendationService) queryVectorCandidates(ctx context.Context, userID, excludeBookID uuid.UUID, vec []float32, limit int) ([]BookCandidate, error) {
	vecLit := float32SliceToLiteral(vec)

	excludeClause := `b.id NOT IN (SELECT book_id FROM bookshelf WHERE user_id = $2)`
	args := []any{vecLit, userID, limit}
	argIdx := 4

	if excludeBookID != uuid.Nil {
		excludeClause += fmt.Sprintf(` AND b.id != $%d`, argIdx)
		args = append(args, excludeBookID)
	}

	query := fmt.Sprintf(`
		SELECT b.id::text, b.title, b.authors, COALESCE(b.cover_url, ''),
		       b.categories,
		       1 - (b.embedding <=> $1::vector) AS similarity_score
		FROM books b
		WHERE %s
		  AND b.embedding IS NOT NULL
		ORDER BY b.embedding <=> $1::vector
		LIMIT $3
	`, excludeClause)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BookCandidate
	for rows.Next() {
		var c BookCandidate
		if err := rows.Scan(&c.ID, &c.Title, &c.Authors, &c.CoverURL, &c.Categories, &c.SimilarityScore); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *RecommendationService) getBookEmbedding(ctx context.Context, bookID uuid.UUID) ([]float32, error) {
	var lit string
	err := s.pool.QueryRow(ctx,
		`SELECT embedding::text FROM books WHERE id = $1 AND embedding IS NOT NULL`,
		bookID,
	).Scan(&lit)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return parsePGVectorLiteral(lit)
}

func (s *RecommendationService) getSameAuthorBooks(ctx context.Context, bookID uuid.UUID, limit int) ([]BookCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b2.id::text, b2.title, b2.authors, COALESCE(b2.cover_url, ''), b2.categories, 0.5::float8
		FROM books b1
		JOIN books b2 ON b2.authors && b1.authors
		WHERE b1.id = $1
		  AND b2.id != $1
		LIMIT $2
	`, bookID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BookCandidate
	for rows.Next() {
		var c BookCandidate
		if err := rows.Scan(&c.ID, &c.Title, &c.Authors, &c.CoverURL, &c.Categories, &c.SimilarityScore); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// fallback returns popular books in the user's favourite genres (cold start).
// Never returns an error — always returns something.
func (s *RecommendationService) fallback(ctx context.Context, userID string) []BookCandidate {
	genres := s.getUserGenres(ctx, userID)
	if len(genres) > 0 {
		out := s.queryByGenres(ctx, genres)
		if len(out) > 0 {
			return out
		}
	}
	return s.queryRecentBooks(ctx)
}

func (s *RecommendationService) queryByGenres(ctx context.Context, genres []string) []BookCandidate {
	placeholders := make([]string, len(genres))
	args := make([]any, len(genres)+1)
	for i, g := range genres {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = strings.ToLower(g)
	}
	args[len(genres)] = 20

	query := fmt.Sprintf(`
		SELECT b.id::text, b.title, b.authors, COALESCE(b.cover_url, ''), b.categories, 0.0::float8
		FROM books b
		WHERE EXISTS (
		  SELECT 1 FROM unnest(b.categories) c
		  WHERE lower(c) = ANY(ARRAY[%s])
		)
		ORDER BY b.created_at DESC
		LIMIT $%d
	`, strings.Join(placeholders, ","), len(genres)+1)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		slog.Warn("fallback genre query failed", "error", err)
		return nil
	}
	defer rows.Close()
	return scanBookCandidates(rows)
}

func (s *RecommendationService) queryRecentBooks(ctx context.Context) []BookCandidate {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, title, authors, COALESCE(cover_url, ''), categories, 0.0::float8
		FROM books
		ORDER BY created_at DESC
		LIMIT 20
	`)
	if err != nil {
		slog.Warn("fallback recent-books query failed", "error", err)
		return nil
	}
	defer rows.Close()
	return scanBookCandidates(rows)
}

func scanBookCandidates(rows pgx.Rows) []BookCandidate {
	var out []BookCandidate
	for rows.Next() {
		var c BookCandidate
		if err := rows.Scan(&c.ID, &c.Title, &c.Authors, &c.CoverURL, &c.Categories, &c.SimilarityScore); err != nil {
			slog.Warn("scan book candidate", "error", err)
			continue
		}
		out = append(out, c)
	}
	return out
}

func (s *RecommendationService) getUserGenres(ctx context.Context, userID string) []string {
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(favorite_genres, '{}') FROM users WHERE id = $1`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	if rows.Next() {
		var genres []string
		_ = rows.Scan(&genres)
		return genres
	}
	return nil
}

func cosineSimilarityByScore(a, b float64) float64 {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return 1.0 - diff
}

// float32SliceToLiteral converts []float32 to the Postgres vector literal '[a,b,c]'.
func float32SliceToLiteral(v []float32) string {
	sb := strings.Builder{}
	sb.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%g", x)
	}
	sb.WriteByte(']')
	return sb.String()
}

// deduplicateByTitle removes duplicate editions of the same book by normalising
// titles to their base form (strips subtitle after ":" or "(").
func deduplicateByTitle(books []BookCandidate) []BookCandidate {
	seen := make(map[string]bool, len(books))
	result := make([]BookCandidate, 0, len(books))
	for _, b := range books {
		key := strings.ToLower(b.Title)
		if idx := strings.Index(key, ":"); idx != -1 {
			key = key[:idx]
		}
		if idx := strings.Index(key, "("); idx != -1 {
			key = key[:idx]
		}
		key = strings.TrimSpace(key)
		if !seen[key] {
			seen[key] = true
			result = append(result, b)
		}
	}
	return result
}

// parsePGVectorLiteral parses '[0.1,0.2,...]' returned by pgvector::text cast.
func parsePGVectorLiteral(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("unexpected vector format: %q", s)
	}
	parts := strings.Split(s[1:len(s)-1], ",")
	vec := make([]float32, len(parts))
	for i, p := range parts {
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%g", &f); err != nil {
			return nil, fmt.Errorf("parse element %d: %w", i, err)
		}
		vec[i] = float32(f)
	}
	return vec, nil
}
