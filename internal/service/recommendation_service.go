package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/util"
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
	ReasonType      string   `json:"reasonType,omitempty"`
	// Confidence is the human sentence, never the number — a percentage
	// invites arithmetic the engine cannot back up.
	Confidence  string `json:"confidence,omitempty"`
	IsHiddenGem bool   `json:"isHiddenGem,omitempty"`
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
	BookID      string
	Title       string
	Authors     []string
	Categories  []string
	CoverURL    string
	Embedding   []float32 // book embedding; populated before ranking for scoreV2
	VectorScore float32   // cosine similarity from Path A
	SocialScore float32   // friend-read score from Path B
	FinalScore  float32   // set during ranking
	Source      CandidateSource
	FriendNames []string // friends who read/liked this book
	// FriendLovedCount is how many of FriendNames liked it rather than only
	// read it — "loved this" is a stronger sentence and must be earned.
	FriendLovedCount int
	// LikeCount / TotalReads are counted live off bookshelf by
	// fetchCandidateCommunity; the books.* columns of the same name are dead.
	LikeCount  int
	TotalReads int
	// IsTrending: shelved by 2+ live accounts this week (getTrendingCandidates).
	IsTrending bool
	// TwinCount / TwinNames: taste twins who rated this 4+ (fetchTwinSignals).
	TwinCount int
	TwinNames []string
	// Anchor is the reader's own shelf book this candidate most resembles,
	// when one is close enough to name — see nearestAnchor.
	AnchorTitle string
	AnchorKind  string
	AnchorSim   float64
	// AnchorCount is how many loved shelf books clear anchorMinSim — three or
	// more is "this connects several books you rated highly".
	AnchorCount     int
	SimilarityScore float32 // kept for backwards-compatible JSON conversion
	Reason          string  // human-readable reason string
	ReasonType      string  // set by ReasonEngine, read by candidateToBookCandidate
	// scoreV2 outputs — set by rankCandidates, read by reason engine (Phase 5)
	VelocityBoost float32
	DiaryBoost    float32
	IsAbandoned   bool

	// Trait axes for this book, loaded by fetchCandidateTraits. Nil when the
	// book has not been through the extractor yet, which ranking treats as
	// "no trait term" rather than as a neutral book.
	Traits          map[string]float64
	TraitConfidence float64
	IsSeries        bool
	// TraitFit / TraitClash outputs, set during ranking and read by the reason
	// engine so the sentence a reader sees is derived from the same numbers
	// that ranked the book.
	TraitFitScore   float64
	TraitClashScore float64
	HasTraitFit     bool
	RecentFitScore  float64

	// Community signal, used to tell a hidden gem from an unread dud.
	AverageRating float64
	RatingsCount  int
	PageCount     int

	// Confidence and its human label, set during ranking.
	Confidence      float64
	ConfidenceLabel string
	IsHiddenGem     bool
}

func candidateToBookCandidate(c Candidate) BookCandidate {
	score := float64(c.VectorScore)
	if c.FinalScore > 0 {
		score = float64(c.FinalScore)
	}
	rt := c.ReasonType
	if rt == "" {
		rt = fallbackReasonType(c.Reason)
	}
	return BookCandidate{
		ID:              c.BookID,
		Title:           c.Title,
		Authors:         c.Authors,
		CoverURL:        c.CoverURL,
		Categories:      c.Categories,
		SimilarityScore: score,
		Reason:          c.Reason,
		ReasonType:      rt,
		Confidence:      c.ConfidenceLabel,
		IsHiddenGem:     c.IsHiddenGem,
	}
}

