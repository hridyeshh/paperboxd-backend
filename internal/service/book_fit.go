package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/hridyesh/paperboxd-backend/internal/util"
)

// BookFit is the book page's "Why you'll like this": the same ranking,
// confidence and reason engine the home feed uses, run on one book for one
// reader. Phase 18's rule — every surface reinforces the same intelligence —
// only holds if the sentence under a book is the same sentence wherever it
// appears, so this deliberately builds nothing new; it hydrates one
// candidate and hands it to rankCandidates.
//
// Returns nil when the engine has nothing personal to say: "Picked for you"
// under a book the reader searched for themselves would be noise.
func (s *RecommendationService) BookFit(ctx context.Context, userID, bookID string) (*BookCandidate, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("invalid user id: %w", err)
	}
	bid, err := uuid.Parse(bookID)
	if err != nil {
		return nil, fmt.Errorf("invalid book id: %w", err)
	}

	var c Candidate
	err = s.pool.QueryRow(ctx, `
		SELECT id::text, title, authors, categories, COALESCE(cover_url, '')
		FROM books WHERE id = $1
	`, bid).Scan(&c.BookID, &c.Title, &c.Authors, &c.Categories, &c.CoverURL)
	if err != nil {
		return nil, err
	}
	c.Source = SourceVector

	profile, _ := s.GetOrComputeSignalProfile(ctx, userID)
	profile.Anchors = s.loadAnchors(ctx, uid)

	pool := []Candidate{c}
	s.hydrateForRanking(ctx, userID, pool)

	// Taste similarity for the vector term; the feed gets this from the ANN
	// query, a single book has to compute it.
	if taste, err := s.getUserTasteVector(ctx, uid); err == nil && taste != nil && len(pool[0].Embedding) == len(taste) {
		sim := float32(util.CosineSimilarity(pool[0].Embedding, taste))
		pool[0].VectorScore, pool[0].SimilarityScore = sim, sim
	}

	// Friends who shelved it, for the social line.
	rows, err := s.pool.Query(ctx, `
		SELECT u.username, bs.status = 'liked'
		FROM follows f
		JOIN bookshelf bs ON bs.user_id = f.following_id AND bs.book_id = $2
		JOIN users u ON u.id = f.following_id AND u.deleted_at IS NULL
		WHERE f.follower_id = $1 AND bs.status IN ('read', 'liked')
		LIMIT 10
	`, uid, bid)
	if err == nil {
		for rows.Next() {
			var name string
			var loved bool
			if rows.Scan(&name, &loved) != nil {
				continue
			}
			pool[0].FriendNames = append(pool[0].FriendNames, name)
			pool[0].SocialScore++
			if loved {
				pool[0].FriendLovedCount++
				pool[0].SocialScore++
			}
		}
		rows.Close()
	}

	ranked := s.rankCandidates(ctx, pool, profile)
	out := candidateToBookCandidate(ranked[0])
	if out.ReasonType == "favorites" || out.ReasonType == "cold" {
		return nil, nil
	}
	return &out, nil
}
