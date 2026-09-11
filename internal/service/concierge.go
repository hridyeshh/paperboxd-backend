package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// Jazy as a personal librarian.
//
// The old flow was a one-shot: embed the query, take five nearest books, ask
// Claude to write a reason for each. That is "ChatGPT that knows books". The
// difference between that and a librarian is not the model — it is what the
// model is told, and whether it is allowed to ask before it answers.
//
// Two changes here:
//   - The prompt sees the full ReaderContext: abandoned books, what they keep
//     saving, who they follow, how fast they read, what they asked last week.
//   - When the ask is genuinely ambiguous, Jazy asks one short question
//     instead of guessing. One, not a form: "immersive and slow, or something
//     you can tear through?" resolves most ambiguity in a single tap.

const (
	conciergeModel   = "claude-sonnet-4-6"
	conciergeTimeout = 35 * time.Second
	conciergeDeck    = 5
)

// ConciergeRequest is one turn with Jazy.
type ConciergeRequest struct {
	Query  string
	UserID string
	AnonID string
	// SessionID continues a search conversation, so "shorter" works here too.
	SessionID string
	// Answer is the reader's reply to a previous clarifying question, if any.
	// Sent back with the original query so Jazy does not ask again.
	Answer string
	Limit  int
}

// ConciergeResponse is either a deck of books or one question.
type ConciergeResponse struct {
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId"`
	Query     string `json:"query"`

	// Question, when set, means Jazy wants one thing clarified before
	// answering. Options are the tappable replies; the client sends the
	// chosen one back as Answer.
	Question string   `json:"question,omitempty"`
	Options  []string `json:"options,omitempty"`

	// Intro is Jazy's one-line framing above the deck ("I found three that
	// feel particularly you"). Empty when a question was asked instead.
	Intro        string                 `json:"intro,omitempty"`
	Personalised bool                   `json:"personalised"`
	Items        []types.VibeBookResult `json:"items"`
	// Understood is the accumulated ask ("like Murakami · under 300 pages")
	// and Refined whether this turn edited the previous one — the same two
	// fields search returns, so mobile can show the conversation too.
	Understood string `json:"understood,omitempty"`
	Refined    bool   `json:"refined"`
}

// Concierge runs one turn.
func (s *RecommendationService) Concierge(ctx context.Context, queries *db.Queries, req ConciergeRequest) (ConciergeResponse, error) {
	if req.Limit <= 0 || req.Limit > 10 {
		req.Limit = conciergeDeck
	}

	// Fold a clarifying answer into the query so the parser and the embedder
	// both see it. "something for my flight" + "tear through" is a different
	// search from either half alone.
	query := strings.TrimSpace(req.Query)
	if req.Answer != "" {
		query = query + ", " + strings.TrimSpace(req.Answer)
	}

	var profile *UserSignalProfile
	if req.UserID != "" {
		if p, err := s.GetOrComputeSignalProfile(ctx, req.UserID); err == nil {
			profile = &p
		}
	}
	reader := s.BuildReaderContext(ctx, req.UserID, profile)

	// 1. Ask first if the request is too open to answer well. Only when there
	// was no answer already — a second question is an interrogation.
	sess, refined := s.AdvanceSearchSession(ctx, req.SessionID, req.UserID, req.AnonID, query)
	if req.Answer == "" {
		if q, opts := clarifyingQuestion(sess.Current, reader); q != "" {
			s.SaveSearchSession(ctx, sess)
			return ConciergeResponse{
				Kind:      "jazy#question",
				SessionID: sess.ID,
				Query:     req.Query,
				Question:  q,
				Options:   opts,
				Items:     []types.VibeBookResult{},
			}, nil
		}
	}

	// 2. Retrieve + rank through the same pipeline as search, so Jazy's deck
	// respects the reader's constraints and negative signals. The concierge is
	// search with a voice, not a separate engine. The session was advanced
	// above; going through Search would advance it again and apply "shorter"
	// twice.
	searchResp, err := s.searchWithSession(ctx, queries, SearchRequest{
		Query:     query,
		SessionID: sess.ID,
		UserID:    req.UserID,
		AnonID:    req.AnonID,
		Limit:     req.Limit,
	}, sess, refined)
	if err != nil {
		return ConciergeResponse{}, err
	}

	resp := ConciergeResponse{
		Kind:         "jazy#deck",
		SessionID:    searchResp.SessionID,
		Query:        req.Query,
		Personalised: searchResp.Personalised,
		Items:        searchResp.Items,
		Understood:   searchResp.Understood,
		Refined:      searchResp.Refined,
	}

	// 3. Voice. Claude writes the intro and per-book reasons with the full
	// reader context. On any failure the deck ships with the engine's own
	// reasons — a librarian who is briefly hoarse still hands you the books.
	if s.reasoner != nil && len(resp.Items) > 0 {
		s.applyConciergeVoice(ctx, query, &resp, reader)
	}

	if req.UserID != "" {
		if uid, err := uuid.Parse(req.UserID); err == nil {
			go s.eventSvc.Emit(context.Background(), EmitParams{
				UserID:    uid,
				EventType: EventVibeSearch,
				Source:    "server",
				Metadata:  map[string]any{"query": req.Query, "answered": req.Answer != "", "deck": len(resp.Items)},
			})
		}
	}
	return resp, nil
}

