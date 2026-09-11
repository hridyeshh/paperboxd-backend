package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The personalised home feed.
//
// The old home was one ranked list split into tabs by string-matching the
// reason text. This assembles the page as modules, each with its own
// retrieval and its own reason for existing, so the page reads like a
// magazine put together for one person rather than a list that was cut up
// afterwards. Modules with nothing to show are omitted, not padded — an empty
// "your people are reading" rail is worse than no rail.

// FeedModule is one section of the home page.
type FeedModule struct {
	// Kind is a stable machine id the client keys layouts on.
	Kind string `json:"kind"`
	// Title and Subtitle are server-authored so all three clients say the
	// same thing, and so copy can change without an app release.
	Title    string          `json:"title"`
	Subtitle string          `json:"subtitle,omitempty"`
	Books    []BookCandidate `json:"books"`
	// Twin is set on the taste-twin module only.
	Twin *TasteTwin `json:"twin,omitempty"`
}

// FeedResponse is the assembled home page.
type FeedResponse struct {
	Greeting string       `json:"greeting"`
	Modules  []FeedModule `json:"modules"`
	Source   string       `json:"source"`
}

// Module kinds. Order in the page is decided per reader below.
const (
	ModulePickedForYou  = "picked_for_you"
	ModuleNextRead      = "next_read"
	ModuleBecauseLoved  = "because_you_loved"
	ModuleYourPeople    = "your_people"
	ModulePeopleLikeYou = "people_like_you"
	ModuleTasteTwin     = "taste_twin"
	ModuleFromTBR       = "from_your_tbr"
	ModuleHiddenGem     = "hidden_gem"
	ModuleWildCard      = "wild_card"
	ModuleContinue      = "continue_reading"
)

// GetFeed assembles the home page for a reader.
func (s *RecommendationService) GetFeed(ctx context.Context, userID string) (FeedResponse, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return FeedResponse{}, err
	}

	resp := FeedResponse{Greeting: greeting(time.Now()), Source: "feed"}
	profile, _ := s.GetOrComputeSignalProfile(ctx, userID)

	// One ranked pool, sliced by reason type. Cheaper than N retrievals and
	// the pool already went through suppression, dedupe and diversity.
	pool, _, err := s.GetHomeRecommendations(ctx, userID)
	if err != nil {
		slog.Warn("feed: home pool", "error", err)
	}

	byType := map[string][]BookCandidate{}
	for _, b := range pool {
		byType[b.ReasonType] = append(byType[b.ReasonType], b)
	}
	used := map[string]bool{}
	take := func(from []BookCandidate, n int) []BookCandidate {
		out := make([]BookCandidate, 0, n)
		for _, b := range from {
			if used[b.ID] {
				continue
			}
			used[b.ID] = true
			out = append(out, b)
			if len(out) == n {
				break
			}
		}
		return out
	}

	// 1. Continue reading — the reader's own book comes before any suggestion.
	if books := s.currentlyReading(ctx, uid, 3); len(books) > 0 {
		resp.Modules = append(resp.Modules, FeedModule{
			Kind: ModuleContinue, Title: "Pick up where you left off", Books: books,
		})
	}

	// 2. Your next read — only after a recent finish, when intent is highest.
	if next := s.nextRead(ctx, uid, pool, used); next != nil {
		resp.Modules = append(resp.Modules, *next)
	}

	// 3. Picked for you — the strongest personal matches with a reason:
	// trait lines, "because you loved X", TBR neighbours, then bare vector.
	picked := take(append(append(append(byType["trait"], byType["recent"]...),
		byType["because_loved"]...), append(byType["tbr_similar"], byType["favorites"]...)...), 8)
	if len(picked) > 0 {
		resp.Modules = append(resp.Modules, FeedModule{
			Kind: ModulePickedForYou, Title: "Picked for you",
			Subtitle: pickedSubtitle(profile), Books: picked,
		})
	}

	// 4. Because you loved X — anchored on one specific book.
	if mod := s.becauseYouLoved(ctx, uid, userID, used); mod != nil {
		resp.Modules = append(resp.Modules, *mod)
	}

	// 5. Your people are reading — social reasons.
	if social := take(byType["social"], 8); len(social) > 0 {
		resp.Modules = append(resp.Modules, FeedModule{
			Kind: ModuleYourPeople, Title: "Your people are reading", Books: social,
		})
	}

	// 6. Readers with your taste loved — twins, when the table has them.
	// The pool already carries twin evidence (fetchTwinSignals) and the
	// reason engine wrote the line, so this is a slice, not a second query.
	if s.flags.Bool(ctx, "taste_twins") {
		if twinsLoved := take(byType["people_like_you"], 8); len(twinsLoved) > 0 {
			resp.Modules = append(resp.Modules, FeedModule{
				Kind: ModulePeopleLikeYou, Title: "Readers with your taste loved these", Books: twinsLoved,
			})
		}
		if twins, err := s.GetTasteTwins(ctx, userID, 1); err == nil && len(twins) > 0 && twins[0].OverlapPct >= 40 {
			tw := twins[0]
			books := s.booksByIDs(ctx, tw.CouldRead, 6)
			resp.Modules = append(resp.Modules, FeedModule{
				Kind:     ModuleTasteTwin,
				Title:    fmt.Sprintf("%d%% taste overlap with @%s", tw.OverlapPct, tw.Username),
				Subtitle: "Books they loved that you haven't read",
				Books:    books,
				Twin:     &tw,
			})
		}
	}

	// 7. From your TBR — nudge the pile.
	if tbr := s.tbrPicks(ctx, uid, profile, 5); len(tbr) > 0 {
		resp.Modules = append(resp.Modules, FeedModule{
			Kind: ModuleFromTBR, Title: "From your to-read pile",
			Subtitle: "You saved these. One of them is the one.", Books: tbr,
		})
	}

	// 8. Hidden gem — at most two, they are precious.
	if gems := take(byType["hidden_gem"], 2); len(gems) > 0 {
		resp.Modules = append(resp.Modules, FeedModule{
			Kind: ModuleHiddenGem, Title: "You probably haven't discovered this",
			Subtitle: "Barely read, well loved, and very you", Books: gems,
		})
	}

	// 9. Wild card — the exploration slots, explained.
	if wild := take(byType["explore"], 3); len(wild) > 0 {
		resp.Modules = append(resp.Modules, FeedModule{
			Kind: ModuleWildCard, Title: "Not your usual thing",
			Subtitle: "A deliberate stretch, not a random draw", Books: wild,
		})
	}

	if len(resp.Modules) == 0 {
		resp.Source = "fallback"
	}
	return resp, nil
}

