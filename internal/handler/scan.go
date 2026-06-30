package handler

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	Pool    *pgxpool.Pool
	Queries *db.Queries
	Config  *config.Config
	ISBNdb  *external.ISBNdbClient
	Client  *http.Client
}

func NewScanHandler(pool *pgxpool.Pool, queries *db.Queries, cfg *config.Config, isbndb *external.ISBNdbClient) *ScanHandler {
	return &ScanHandler{
		Pool:    pool,
		Queries: queries,
		Config:  cfg,
		ISBNdb:  isbndb,
		Client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// BookSummary is a compact book representation used inside UserReadingProfile.
type BookSummary struct {
	Title  string  `json:"title"`
	Author string  `json:"author"`
	Rating float64 `json:"rating"`
}

// UserReadingProfile captures everything PaperBoxd knows about a reader,
// assembled just-in-time for the Claude prompt in Phase 4.
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

// Analyze handles POST /api/v1/scan/analyze.
func (h *ScanHandler) Analyze(w http.ResponseWriter, r *http.Request) {
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

	if scansRemaining == 0 {
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

	var communitySummary string

	var cachedSummary string
	cacheErr := h.Pool.QueryRow(r.Context(),
		"SELECT community_summary FROM scan_community_cache WHERE isbn = $1 AND cached_at > NOW() - INTERVAL '24 hours'",
		isbn13,
	).Scan(&cachedSummary)

	if cacheErr == nil {
		slog.Info("scan cache HIT", "isbn", isbn13)
		communitySummary = cachedSummary
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

		wg.Add(3)

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
			`INSERT INTO scan_community_cache (isbn, community_summary, cached_at)
			 VALUES ($1, $2, NOW())
			 ON CONFLICT (isbn) DO UPDATE
			 SET community_summary = EXCLUDED.community_summary,
			     cached_at = NOW()`,
			isbn13, communitySummary,
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

	types.WriteJSON(w, http.StatusOK, map[string]any{
		"book": map[string]any{
			"isbn":        isbn13,
			"title":       book.Title,
			"authors":     authors,
			"genres":      genres,
			"pages":       book.Pages,
			"description": book.Synopsis,
			"cover_url":   book.Image,
		},
		"community_summary": communitySummary,
		"user_profile":      profile,
		"scans_remaining":   scansRemaining,
	})
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
		   AND b.isbn13 = $2
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
