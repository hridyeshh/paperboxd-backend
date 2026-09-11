package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ReaderContext is everything Jazy is allowed to know about the person asking.
//
// Before this, the prompt saw three genres and a favourites list — the
// equivalent of a librarian who has glanced at your bag. The roadmap's bar is
// a librarian who has been talking to you for a year: what you abandoned, what
// you keep saving and never starting, who you follow, how fast you read, and
// what you asked last week. Every field below is something that changes the
// recommendation, and each is capped so the prompt stays a page, not a dossier.
type ReaderContext struct {
	ReaderTaste // the original three fields, kept for the vibe reason prompt

	// Shape of the reader.
	AvgPages       int    // mean page count of finished books
	VelocityBucket string // "fast" | "medium" | "slow"
	FinishRate     float64

	// Recent history, most recent first.
	RecentlyFinished []string // "Title (5★)" for the last few finished
	CurrentlyReading []string
	Abandoned        []string // started and dropped

	// Negative taste in the reader's own words.
	Disliked   []string // rated 1-2, or "not for me"
	RejectedAs []string // reason codes, de-duplicated, e.g. "too slow"

	// Aspiration vs appetite: what they keep saving.
	TBRSample         []string
	TBRCount          int
	SavedNeverStarted int

	// Interpretable taste, as sentences.
	TasteLines []string // from DescribeTaste
	// Recently trending axes vs. long-term, e.g. "reading darker than usual".
	RecentShift []string

	// Social.
	FollowingCount int
	FriendsLoved   []string // "Title (by @alex)" — books friends rated 4+ the reader hasn't read

	// What they asked Jazy for before.
	PreviousAsks []string
}

// IsEmpty reports whether we know anything at all.
func (rc ReaderContext) IsEmpty() bool {
	return rc.TotalRead == 0 && len(rc.TopGenres) == 0 && len(rc.LovedBooks) == 0 &&
		len(rc.RecentlyFinished) == 0 && rc.TBRCount == 0 && len(rc.TasteLines) == 0
}

const (
	ctxRecentFinished = 6
	ctxLoved          = 8
	ctxDisliked       = 5
	ctxTBR            = 5
	ctxFriendsLoved   = 5
	ctxPreviousAsks   = 4
)