func greeting(t time.Time) string {
	switch h := t.Hour(); {
	case h < 5:
		return "Still up?"
	case h < 12:
		return "Good morning."
	case h < 17:
		return "Good afternoon."
	default:
		return "Good evening."
	}
}

func pickedSubtitle(p UserSignalProfile) string {
	if lines := DescribeTaste(p.Traits, 1); len(lines) > 0 {
		return "Because you tend to love " + strings.ToLower(lines[0][:1]) + lines[0][1:]
	}
	return ""
}

// ── Modules ───────────────────────────────────────────────────────────────────

func (s *RecommendationService) currentlyReading(ctx context.Context, uid uuid.UUID, n int) []BookCandidate {
	rows, err := s.pool.Query(ctx, `
		SELECT bk.id::text, bk.title, bk.authors, COALESCE(bk.cover_url, ''), bk.categories,
		       COALESCE(b.current_page, 0), COALESCE(bk.page_count, 0)
		FROM bookshelf b JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = $1 AND b.status = 'reading' AND b.finished_at IS NULL
		  AND b.updated_at > NOW() - INTERVAL '60 days'
		ORDER BY b.updated_at DESC LIMIT $2
	`, uid, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []BookCandidate
	for rows.Next() {
		var c BookCandidate
		var page, total int
		if rows.Scan(&c.ID, &c.Title, &c.Authors, &c.CoverURL, &c.Categories, &page, &total) != nil {
			continue
		}
		c.ReasonType = "continue"
		if total > 0 && page > 0 {
			c.Reason = fmt.Sprintf("%d%% through", page*100/total)
		}
		out = append(out, c)
	}
	return out
}

// nextRead answers "I just finished a book, what now" with three framed picks:
// the best match, a shorter one, and a deliberate change of direction. Only
// shown within a few days of finishing — that is when the question is live.
func (s *RecommendationService) nextRead(ctx context.Context, uid uuid.UUID, pool []BookCandidate, used map[string]bool) *FeedModule {
	var finishedTitle string
	var finishedAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT bk.title, b.finished_at
		FROM bookshelf b JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = $1 AND b.finished_at IS NOT NULL
		ORDER BY b.finished_at DESC LIMIT 1
	`, uid).Scan(&finishedTitle, &finishedAt)
	if err != nil || time.Since(finishedAt) > 5*24*time.Hour {
		return nil
	}

	// Still reading something? Then the question is not live.
	var reading int
	_ = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bookshelf
		WHERE user_id = $1 AND status = 'reading' AND finished_at IS NULL
		  AND updated_at > NOW() - INTERVAL '14 days'
	`, uid).Scan(&reading)
	if reading > 0 {
		return nil
	}

	var best, shorter, different *BookCandidate
	for i := range pool {
		b := &pool[i]
		if used[b.ID] {
			continue
		}
		switch {
		case best == nil && b.ReasonType != "explore":
			best = b
		case different == nil && b.ReasonType == "explore":
			different = b
		}
	}
	if best == nil {
		return nil
	}
	// Shorter: the highest-ranked non-explore book under 300 pages, other
	// than best. Page counts are not on the candidate, so one indexed query.
	ids := make([]string, 0, len(pool))
	for _, b := range pool {
		if !used[b.ID] && b.ID != best.ID && b.ReasonType != "explore" {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) > 0 {
		var shortID string
		if s.pool.QueryRow(ctx, `
			SELECT id::text FROM books
			WHERE id = ANY($1::uuid[]) AND page_count > 0 AND page_count <= 300
			ORDER BY array_position($1::uuid[], id) LIMIT 1
		`, ids).Scan(&shortID) == nil {
			for i := range pool {
				if pool[i].ID == shortID {
					shorter = &pool[i]
					break
				}
			}
		}
	}

	mod := &FeedModule{
		Kind:     ModuleNextRead,
		Title:    "Your next read",
		Subtitle: "You finished " + finishedTitle + ". Based on your reading:",
	}
	add := func(b *BookCandidate, frame string) {
		if b == nil {
			return
		}
		c := *b
		c.Reason = frame + " — " + c.Reason
		used[c.ID] = true
		mod.Books = append(mod.Books, c)
	}
	add(best, "Your next read")
	add(shorter, "If you want something shorter")
	add(different, "If you want something completely different")
	return mod
}

// becauseYouLoved anchors a rail on the reader's most recent 5-star book.
func (s *RecommendationService) becauseYouLoved(ctx context.Context, uid uuid.UUID, userID string, used map[string]bool) *FeedModule {
	var anchorID, anchorTitle string
	err := s.pool.QueryRow(ctx, `
		SELECT bk.id::text, bk.title
		FROM bookshelf b JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = $1 AND b.rating = 5 AND bk.embedding IS NOT NULL
		ORDER BY COALESCE(b.finished_at, b.updated_at) DESC LIMIT 1
	`, uid).Scan(&anchorID, &anchorTitle)
	if err != nil {
		return nil
	}
	similar, err := s.GetSimilarBooks(ctx, anchorID, userID)
	if err != nil {
		return nil
	}
	out := make([]BookCandidate, 0, 8)
	for _, b := range similar {
		if used[b.ID] {
			continue
		}
		used[b.ID] = true
		b.Reason = "Because you loved " + anchorTitle
		b.ReasonType = "because_loved"
		out = append(out, b)
		if len(out) == 8 {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return &FeedModule{Kind: ModuleBecauseLoved, Title: "Because you loved " + anchorTitle, Books: out}
}

// tbrPicks surfaces TBR books that best fit the reader's current shape, so the
// pile is sorted by "you'd love this now" rather than by when it was added.
func (s *RecommendationService) tbrPicks(ctx context.Context, uid uuid.UUID, profile UserSignalProfile, n int) []BookCandidate {
	rows, err := s.pool.Query(ctx, `
		SELECT bk.id::text, bk.title, bk.authors, COALESCE(bk.cover_url, ''), bk.categories,
		       COALESCE(bk.like_count, 0), COALESCE(bk.total_reads_count, 0)
		FROM bookshelf b JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = $1 AND b.status = 'pending'
		ORDER BY b.tbr_added_at DESC NULLS LAST LIMIT 40
	`, uid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var cands []Candidate
	for rows.Next() {
		var c Candidate
		var likes, reads int32
		if rows.Scan(&c.BookID, &c.Title, &c.Authors, &c.CoverURL, &c.Categories, &likes, &reads) != nil {
			continue
		}
		c.LikeCount, c.TotalReads = int(likes), int(reads)
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return nil
	}
	_ = s.fetchCandidateTraits(ctx, cands)
	for i := range cands {
		c := &cands[i]
		if fit, ok := TraitFit(c.Traits, profile.Traits); ok {
			c.FinalScore = float32(fit)
			c.HasTraitFit = true
		}
	}
	sortByScore(cands)
	if len(cands) > n {
		cands = cands[:n]
	}
	out := make([]BookCandidate, len(cands))
	for i, c := range cands {
		bc := candidateToBookCandidate(c)
		bc.ReasonType = "tbr"
		if c.HasTraitFit && c.FinalScore > 0.7 {
			bc.Reason = "Fits what you've been loving lately"
		} else {
			bc.Reason = "On your list"
		}
		out[i] = bc
	}
	return out
}

func (s *RecommendationService) booksByIDs(ctx context.Context, ids []string, n int) []BookCandidate {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > n {
		ids = ids[:n]
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, title, authors, COALESCE(cover_url, ''), categories
		FROM books WHERE id = ANY($1::uuid[])
		ORDER BY array_position($1::uuid[], id)
	`, ids)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []BookCandidate
	for rows.Next() {
		var c BookCandidate
		if rows.Scan(&c.ID, &c.Title, &c.Authors, &c.CoverURL, &c.Categories) == nil {
			c.ReasonType = "twin"
			c.Reason = "Your taste twin loved this"
			out = append(out, c)
		}
	}
	return out
}
