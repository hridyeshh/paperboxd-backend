package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5"
)

// Right Now floors. A card only appears when its number is worth saying out
// loud: "2 readers started X today" makes the place feel empty, not alive.
// ponytail: fixed floors tuned for launch-size traffic; scale them with weekly
// actives once the numbers are routinely far above them.
const (
	nowMinStarted   = 3
	nowMinSurging   = 5
	nowMinListSaves = 3
	nowMinLikes     = 3
	// nowMinCards: below this the strip reads as a quiet room; clients fall
	// back to the community activity feed instead.
	nowMinCards = 3
)

// NowCard is one "Right Now on Paperboxd" event. Headline is server-authored
// so every client makes the same claim with the same number.
type NowCard struct {
	Kind      string              `json:"kind"` // started_today | surging | list_rising | thought_hot | milestone
	Headline  string              `json:"headline"`
	Count     int32               `json:"count"`
	Book      *types.BookResponse `json:"book,omitempty"`
	ListID    *string             `json:"list_id,omitempty"`
	ThoughtID *string             `json:"thought_id,omitempty"`
	Username  *string             `json:"username,omitempty"`
	Excerpt   *string             `json:"excerpt,omitempty"`
}

// nowSignals is what the queries found; nil means nothing qualified or the
// query failed. Kept separate from the queries so the floors are testable.
type nowSignals struct {
	started   *db.GetMostStartedTodayRow
	rising    *db.GetRisingBooksRow
	list      *db.GetRisingListRow
	thought   *db.GetHotThoughtRow
	milestone *db.GetPublicActivitiesRow
}

func (h *CommunityHandler) loadNowSignals(ctx context.Context, rising []db.GetRisingBooksRow, activity []db.GetPublicActivitiesRow) nowSignals {
	var s nowSignals
	if row, err := h.Queries.GetMostStartedToday(ctx); err == nil {
		s.started = &row
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("right now: started today", "error", err)
	}
	if row, err := h.Queries.GetRisingList(ctx); err == nil {
		s.list = &row
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("right now: rising list", "error", err)
	}
	if row, err := h.Queries.GetHotThought(ctx); err == nil {
		s.thought = &row
	} else if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("right now: hot thought", "error", err)
	}
	if len(rising) > 0 {
		s.rising = &rising[0]
	}
	cutoff := time.Now().Add(-48 * time.Hour)
	for i := range activity {
		if activity[i].ActivityType == "milestone" && activity[i].CreatedAt.Time.After(cutoff) {
			s.milestone = &activity[i]
			break
		}
	}
	return s
}

// buildNow applies the floors and returns the cards, or none at all when too
// few qualify.
func buildNow(s nowSignals) []NowCard {
	cards := []NowCard{}

	if r := s.started; r != nil && r.Started >= nowMinStarted {
		b := bookToResponse(r.Book)
		cards = append(cards, NowCard{
			Kind:     "started_today",
			Headline: fmt.Sprintf("%d readers started %s today", r.Started, r.Book.Title),
			Count:    r.Started,
			Book:     &b,
		})
	}

	// Skip surging when it is the same book as started_today: one book, one card.
	if r := s.rising; r != nil && r.Recent >= nowMinSurging &&
		(s.started == nil || s.started.Book.ID != r.Book.ID || s.started.Started < nowMinStarted) {
		b := bookToResponse(r.Book)
		cards = append(cards, NowCard{
			Kind:     "surging",
			Headline: fmt.Sprintf("%s is picking up — shelved by %d readers this week", r.Book.Title, r.Recent),
			Count:    r.Recent,
			Book:     &b,
		})
	}

	if r := s.list; r != nil && r.Saves7d >= nowMinListSaves {
		id, user := r.ID.String(), r.Username
		cards = append(cards, NowCard{
			Kind:     "list_rising",
			Headline: fmt.Sprintf("“%s” by %s — saved by %d readers this week", r.Title, displayName(r.Name.String, r.Username), r.Saves7d),
			Count:    r.Saves7d,
			ListID:   &id,
			Username: &user,
		})
	}

	if r := s.thought; r != nil && r.Likes48h >= nowMinLikes {
		id, user := r.ID.String(), r.Username
		excerpt := firstWordsPlain(service.StripHTML(r.Content), 24)
		headline := fmt.Sprintf("%s wrote something readers can't stop liking", displayName(r.Name.String, r.Username))
		if r.BookTitle.Valid {
			headline = fmt.Sprintf("%s on %s", displayName(r.Name.String, r.Username), r.BookTitle.String)
		}
		cards = append(cards, NowCard{
			Kind:      "thought_hot",
			Headline:  headline,
			Count:     r.Likes48h,
			ThoughtID: &id,
			Username:  &user,
			Excerpt:   &excerpt,
		})
	}

	if r := s.milestone; r != nil {
		var meta struct {
			BooksRead int32 `json:"books_read"`
		}
		if json.Unmarshal(r.Metadata, &meta) == nil && meta.BooksRead > 0 {
			user := r.Username
			cards = append(cards, NowCard{
				Kind:     "milestone",
				Headline: fmt.Sprintf("%s just finished their %s book", displayName(r.Name.String, r.Username), ordinal(meta.BooksRead)),
				Count:    meta.BooksRead,
				Username: &user,
			})
		}
	}

	if len(cards) < nowMinCards {
		return []NowCard{}
	}
	return cards
}

func displayName(name, username string) string {
	if name != "" {
		return name
	}
	return "@" + username
}

func ordinal(n int32) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

func firstWordsPlain(s string, n int) string {
	words := strings.Fields(s)
	if len(words) <= n {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:n], " ") + "…"
}