// clarifyingQuestion decides whether to ask before answering, and what.
//
// The rule is one question, and only when answering without it would be a
// coin flip. A query with a topic, a similarity target, a mood word, or a hard
// constraint has enough to go on. What is left is the genuinely open ask —
// "recommend me something", "what should I read next" — where the single most
// useful bit of information is pace, and the second is how close to home.
func clarifyingQuestion(pq ParsedQuery, reader ReaderContext) (string, []string) {
	c := pq.Constraints
	specific := c.SimilarTo != "" || c.HasHardFilter() || len(c.PreferAxes) > 0 ||
		len(c.ExcludeAxes) > 0 || c.Context != ""
	if specific {
		return "", nil
	}
	// A topic with real content ("books about grief in postwar Japan") is
	// specific enough; three vague words ("something good please") are not.
	if len(strings.Fields(pq.EmbedText)) >= 4 && !looksOpenEnded(pq.EmbedText) {
		return "", nil
	}

	// For a reader we know, the interesting question is stretch vs. comfort.
	// For a stranger, pace is the safer first cut.
	if !reader.IsEmpty() && len(reader.TasteLines) > 0 {
		return "Stay close to your usual taste, or something different?",
			[]string{"Close to my usual", "Surprise me"}
	}
	return "Something immersive and slow, or something you can tear through?",
		[]string{"Immersive and slow", "Something I can tear through"}
}

var openEndedWords = []string{
	"something", "anything", "recommend", "suggest", "what should i read",
	"good book", "next read", "next book", "any ideas", "help me",
}

