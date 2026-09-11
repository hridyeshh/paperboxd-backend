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

// Book trait extraction.
//
// A book's embedding is accurate and useless for explanation: it can say "this
// is 0.83 close to that" but never "you like this because it is quiet and
// character-driven". Genres are readable but far too coarse -- "Fiction"
// covers both halves of every taste split worth making.
//
// This turns a title + description into the vocabulary the roadmap is written
// in, so that both the ranking signal and the sentence under the cover come
// from the same nine numbers.

const (
	// Same model family the vibe reasons and Scan & Know flows use. Haiku is
	// deliberate: this runs over the whole corpus, the task is closer to
	// classification than to writing, and a book is re-extracted whenever the
	// extractor version changes.
	traitModel = "claude-haiku-4-5-20251001"
	// Batch of 8 books answers in ~10s. The backfill is not latency-sensitive;
	// the ceiling exists so one wedged request cannot stall the whole run.
	traitTimeout = 90 * time.Second
	// TraitBatchSize is how many books go in one prompt. Larger batches cost
	// less per book but make one malformed response lose more work, since the
	// index-alignment check rejects the whole batch.
	TraitBatchSize = 8
	// TraitExtractorVersion is bumped when the axes or the prompt change, which
	// re-queues every book for extraction.
	TraitExtractorVersion = 1
)

// TraitAxes is the canonical ordered list of scalar dimensions.
//
// Order matters: profile maths, the taste dashboard and the reason engine all
// iterate this, so a stable order keeps those outputs comparable across
// versions. Adding an axis means bumping TraitExtractorVersion.
var TraitAxes = []string{
	"character_driven",
	"emotional_intensity",
	"plot_intensity",
	"pacing",
	"prose_density",
	"narrative_complexity",
	"darkness",
	"romance_centrality",
	"worldbuilding",
}

// traitAxisMeaning documents what 1.0 means on each axis. Used to build the
// prompt so the model and the schema cannot drift apart.
var traitAxisMeaning = map[string]string{
	"character_driven":     "1.0 = interior, character-led; 0.0 = driven by external plot machinery",
	"emotional_intensity":  "1.0 = devastating, emotionally heavy; 0.0 = light, low emotional stakes",
	"plot_intensity":       "1.0 = twisty, high-stakes, propulsive events; 0.0 = almost nothing happens",
	"pacing":               "1.0 = fast, hard to put down; 0.0 = slow burn, unhurried",
	"prose_density":        "1.0 = dense literary prose that rewards rereading; 0.0 = plain and transparent",
	"narrative_complexity": "1.0 = nonlinear, many POVs, demands attention; 0.0 = single clear thread",
	"darkness":             "1.0 = bleak, brutal, despairing; 0.0 = warm, cosy, comforting",
	"romance_centrality":   "1.0 = the romance IS the plot; 0.0 = no romantic thread at all",
	"worldbuilding":        "1.0 = dense invented world with its own rules; 0.0 = our world, unremarked",
}

// BookTraits is one book's extracted shape.
type BookTraits struct {
	// Scalars holds every axis in TraitAxes, each clamped to 0..1.
	Scalars map[string]float64 `json:"scalars"`

	Moods      []string `json:"moods"`       // "melancholy", "hopeful", "tense"
	Themes     []string `json:"themes"`      // "grief", "class", "coming of age"
	Tone       string   `json:"tone"`        // one word
	POV        string   `json:"pov"`         // "first", "third limited", "omniscient", "multiple"
	Setting    string   `json:"setting"`     // "rural Ireland", "near-future Lagos"
	TimePeriod string   `json:"time_period"` // "contemporary", "1940s", "far future"
	Audience   string   `json:"audience"`    // "adult", "young adult", "middle grade"
	IsSeries   bool     `json:"is_series"`

	// Confidence is the model's own read on how much the description supported
	// the answer. A one-line blurb should produce a low number, and ranking
	// discounts the whole book rather than trusting a guess.
	Confidence float64 `json:"confidence"`
}

// TraitInput is the slice of book metadata the prompt sees.
type TraitInput struct {
	BookID      string
	Title       string
	Authors     []string
	Categories  []string
	Description string
	PageCount   int
}

// TraitExtractor turns book metadata into BookTraits via Claude.
type TraitExtractor struct {
	apiKey string
	client *http.Client
}

// NewTraitExtractor returns nil when no API key is configured. Callers treat a
// nil extractor as "skip extraction" -- the same contract as ClaudeReasoner, so
// the backend keeps working without Anthropic credentials and ranking simply
// falls back to the genre/author/vector signals it used before.
func NewTraitExtractor(apiKey string) *TraitExtractor {
	if apiKey == "" {
		return nil
	}
	return &TraitExtractor{
		apiKey: apiKey,
		client: &http.Client{Timeout: traitTimeout},
	}
}

