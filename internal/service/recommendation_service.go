package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BookCandidate is a recommendation result returned to the handler.
type BookCandidate struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Authors         []string `json:"authors"`
	CoverURL        string   `json:"cover_url"`
	Categories      []string `json:"categories"`
	SimilarityScore float64  `json:"similarity_score"`
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

// RecommendationService provides home and similar-book recommendations.
type RecommendationService struct {
	pool     *pgxpool.Pool
	embedder Embedder
}

func NewRecommendationService(pool *pgxpool.Pool, embedder Embedder) *RecommendationService {
	return &RecommendationService{pool: pool, embedder: embedder}
}

// ── Public API ────────────────────────────────────────────────────────────────

// GetHomeRecommendations returns up to 20 personalised books for the user.
// Falls back to popular books in the user's genres when no taste vector exists.
func (s *RecommendationService) GetHomeRecommendations(ctx context.Context, userID string) ([]BookCandidate, string, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return s.fallback(ctx, userID), "fallback", nil
	}

	tasteVector, err := s.getUserTasteVector(ctx, uid)
	if err != nil || tasteVector == nil {
		slog.Info("recommendation cold start", "user_id", userID)
		return s.fallback(ctx, userID), "fallback", nil
	}

	candidates, err := s.getVectorCandidates(ctx, uid, tasteVector, 200)
	if err != nil {
		slog.Error("get vector candidates", "error", err, "user_id", userID)
		return s.fallback(ctx, userID), "fallback", nil
	}

	if len(candidates) < 20 {
		extra := s.fallback(ctx, userID)
		seen := make(map[string]bool, len(candidates))
		for _, c := range candidates {
			seen[c.ID] = true
		}
		for _, e := range extra {
			if !seen[e.ID] {
				candidates = append(candidates, e)
			}
		}
	}

	candidates = deduplicateByTitle(candidates)
	candidates = mmrDiversify(candidates, 20)
	return candidates, "vector", nil
}

// GetSimilarBooks returns up to 10 books similar to bookID, excluding the book itself.
func (s *RecommendationService) GetSimilarBooks(ctx context.Context, bookID, userID string) ([]BookCandidate, error) {
	bid, err := uuid.Parse(bookID)
	if err != nil {
		return nil, fmt.Errorf("invalid book id: %w", err)
	}

	embedding, err := s.getBookEmbedding(ctx, bid)
	if err != nil || embedding == nil {
		// No embedding — fall back to same-author books.
		return s.getSameAuthorBooks(ctx, bid, 10)
	}

	uid, _ := uuid.Parse(userID)
	candidates, err := s.getVectorCandidatesForBook(ctx, uid, bid, embedding, 10)
	if err != nil {
		slog.Error("get similar books vector candidates", "error", err)
		return s.getSameAuthorBooks(ctx, bid, 10)
	}

	candidates = deduplicateByTitle(candidates)
	return candidates, nil
}

// ── Embedding operations ──────────────────────────────────────────────────────

// SaveBookEmbedding persists a float32 slice as a pgvector in the books table.
func (s *RecommendationService) SaveBookEmbedding(ctx context.Context, bookID string, embedding []float32) error {
	// Format vector as Postgres literal: '[0.1,0.2,...]'
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

// UpdateImpressions records or increments that a user saw a recommendation.
func (s *RecommendationService) UpdateImpressions(ctx context.Context, userID, bookID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO recommendation_impressions (user_id, book_id)
		VALUES ($1, $2)
		ON CONFLICT (user_id, book_id) DO UPDATE
		  SET seen_count = recommendation_impressions.seen_count + 1,
		      last_seen  = NOW()
	`, userID, bookID)
	return err
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func (s *RecommendationService) getUserTasteVector(ctx context.Context, userID uuid.UUID) ([]float32, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.embedding::text
		FROM bookshelf bs
		JOIN books b ON b.id = bs.book_id
		WHERE bs.user_id = $1
		  AND bs.status IN ('read', 'liked')
		  AND b.embedding IS NOT NULL
		ORDER BY bs.updated_at DESC
		LIMIT 50
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vecs [][]float32
	for rows.Next() {
		var lit string
		if err := rows.Scan(&lit); err != nil {
			continue
		}
		v, err := parsePGVectorLiteral(lit)
		if err != nil {
			continue
		}
		vecs = append(vecs, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	return averageVectors(vecs), nil
}

func (s *RecommendationService) getVectorCandidates(ctx context.Context, userID uuid.UUID, taste []float32, limit int) ([]BookCandidate, error) {
	return s.queryVectorCandidates(ctx, userID, uuid.Nil, taste, limit)
}

func (s *RecommendationService) getVectorCandidatesForBook(ctx context.Context, userID, excludeBookID uuid.UUID, vec []float32, limit int) ([]BookCandidate, error) {
	return s.queryVectorCandidates(ctx, userID, excludeBookID, vec, limit)
}

func (s *RecommendationService) queryVectorCandidates(ctx context.Context, userID, excludeBookID uuid.UUID, vec []float32, limit int) ([]BookCandidate, error) {
	vecLit := float32SliceToLiteral(vec)

	// Exclude books already on the user's shelf, and (optionally) the source book.
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
	// Try genre-based fallback first; if it returns 0 results, always run the
	// general recent-books query so the list is never empty just because the user's
	// genres don't overlap with the current catalogue.
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

// ── MMR diversity ─────────────────────────────────────────────────────────────

// mmrDiversify applies Maximal Marginal Relevance to pick k candidates that
// balance relevance (similarity_score) and diversity (λ = 0.5).
func mmrDiversify(candidates []BookCandidate, k int) []BookCandidate {
	if len(candidates) <= k {
		return candidates
	}

	selected := make([]BookCandidate, 0, k)
	remaining := make([]BookCandidate, len(candidates))
	copy(remaining, candidates)

	for len(selected) < k && len(remaining) > 0 {
		bestIdx := 0
		bestScore := math.Inf(-1)

		for i, c := range remaining {
			relevance := c.SimilarityScore
			maxSim := 0.0
			for _, s := range selected {
				sim := cosineSimilarityByScore(s.SimilarityScore, c.SimilarityScore)
				if sim > maxSim {
					maxSim = sim
				}
			}
			score := 0.5*relevance - 0.5*maxSim
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

// cosineSimilarityByScore is a lightweight proxy using pre-computed similarity scores.
func cosineSimilarityByScore(a, b float64) float64 {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return 1.0 - diff
}

// ── Vector maths ──────────────────────────────────────────────────────────────

func averageVectors(vecs [][]float32) []float32 {
	if len(vecs) == 0 {
		return nil
	}
	dim := len(vecs[0])
	avg := make([]float32, dim)
	for _, v := range vecs {
		for i, x := range v {
			avg[i] += x
		}
	}
	n := float32(len(vecs))
	for i := range avg {
		avg[i] /= n
	}
	return avg
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

// deduplicateByTitle removes duplicate editions of the same book.
// It normalises each title to its base form by stripping subtitles after ":" or "(",
// then keeps only the first occurrence of each normalised key.
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