// BuildReaderContext gathers the reader's full picture in a handful of
// queries. Every sub-query is best-effort: a missing table or a slow join
// costs one section of the prompt, never the answer.
func (s *RecommendationService) BuildReaderContext(ctx context.Context, userID string, profile *UserSignalProfile) ReaderContext {
	var rc ReaderContext
	if userID == "" {
		return rc
	}

	if profile != nil {
		rc.TopGenres = topWeighted(profile.GenreWeights, 3)
		if profile.VelocitySignal != nil {
			rc.VelocityBucket = profile.VelocitySignal.VelocityBucket
		}
		rc.TasteLines = DescribeTaste(profile.Traits, 3)
	}

	// Shelf: finished, reading, abandoned, loved, disliked, average length.
	rows, err := s.pool.Query(ctx, `
		SELECT bk.title, b.status, b.rating,
		       COALESCE(bk.page_count, 0),
		       b.finished_at IS NOT NULL AS finished,
		       (b.status = 'reading' AND b.finished_at IS NULL
		        AND b.updated_at < NOW() - INTERVAL '60 days') AS abandoned,
		       COALESCE(b.finished_at, b.updated_at) AS ts
		FROM bookshelf b
		JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = $1
		ORDER BY COALESCE(b.finished_at, b.updated_at) DESC
		LIMIT 300
	`, userID)
	if err == nil {
		var pageSum, pageN, started, finished int
		for rows.Next() {
			var title, status string
			var rating *int32
			var pages int
			var isFinished, isAbandoned bool
			var ts time.Time
			if rows.Scan(&title, &status, &rating, &pages, &isFinished, &isAbandoned, &ts) != nil {
				continue
			}
			if isFinished {
				finished++
				started++
				rc.TotalRead++
				if pages > 0 {
					pageSum += pages
					pageN++
				}
				if len(rc.RecentlyFinished) < ctxRecentFinished {
					rc.RecentlyFinished = append(rc.RecentlyFinished, titleWithRating(title, rating))
				}
			} else if status == "reading" {
				started++
				if isAbandoned {
					if len(rc.Abandoned) < ctxDisliked {
						rc.Abandoned = append(rc.Abandoned, title)
					}
				} else if len(rc.CurrentlyReading) < 3 {
					rc.CurrentlyReading = append(rc.CurrentlyReading, title)
				}
			}
			if rating != nil {
				switch {
				case *rating >= 4 && len(rc.LovedBooks) < ctxLoved:
					rc.LovedBooks = append(rc.LovedBooks, title)
				case *rating <= 2 && len(rc.Disliked) < ctxDisliked:
					rc.Disliked = append(rc.Disliked, title)
				}
			}
		}
		rows.Close()
		if pageN > 0 {
			rc.AvgPages = pageSum / pageN
		}
		if started > 0 {
			rc.FinishRate = float64(finished) / float64(started)
		}
	} else {
		slog.Warn("reader context: shelf", "error", err)
	}

	// TBR.
	rows, err = s.pool.Query(ctx, `
		SELECT bk.title, COUNT(*) OVER () AS total
		FROM bookshelf b
		JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = $1 AND b.status = 'pending'
		ORDER BY b.tbr_added_at DESC NULLS LAST, b.created_at DESC
		LIMIT $2
	`, userID, ctxTBR)
	if err == nil {
		for rows.Next() {
			var title string
			var total int
			if rows.Scan(&title, &total) == nil {
				rc.TBRSample = append(rc.TBRSample, title)
				rc.TBRCount = total
			}
		}
		rows.Close()
	}
	if profile != nil {
		// depth_signal was computed nightly; read the one number that matters.
		var saved *int
		_ = s.pool.QueryRow(ctx, `
			SELECT (depth_signal->>'saved_never_started')::int
			FROM user_signal_profiles WHERE user_id = $1
		`, userID).Scan(&saved)
		if saved != nil {
			rc.SavedNeverStarted = *saved
		}
	}

	// Explicit rejections and their reasons.
	rows, err = s.pool.Query(ctx, `
		SELECT bk.title, f.reason_codes
		FROM recommendation_feedback f
		JOIN books bk ON bk.id = f.book_id
		WHERE f.user_id = $1 AND f.verdict = 'not_for_me'
		ORDER BY f.updated_at DESC
		LIMIT $2
	`, userID, ctxDisliked)
	if err == nil {
		seen := map[string]bool{}
		for rows.Next() {
			var title string
			var codes []string
			if rows.Scan(&title, &codes) != nil {
				continue
			}
			if len(rc.Disliked) < ctxDisliked*2 {
				rc.Disliked = append(rc.Disliked, title)
			}
			for _, c := range codes {
				if !seen[c] {
					seen[c] = true
					rc.RejectedAs = append(rc.RejectedAs, strings.ReplaceAll(c, "_", " "))
				}
			}
		}
		rows.Close()
	}

	// Social: books friends loved that this reader has not read.
	rows, err = s.pool.Query(ctx, `
		SELECT bk.title, u.username
		FROM follows f
		JOIN bookshelf b ON b.user_id = f.following_id AND b.rating >= 4
		JOIN books bk ON bk.id = b.book_id
		JOIN users u ON u.id = f.following_id
		WHERE f.follower_id = $1
		  AND NOT EXISTS (SELECT 1 FROM bookshelf mine WHERE mine.user_id = $1 AND mine.book_id = b.book_id)
		ORDER BY b.updated_at DESC
		LIMIT $2
	`, userID, ctxFriendsLoved)
	if err == nil {
		for rows.Next() {
			var title, username string
			if rows.Scan(&title, &username) == nil {
				rc.FriendsLoved = append(rc.FriendsLoved, fmt.Sprintf("%s (by @%s)", title, username))
			}
		}
		rows.Close()
	}
	_ = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM follows WHERE follower_id = $1`, userID).Scan(&rc.FollowingCount)

	// What they asked before. Vibe and personalised search both emit this.
	rows, err = s.pool.Query(ctx, `
		SELECT DISTINCT ON (metadata->>'query') metadata->>'query', created_at
		FROM events
		WHERE user_id = $1
		  AND event_type = 'vibe_search_performed'
		  AND metadata->>'query' IS NOT NULL
		  AND created_at > NOW() - INTERVAL '30 days'
		ORDER BY metadata->>'query', created_at DESC
	`, userID)
	if err == nil {
		type ask struct {
			q  string
			at time.Time
		}
		var asks []ask
		for rows.Next() {
			var a ask
			if rows.Scan(&a.q, &a.at) == nil && a.q != "" {
				asks = append(asks, a)
			}
		}
		rows.Close()
		// DISTINCT ON forced query order; re-sort by recency and take the tail.
		for i := 1; i < len(asks); i++ {
			for j := i; j > 0 && asks[j].at.After(asks[j-1].at); j-- {
				asks[j], asks[j-1] = asks[j-1], asks[j]
			}
		}
		for i, a := range asks {
			if i == ctxPreviousAsks {
				break
			}
			rc.PreviousAsks = append(rc.PreviousAsks, a.q)
		}
	}

	rc.RecentShift = s.recentTasteShift(ctx, userID, profile)
	return rc
}

func titleWithRating(title string, rating *int32) string {
	if rating == nil {
		return title
	}
	return fmt.Sprintf("%s (%d★)", title, *rating)
}

// recentTasteShift compares the last ~90 days of loved books against the
// long-term trait profile and names the axes that moved. This is the roadmap's
// "you've been reading heavier books lately" — a sentence the engine can only
// say if it keeps two clocks.
func (s *RecommendationService) recentTasteShift(ctx context.Context, userID string, profile *UserSignalProfile) []string {
	if profile == nil || profile.Traits == nil || !profile.Traits.HasSignal() {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.character_driven, t.emotional_intensity, t.plot_intensity,
		       t.pacing, t.prose_density, t.narrative_complexity, t.darkness,
		       t.romance_centrality, t.worldbuilding
		FROM bookshelf b
		JOIN book_traits t ON t.book_id = b.book_id
		WHERE b.user_id = $1
		  AND b.rating >= 4
		  AND COALESCE(b.finished_at, b.updated_at) > NOW() - INTERVAL '90 days'
	`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	sums := make([]float64, len(TraitAxes))
	var n int
	for rows.Next() {
		vals := make([]float64, len(TraitAxes))
		dest := make([]any, len(vals))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if rows.Scan(dest...) != nil {
			continue
		}
		for i, v := range vals {
			sums[i] += v
		}
		n++
	}
	// Under three books a "shift" is one enthusiastic weekend.
	if n < 3 {
		return nil
	}

	var out []string
	for i, axis := range TraitAxes {
		longTerm, ok := profile.Traits.Prefs[axis]
		if !ok || profile.Traits.Confidence[axis] < 0.4 {
			continue
		}
		recent := sums[i] / float64(n)
		delta := recent - longTerm
		if delta > 0.2 || delta < -0.2 {
			p, ok := traitPhrases[axis]
			if !ok {
				continue
			}
			if delta > 0 {
				out = append(out, "lately more drawn to "+p[1])
			} else {
				out = append(out, "lately more drawn to "+p[0])
			}
		}
		if len(out) == 2 {
			break
		}
	}
	return out
}

