package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hridyesh/paperboxd-backend/internal/config"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/reqctx"
	"github.com/hridyesh/paperboxd-backend/internal/types"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ScanHandler handles the /scan/* endpoints.
type ScanHandler struct {
	Pool         *pgxpool.Pool
	Queries      *db.Queries
	Config       *config.Config
	ISBNdb       *external.ISBNdbClient
	Client       *http.Client
	ClaudeClient *http.Client
}

func NewScanHandler(pool *pgxpool.Pool, queries *db.Queries, cfg *config.Config, isbndb *external.ISBNdbClient) *ScanHandler {
	return &ScanHandler{
		Pool:         pool,
		Queries:      queries,
		Config:       cfg,
		ISBNdb:       isbndb,
		Client:       &http.Client{Timeout: 10 * time.Second},
		ClaudeClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// BookSummary is a compact book representation used inside UserReadingProfile.
type BookSummary struct {
	Title  string  `json:"title"`
	Author string  `json:"author"`
	Rating float64 `json:"rating"`
}

// UserReadingProfile captures everything PaperBoxd knows about a reader.
type UserReadingProfile struct {
	TotalBooksRead           int            `json:"total_books_read"`
	GenreDistribution        map[string]int `json:"genre_distribution"`
	TopGenres                []string       `json:"top_genres"`
	FavoriteBooks            []BookSummary  `json:"favorite_books"`
	RepeatAuthors            []string       `json:"repeat_authors"`
	AverageRatingGiven       float64        `json:"average_rating_given"`
	RecentReads              []BookSummary  `json:"recent_reads"`
	StaleTBRCount            int            `json:"stale_tbr_count"`
	ReadingPaceBooksPerMonth float64        `json:"reading_pace_books_per_month"`
	FollowedUsersWithBook    []string       `json:"followed_users_with_book"`
}

// ScoreDimensions holds the five per-dimension scores from Claude.
type ScoreDimensions struct {
	GenreFit         int `json:"genre_fit"`
	WritingStyle     int `json:"writing_style"`
	LengthComplexity int `json:"length_complexity"`
	CommunityLove    int `json:"community_love"`
	PersonalFit      int `json:"personal_fit"`
}

// ScanResult is the parsed Claude scoring output.
type ScanResult struct {
	OverallScore int             `json:"overall_score"`
	Dimensions   ScoreDimensions `json:"dimensions"`
	Verdict      string          `json:"verdict"`
	ForYou       []string        `json:"for_you"`
	AgainstYou   []string        `json:"against_you"`
	OneLine      string          `json:"one_line"`
}

// scanBookMeta is the book data passed into the Claude scorer.
type scanBookMeta struct {
	Title       string
	Authors     []string
	Genres      []string
	Pages       int
	Description string
}

// scanUnlimited bypasses the free-scan quota (no gate, no decrement) while testing.
// Flip to false to re-enable the paywall.
const scanUnlimited = true

// Analyze handles POST /api/v1/scan/analyze.
func (h *ScanHandler) Analyze(w http.ResponseWriter, r *http.Request) {
	debug := r.URL.Query().Get("debug") == "true"

	userIDStr, ok := reqctx.GetUserID(r.Context())
	if !ok {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		types.WriteError(w, http.StatusUnauthorized, types.ErrCodeUnauthorized, "Unauthorized")
		return
	}

	var req struct {
		ISBN string `json:"isbn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Invalid JSON body")
		return
	}
	isbn := strings.TrimSpace(req.ISBN)
	if isbn == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "isbn is required")
		return
	}

	var scansRemaining int32
	err = h.Pool.QueryRow(r.Context(),
		"SELECT scan_uses_remaining FROM users WHERE id = $1",
		userID,
	).Scan(&scansRemaining)
	if err != nil {
		slog.Error("query scan_uses_remaining", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}

	if scansRemaining == 0 && !scanUnlimited {
		types.WriteJSON(w, http.StatusForbidden, map[string]any{
			"error":           "scans_exhausted",
			"scans_remaining": 0,
		})
		return
	}

	if h.ISBNdb == nil {
		types.WriteError(w, http.StatusInternalServerError, types.ErrCodeInternalServer, "Book lookup not configured")
		return
	}

	book, err := h.ISBNdb.GetByISBN(r.Context(), isbn)
	if err != nil {
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "400") {
			types.WriteJSON(w, http.StatusNotFound, map[string]any{
				"error":   "book_not_found",
				"message": "Couldn't find this book — try searching by title",
			})
			return
		}
		slog.Error("isbndb get by isbn", "error", err, "isbn", isbn)
		types.WriteInternalError(w)
		return
	}
	if book == nil {
		types.WriteJSON(w, http.StatusNotFound, map[string]any{
			"error":   "book_not_found",
			"message": "Couldn't find this book — try searching by title",
		})
		return
	}

	isbn13 := book.ISBN13
	if isbn13 == "" {
		isbn13 = book.ISBN
	}
	if isbn13 == "" {
		isbn13 = isbn
	}

	authors := book.Authors
	if authors == nil {
		authors = []string{}
	}
	genres := book.Subjects
	if genres == nil {
		genres = []string{}
	}

	// ── Community research ────────────────────────────────────────────────────

	var (
		communitySummary string
		readersCount     int
		ratingsCount     int
	)

	var (
		cachedSummary string
		cachedReaders int
		cachedRatings int
	)
	cacheErr := h.Pool.QueryRow(r.Context(),
		"SELECT community_summary, readers_count, ratings_count FROM scan_community_cache WHERE isbn = $1 AND cached_at > NOW() - INTERVAL '24 hours'",
		isbn13,
	).Scan(&cachedSummary, &cachedReaders, &cachedRatings)

	if cacheErr == nil {
		slog.Info("scan cache HIT", "isbn", isbn13)
		communitySummary = cachedSummary
		readersCount = cachedReaders
		ratingsCount = cachedRatings
	} else {
		slog.Info("scan cache MISS", "isbn", isbn13)

		firstAuthor := ""
		if len(book.Authors) > 0 {
			firstAuthor = book.Authors[0]
		}

		var (
			wg              sync.WaitGroup
			redditSignal    string
			goodreadsSignal string
			amazonSignal    string
		)

		// 3 Brave sentiment queries + 1 Open Library reader-stats lookup, in parallel.
		wg.Add(4)

		go func() {
			defer wg.Done()
			readersCount, ratingsCount = h.fetchOpenLibraryCounts(r.Context(), isbn13)
		}()

		go func() {
			defer wg.Done()
			result, err := h.searchBrave(r, book.Title+" "+firstAuthor+" reddit review")
			if err != nil {
				slog.Warn("scan brave reddit query failed", "error", err, "isbn", isbn13)
				return
			}
			redditSignal = result
		}()

		go func() {
			defer wg.Done()
			result, err := h.searchBrave(r, book.Title+" "+firstAuthor+" goodreads review")
			if err != nil {
				slog.Warn("scan brave goodreads query failed", "error", err, "isbn", isbn13)
				return
			}
			goodreadsSignal = result
		}()

		go func() {
			defer wg.Done()
			result, err := h.searchBrave(r, book.Title+" "+firstAuthor+" amazon review")
			if err != nil {
				slog.Warn("scan brave amazon query failed", "error", err, "isbn", isbn13)
				return
			}
			amazonSignal = result
		}()

		wg.Wait()

		var sb strings.Builder
		if redditSignal != "" {
			sb.WriteString("Reddit reader discussions:\n")
			sb.WriteString(redditSignal)
			sb.WriteString("\n\n")
		}
		if goodreadsSignal != "" {
			sb.WriteString("Goodreads reviews:\n")
			sb.WriteString(goodreadsSignal)
			sb.WriteString("\n\n")
		}
		if amazonSignal != "" {
			sb.WriteString("Amazon reviews:\n")
			sb.WriteString(amazonSignal)
			sb.WriteString("\n\n")
		}

		if sb.Len() == 0 {
			slog.Warn("scan: all community sources failed", "isbn", isbn13)
			communitySummary = "No community data available for this book."
		} else {
			communitySummary = sb.String()
		}

		_, writeErr := h.Pool.Exec(r.Context(),
			`INSERT INTO scan_community_cache (isbn, community_summary, readers_count, ratings_count, cached_at)
			 VALUES ($1, $2, $3, $4, NOW())
			 ON CONFLICT (isbn) DO UPDATE
			 SET community_summary = EXCLUDED.community_summary,
			     readers_count = EXCLUDED.readers_count,
			     ratings_count = EXCLUDED.ratings_count,
			     cached_at = NOW()`,
			isbn13, communitySummary, readersCount, ratingsCount,
		)
		if writeErr != nil {
			slog.Error("scan cache write failed", "error", writeErr, "isbn", isbn13)
		}
	}

	// ── User reading profile ──────────────────────────────────────────────────

	profile, err := h.buildUserReadingProfile(r.Context(), userID, isbn13)
	if err != nil {
		slog.Error("build user reading profile", "error", err, "user_id", userID)
		types.WriteInternalError(w)
		return
	}

	// ── Claude scoring ────────────────────────────────────────────────────────

	meta := scanBookMeta{
		Title:       book.Title,
		Authors:     authors,
		Genres:      genres,
		Pages:       book.Pages,
		Description: book.Synopsis,
	}

	score, err := h.callClaudeForScore(r.Context(), meta, communitySummary, profile)
	if err != nil {
		slog.Error("claude scoring failed", "error", err, "isbn", isbn13, "user_id", userID)
		types.WriteJSON(w, http.StatusBadGateway, map[string]any{
			"error":   "scoring_failed",
			"message": "Something took too long — your scan hasn't been used",
		})
		return
	}

	// ── Decrement quota (only after successful score) ─────────────────────────

	var newRemaining int32
	if scanUnlimited {
		// Testing: don't consume quota.
		newRemaining = scansRemaining
	} else {
		decrementErr := h.Pool.QueryRow(r.Context(),
			"UPDATE users SET scan_uses_remaining = scan_uses_remaining - 1 WHERE id = $1 AND scan_uses_remaining > 0 RETURNING scan_uses_remaining",
			userID,
		).Scan(&newRemaining)
		if decrementErr != nil {
			// Race condition: quota hit 0 between check and now. Still return the score.
			slog.Warn("scan quota decrement returned no rows", "user_id", userID)
			newRemaining = 0
		}
	}

	// ── Response ──────────────────────────────────────────────────────────────

	// Real source counts surfaced to the analyzing screen. readers/ratings come from
	// Open Library's global reading-log; shelf/friends from the reader's own profile.
	resp := map[string]any{
		"book": map[string]any{
			"isbn":        isbn13,
			"title":       book.Title,
			"authors":     authors,
			"genres":      genres,
			"pages":       book.Pages,
			"description": book.Synopsis,
			"cover_url":   book.Image,
		},
		"score": score,
		"sources": map[string]any{
			"readers": readersCount,
			"ratings": ratingsCount,
			"shelf":   profile.TotalBooksRead,
			"friends": len(profile.FollowedUsersWithBook),
		},
		"scans_remaining": newRemaining,
	}

	if debug {
		resp["community_summary"] = communitySummary
		resp["user_profile"] = profile
	}

	types.WriteJSON(w, http.StatusOK, resp)
}

// callClaudeForScore calls the Claude API and retries once on JSON parse failure.
func (h *ScanHandler) callClaudeForScore(ctx context.Context, book scanBookMeta, communitySummary string, profile *UserReadingProfile) (*ScanResult, error) {
	result, err := h.doClaudeCall(ctx, book, communitySummary, profile)
	if err != nil {
		slog.Warn("claude call failed, retrying", "error", err)
		result, err = h.doClaudeCall(ctx, book, communitySummary, profile)
	}
	return result, err
}

func (h *ScanHandler) doClaudeCall(ctx context.Context, book scanBookMeta, communitySummary string, profile *UserReadingProfile) (*ScanResult, error) {
	if h.Config.AnthropicAPIKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not configured")
	}

	prompt := buildClaudePrompt(book, communitySummary, profile)

	reqBody, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 1000,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal claude request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build claude request: %w", err)
	}
	req.Header.Set("x-api-key", h.Config.AnthropicAPIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := h.ClaudeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude API %d: %s", resp.StatusCode, raw)
	}

	var claudeResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &claudeResp); err != nil {
		return nil, fmt.Errorf("decode claude response: %w", err)
	}
	if len(claudeResp.Content) == 0 || claudeResp.Content[0].Text == "" {
		return nil, fmt.Errorf("claude returned empty content")
	}

	text := claudeResp.Content[0].Text

	// Strip markdown code fences if present.
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx != -1 {
			text = text[idx+1:]
		}
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}

	var result ScanResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return nil, fmt.Errorf("parse claude JSON: %w (raw: %.200s)", err, text)
	}

	return &result, nil
}

// buildClaudePrompt assembles the personalized scoring prompt.
func buildClaudePrompt(book scanBookMeta, communitySummary string, profile *UserReadingProfile) string {
	authorText := "Unknown"
	if len(book.Authors) > 0 {
		authorText = strings.Join(book.Authors, ", ")
	}

	bookGenreText := "Unknown"
	if len(book.Genres) > 0 {
		bookGenreText = strings.Join(book.Genres, ", ")
	}

	topGenreText := "None"
	if len(profile.TopGenres) > 0 {
		topGenreText = strings.Join(profile.TopGenres, ", ")
	}

	// Genre distribution sorted by count desc.
	genreDistText := "None"
	if len(profile.GenreDistribution) > 0 {
		type kv struct {
			k string
			v int
		}
		pairs := make([]kv, 0, len(profile.GenreDistribution))
		for k, v := range profile.GenreDistribution {
			pairs = append(pairs, kv{k, v})
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
		parts := make([]string, 0, len(pairs))
		for _, p := range pairs {
			parts = append(parts, fmt.Sprintf("%s (%d)", p.k, p.v))
		}
		genreDistText = strings.Join(parts, ", ")
	}

	repeatText := "None yet"
	if len(profile.RepeatAuthors) > 0 {
		repeatText = strings.Join(profile.RepeatAuthors, ", ")
	}

	var recentParts []string
	for _, b := range profile.RecentReads {
		if b.Rating > 0 {
			recentParts = append(recentParts, fmt.Sprintf("%s by %s (rated %.0f)", b.Title, b.Author, b.Rating))
		} else {
			recentParts = append(recentParts, fmt.Sprintf("%s by %s (unrated)", b.Title, b.Author))
		}
	}
	recentText := "None"
	if len(recentParts) > 0 {
		recentText = strings.Join(recentParts, "; ")
	}

	var favParts []string
	for _, b := range profile.FavoriteBooks {
		favParts = append(favParts, fmt.Sprintf("%s by %s", b.Title, b.Author))
	}
	favText := "None"
	if len(favParts) > 0 {
		favText = strings.Join(favParts, "; ")
	}

	followText := "None"
	if len(profile.FollowedUsersWithBook) > 0 {
		followText = strings.Join(profile.FollowedUsersWithBook, ", ")
	}

	avgRating := "not rated any books yet"
	if profile.AverageRatingGiven > 0 {
		avgRating = fmt.Sprintf("%.1f out of 5", profile.AverageRatingGiven)
	}

	pace := "unknown"
	if profile.ReadingPaceBooksPerMonth > 0 {
		pace = fmt.Sprintf("%.1f books/month", profile.ReadingPaceBooksPerMonth)
	}

	return fmt.Sprintf(`You are a personalized book recommendation analyst for PaperBoxd.
Your job is to score how well a specific book matches a specific reader — not whether the book is good in general, but whether THIS reader will enjoy it based on their proven reading history.

Be honest, not flattering. A score of 45 is more useful to this reader than an inflated 78 if 45 is accurate. Avoid generic praise that could apply to any reader — every reason you give must reference something specific from this reader's actual data below.

## Book Being Evaluated
Title: %s
Author(s): %s
Genres: %s
Pages: %d
Description: %s

## Community Sentiment
%s

## This Reader's Profile
Total books read: %d
Top genres (by count): %s
Genre distribution: %s
Authors read 2+ times: %s
Average rating given: %s (note: this tells you if they rate generously or critically)
Recent reads: %s
Favorite books: %s
Books abandoned in TBR 90+ days: %d
Reading pace: %s
People they follow who have this book: %s

## Task
Score this book for THIS specific reader on these five dimensions, out of 20 points each:
- genre_fit: Does it match their proven genre preferences?
- writing_style: Based on authors they've rated highly, will the prose style suit them?
- length_complexity: Does the page count and apparent complexity fit their completion patterns (consider their stale TBR count and reading pace)?
- community_love: How does the broader reading community feel about this book, based on the sentiment provided?
- personal_fit: A wildcard dimension — what unique insight from this reader's specific profile makes this especially right or wrong for them?

Respond with ONLY valid JSON. No preamble, no markdown code fences, no explanation outside the JSON structure below:

{
  "overall_score": <int, sum of the five dimensions>,
  "dimensions": {
    "genre_fit": <int 0-20>,
    "writing_style": <int 0-20>,
    "length_complexity": <int 0-20>,
    "community_love": <int 0-20>,
    "personal_fit": <int 0-20>
  },
  "verdict": "<one short phrase, 4-8 words>",
  "for_you": ["<specific reason citing their actual data>", "<second reason>"],
  "against_you": ["<specific concern citing their actual data>", "<second concern>"],
  "one_line": "<the single most honest, specific sentence you can write about whether this reader will enjoy this book — this is the most important field in the response>"
}`,
		book.Title,
		authorText,
		bookGenreText,
		book.Pages,
		book.Description,
		communitySummary,
		profile.TotalBooksRead,
		topGenreText,
		genreDistText,
		repeatText,
		avgRating,
		recentText,
		favText,
		profile.StaleTBRCount,
		pace,
		followText,
	)
}

// buildUserReadingProfile assembles a structured reading profile for the user.
// Returns a zeroed profile (not an error) when the user has no reading history.
func (h *ScanHandler) buildUserReadingProfile(ctx context.Context, userID uuid.UUID, scannedISBN string) (*UserReadingProfile, error) {
	p := &UserReadingProfile{
		GenreDistribution:     map[string]int{},
		TopGenres:             []string{},
		FavoriteBooks:         []BookSummary{},
		RepeatAuthors:         []string{},
		RecentReads:           []BookSummary{},
		FollowedUsersWithBook: []string{},
	}

	// 1. Total books read
	if err := h.Pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM bookshelf WHERE user_id = $1 AND status = 'read'",
		userID,
	).Scan(&p.TotalBooksRead); err != nil {
		return nil, fmt.Errorf("total books read: %w", err)
	}

	// 2. Genre distribution from read books (categories[] on books table)
	genreRows, err := h.Pool.Query(ctx,
		`SELECT unnest(b.categories) AS genre, COUNT(*) AS cnt
		 FROM bookshelf bs
		 JOIN books b ON b.id = bs.book_id
		 WHERE bs.user_id = $1 AND bs.status = 'read'
		   AND b.categories IS NOT NULL AND cardinality(b.categories) > 0
		 GROUP BY genre
		 ORDER BY cnt DESC
		 LIMIT 20`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("genre distribution: %w", err)
	}
	defer genreRows.Close()
	for genreRows.Next() {
		var genre string
		var cnt int
		if err := genreRows.Scan(&genre, &cnt); err != nil {
			return nil, fmt.Errorf("genre scan: %w", err)
		}
		if genre == "" {
			continue
		}
		p.GenreDistribution[genre] = cnt
		if len(p.TopGenres) < 5 {
			p.TopGenres = append(p.TopGenres, genre)
		}
	}
	if err := genreRows.Err(); err != nil {
		return nil, fmt.Errorf("genre rows: %w", err)
	}

	// 3. Favorite books (favorites table, ordered by display_order)
	favRows, err := h.Pool.Query(ctx,
		`SELECT b.title, b.authors, bs.rating
		 FROM favorites f
		 JOIN books b ON b.id = f.book_id
		 LEFT JOIN bookshelf bs ON bs.book_id = f.book_id AND bs.user_id = f.user_id
		 WHERE f.user_id = $1
		 ORDER BY f.display_order ASC
		 LIMIT 5`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("favorite books: %w", err)
	}
	defer favRows.Close()
	for favRows.Next() {
		var title string
		var bookAuthors []string
		var rating pgtype.Int4
		if err := favRows.Scan(&title, &bookAuthors, &rating); err != nil {
			return nil, fmt.Errorf("favorite scan: %w", err)
		}
		s := BookSummary{Title: title}
		if len(bookAuthors) > 0 {
			s.Author = bookAuthors[0]
		}
		if rating.Valid {
			s.Rating = float64(rating.Int32)
		}
		p.FavoriteBooks = append(p.FavoriteBooks, s)
	}
	if err := favRows.Err(); err != nil {
		return nil, fmt.Errorf("favorite rows: %w", err)
	}

	// 4. Repeat authors (pre-computed user_authors_read table)
	authorRows, err := h.Pool.Query(ctx,
		`SELECT author_name FROM user_authors_read
		 WHERE user_id = $1 AND books_read >= 2
		 ORDER BY books_read DESC
		 LIMIT 10`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("repeat authors: %w", err)
	}
	defer authorRows.Close()
	for authorRows.Next() {
		var name string
		if err := authorRows.Scan(&name); err != nil {
			return nil, fmt.Errorf("author scan: %w", err)
		}
		p.RepeatAuthors = append(p.RepeatAuthors, name)
	}
	if err := authorRows.Err(); err != nil {
		return nil, fmt.Errorf("author rows: %w", err)
	}

	// 5. Average rating given (NULL if no ratings yet → 0)
	var avgRating pgtype.Float8
	if err := h.Pool.QueryRow(ctx,
		"SELECT AVG(rating::float8) FROM bookshelf WHERE user_id = $1 AND rating IS NOT NULL",
		userID,
	).Scan(&avgRating); err != nil {
		return nil, fmt.Errorf("average rating: %w", err)
	}
	if avgRating.Valid {
		p.AverageRatingGiven = avgRating.Float64
	}

	// 6. Recent reads (last 5 by finish/update date)
	recentRows, err := h.Pool.Query(ctx,
		`SELECT b.title, b.authors, bs.rating
		 FROM bookshelf bs
		 JOIN books b ON b.id = bs.book_id
		 WHERE bs.user_id = $1 AND bs.status = 'read'
		 ORDER BY COALESCE(bs.finished_at, bs.updated_at) DESC
		 LIMIT 5`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("recent reads: %w", err)
	}
	defer recentRows.Close()
	for recentRows.Next() {
		var title string
		var bookAuthors []string
		var rating pgtype.Int4
		if err := recentRows.Scan(&title, &bookAuthors, &rating); err != nil {
			return nil, fmt.Errorf("recent reads scan: %w", err)
		}
		s := BookSummary{Title: title}
		if len(bookAuthors) > 0 {
			s.Author = bookAuthors[0]
		}
		if rating.Valid {
			s.Rating = float64(rating.Int32)
		}
		p.RecentReads = append(p.RecentReads, s)
	}
	if err := recentRows.Err(); err != nil {
		return nil, fmt.Errorf("recent reads rows: %w", err)
	}

	// 7. Stale TBR count (in TBR list 90+ days, still unread)
	if err := h.Pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM bookshelf WHERE user_id = $1 AND status = 'tbr' AND created_at < NOW() - INTERVAL '90 days'",
		userID,
	).Scan(&p.StaleTBRCount); err != nil {
		return nil, fmt.Errorf("stale tbr: %w", err)
	}

	// 8. Reading pace — only meaningful with 3+ books read
	if p.TotalBooksRead >= 3 {
		var recentCount int
		if err := h.Pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM bookshelf WHERE user_id = $1 AND status = 'read' AND finished_at > NOW() - INTERVAL '90 days'",
			userID,
		).Scan(&recentCount); err != nil {
			return nil, fmt.Errorf("reading pace: %w", err)
		}
		p.ReadingPaceBooksPerMonth = float64(recentCount) / 3.0
	}

	// 9. Followed users who have the scanned book on their shelf
	followRows, err := h.Pool.Query(ctx,
		`SELECT u.username
		 FROM follows f
		 JOIN users u ON u.id = f.following_id
		 JOIN bookshelf bs ON bs.user_id = f.following_id
		 JOIN books b ON b.id = bs.book_id
		 WHERE f.follower_id = $1
		   AND b.isbn_13 = $2
		   AND u.deleted_at IS NULL
		 LIMIT 5`,
		userID, scannedISBN,
	)
	if err != nil {
		return nil, fmt.Errorf("followed users with book: %w", err)
	}
	defer followRows.Close()
	for followRows.Next() {
		var username string
		if err := followRows.Scan(&username); err != nil {
			return nil, fmt.Errorf("follow scan: %w", err)
		}
		p.FollowedUsersWithBook = append(p.FollowedUsersWithBook, username)
	}
	if err := followRows.Err(); err != nil {
		return nil, fmt.Errorf("follow rows: %w", err)
	}

	return p, nil
}

// fetchOpenLibraryCounts looks the book up on Open Library by ISBN and returns two
// real community numbers: readers (total reading-log entries — want-to-read +
// currently-reading + already-read) and ratings (rating count). Open Library needs
// no API key and reliably populates these for real books. Returns (0, 0) on failure.
func (h *ScanHandler) fetchOpenLibraryCounts(ctx context.Context, isbn string) (readers, ratings int) {
	apiURL := fmt.Sprintf(
		"https://openlibrary.org/search.json?isbn=%s&fields=readinglog_count,ratings_count",
		url.QueryEscape(isbn),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		slog.Warn("openlibrary request build failed", "error", err)
		return 0, 0
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "paperboxd-scan/1.0")

	resp, err := h.Client.Do(req)
	if err != nil {
		slog.Warn("openlibrary request failed", "error", err)
		return 0, 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("openlibrary non-200", "status", resp.StatusCode)
		return 0, 0
	}

	var payload struct {
		Docs []struct {
			ReadinglogCount int `json:"readinglog_count"`
			RatingsCount    int `json:"ratings_count"`
		} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		slog.Warn("openlibrary decode failed", "error", err)
		return 0, 0
	}
	if len(payload.Docs) == 0 {
		return 0, 0
	}
	return payload.Docs[0].ReadinglogCount, payload.Docs[0].RatingsCount
}

// searchBrave calls the Brave Search API with the given query and returns
// joined result descriptions capped at 1500 characters.
func (h *ScanHandler) searchBrave(r *http.Request, query string) (string, error) {
	if h.Config.BraveAPIKey == "" {
		return "", fmt.Errorf("brave api key not configured")
	}

	searchURL := fmt.Sprintf(
		"https://api.search.brave.com/res/v1/web/search?q=%s&count=5",
		url.QueryEscape(query),
	)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, searchURL, nil)
	if err != nil {
		return "", fmt.Errorf("brave request build: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", h.Config.BraveAPIKey)

	resp, err := h.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("brave request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("brave status: %d", resp.StatusCode)
	}

	var body io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return "", fmt.Errorf("brave gzip decode: %w", err)
		}
		defer gz.Close()
		body = gz
	}

	var payload struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return "", fmt.Errorf("brave decode: %w", err)
	}

	var parts []string
	for _, res := range payload.Web.Results {
		if d := strings.TrimSpace(res.Description); d != "" {
			parts = append(parts, res.Title+": "+d)
		}
	}
	if len(parts) == 0 {
		return "", nil
	}

	result := strings.Join(parts, "\n")
	if len(result) > 1500 {
		result = result[:1500]
	}
	return result, nil
}
