package service

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Paperboxd Daily.
//
// The feed's modules answer "what is happening"; Daily answers "give me
// something interesting". It is a finite package — at most five atoms, then
// "that's all for today" — built from things people wrote (daily_atoms), real
// reader thoughts, and one book from the reader's own recommendation pool.
// Nothing here is generated at request time.
//
// Picks are deterministic per reader per day, so a reload never reshuffles
// the page and a link shared at lunch still matches the card at dinner.

// Daily atom kinds, in page order. question and idea share the first slot.
const (
	DailyQuestion   = "question"
	DailyIdea       = "idea"
	DailyPick       = "pick"
	DailyThought    = "thought"
	DailyRabbitHole = "rabbit_hole"
	DailyPassage    = "passage"
)

// DailyBook is a book an atom is about, with the line saying why it is there.
type DailyBook struct {
	ID       string   `json:"id"`
	Slug     string   `json:"slug"`
	Title    string   `json:"title"`
	Authors  []string `json:"authors"`
	CoverURL string   `json:"cover_url"`
	Note     string   `json:"note,omitempty"`
}

// DailyThoughtRef is a public reader thought quoted on the page. Content is
// the thought's stored HTML, as on every other thought endpoint.
type DailyThoughtRef struct {
	ID        string     `json:"id"`
	Username  string     `json:"username"`
	Name      string     `json:"name"`
	AvatarURL string     `json:"avatar_url,omitempty"`
	Content   string     `json:"content"`
	Book      *DailyBook `json:"book,omitempty"`
}

// DailyAtom is one card in the Daily package. Clients key the layout on Kind;
// every text on the card is server-authored so all three apps say the same.
type DailyAtom struct {
	Kind        string      `json:"kind"`
	Slug        string      `json:"slug,omitempty"`
	Eyebrow     string      `json:"eyebrow"`
	Title       string      `json:"title"`
	Dek         string      `json:"dek,omitempty"`
	ReadSeconds int         `json:"read_seconds"`
	Provenance  string      `json:"provenance"`
	Books       []DailyBook `json:"books"`
	// Pick is set on the pick atom only.
	Pick *BookCandidate `json:"pick,omitempty"`
	// Thought is set on the thought atom only.
	Thought *DailyThoughtRef `json:"thought,omitempty"`
	// Body, SourceNote and SourceURL are sent on the detail endpoint only;
	// the feed carries cards, not articles.
	Body       string   `json:"body,omitempty"`
	SourceNote string   `json:"source_note,omitempty"`
	SourceURL  string   `json:"source_url,omitempty"`
	Topics     []string `json:"-"`
}

var dailyEyebrows = map[string]string{
	DailyQuestion:   "A question",
	DailyIdea:       "An idea",
	DailyRabbitHole: "Today's rabbit hole",
	DailyPassage:    "A passage",
}

var dailyProvenance = map[string]string{
	"editorial":        "Paperboxd Editorial",
	"public_domain":    "Public-domain text",
	"author_interview": "Author interview",
	"readers":          "From Paperboxd readers",
}

