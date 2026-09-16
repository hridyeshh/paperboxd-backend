package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/hridyesh/paperboxd-backend/internal/util"
)

// Smart TBR answers "what should I read next?" from the shelf the reader
// already built. It is the home-feed ranker run over the TBR instead of the
// catalogue — same scores, same reasons, same confidence — plus two things
// only a TBR has: how long each book has waited, and whether it is shorter
// or longer than what this reader actually finishes.
//
// Plus-only; the handler gates.

// Buckets, in reading order.
const (
	TBRBucketReadNow  = "read_now"  // top of the ranking
	TBRBucketReadSoon = "read_soon" // next tier
	TBRBucketMaybe    = "maybe"     // the long tail
	TBRBucketStale    = "stale"     // untouched for a year: still want it?
)

// tbrStaleAfter is how long a TBR book can sit untouched before it is
// asked about rather than recommended.
const tbrStaleAfter = 365 * 24 * time.Hour

type SmartTBRItem struct {
	BookCandidate
	Bucket      string    `json:"bucket"`
	AddedAt     time.Time `json:"added_at"`
	DaysOnShelf int       `json:"days_on_shelf"`
	PageCount   int       `json:"page_count,omitempty"`
	// Lines are the "why now" the card shows under the reason: how long it
	// has waited, and how its length sits against the reader's habit.
	Lines []string `json:"lines"`
}

type SmartTBR struct {
	Next    *SmartTBRItem             `json:"next"`
	Queue   []SmartTBRItem            `json:"queue"` // "My Next 5"
	Buckets map[string][]SmartTBRItem `json:"buckets"`
	Total   int                       `json:"total"`
	// MedianPages is the reader's typical finished length, 0 when unknown.
	MedianPages int `json:"median_pages"`
}

// SmartTBR ranks the reader's TBR.
func (s *RecommendationService) SmartTBR(ctx context.Context, userID string) (SmartTBR, error) {
	out := SmartTBR{Buckets: map[string][]SmartTBRItem{}}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return out, fmt.Errorf("invalid user id: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT b.id::text, b.title, b.authors, b.categories, COALESCE(b.cover_url, ''), COALESCE(b.page_count, 0),
		       COALESCE(bs.tbr_added_at, bs.created_at), GREATEST(bs.updated_at, COALESCE(bs.tbr_added_at, bs.created_at))
		FROM bookshelf bs JOIN books b ON b.id = bs.book_id
		WHERE bs.user_id = $1 AND bs.status = 'tbr'
	`, uid)
	if err != nil {
		return out, err
	}
	type shelfMeta struct {
		added, touched time.Time
		pages          int
	}
	meta := map[string]shelfMeta{}
	var pool []Candidate
	for rows.Next() {
		var c Candidate
		var m shelfMeta
		if err := rows.Scan(&c.BookID, &c.Title, &c.Authors, &c.Categories, &c.CoverURL, &m.pages, &m.added, &m.touched); err != nil {
			continue
		}
		c.Source = SourceVector
		c.PageCount = m.pages
		meta[c.BookID] = m
		pool = append(pool, c)
	}
	rows.Close()
	out.Total = len(pool)
	if len(pool) == 0 {
		return out, nil
	}

	// Typical finished length, same query Fusion uses.
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY bk.page_count), 0)
		FROM bookshelf b JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = $1 AND b.status IN ('read', 'reading') AND bk.page_count > 0
		HAVING COUNT(*) >= 5
	`, uid).Scan(&out.MedianPages)

	profile, _ := s.GetOrComputeSignalProfile(ctx, userID)
	profile.Anchors = s.loadAnchors(ctx, uid)
	s.hydrateForRanking(ctx, userID, pool)
	if taste, err := s.getUserTasteVector(ctx, uid); err == nil && taste != nil {
		for i := range pool {
			if len(pool[i].Embedding) == len(taste) {
				sim := float32(util.CosineSimilarity(pool[i].Embedding, taste))
				pool[i].VectorScore, pool[i].SimilarityScore = sim, sim
			}
		}
	}
	ranked := s.rankCandidates(ctx, pool, profile)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].FinalScore > ranked[j].FinalScore })

	now := time.Now()
	items := make([]SmartTBRItem, 0, len(ranked))
	for i, c := range ranked {
		m := meta[c.BookID]
		it := SmartTBRItem{
			BookCandidate: candidateToBookCandidate(c),
			AddedAt:       m.added,
			DaysOnShelf:   int(now.Sub(m.added).Hours() / 24),
			PageCount:     m.pages,
		}
		// ponytail: buckets by rank position, not score thresholds — scores
		// are a blend with no fixed scale. Top fifth reads now, next fifth
		// soon, the rest maybe; a year untouched overrides all three.
		switch {
		case now.Sub(m.touched) > tbrStaleAfter:
			it.Bucket = TBRBucketStale
		case i < max(1, len(ranked)/5):
			it.Bucket = TBRBucketReadNow
		case i < max(2, 2*len(ranked)/5):
			it.Bucket = TBRBucketReadSoon
		default:
			it.Bucket = TBRBucketMaybe
		}
		it.Lines = tbrLines(it, out.MedianPages, profile.VelocitySignal)
		items = append(items, it)
	}

	for _, it := range items {
		out.Buckets[it.Bucket] = append(out.Buckets[it.Bucket], it)
		if it.Bucket != TBRBucketStale && len(out.Queue) < 5 {
			out.Queue = append(out.Queue, it)
		}
	}
	if len(out.Queue) > 0 {
		out.Next = &out.Queue[0]
	}
	return out, nil
}

// tbrLines writes the two "why now" lines for a card.
func tbrLines(it SmartTBRItem, medianPages int, v *VelocitySignal) []string {
	var lines []string
	switch d := it.DaysOnShelf; {
	case d >= 365:
		lines = append(lines, fmt.Sprintf("On your shelf %d months", d/30))
	case d >= 60:
		lines = append(lines, fmt.Sprintf("Added %d months ago", d/30))
	case d >= 14:
		lines = append(lines, fmt.Sprintf("Added %d weeks ago", d/7))
	case d >= 1:
		lines = append(lines, fmt.Sprintf("Added %d days ago", d))
	}
	if it.PageCount > 0 && medianPages > 0 {
		switch {
		case float64(it.PageCount) < 0.7*float64(medianPages):
			lines = append(lines, "Shorter than you usually read — a quick one")
		case float64(it.PageCount) > 1.4*float64(medianPages):
			if v != nil && v.VelocityBucket == "slow" {
				lines = append(lines, "Longer than your usual — give it a clear stretch")
			} else {
				lines = append(lines, "Longer than your usual")
			}
		default:
			if v != nil && v.VelocityBucket == "fast" {
				lines = append(lines, "Your usual length — you finish these quickly")
			}
		}
	}
	return lines
}

// TouchTBR records that the reader still wants a stale book: it drops out
// of the stale bucket for another year.
func (s *RecommendationService) TouchTBR(ctx context.Context, userID, bookID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return err
	}
	bid, err := uuid.Parse(bookID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE bookshelf SET updated_at = NOW() WHERE user_id = $1 AND book_id = $2 AND status = 'tbr'`, uid, bid)
	return err
}
