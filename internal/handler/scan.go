package handler

import (
	"compress/gzip"
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

// Analyze handles POST /api/v1/scan/analyze.
// Validates quota, fetches ISBNdb metadata, runs three parallel Brave Search
// queries (Reddit, Goodreads, Amazon sentiment) with a 24-hour ISBN-keyed
// cache, then returns everything in a single response.
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
		"scans_remaining":   scansRemaining,
	})
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