func looksOpenEnded(text string) bool {
	t := strings.ToLower(text)
	for _, w := range openEndedWords {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// conciergeVoice is what Claude writes back.
type conciergeVoice struct {
	Intro string `json:"intro"`
	Books []struct {
		Match  int    `json:"match"`
		Why    string `json:"why"`
		Caveat string `json:"caveat"`
	} `json:"books"`
}

func (s *RecommendationService) applyConciergeVoice(ctx context.Context, query string, resp *ConciergeResponse, reader ReaderContext) {
	ctx, cancel := context.WithTimeout(ctx, conciergeTimeout)
	defer cancel()

	prompt := buildConciergePrompt(query, resp.Items, reader)
	reqBody, err := json.Marshal(map[string]any{
		"model":      conciergeModel,
		"max_tokens": 1500,
		"messages":   []map[string]any{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return
	}
	httpReq.Header.Set("x-api-key", s.reasoner.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("content-type", "application/json")

	httpResp, err := s.reasoner.client.Do(httpReq)
	if err != nil {
		slog.Warn("concierge voice unavailable", "error", err)
		return
	}
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode != http.StatusOK {
		slog.Warn("concierge voice", "status", httpResp.StatusCode, "body", string(raw[:min(200, len(raw))]))
		return
	}

	var claudeResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &claudeResp) != nil || len(claudeResp.Content) == 0 {
		return
	}
	var voice conciergeVoice
	if err := json.Unmarshal([]byte(stripFence(claudeResp.Content[0].Text)), &voice); err != nil {
		slog.Warn("concierge voice parse", "error", err)
		return
	}
	if len(voice.Books) != len(resp.Items) {
		slog.Warn("concierge voice count mismatch", "got", len(voice.Books), "want", len(resp.Items))
		return
	}

	resp.Intro = strings.TrimSpace(voice.Intro)
	for i := range resp.Items {
		resp.Items[i].MatchPercent = min(100, max(0, voice.Books[i].Match))
		resp.Items[i].MatchReason = voice.Books[i].Why
		resp.Items[i].MatchCaveat = voice.Books[i].Caveat
		resp.Items[i].ReasonType = "claude"
	}
}

func buildConciergePrompt(query string, items []types.VibeBookResult, reader ReaderContext) string {
	var b strings.Builder

	b.WriteString("You are Jazy, the personal librarian inside a reading app called Paperboxd. ")
	b.WriteString("You know this reader's shelf and you speak to them like someone who has been recommending them books for a year: ")
	b.WriteString("warm, specific, brief, never salesy.\n\n")

	fmt.Fprintf(&b, "They asked: %q\n\n", query)
	b.WriteString("The engine has already retrieved and ranked these books for them, using their taste and any constraints in the request:\n\n")

	for i, it := range items {
		fmt.Fprintf(&b, "%d. %q", i, it.VolumeInfo.Title)
		if len(it.VolumeInfo.Authors) > 0 {
			fmt.Fprintf(&b, " by %s", strings.Join(it.VolumeInfo.Authors, ", "))
		}
		b.WriteString("\n")
		if len(it.VolumeInfo.Categories) > 0 {
			fmt.Fprintf(&b, "   genres: %s\n", strings.Join(it.VolumeInfo.Categories, ", "))
		}
		if it.VolumeInfo.PageCount > 0 {
			fmt.Fprintf(&b, "   pages: %d\n", it.VolumeInfo.PageCount)
		}
		if it.MatchReason != "" {
			fmt.Fprintf(&b, "   engine's reason: %s\n", it.MatchReason)
		}
		if d := strings.TrimSpace(it.VolumeInfo.Description); d != "" {
			if len(d) > 400 {
				d = d[:400] + "…"
			}
			fmt.Fprintf(&b, "   blurb: %s\n", d)
		}
	}

	b.WriteString(reader.PromptSection())

	b.WriteString(`
Write, as JSON:
- "intro": one sentence (under 120 characters) framing the deck for this reader. If you know their history, it should sound like it — reference a book they loved or a pattern in their reading, not a generic "here are some picks". No exclamation marks.
- "books": one object per book, in the same order:
  - "match": integer 0-100, how well it answers THIS reader's request given everything above. Use the whole range; no two books the same number.
  - "why": one sentence, under 110 characters, tying the book to something concrete — their request, a book they rated highly, a pattern you were told about. When a book they rated 4★+ genuinely resembles this one, name it with the rating ("you gave Never Let Me Go 5★, and this has the same slow build"); when they wrote about a book in their diary, you may quote the feeling back. The reader should recognise themselves in it. Never start with "This book".
  - "caveat": one honest note under 110 characters on what might not land. Never empty, never a compliment in disguise. If they have dropped or disliked something similar, say so.

Rules:
- Only claim things supported by the blurbs, genres, or what you were told about the reader. Never invent reading history. A reason that is merely plausible is worse than a plain one — trust is the whole product.
- If they have turned recommendations down for a stated reason, do not recommend the same thing again without acknowledging it.
- No spoilers, no marketing voice.

Return ONLY the JSON object: {"intro": "...", "books": [{"match": 88, "why": "...", "caveat": "..."}]}`)

	return b.String()
}