// Extract returns one BookTraits per input book, index-aligned.
//
// Returns an error rather than a partial result: a misaligned batch would
// attach one book's traits to another, and wrong traits are worse than none --
// they produce a confident, specific, false reason, which is exactly the
// failure the roadmap's "never fabricate reasons" rule exists to prevent.
func (t *TraitExtractor) Extract(ctx context.Context, books []TraitInput) ([]BookTraits, error) {
	if len(books) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, traitTimeout)
	defer cancel()

	reqBody, err := json.Marshal(map[string]any{
		"model":      traitModel,
		"max_tokens": 4096,
		"messages": []map[string]any{
			{"role": "user", "content": buildTraitPrompt(books)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal trait request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build trait request: %w", err)
	}
	req.Header.Set("x-api-key", t.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("trait request: %w", err)
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
		return nil, fmt.Errorf("decode trait response: %w", err)
	}
	if len(claudeResp.Content) == 0 || claudeResp.Content[0].Text == "" {
		return nil, fmt.Errorf("claude returned empty content")
	}

	out, err := ParseTraitResponse(claudeResp.Content[0].Text, len(books))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ParseTraitResponse decodes, validates and normalises a raw model response.
// Split out from Extract so the parsing rules are testable without a network
// call -- they are where this silently degrades.
func ParseTraitResponse(text string, want int) ([]BookTraits, error) {
	var out []BookTraits
	if err := json.Unmarshal([]byte(stripFence(text)), &out); err != nil {
		return nil, fmt.Errorf("parse trait JSON: %w (raw: %.200s)", err, text)
	}
	if len(out) != want {
		return nil, fmt.Errorf("trait count %d != book count %d", len(out), want)
	}
	for i := range out {
		out[i].normalise()
	}
	return out, nil
}

// normalise clamps every axis into 0..1 and fills in any the model omitted.
//
// A missing axis becomes 0.5 (no opinion) rather than 0.0: on
// `character_driven`, 0.0 is the confident claim "this is pure plot", which is
// a very different statement from "the description did not say".
func (b *BookTraits) normalise() {
	if b.Scalars == nil {
		b.Scalars = make(map[string]float64, len(TraitAxes))
	}
	for _, axis := range TraitAxes {
		v, ok := b.Scalars[axis]
		if !ok {
			b.Scalars[axis] = 0.5
			continue
		}
		b.Scalars[axis] = clamp01(v)
	}
	// Drop axes the model invented; they would widen every distance
	// calculation against books that do not have them.
	for k := range b.Scalars {
		if _, known := traitAxisMeaning[k]; !known {
			delete(b.Scalars, k)
		}
	}
	b.Confidence = clamp01(b.Confidence)
	b.Moods = trimStrings(b.Moods, 5)
	b.Themes = trimStrings(b.Themes, 5)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// trimStrings lowercases, de-duplicates and caps a tag list. Tags are compared
// across books, so "Grief" and "grief" must not become two themes.
func trimStrings(in []string, max int) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, max)
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == max {
			break
		}
	}
	return out
}

func buildTraitPrompt(books []TraitInput) string {
	var b strings.Builder

	b.WriteString("You are cataloguing books for a reading app's recommendation engine.\n")
	b.WriteString("For each book below, judge it on nine axes. Every axis is a number from 0.0 to 1.0.\n\n")
	for _, axis := range TraitAxes {
		b.WriteString("- " + axis + ": " + traitAxisMeaning[axis] + "\n")
	}

	b.WriteString(`
Also give, for each book:
  moods       - up to 5 single words for how reading it feels
  themes      - up to 5 short noun phrases for what it is about
  tone        - one word
  pov         - "first", "third limited", "omniscient", "multiple", or "" if unclear
  setting     - short phrase, or "" if unclear
  time_period - e.g. "contemporary", "1940s", "far future", or "" if unclear
  audience    - "adult", "young adult", or "middle grade"
  is_series   - true if this book is part of a series
  confidence  - 0.0 to 1.0: how well the description actually supported your answer

Rules that matter more than completeness:
- Judge only from what you are given plus what you genuinely know about the book.
- If the description is thin and you do not know the book, still answer, but set
  confidence low. A low-confidence answer is used differently downstream; an
  invented high-confidence one corrupts a reader's taste profile.
- Use 0.5 for an axis the material says nothing about. Do not use 0.0 as a
  synonym for "unknown" — 0.0 is a strong claim.

Return ONLY a JSON array with one object per book, in the same order, shaped:
[{"scalars":{"character_driven":0.9,"emotional_intensity":0.8,"plot_intensity":0.3,"pacing":0.2,"prose_density":0.8,"narrative_complexity":0.4,"darkness":0.6,"romance_centrality":0.1,"worldbuilding":0.0},"moods":["melancholy","tender"],"themes":["grief","art","obsession"],"tone":"elegiac","pov":"first","setting":"New York","time_period":"contemporary","audience":"adult","is_series":false,"confidence":0.9}]

No prose, no markdown fence.

Books:
`)

	for i, bk := range books {
		fmt.Fprintf(&b, "\n%d. %s", i+1, bk.Title)
		if len(bk.Authors) > 0 {
			b.WriteString(" by " + strings.Join(bk.Authors, ", "))
		}
		b.WriteString("\n")
		if len(bk.Categories) > 0 {
			b.WriteString("   Categories: " + strings.Join(bk.Categories, ", ") + "\n")
		}
		if bk.PageCount > 0 {
			fmt.Fprintf(&b, "   Pages: %d\n", bk.PageCount)
		}
		if bk.Description != "" {
			desc := bk.Description
			// The axes are decided in the first couple of paragraphs; the rest
			// of a publisher blurb is review quotes and series marketing.
			if len(desc) > 1500 {
				desc = desc[:1500] + "..."
			}
			b.WriteString("   Description: " + desc + "\n")
		} else {
			b.WriteString("   Description: (none available)\n")
		}
	}

	return b.String()
}
