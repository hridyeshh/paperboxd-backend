package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// SearchSession is the memory that makes search conversational.
//
// "Books like Murakami" → "shorter" → "more emotional" → "less weird". Each
// turn is meaningless on its own; the session is what "shorter" is shorter
// than. Kept in Redis with a short TTL because it is a conversation, not a
// record: a reader coming back tomorrow is starting over, and persisting
// half-finished searches to Postgres would only accumulate noise.

const (
	searchSessionTTL      = 30 * time.Minute
	searchSessionMaxTurns = 8
)

// SearchSession is one reader's current search conversation.
type SearchSession struct {
	ID     string `json:"id"`
	UserID string `json:"user_id,omitempty"`
	AnonID string `json:"anon_id,omitempty"`

	// Current is the accumulated parse — what the next search actually runs.
	Current ParsedQuery `json:"current"`

	// Turns is the history of raw inputs, oldest first. Capped so a runaway
	// client cannot grow a session without bound.
	Turns []SearchTurn `json:"turns"`

	// Shown is every book id returned so far. A refinement should surface
	// books the previous turn did not, or "shorter" just reorders the same
	// deck and reads as though nothing happened.
	Shown []string `json:"shown"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SearchTurn is one input in the conversation.
type SearchTurn struct {
	Query      string       `json:"query"`
	Intent     SearchIntent `json:"intent"`
	Refinement bool         `json:"refinement"`
	At         time.Time    `json:"at"`
}

func searchSessionKey(id string) string { return "search:session:" + id }

// LoadSearchSession fetches a session by id. Returns (nil, nil) when absent or
// expired — callers start a fresh one rather than failing the search.
func (s *RecommendationService) LoadSearchSession(ctx context.Context, id string) (*SearchSession, error) {
	if s.redisClient == nil || id == "" {
		return nil, nil
	}
	data, err := s.redisClient.Get(ctx, searchSessionKey(id)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sess SearchSession
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, nil
	}
	return &sess, nil
}

// SaveSearchSession writes the session and refreshes its TTL.
func (s *RecommendationService) SaveSearchSession(ctx context.Context, sess *SearchSession) {
	if s.redisClient == nil || sess == nil {
		return
	}
	sess.UpdatedAt = time.Now()
	if len(sess.Turns) > searchSessionMaxTurns {
		sess.Turns = sess.Turns[len(sess.Turns)-searchSessionMaxTurns:]
	}
	if len(sess.Shown) > 200 {
		sess.Shown = sess.Shown[len(sess.Shown)-200:]
	}
	if data, err := json.Marshal(sess); err == nil {
		s.redisClient.Set(ctx, searchSessionKey(sess.ID), data, searchSessionTTL)
	}
}

// AdvanceSearchSession applies a new input to a session, creating one if
// needed. Returns the session and whether the input was read as a refinement
// of the previous turn rather than a fresh search.
//
// Ownership is checked: a session id is a bearer credential to a stranger's
// search history, so one created by user A is not continued by user B, and an
// anonymous session is not continued by a signed-in user with a different
// anon id. Mismatch starts fresh rather than erroring — the reader typed a
// query and should get results.
func (s *RecommendationService) AdvanceSearchSession(
	ctx context.Context, sessionID, userID, anonID, query string,
) (*SearchSession, bool) {
	sess, _ := s.LoadSearchSession(ctx, sessionID)

	if sess != nil {
		owned := (userID != "" && sess.UserID == userID) ||
			(userID == "" && anonID != "" && sess.AnonID == anonID)
		if !owned {
			sess = nil
		}
	}

	now := time.Now()
	if sess == nil {
		sess = &SearchSession{
			ID:        uuid.NewString(),
			UserID:    userID,
			AnonID:    anonID,
			CreatedAt: now,
		}
		sess.Current = ParseQuery(query)
		sess.Turns = append(sess.Turns, SearchTurn{Query: query, Intent: sess.Current.Intent, At: now})
		return sess, false
	}

	// A follow-up is a refinement when it is a comparative ("shorter") or when
	// it carries only constraints ("under 300 pages, no romance"). Anything
	// with its own topic is a new search that happens to share a session.
	if refined, ok := ParseRefinement(sess.Current, query); ok {
		sess.Current = refined
		sess.Turns = append(sess.Turns, SearchTurn{Query: query, Intent: refined.Intent, Refinement: true, At: now})
		return sess, true
	}

	next := ParseQuery(query)
	if isRefinementOnly(next) {
		sess.Current = sess.Current.Merge(next)
		sess.Turns = append(sess.Turns, SearchTurn{Query: query, Intent: sess.Current.Intent, Refinement: true, At: now})
		return sess, true
	}

	// Fresh search: the constraints do not carry over. A reader who asked
	// for "short" books like Murakami and then types "books about grief" has
	// changed subject, and silently keeping the page limit would make the
	// second search look broken.
	sess.Current = next
	sess.Shown = nil
	sess.Turns = append(sess.Turns, SearchTurn{Query: query, Intent: next.Intent, At: now})
	return sess, false
}

// RecordShown appends result ids so the next refinement can prefer new books.
func (sess *SearchSession) RecordShown(ids []string) {
	seen := make(map[string]bool, len(sess.Shown))
	for _, id := range sess.Shown {
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			sess.Shown = append(sess.Shown, id)
			seen[id] = true
		}
	}
}

// Describe renders the accumulated constraints as a short human line, so the
// client can show "Murakami-like · under 300 pages · more emotional" and the
// reader can see what the engine thinks they asked for.
func (sess *SearchSession) Describe() string {
	c := sess.Current.Constraints
	var parts []string
	if c.SimilarTo != "" {
		parts = append(parts, "like "+c.SimilarTo)
	}
	if c.MaxPages > 0 {
		parts = append(parts, fmt.Sprintf("under %d pages", c.MaxPages))
	}
	if c.MinPages > 0 {
		parts = append(parts, fmt.Sprintf("over %d pages", c.MinPages))
	}
	if c.Standalone {
		parts = append(parts, "standalone")
	}
	for axis, v := range c.PreferAxes {
		if p, ok := traitPhrases[axis]; ok {
			idx := 0
			if v > 0.5 {
				idx = 1
			}
			parts = append(parts, p[idx])
		}
	}
	if len(parts) == 0 {
		return sess.Current.EmbedText
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " · " + p
	}
	return out
}