// buildDaily assembles today's package. take is the feed's dedupe-aware
// slicer, so the pick never reappears in a module further down the page.
func (s *RecommendationService) buildDaily(
	ctx context.Context,
	uid uuid.UUID,
	now time.Time,
	profile UserSignalProfile,
	byType map[string][]BookCandidate,
	take func([]BookCandidate, int) []BookCandidate,
) []DailyAtom {
	atoms, err := s.loadDailyAtoms(ctx, "a.status = 'published'")
	if err != nil {
		slog.Warn("daily: load atoms", "error", err)
	}
	byKind := map[string][]DailyAtom{}
	for _, a := range atoms {
		a.Body = ""
		byKind[a.Kind] = append(byKind[a.Kind], a)
	}

	day := uint64(now.Unix()+int64(offsetSeconds(now))) / 86400
	personal := day + hashString(uid.String())
	var out []DailyAtom

	// 01 — Think. The shared editorial voice: every reader gets the same one
	// today, so it is something people can talk about.
	if a, ok := pickAtom(append(byKind[DailyQuestion], byKind[DailyIdea]...), nil, day); ok {
		out = append(out, a)
	}

	// 02 — Your pick. A hidden gem when the pool has one, else the strongest
	// reasoned match.
	pick := take(byType["hidden_gem"], 1)
	eyebrow := "You probably haven't found this yet"
	if len(pick) == 0 {
		pick = take(append(append([]BookCandidate{}, byType["because_loved"]...), byType["trait"]...), 1)
		eyebrow = "We found something very you"
	}
	if len(pick) == 1 {
		b := pick[0]
		out = append(out, DailyAtom{
			Kind: DailyPick, Eyebrow: eyebrow, Title: b.Title, ReadSeconds: 30,
			Provenance: "From your taste", Books: []DailyBook{}, Pick: &b,
		})
	}

	// 03 — One reader's thought. A human voice, rotated per reader.
	if t := s.featuredThought(ctx, uid, personal); t != nil {
		books := []DailyBook{}
		if t.Book != nil {
			books = append(books, *t.Book)
		}
		out = append(out, DailyAtom{
			Kind: DailyThought, Eyebrow: "One reader's thought", Title: firstWords(t.Content, 12),
			ReadSeconds: readSeconds(t.Content), Provenance: "From a Paperboxd reader",
			Books: books, Thought: t,
		})
	}

	// 04 — Rabbit hole and 05 — passage lean toward the reader's genres.
	if a, ok := pickAtom(byKind[DailyRabbitHole], profile.GenreWeights, personal); ok {
		out = append(out, a)
	}
	if a, ok := pickAtom(byKind[DailyPassage], profile.GenreWeights, personal); ok {
		out = append(out, a)
	}
	return out
}

// GetDailyAtom returns one published atom with its body, for the detail page
// and shared links. pgx.ErrNoRows when there is no such published atom.
func (s *RecommendationService) GetDailyAtom(ctx context.Context, slug string) (DailyAtom, error) {
	atoms, err := s.loadDailyAtoms(ctx, "a.status = 'published' AND a.slug = $1", slug)
	if err != nil {
		return DailyAtom{}, err
	}
	if len(atoms) == 0 {
		return DailyAtom{}, pgx.ErrNoRows
	}
	return atoms[0], nil
}

// loadDailyAtoms reads atoms with their books hydrated in order.
// ponytail: loads the whole published shelf per feed request; cache it per
// day once the shelf is in the hundreds.
func (s *RecommendationService) loadDailyAtoms(ctx context.Context, where string, args ...any) ([]DailyAtom, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.slug, a.kind, a.title, COALESCE(a.dek, ''), a.body, a.read_seconds,
		       a.source_kind, COALESCE(a.source_note, ''), COALESCE(a.source_url, ''), a.topics,
		       COALESCE((
		           SELECT json_agg(json_build_object(
		               'id', b.id, 'slug', b.slug, 'title', b.title, 'authors', b.authors,
		               'cover_url', COALESCE(b.cover_url, ''), 'note', COALESCE(e.item->>'note', '')
		           ) ORDER BY e.ord)
		           FROM jsonb_array_elements(a.books) WITH ORDINALITY AS e(item, ord)
		           JOIN books b ON b.id = (e.item->>'id')::uuid
		       ), '[]'::json)
		FROM daily_atoms a
		WHERE `+where+`
		ORDER BY a.created_at, a.slug
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyAtom
	for rows.Next() {
		var a DailyAtom
		var sourceKind string
		var books []byte
		if err := rows.Scan(&a.Slug, &a.Kind, &a.Title, &a.Dek, &a.Body, &a.ReadSeconds,
			&sourceKind, &a.SourceNote, &a.SourceURL, &a.Topics, &books); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(books, &a.Books); err != nil || a.Books == nil {
			a.Books = []DailyBook{}
		}
		a.Eyebrow = dailyEyebrows[a.Kind]
		a.Provenance = dailyProvenance[sourceKind]
		out = append(out, a)
	}
	return out, rows.Err()
}