// PromptSection renders the context as the "What we know about this reader"
// block. Kept here so the vibe reason prompt and the concierge prompt say the
// same things about the same person.
func (rc ReaderContext) PromptSection() string {
	if rc.IsEmpty() {
		return "\nThis reader is anonymous — speak to the request itself, never invent reading history.\n"
	}
	var b strings.Builder
	b.WriteString("\nWhat we know about this reader:\n")

	if rc.TotalRead > 0 {
		fmt.Fprintf(&b, "- has finished %d books", rc.TotalRead)
		if rc.AvgPages > 0 {
			fmt.Fprintf(&b, ", averaging %d pages", rc.AvgPages)
		}
		if rc.VelocityBucket != "" {
			fmt.Fprintf(&b, ", a %s reader", rc.VelocityBucket)
		}
		if rc.FinishRate > 0 && rc.FinishRate < 0.7 {
			fmt.Fprintf(&b, "; finishes about %d%% of what they start", int(rc.FinishRate*100))
		}
		b.WriteString("\n")
	}
	if len(rc.TopGenres) > 0 {
		fmt.Fprintf(&b, "- reads mostly: %s\n", strings.Join(rc.TopGenres, ", "))
	}
	for _, line := range rc.TasteLines {
		fmt.Fprintf(&b, "- tends to love %s\n", strings.ToLower(line[:1])+line[1:])
	}
	for _, line := range rc.RecentShift {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	if len(rc.LovedBooks) > 0 {
		fmt.Fprintf(&b, "- rated 4★+: %s\n", strings.Join(rc.LovedBooks, "; "))
	}
	if len(rc.RecentlyFinished) > 0 {
		fmt.Fprintf(&b, "- finished recently: %s\n", strings.Join(rc.RecentlyFinished, "; "))
	}
	if len(rc.CurrentlyReading) > 0 {
		fmt.Fprintf(&b, "- reading now: %s\n", strings.Join(rc.CurrentlyReading, "; "))
	}
	if len(rc.Abandoned) > 0 {
		fmt.Fprintf(&b, "- started but dropped: %s\n", strings.Join(rc.Abandoned, "; "))
	}
	if len(rc.Disliked) > 0 {
		fmt.Fprintf(&b, "- did not get on with: %s\n", strings.Join(rc.Disliked, "; "))
	}
	if len(rc.RejectedAs) > 0 {
		fmt.Fprintf(&b, "- has turned recommendations down for being: %s\n", strings.Join(rc.RejectedAs, ", "))
	}
	if rc.TBRCount > 0 {
		fmt.Fprintf(&b, "- has %d books saved to read", rc.TBRCount)
		if len(rc.TBRSample) > 0 {
			fmt.Fprintf(&b, " (e.g. %s)", strings.Join(rc.TBRSample, "; "))
		}
		if rc.SavedNeverStarted >= 5 {
			b.WriteString(" — saves a lot more than they start")
		}
		b.WriteString("\n")
	}
	if len(rc.FriendsLoved) > 0 {
		fmt.Fprintf(&b, "- people they follow loved: %s\n", strings.Join(rc.FriendsLoved, "; "))
	}
	if len(rc.PreviousAsks) > 0 {
		fmt.Fprintf(&b, "- recently asked for: %s\n", strings.Join(quoteAll(rc.PreviousAsks), ", "))
	}
	return b.String()
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = `"` + s + `"`
	}
	return out
}