// fallbackReasonType infers a ReasonType from a legacy Reason string.
// Used for cached candidates that predate Phase 5.
func fallbackReasonType(reason string) string {
	switch {
	case strings.Contains(reason, "read this"), strings.Contains(reason, "loved this"):
		return "social"
	case strings.HasPrefix(reason, "You read"):
		return "author"
	case strings.HasPrefix(reason, "Matches your"):
		return "genre"
	case reason == "Picked for you":
		return "favorites"
	case reason == "Something different":
		return "explore"
	default:
		return "cold"
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

const (
	// homeRecCount is how many books one home request returns.
	homeRecCount = 20
	// diversityLambda weights relevance against variety inside diversify.
	// High enough that the first picks stay in score order — the top of the
	// page has to be the best books — low enough that the tail spreads out.
	diversityLambda = 0.72
)

// vectorRow holds an embedding and the timestamp used for time-weighting.
type vectorRow struct {
	Embedding       []float32
	InteractionTime time.Time
}

// RecommendationService provides home and similar-book recommendations.
type RecommendationService struct {
	pool        *pgxpool.Pool
	queries     *db.Queries
	embedder    Embedder
	redisClient *redis.Client
	flags       *FeatureFlags
	eventSvc    *EventService
	// reasoner writes the Ask Jazy match reasons. nil without an Anthropic key —
	// vibe search then falls back to the templated ReasonEngine text.
	reasoner *ClaudeReasoner
	// traitExtractor reads the nine characteristic axes off a book. nil without
	// an Anthropic key, in which case books simply carry no traits and ranking
	// omits the trait term for them.
	traitExtractor *TraitExtractor
}

func NewRecommendationService(pool *pgxpool.Pool, embedder Embedder, redisClient *redis.Client, eventSvc *EventService, anthropicKey string) *RecommendationService {
	return &RecommendationService{
		pool:           pool,
		queries:        db.New(pool),
		embedder:       embedder,
		redisClient:    redisClient,
		flags:          NewFeatureFlags(pool),
		traitExtractor: NewTraitExtractor(anthropicKey),
		eventSvc:       eventSvc,
		reasoner:       NewClaudeReasoner(anthropicKey),
	}
}

// Embedder exposes the underlying embedder for use by CLI tools.
func (s *RecommendationService) Embedder() Embedder { return s.embedder }

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
		profile.Anchors = s.loadAnchors(ctx, uid)
		s.hydrateForRanking(ctx, userID, cached)
		results := s.rankCandidates(ctx, cached, profile)
		results = s.filterSuppressed(ctx, userID, results)
		results = deduplicateCandidates(results)
		ranked := diversify(results, homeRecCount, diversityLambda, knownAuthors(profile))
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

		// Hidden gems are close to the reader's taste but sit far enough down
		// the popularity curve that the similarity-ordered query above never
		// reaches them. Retrieved separately and merged so they compete on
		// ranking rather than on reach.
		gems := s.getHiddenGemCandidates(ctx, uid, tasteVector, 30)
		seen := make(map[string]bool, len(candidates))
		for _, c := range candidates {
			seen[c.BookID] = true
		}
		for _, g := range gems {
			if !seen[g.BookID] {
				candidates = append(candidates, g)
			}
		}

		vectorCh <- pathResult{candidates, nil}
	}()

	go func() {
		candidates, err := s.getSocialCandidates(ctx, userID, 100)
		socialCh <- pathResult{candidates, err}
	}()

	// Path C: the smaller sources. Cheap indexed queries, so one goroutine
	// runs them in sequence rather than four racing the pool.
	profile, _ := s.GetOrComputeSignalProfile(ctx, userID)
	profile.Anchors = s.loadAnchors(ctx, uid)
	otherCh := make(chan []Candidate, 1)
	go func() {
		var other []Candidate
		other = append(other, s.getAuthorCandidates(ctx, uid, profile, 30)...)
		other = append(other, s.getTBRCandidates(ctx, uid, profile.Anchors, 30)...)
		other = append(other, s.getTrendingCandidates(ctx, uid, 20)...)
		if s.flags.Bool(ctx, "taste_twins") {
			other = append(other, s.getTwinCandidates(ctx, uid, 30)...)
		}
		otherCh <- other
	}()

	vectorResult := <-vectorCh
	socialResult := <-socialCh
	otherCandidates := <-otherCh

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

	// 4. Merge into candidate pool (max 400). Vector first so the pool is
	// taste-ordered before the cap bites; the other sources add books the
	// taste query could not reach, and the cap leaves room for all of them.
	pool := mergeCandidatePools(400, vectorResult.candidates, socialResult.candidates, otherCandidates)

	// 5. Fallback if pool is empty.
	if len(pool) == 0 {
		return s.fallback(ctx, userID), "fallback", nil
	}

	// 6. Cache the raw pool before ranking mutates it.
	rawPool := make([]Candidate, len(pool))
	copy(rawPool, pool)
	go s.setCachedPool(context.Background(), userID, rawPool)

	// 7. Rank → suppress → dedup → diversify → exploration blend.
	s.hydrateForRanking(ctx, userID, pool)
	pool = s.rankCandidates(ctx, pool, profile)
	pool = s.filterSuppressed(ctx, userID, pool)
	pool = deduplicateCandidates(pool)
	ranked := diversify(pool, homeRecCount, diversityLambda, knownAuthors(profile))
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
//
// metadata is optional and carries the recommendation's reason_type. Without it
// /analytics/discovery cannot split the funnel by reason, which is the whole
// point of measuring: a reason with fewer clicks but more finished 4-star books
// is the better reason, and raw CTR cannot see that.
func (s *RecommendationService) TrackEvent(ctx context.Context, userID, bookID, eventType string, metadata map[string]any) error {
	if s.eventSvc == nil {
		return nil
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil
	}
	var bookIDPtr *uuid.UUID
	if bookID != "" {
		if bid, parseErr := uuid.Parse(bookID); parseErr == nil {
			bookIDPtr = &bid
		}
	}
	s.eventSvc.Emit(ctx, EmitParams{
		UserID:    uid,
		BookID:    bookIDPtr,
		EventType: eventType,
		Metadata:  metadata,
		Source:    "server",
	})
	return nil
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
			SUM(CASE WHEN bs.status = 'liked' THEN 2 ELSE 1 END) AS social_score,
			COUNT(*) FILTER (WHERE bs.status = 'liked') AS loved_count,
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
		GROUP BY b.id, b.title, b.authors, b.categories, b.cover_url
		ORDER BY social_score DESC
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
		var lovedCount int64
		if err := rows.Scan(
			&c.BookID, &c.Title, &c.Authors, &c.Categories,
			&c.CoverURL, &socialScore, &lovedCount, &c.FriendNames,
		); err != nil {
			slog.Warn("scan social candidate", "error", err)
			continue
		}
		c.FriendLovedCount = int(lovedCount)
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

// rankCandidates scores and sorts candidates using scoreV1 or scoreV2 based
// on the ranking_v2 feature flag.
func (s *RecommendationService) rankCandidates(ctx context.Context, candidates []Candidate, profile UserSignalProfile) []Candidate {
	useV2 := s.flags.Bool(ctx, "ranking_v2")
	if useV2 {
		slog.Debug("ranking using scoreV2", "candidates", len(candidates))
	}
	// negative_taste is a kill switch on the ranking *effect* only. Verdicts
	// and profile recomputes always run, so turning it off never loses data;
	// it just stops clash and dislike from moving scores while the extracted
	// traits are being sanity-checked.
	opts := scoreOptions{
		negativeTaste: s.flags.Bool(ctx, "negative_taste"),
		recentTaste:   s.flags.Bool(ctx, "recent_taste"),
	}
	re := &ReasonEngine{}
	for i := range candidates {
		c := &candidates[i]
		if useV2 {
			c.FinalScore = s.scoreV2(c, &profile, opts)
		} else {
			c.FinalScore = scoreV1(*c, profile)
		}

		// Confidence is computed after scoring and before the reason, because
		// a low-confidence pick should be introduced as a wild card rather
		// than asserted — the reason text is the same either way, the framing
		// around it is not.
		nearestAnchor(c, profile.Anchors)
		c.Confidence = RecConfidence(*c, &profile)
		c.ConfidenceLabel = ConfidenceLabel(c.Confidence)
		c.IsHiddenGem = IsHiddenGem(*c, c.AverageRating, c.RatingsCount)

		result := re.Build(*c, &profile, "")
		c.Reason = result.Text
		c.ReasonType = result.Type
		if c.IsHiddenGem {
			c.ReasonType = "hidden_gem"
			if result.Type == "favorites" || result.Type == "cold" {
				// Only override a reason that said nothing. A trait or social
				// reason is more specific than "hidden gem" and should win.
				c.Reason = "You probably haven't discovered this yet"
			}
		}
	}
	sortByScore(candidates)
	return candidates
}

// scoreV1 is the original ranking formula (four signals).
func scoreV1(c Candidate, profile UserSignalProfile) float32 {
	genreBoost := 0.0
	for _, cat := range c.Categories {
		if w, ok := profile.GenreWeights[cat]; ok {
			genreBoost += w * 0.15
		}
	}
	if genreBoost > 0.15 {
		genreBoost = 0.15
	}

	authorBoost := 0.0
	for _, author := range c.Authors {
		if w, ok := profile.AuthorWeights[author]; ok {
			authorBoost += w * 0.10
		}
	}
	if authorBoost > 0.10 {
		authorBoost = 0.10
	}

	socialBoost := 0.0
	if c.SocialScore > 0 {
		socialBoost = math.Min(float64(c.SocialScore)/5.0, 1.0) * 0.20
	}

	recencyBoost := 0.0
	if c.LikeCount+c.TotalReads > 0 {
		recencyBoost = math.Min(float64(c.LikeCount+c.TotalReads)/100.0, 1.0) * 0.05
	}

	return float32(float64(c.VectorScore)*0.50 + socialBoost + genreBoost + authorBoost + recencyBoost)
}

// Ranking weights. They are normalised by the weight of the signals that are
// actually present, so a book nobody has extracted traits for is not quietly
// penalised against one that has them.
const (
	wVector     = 0.30
	wSocial     = 0.17
	wTrait      = 0.14
	wTwin       = 0.10 // taste twins rated it 4+; social proof from strangers who read like you
	wRecent     = 0.08 // last-90-day trait fit; the roadmap's "recent taste"
	wGenre      = 0.08
	wAuthor     = 0.07
	wVelocity   = 0.06
	wDiary      = 0.06
	wQuality    = 0.05 // external average rating, discounted by ratings count
	wPopularity = 0.04 // Paperboxd shelf count, counted live

	// Penalties are applied after normalisation, in score units.
	penaltyAbandoned  = 0.10
	penaltyTraitClash = 0.12
)

// scoreOptions are the per-request flags scoreV2 reads.
type scoreOptions struct {
	negativeTaste bool
	recentTaste   bool
}

// scoreV2 is the multi-signal ranking formula. Sets VelocityBoost, DiaryBoost,
// TraitFitScore, TraitClashScore and IsAbandoned on c as side effects, so the
// reason engine can explain the book using the same numbers that ranked it.
//
// Every term is accumulated as a (value, weight) pair and divided by the weight
// present. Summing fixed-weight terms and letting absent ones contribute zero
// would mean a book with no extracted traits could never score above 0.85 while
// an extracted one could reach 1.0 — a ranking difference caused by the state of
// our own backfill rather than by anything about the book.
func (s *RecommendationService) scoreV2(c *Candidate, profile *UserSignalProfile, opts scoreOptions) float32 {
	var sum, weight float64

	add := func(value, w float64) {
		sum += value * w
		weight += w
	}

	add(float64(c.VectorScore), wVector)

	if c.SocialScore > 0 {
		add(math.Min(float64(c.SocialScore)/5.0, 1.0), wSocial)
	}

	if profile.GenreWeights != nil {
		var g float64
		for _, cat := range c.Categories {
			if w, ok := profile.GenreWeights[cat]; ok {
				g += w
			}
		}
		add(math.Min(g, 1.0), wGenre)
	}

	if profile.AuthorWeights != nil {
		var a float64
		for _, author := range c.Authors {
			if w, ok := profile.AuthorWeights[author]; ok {
				a += w
			}
		}
		add(math.Min(a, 1.0), wAuthor)
	}

	if c.TotalReads > 0 {
		add(math.Min(float64(c.TotalReads)/float64(popularShelfCount), 1.0), wPopularity)
	}

	if c.TwinCount > 0 {
		add(math.Min(float64(c.TwinCount)/3.0, 1.0), wTwin)
	}

	// Community quality is a prior, not a taste signal: it keeps a badly
	// reviewed book that happens to match from outranking a good one that
	// matches slightly less. Small weight so it can break ties, not set them.
	if q := qualityScore(c.AverageRating, c.RatingsCount); q > 0 {
		add(q, wQuality)
	}

	if profile.FastFinishEmbedding != nil && c.Embedding != nil {
		sim := util.CosineSimilarity(c.Embedding, profile.FastFinishEmbedding)
		c.VelocityBoost = float32(sim * wVelocity)
		add(sim, wVelocity)
	}

	if profile.DiaryEmbedding != nil && c.Embedding != nil {
		sim := util.CosineSimilarity(c.Embedding, profile.DiaryEmbedding)
		c.DiaryBoost = float32(sim * wDiary)
		add(sim, wDiary)
	}

	// Trait fit is scaled by the extractor's own confidence in the book: a
	// guess made from a two-line blurb should move the ranking less than a
	// reading of a full description.
	if fit, ok := TraitFit(c.Traits, profile.Traits); ok {
		c.TraitFitScore = fit
		c.HasTraitFit = true
		conf := c.TraitConfidence
		if conf <= 0 {
			conf = 0.5
		}
		add(fit, wTrait*conf)
	}

	// Recent taste: the same fit against the last 90 days. A reader whose
	// last ten books were all bleak is, right now, a reader of bleak books,
	// whatever their five-year average says. Separate term rather than a
	// blended profile so the two clocks stay readable on the dashboard.
	if opts.recentTaste {
		if fit, ok := TraitFit(c.Traits, profile.RecentTraits); ok {
			c.RecentFitScore = fit
			conf := c.TraitConfidence
			if conf <= 0 {
				conf = 0.5
			}
			add(fit, wRecent*conf)
		}
	}

	if weight == 0 {
		return 0
	}
	score := sum / weight

	// Negative taste. A book that looks like what this reader has rejected is
	// pushed down even when it scores well on genre and author — which is the
	// whole point: "likes historical fiction, bounces off plot-dense
	// historical fiction" is invisible to every positive signal above.
	if clash, ok := TraitClash(c.Traits, profile.Traits); ok && clash > 0.5 {
		c.TraitClashScore = clash
		if opts.negativeTaste {
			score -= (clash - 0.5) * 2 * penaltyTraitClash
		}
	}

	if profile.VelocitySignal != nil {
		for _, id := range profile.VelocitySignal.AbandonedBookIDs {
			if id == c.BookID {
				score -= penaltyAbandoned
				c.IsAbandoned = true
				break
			}
		}
	}

	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return float32(score)
}

// knownAuthors is the lower-cased set of authors already on the reader's
// shelf, for the familiar-vs-new cap in diversify.
func knownAuthors(p UserSignalProfile) map[string]bool {
	out := make(map[string]bool, len(p.AuthorWeights))
	for a := range p.AuthorWeights {
		out[strings.ToLower(a)] = true
	}
	return out
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

	if profile.VelocitySignal != nil {
		ffEmb, ffErr := s.ComputeFastFinishEmbedding(ctx, profile.VelocitySignal)
		if ffErr != nil {
			slog.Warn("compute fast finish embedding", "error", ffErr, "user_id", userID)
		} else {
			profile.FastFinishEmbedding = ffEmb
		}
	}

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
	var genreJSON, authorJSON, velocityJSON []byte
	var traitPrefsJSON, traitDislikesJSON, traitConfJSON []byte
	var recentPrefsJSON, recentConfJSON []byte
	var diaryVecLit, fastFinishVecLit *string

	err := s.pool.QueryRow(ctx, `
		SELECT user_id::text, genre_weights, author_weights, computed_at,
		       velocity_signal,
		       CASE WHEN diary_embedding IS NOT NULL THEN diary_embedding::text END,
		       CASE WHEN fast_finish_embedding IS NOT NULL THEN fast_finish_embedding::text END,
		       trait_prefs, trait_dislikes, trait_confidence,
		       trait_recent, trait_recent_confidence
		FROM user_signal_profiles
		WHERE user_id = $1
	`, userID).Scan(
		&profile.UserID, &genreJSON, &authorJSON, &profile.ComputedAt,
		&velocityJSON, &diaryVecLit, &fastFinishVecLit,
		&traitPrefsJSON, &traitDislikesJSON, &traitConfJSON,
		&recentPrefsJSON, &recentConfJSON,
	)
	if err != nil {
		return UserSignalProfile{}, err
	}

	_ = json.Unmarshal(genreJSON, &profile.GenreWeights)
	_ = json.Unmarshal(authorJSON, &profile.AuthorWeights)
	if len(velocityJSON) > 0 {
		var vel VelocitySignal
		if json.Unmarshal(velocityJSON, &vel) == nil {
			profile.VelocitySignal = &vel
		}
	}
	if diaryVecLit != nil {
		if vec, err := parsePGVectorLiteral(*diaryVecLit); err == nil {
			profile.DiaryEmbedding = vec
		}
	}
	if fastFinishVecLit != nil {
		if vec, err := parsePGVectorLiteral(*fastFinishVecLit); err == nil {
			profile.FastFinishEmbedding = vec
		}
	}
	// Only attach a trait profile when at least one axis has evidence behind
	// it. An all-zero-confidence profile would make TraitFit return a valid
	// looking score with nothing underneath it.
	if len(traitPrefsJSON) > 0 {
		var tp TraitProfile
		_ = json.Unmarshal(traitPrefsJSON, &tp.Prefs)
		_ = json.Unmarshal(traitDislikesJSON, &tp.Dislikes)
		_ = json.Unmarshal(traitConfJSON, &tp.Confidence)
		if tp.HasSignal() {
			profile.Traits = &tp
		}
	}
	if len(recentPrefsJSON) > 0 {
		var rp TraitProfile
		_ = json.Unmarshal(recentPrefsJSON, &rp.Prefs)
		_ = json.Unmarshal(recentConfJSON, &rp.Confidence)
		if rp.HasSignal() {
			profile.RecentTraits = &rp
		}
	}
	return profile, nil
}

func (s *RecommendationService) saveSignalProfile(ctx context.Context, profile UserSignalProfile) error {
	genreJSON, _ := json.Marshal(profile.GenreWeights)
	authorJSON, _ := json.Marshal(profile.AuthorWeights)
	velocityJSON, _ := json.Marshal(profile.VelocitySignal)

	var fastFinishArg interface{}
	if profile.FastFinishEmbedding != nil {
		fastFinishArg = float32SliceToLiteral(profile.FastFinishEmbedding)
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_signal_profiles (user_id, genre_weights, author_weights, velocity_signal, fast_finish_embedding, computed_at, signal_version)
		VALUES ($1, $2, $3, $4, $5::vector, $6, 1)
		ON CONFLICT (user_id) DO UPDATE SET
		    genre_weights         = EXCLUDED.genre_weights,
		    author_weights        = EXCLUDED.author_weights,
		    velocity_signal       = EXCLUDED.velocity_signal,
		    fast_finish_embedding = EXCLUDED.fast_finish_embedding,
		    signal_version        = user_signal_profiles.signal_version + 1,
		    computed_at           = EXCLUDED.computed_at
	`, profile.UserID, genreJSON, authorJSON, velocityJSON, fastFinishArg, profile.ComputedAt)
	return err
}

// ComputeFastFinishEmbedding returns the centroid of fast-finish book embeddings.
// Returns nil, nil when the user has no fast-finish books or none have embeddings.
func (s *RecommendationService) ComputeFastFinishEmbedding(ctx context.Context, vel *VelocitySignal) ([]float32, error) {
	if vel == nil || len(vel.FastFinishBookIDs) == 0 {
		return nil, nil
	}
	rows, err := s.queries.GetBookEmbeddingsByIDs(ctx, vel.FastFinishBookIDs)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	vecs := make([][]float32, 0, len(rows))
	for _, row := range rows {
		if slice := row.Embedding.Slice(); len(slice) > 0 {
			vecs = append(vecs, slice)
		}
	}
	return util.ComputeCentroid(vecs), nil
}

// fetchCandidateCommunity bulk-loads the global counts that separate a hidden
// gem from a book nobody finished.
//
// Loaded here rather than threaded through each retrieval path because the
// vector, social and fallback queries all produce candidates and all three
// would otherwise need the same four columns bolted on.
func (s *RecommendationService) fetchCandidateCommunity(ctx context.Context, candidates []Candidate) error {
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.BookID)
	}

	// Shelf counts come from bookshelf, not books.like_count /
	// total_reads_count — those columns have been 0 since migration 000003
	// and nothing writes them.
	rows, err := s.pool.Query(ctx, `
		SELECT b.id::text,
		       COALESCE(b.average_rating, 0),
		       COALESCE(b.ratings_count, 0),
		       COUNT(bs.id) FILTER (WHERE bs.status = 'liked'),
		       COUNT(bs.id),
		       COALESCE(b.page_count, 0)
		FROM books b
		LEFT JOIN bookshelf bs ON bs.book_id = b.id
		WHERE b.id = ANY($1::uuid[])
		GROUP BY b.id
	`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()

	type stats struct {
		avg    float64
		counts [4]int64
	}
	byID := make(map[string]stats, len(candidates))
	for rows.Next() {
		var id string
		var st stats
		if err := rows.Scan(&id, &st.avg, &st.counts[0], &st.counts[1], &st.counts[2], &st.counts[3]); err != nil {
			continue
		}
		byID[id] = st
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range candidates {
		st, ok := byID[candidates[i].BookID]
		if !ok {
			continue
		}
		candidates[i].AverageRating = st.avg
		candidates[i].RatingsCount = int(st.counts[0])
		candidates[i].LikeCount = int(st.counts[1])
		candidates[i].TotalReads = int(st.counts[2])
		candidates[i].PageCount = int(st.counts[3])
	}
	return nil
}

// getHiddenGemCandidates finds books close to the reader's taste that almost
// nobody has read but that the people who did read rated highly.
//
// A separate retrieval path because the main vector query orders by similarity
// alone, and popular books dominate any pool built that way — the gems are
// real candidates that simply never surface above them.
func (s *RecommendationService) getHiddenGemCandidates(ctx context.Context, userID uuid.UUID, taste []float32, limit int) []Candidate {
	if taste == nil {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT b.id::text, b.title, b.authors, COALESCE(b.cover_url, ''), b.categories,
		       1 - (b.embedding <=> $1::vector) AS similarity_score,
		       COALESCE(b.average_rating, 0), COALESCE(b.ratings_count, 0)
		FROM books b
		WHERE b.embedding IS NOT NULL
		  AND b.id NOT IN (SELECT book_id FROM bookshelf WHERE user_id = $2)
		  AND COALESCE(b.ratings_count, 0) BETWEEN 5 AND $3
		  AND COALESCE(b.average_rating, 0) >= $4
		ORDER BY b.embedding <=> $1::vector
		LIMIT $5
	`, float32SliceToLiteral(taste), userID, hiddenGemMaxGlobalRatings, hiddenGemMinRating, limit)
	if err != nil {
		slog.Warn("hidden gem candidates", "error", err)
		return nil
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		var sim float64
		if err := rows.Scan(&c.BookID, &c.Title, &c.Authors, &c.CoverURL, &c.Categories,
			&sim, &c.AverageRating, &c.RatingsCount); err != nil {
			continue
		}
		c.VectorScore = float32(sim)
		c.SimilarityScore = float32(sim)
		c.Source = SourceVector
		out = append(out, c)
	}
	return out
}

// hydrateForRanking loads the per-book data the active ranking formula needs.
//
// Both lookups are one indexed query over the whole pool, and both are
// best-effort: a failure means the corresponding term is skipped for every
// candidate equally, which degrades ranking rather than breaking the page.
func (s *RecommendationService) hydrateForRanking(ctx context.Context, userID string, candidates []Candidate) {
	if s.flags.Bool(ctx, "ranking_v2") {
		if err := s.fetchCandidateEmbeddings(ctx, candidates); err != nil {
			slog.Warn("fetch candidate embeddings", "error", err)
		}
	}
	if s.flags.Bool(ctx, "trait_ranking") {
		if err := s.fetchCandidateTraits(ctx, candidates); err != nil {
			slog.Warn("fetch candidate traits", "error", err)
		}
	}
	if err := s.fetchCandidateCommunity(ctx, candidates); err != nil {
		slog.Warn("fetch candidate community stats", "error", err)
	}
	if s.flags.Bool(ctx, "taste_twins") {
		s.fetchTwinSignals(ctx, userID, candidates)
	}
}

// fetchTwinSignals attaches "N readers with your taste loved this" evidence
// to every candidate a top twin rated 4+. One query over the pool.
func (s *RecommendationService) fetchTwinSignals(ctx context.Context, userID string, candidates []Candidate) {
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.BookID
	}
	signals, err := s.PeopleLikeYouLoved(ctx, userID, ids)
	if err != nil {
		slog.Warn("fetch twin signals", "error", err)
		return
	}
	for i := range candidates {
		if sig, ok := signals[candidates[i].BookID]; ok {
			candidates[i].TwinCount = sig.Count
			candidates[i].TwinNames = sig.Names
		}
	}
}

// fetchCandidateEmbeddings bulk-fetches book embeddings for all candidates
// and sets Candidate.Embedding. Missing books are silently skipped.
func (s *RecommendationService) fetchCandidateEmbeddings(ctx context.Context, candidates []Candidate) error {
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.BookID
	}
	rows, err := s.queries.GetBookEmbeddingsByIDs(ctx, ids)
	if err != nil {
		return err
	}
	index := make(map[string][]float32, len(rows))
	for _, row := range rows {
		if slice := row.Embedding.Slice(); len(slice) > 0 {
			index[row.ID] = slice
		}
	}
	for i := range candidates {
		if emb, ok := index[candidates[i].BookID]; ok {
			candidates[i].Embedding = emb
		}
	}
	return nil
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

// SaveBookEmbeddingWithText persists the embedding vector and the composite text that was embedded.
func (s *RecommendationService) SaveBookEmbeddingWithText(ctx context.Context, bookID, embedText string, embedding []float32) error {
	vec := float32SliceToLiteral(embedding)
	_, err := s.pool.Exec(ctx,
		`UPDATE books SET embedding = $1::vector, embedding_text = $2 WHERE id = $3`,
		vec, embedText, bookID,
	)
	return err
}

// EmbedBookAsync enriches a newly cached book's description (if thin) then embeds it.
// Designed to run as a fire-and-forget goroutine using context.Background().
func (s *RecommendationService) EmbedBookAsync(book EnrichableBook, enricher *Enricher) {
	ctx := context.Background()

	desc := book.Description
	descSource := ""

	// Enrich thin or missing descriptions before embedding
	if enricher != nil && len(desc) < 200 {
		enrichedDesc, src, err := enricher.EnrichBookDescription(ctx, book)
		if err == nil && len(enrichedDesc) > len(desc) {
			desc = enrichedDesc
			descSource = src
			if _, dbErr := s.pool.Exec(ctx,
				`UPDATE books SET description = $1, description_source = $2 WHERE id = $3`,
				desc, descSource, book.ID,
			); dbErr != nil {
				slog.Warn("save enriched description failed", "book_id", book.ID, "error", dbErr)
			}
		}
	}

	embedText := BookEmbedText(book.Title, book.Subtitle, book.Authors, book.Publisher, book.Categories, desc)

	vecs, err := s.embedder.EmbedTexts([]string{embedText}, "search_document")
	if err != nil || len(vecs) == 0 {
		slog.Warn("embed book failed", "book_id", book.ID, "error", err)
		return
	}

	if err := s.SaveBookEmbeddingWithText(ctx, book.ID, embedText, vecs[0]); err != nil {
		slog.Warn("save book embedding failed", "book_id", book.ID, "error", err)
		return
	}

	slog.Debug("embedded book", "book_id", book.ID, "title", book.Title, "source", descSource)

	// Extract traits from the same (possibly just-enriched) description while
	// we have it. Doing it here rather than leaving every new book to the
	// nightly backfill means a book added today can carry a trait-based reason
	// the first time it is recommended.
	s.extractTraitsForBook(ctx, book, desc)
}

// extractTraitsForBook runs the trait extractor for a single freshly ingested
// book. Best-effort: a failure leaves the book without traits, which ranking
// already handles, and the backfill queue will retry it.
func (s *RecommendationService) extractTraitsForBook(ctx context.Context, book EnrichableBook, desc string) {
	if s.traitExtractor == nil {
		return
	}
	// Below this the model is guessing from a title, and a low-confidence guess
	// still costs a call. Matches the floor in GetBooksNeedingTraits.
	if len(desc) < 80 {
		return
	}

	traits, err := s.traitExtractor.Extract(ctx, []TraitInput{{
		BookID:      book.ID,
		Title:       book.Title,
		Authors:     book.Authors,
		Categories:  book.Categories,
		Description: desc,
	}})
	if err != nil || len(traits) != 1 {
		slog.Warn("extract book traits failed", "book_id", book.ID, "error", err)
		return
	}
	if err := s.SaveBookTraits(ctx, book.ID, traits[0]); err != nil {
		slog.Warn("save book traits failed", "book_id", book.ID, "error", err)
	}
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

// GetBooksForEnrichment returns books with thin or missing descriptions (for the --enrich backfill pass).
// limit=0 means no limit (returns all matching books).
func (s *RecommendationService) GetBooksForEnrichment(ctx context.Context, limit int) ([]EnrichableBook, error) {
	query := `
		SELECT id, title, COALESCE(subtitle,''), authors, COALESCE(publisher,''),
		       categories, COALESCE(description,''),
		       COALESCE(open_library_id,''), COALESCE(isbndb_id,''),
		       COALESCE(isbn_13,''), COALESCE(google_books_id,''),
		       COALESCE(metadata, '{}')
		FROM books
		WHERE embedding IS NULL
		   OR description IS NULL
		   OR description = ''
		   OR LENGTH(description) < 200
	`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []EnrichableBook
	for rows.Next() {
		var b EnrichableBook
		var metadata []byte
		if err := rows.Scan(
			&b.ID, &b.Title, &b.Subtitle, &b.Authors, &b.Publisher,
			&b.Categories, &b.Description,
			&b.OpenLibraryID, &b.ISBNdbID, &b.ISBN13, &b.GoogleBooksID,
			&metadata,
		); err != nil {
			return nil, err
		}
		b.Metadata = metadata
		books = append(books, b)
	}
	return books, rows.Err()
}

// filterSuppressed removes books the user has already seen 3+ times recently.
func (s *RecommendationService) filterSuppressed(ctx context.Context, userID string, candidates []Candidate) []Candidate {
	// Three reasons a book is held back, with different lifetimes:
	//   - seen too often without acting (24h rest, set by UpdateImpressions)
	//   - "not now" / "maybe" (a dated rest, set by RecordFeedback)
	//   - "not for me" / "already read" (permanent)
	// Re-showing something a reader explicitly dismissed is the fastest way to
	// teach them that the feedback controls are decorative.
	rows, err := s.pool.Query(ctx, `
		SELECT book_id::text
		FROM recommendation_impressions
		WHERE user_id = $1
		  AND (
		       (seen_count >= 3 AND suppress_until > NOW())
		    OR dismissed_forever
		    OR dismissed_until > NOW()
		  )
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

// getExplorationCandidates returns well-known books from genres the user has
// never engaged with. Ordered by external ratings count: the old ORDER BY
// total_reads_count sorted on a column that is always 0, i.e. not at all.
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
		       COALESCE(b.cover_url, '')
		FROM books b
		WHERE b.embedding IS NOT NULL
		  AND b.id NOT IN (
		      SELECT book_id FROM bookshelf WHERE user_id = $1
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM unnest(b.categories) AS cat
		      WHERE cat = ANY($2::text[])
		  )
		ORDER BY COALESCE(b.ratings_count, 0) DESC
		LIMIT $3
	`, userID, knownGenres, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var candidates []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(
			&c.BookID, &c.Title, &c.Authors, &c.Categories, &c.CoverURL,
		); err != nil {
			continue
		}
		c.Source = SourceFallback
		c.Reason = "Something different"
		c.ReasonType = "explore"
		candidates = append(candidates, c)
	}
	return candidates
}

// blendExploration replaces the last 2 slots of ranked with exploration picks.
func (s *RecommendationService) blendExploration(ctx context.Context, userID string, profile UserSignalProfile, ranked []Candidate) []Candidate {
	exploration := s.getExplorationCandidates(ctx, userID, profile, ExplorationSlots(homeRecCount)*2)
	if len(exploration) == 0 {
		return ranked
	}

	// Give the stretch picks a reason that names what *does* match, rather than
	// the bare "Something different". An unexplained exploration slot reads as
	// the algorithm giving up; "not your usual thing, but you love slow-burn
	// books you live inside for a while" reads as a deliberate choice — which
	// it is. Falls back to the flat label when no axis is confident enough.
	if profile.Traits != nil {
		if err := s.fetchCandidateTraits(ctx, exploration); err != nil {
			slog.Warn("fetch exploration traits", "error", err)
		}
		for i := range exploration {
			if text := buildExploreTraitReason(exploration[i], profile.Traits); text != "" {
				exploration[i].Reason = text
			}
		}
	}
	return InterleaveExploration(ranked, exploration, ExplorationSlots(len(ranked)))
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

// ExportFloat32Literal is the exported form of float32SliceToLiteral for CLI tools.
func ExportFloat32Literal(v []float32) string { return float32SliceToLiteral(v) }

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

// ── Diary embedding ───────────────────────────────────────────────────────────

// DiaryEmbedText builds the composite text for a diary entry embedding.
func DiaryEmbedText(bookTitle string, bookAuthors []string, content string) string {
	clean := StripHTML(content)
	var parts []string
	if bookTitle != "" {
		parts = append(parts, "Book: "+bookTitle)
	}
	if len(bookAuthors) > 0 {
		parts = append(parts, "Authors: "+strings.Join(bookAuthors, ", "))
	}
	if clean != "" {
		parts = append(parts, "Review: "+clean)
	}
	return strings.Join(parts, "\n")
}

// ClearDiaryEmbedding drops the stored vector and the cleartext embedding_text
// for one entry. Used when an entry becomes private: the centroid query filters
// on `embedding IS NOT NULL`, so clearing the vector also removes the entry from
// the next diary_embedding recompute without touching the aggregate directly.
func (s *RecommendationService) ClearDiaryEmbedding(ctx context.Context, entryID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE diary_entries SET embedding = NULL, embedding_text = NULL WHERE id = $1`,
		entryID,
	)
	return err
}

// EmbedDiaryEntryAsync embeds a diary entry and persists the vector.
// Designed to run as a fire-and-forget goroutine.
func (s *RecommendationService) EmbedDiaryEntryAsync(entryID, bookTitle string, bookAuthors []string, content string) {
	ctx := context.Background()
	embedText := DiaryEmbedText(bookTitle, bookAuthors, content)
	if embedText == "" {
		return
	}

	vecs, err := s.embedder.EmbedTexts([]string{embedText}, "search_document")
	if err != nil || len(vecs) == 0 {
		slog.Warn("embed diary entry failed", "entry_id", entryID, "error", err)
		return
	}

	vec := float32SliceToLiteral(vecs[0])
	_, err = s.pool.Exec(ctx,
		`UPDATE diary_entries SET embedding = $1::vector, embedding_text = $2 WHERE id = $3`,
		vec, embedText, entryID,
	)
	if err != nil {
		slog.Warn("save diary embedding failed", "entry_id", entryID, "error", err)
		return
	}
	slog.Debug("embedded diary entry", "entry_id", entryID)
}

// ── Diary centroid ────────────────────────────────────────────────────────────

// DiarySignal holds stats about a user's diary embedding state.
type DiarySignal struct {
	EntryCount         int    `json:"entry_count"`
	EmbeddedEntryCount int    `json:"embedded_entry_count"`
	LastEntryDate      string `json:"last_entry_date,omitempty"`
	HasCentroid        bool   `json:"has_centroid"`
}

// computeDiaryCentroid computes the element-wise mean of a set of embeddings.
func computeDiaryCentroid(embeddings [][]float32) []float32 {
	return util.ComputeCentroid(embeddings)
}

// ComputeAndSaveDiaryCentroid fetches diary embeddings for a user, computes the
// centroid, and persists it + diary_signal into user_signal_profiles.
func (s *RecommendationService) ComputeAndSaveDiaryCentroid(ctx context.Context, userID string) error {
	rows, err := s.pool.Query(ctx, `
		SELECT embedding_text, embedding::text
		FROM diary_entries
		WHERE user_id = $1 AND embedding IS NOT NULL
		ORDER BY created_at DESC
		LIMIT 50
	`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var embeddings [][]float32
	var lastDate string
	for rows.Next() {
		var embedText string
		var vecLiteral string
		if err := rows.Scan(&embedText, &vecLiteral); err != nil {
			continue
		}
		vec, err := parsePGVectorLiteral(vecLiteral)
		if err != nil {
			continue
		}
		embeddings = append(embeddings, vec)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Count all entries for the signal metadata
	var totalEntries int
	_ = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM diary_entries WHERE user_id = $1`, userID).Scan(&totalEntries)
	_ = s.pool.QueryRow(ctx, `SELECT TO_CHAR(MAX(created_at), 'YYYY-MM-DD') FROM diary_entries WHERE user_id = $1`, userID).Scan(&lastDate)

	signal := DiarySignal{
		EntryCount:         totalEntries,
		EmbeddedEntryCount: len(embeddings),
		LastEntryDate:      lastDate,
		HasCentroid:        len(embeddings) > 0,
	}
	signalJSON, _ := json.Marshal(signal)

	if len(embeddings) == 0 {
		// diary_embedding must be nulled, not merely left unset: a reader whose
		// only embedded entries have since been made private would otherwise keep
		// a centroid still derived from that private text.
		_, err = s.pool.Exec(ctx, `
			INSERT INTO user_signal_profiles (user_id, genre_weights, author_weights, diary_signal, signal_version)
			VALUES ($1, '{}', '{}', $2, 1)
			ON CONFLICT (user_id) DO UPDATE SET
			    diary_embedding = NULL,
			    diary_signal    = EXCLUDED.diary_signal,
			    signal_version  = user_signal_profiles.signal_version + 1,
			    computed_at     = NOW()
		`, userID, signalJSON)
		return err
	}

	centroid := computeDiaryCentroid(embeddings)
	vecLiteral := float32SliceToLiteral(centroid)

	_, err = s.pool.Exec(ctx, `
		INSERT INTO user_signal_profiles (user_id, genre_weights, author_weights, diary_embedding, diary_signal, signal_version)
		VALUES ($1, '{}', '{}', $2::vector, $3, 1)
		ON CONFLICT (user_id) DO UPDATE SET
		    diary_embedding = EXCLUDED.diary_embedding,
		    diary_signal    = EXCLUDED.diary_signal,
		    signal_version  = user_signal_profiles.signal_version + 1,
		    computed_at     = NOW()
	`, userID, vecLiteral, signalJSON)
	return err
}