// featuredThought picks a public, book-linked, standalone thought of card
// length from someone the reader can see: not their own, not a private or
// deleted profile, no block either way.
func (s *RecommendationService) featuredThought(ctx context.Context, uid uuid.UUID, seed uint64) *DailyThoughtRef {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text, u.username, COALESCE(u.name, ''), COALESCE(u.avatar_url, ''), t.content,
		       b.id::text, b.slug, b.title, b.authors, COALESCE(b.cover_url, '')
		FROM thoughts t
		JOIN users u ON u.id = t.user_id
		JOIN books b ON b.id = t.book_id
		WHERE t.is_private = false
		  AND t.thread_root_id IS NULL
		  AND t.user_id <> $1
		  AND u.is_public AND u.deleted_at IS NULL
		  AND t.created_at > NOW() - INTERVAL '180 days'
		  AND length(regexp_replace(t.content, '<[^>]+>', '', 'g')) BETWEEN 80 AND 600
		  AND NOT EXISTS (
		      SELECT 1 FROM blocks bl
		      WHERE (bl.blocker_id = $1 AND bl.blocked_id = u.id)
		         OR (bl.blocker_id = u.id AND bl.blocked_id = $1)
		  )
		ORDER BY (SELECT COUNT(*) FROM thought_likes l WHERE l.thought_id = t.id) DESC, t.created_at DESC
		LIMIT 20
	`, uid)
	if err != nil {
		slog.Warn("daily: featured thoughts", "error", err)
		return nil
	}
	defer rows.Close()
	var cands []DailyThoughtRef
	for rows.Next() {
		var t DailyThoughtRef
		var b DailyBook
		if rows.Scan(&t.ID, &t.Username, &t.Name, &t.AvatarURL, &t.Content,
			&b.ID, &b.Slug, &b.Title, &b.Authors, &b.CoverURL) != nil {
			continue
		}
		t.Book = &b
		cands = append(cands, t)
	}
	if len(cands) == 0 {
		return nil
	}
	return &cands[seed%uint64(len(cands))]
}

// pickAtom chooses one atom for a seed. With genres, atoms whose topics touch
// one of the reader's genres are preferred; a reader with no match — or no
// taste yet — rotates through the whole shelf instead of getting nothing.
func pickAtom(atoms []DailyAtom, genres map[string]float64, seed uint64) (DailyAtom, bool) {
	if len(atoms) == 0 {
		return DailyAtom{}, false
	}
	var matched []DailyAtom
	for _, a := range atoms {
		if topicsMatch(a.Topics, genres) {
			matched = append(matched, a)
		}
	}
	if len(matched) > 0 {
		atoms = matched
	}
	return atoms[seed%uint64(len(atoms))], true
}

func topicsMatch(topics []string, genres map[string]float64) bool {
	for g, w := range genres {
		if w <= 0 {
			continue
		}
		lg := strings.ToLower(g)
		for _, t := range topics {
			if t != "" && strings.Contains(lg, strings.ToLower(t)) {
				return true
			}
		}
	}
	return false
}

var htmlTag = regexp.MustCompile(`<[^>]+>`)

// readSeconds estimates reading time at 230 words a minute, never under 15s.
func readSeconds(html string) int {
	words := len(strings.Fields(htmlTag.ReplaceAllString(html, " ")))
	if secs := words * 60 / 230; secs > 15 {
		return secs
	}
	return 15
}

// firstWords is a plain-text title for a thought card, for clients that
// cannot render the quote itself (notifications, share sheets).
func firstWords(html string, n int) string {
	words := strings.Fields(htmlTag.ReplaceAllString(html, " "))
	if len(words) <= n {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:n], " ") + "…"
}

func hashString(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func offsetSeconds(t time.Time) int {
	_, off := t.Zone()
	return off
}
