package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Same model + wire format the Scan & Know flow uses (internal/handler/scan.go).
const (
	vibeReasonModel   = "claude-sonnet-4-6"
	vibeReasonTimeout = 8 * time.Second
)

// ClaudeReasoner turns a vibe query + the reader's taste into per-book reasons.
//
// The templated ReasonEngine text ("Matches \"a cosy autumn mystery\"") just
// echoes the query back, which is worthless on the Ask Jazy card. This asks
// Claude for the two lines the card actually wants: why this reader would like
// the book, and one honest caveat.
type ClaudeReasoner struct {
	apiKey string
	client *http.Client
}

// NewClaudeReasoner returns nil when no API key is configured — callers treat a
// nil reasoner as "fall back to the templated reason", so vibe search keeps
// working without Anthropic credentials.
func NewClaudeReasoner(apiKey string) *ClaudeReasoner {
	if apiKey == "" {
		return nil
	}
	return &ClaudeReasoner{
		apiKey: apiKey,
		client: &http.Client{Timeout: vibeReasonTimeout},
	}
}

// ReasonBook is the slice of book metadata the prompt sees.
type ReasonBook struct {
	Title       string
	Authors     []string
	Categories  []string
	Description string
}

// ReaderTaste is the signed-in reader's context. Empty for anonymous searches,
// in which case the reasons speak to the query alone.
type ReaderTaste struct {
	TotalRead  int
	TopGenres  []string
	LovedBooks []string // titles rated 4+, most recent first
}

// BookReasons is what one card renders: the match number, why, and an honest
// caveat. Match comes from the model so the percentage and the sentence under
// it make the same claim — raw cosine similarity routinely reads 45% next to
// glowing copy.
type BookReasons struct {
	Match  int    `json:"match"`
	Why    string `json:"why"`
	Caveat string `json:"caveat"`
}

// Reasons returns one BookReasons per input book, index-aligned. On any
// failure it returns an error and the caller keeps the templated reason —
// a vibe search must never fail because the reason model was slow.
func (r *ClaudeReasoner) Reasons(ctx context.Context, query string, books []ReasonBook, taste ReaderTaste) ([]BookReasons, error) {
	if len(books) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, vibeReasonTimeout)
	defer cancel()

	reqBody, err := json.Marshal(map[string]any{
		"model":      vibeReasonModel,
		"max_tokens": 1200,
		"messages": []map[string]any{
			{"role": "user", "content": buildVibeReasonPrompt(query, books, taste)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal reason request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build reason request: %w", err)
	}
	req.Header.Set("x-api-key", r.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reason request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude API %d: %.200s", resp.StatusCode, raw)
	}

	var claudeResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &claudeResp); err != nil {
		return nil, fmt.Errorf("decode reason response: %w", err)
	}
	if len(claudeResp.Content) == 0 || claudeResp.Content[0].Text == "" {
		return nil, fmt.Errorf("claude returned empty content")
	}

	var out []BookReasons
	if err := json.Unmarshal([]byte(stripFence(claudeResp.Content[0].Text)), &out); err != nil {
		return nil, fmt.Errorf("parse reason JSON: %w (raw: %.200s)", err, claudeResp.Content[0].Text)
	}
	if len(out) != len(books) {
		return nil, fmt.Errorf("reason count %d != book count %d", len(out), len(books))
	}
	for i := range out {
		out[i].Match = min(100, max(0, out[i].Match))
	}
	return out, nil
}

// stripFence removes a ```json … ``` wrapper if the model added one.
func stripFence(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "```") {
		return text
	}
	if i := strings.Index(text, "\n"); i != -1 {
		text = text[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(text, "```"))
}

func buildVibeReasonPrompt(query string, books []ReasonBook, taste ReaderTaste) string {
	var b strings.Builder

	b.WriteString("A reader asked a book-recommendation app for: \"")
	b.WriteString(query)
	b.WriteString("\"\n\nSemantic search returned these books, already ranked:\n\n")

	for i, bk := range books {
		fmt.Fprintf(&b, "%d. %q", i, bk.Title)
		if len(bk.Authors) > 0 {
			fmt.Fprintf(&b, " by %s", strings.Join(bk.Authors, ", "))
		}
		b.WriteString("\n")
		if len(bk.Categories) > 0 {
			fmt.Fprintf(&b, "   genres: %s\n", strings.Join(bk.Categories, ", "))
		}
		if d := strings.TrimSpace(bk.Description); d != "" {
			if len(d) > 400 {
				d = d[:400] + "…"
			}
			fmt.Fprintf(&b, "   blurb: %s\n", d)
		}
	}

	if taste.TotalRead > 0 || len(taste.TopGenres) > 0 || len(taste.LovedBooks) > 0 {
		b.WriteString("\nWhat we know about this reader:\n")
		if taste.TotalRead > 0 {
			fmt.Fprintf(&b, "- has finished %d books\n", taste.TotalRead)
		}
		if len(taste.TopGenres) > 0 {
			fmt.Fprintf(&b, "- reads mostly: %s\n", strings.Join(taste.TopGenres, ", "))
		}
		if len(taste.LovedBooks) > 0 {
			fmt.Fprintf(&b, "- rated 4★+: %s\n", strings.Join(taste.LovedBooks, "; "))
		}
	} else {
		b.WriteString("\nThis reader is anonymous — speak to the request itself, never invent reading history.\n")
	}

	b.WriteString(`
For each book, in the same order, write:
- "match": an integer 0-100, how well it answers this reader's request. Judge the book on the request itself, not on its position in the list above — the ranking is a rough vector search and is often wrong. Use the whole range: a book that genuinely nails the request is 90+, a loose thematic cousin is 50-65, a near-miss is under 40. Do not give two books the same number.
- "why": one sentence on why THIS reader would like it, tying the book to their request and (only if you were given it) their history. Concrete and specific — name the tone, the structure, the thing that matches. Never start with "This book". It must justify the number you gave: a low match reads as a partial fit, not a rave.
- "caveat": one short honest note — what might not land, or what to expect. Never empty, never a compliment in disguise.

Rules:
- "why" and "caveat" under 110 characters each.
- No spoilers, no marketing voice, no exclamation marks.
- Only claim things supported by the blurb, genres, or the reader's stated history.

Return ONLY a JSON array, one object per book, in the original order:
[{"match": 88, "why": "...", "caveat": "..."}]`)

	return b.String()
}
