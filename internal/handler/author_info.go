package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hridyesh/paperboxd-backend/internal/cache"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

const (
	wikiUserAgent       = "PaperBoxd/1.0 (paperboxd.in; contact@paperboxd.in)"
	wikiSummaryURL      = "https://en.wikipedia.org/api/rest_v1/page/summary/"
	wikiSearchURL       = "https://en.wikipedia.org/w/api.php"
	authorInfoCachePref = "author:info:"
	authorInfoCacheTTL  = 24 * time.Hour
	wikiHTTPTimeout     = 5 * time.Second
)

// AuthorInfoResponse is the public shape returned by GET /api/v1/authors/info.
type AuthorInfoResponse struct {
	Found        bool   `json:"found"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Extract      string `json:"extract"`
	PhotoURL     string `json:"photo_url"`
	WikipediaURL string `json:"wikipedia_url"`
}

// AuthorInfoHandler holds dependencies for the author info endpoint.
type AuthorInfoHandler struct {
	Cache  *cache.Client
	Client *http.Client
}

// NewAuthorInfoHandler creates an AuthorInfoHandler with a default HTTP client.
func NewAuthorInfoHandler(c *cache.Client) *AuthorInfoHandler {
	return &AuthorInfoHandler{
		Cache:  c,
		Client: &http.Client{Timeout: wikiHTTPTimeout},
	}
}

var authorSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugifyAuthor(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = authorSlugRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// Get handles GET /api/v1/authors/info?name=...
func (h *AuthorInfoHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "name is required")
		return
	}

	ctx := r.Context()
	cacheKey := authorInfoCachePref + slugifyAuthor(name)

	if h.Cache != nil {
		var cached AuthorInfoResponse
		if err := h.Cache.GetJSON(ctx, cacheKey, &cached); err == nil {
			types.WriteJSON(w, http.StatusOK, cached)
			return
		} else if !errors.Is(err, cache.ErrMiss) {
			slog.Warn("author info cache get", "error", err, "name", name)
		}
	}

	resp := h.fetchAuthor(r, name)

	if h.Cache != nil {
		ttl := authorInfoCacheTTL
		if !resp.Found {
			// Cache negative result for a shorter window so a later Wikipedia
			// page creation isn't masked for a full day.
			ttl = time.Hour
		}
		if err := h.Cache.SetJSON(ctx, cacheKey, resp, ttl); err != nil {
			slog.Warn("author info cache set", "error", err, "name", name)
		}
	}

	types.WriteJSON(w, http.StatusOK, resp)
}

// fetchAuthor tries the Wikipedia summary endpoint, then falls back to search.
// Always returns a populated AuthorInfoResponse — Found=false when no usable
// data was retrieved so the client can still render the page.
func (h *AuthorInfoHandler) fetchAuthor(r *http.Request, name string) AuthorInfoResponse {
	notFound := AuthorInfoResponse{Found: false, Name: name}

	title := strings.ReplaceAll(name, " ", "_")
	summary, ok := h.fetchSummary(r, title)
	if ok && summary.Type != "disambiguation" && summary.Extract != "" {
		return summaryToResponse(name, summary)
	}

	// Fallback: search for "<name> author" and retry the summary endpoint.
	searched, err := h.searchTitle(r, name)
	if err != nil {
		slog.Warn("wiki search", "error", err, "name", name)
		return notFound
	}
	if searched == "" {
		return notFound
	}

	summary, ok = h.fetchSummary(r, strings.ReplaceAll(searched, " ", "_"))
	if !ok || summary.Extract == "" {
		return notFound
	}
	return summaryToResponse(name, summary)
}

type wikiSummary struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Extract     string `json:"extract"`
	Thumbnail   struct {
		Source string `json:"source"`
	} `json:"thumbnail"`
	OriginalImage struct {
		Source string `json:"source"`
	} `json:"originalimage"`
	ContentURLs struct {
		Desktop struct {
			Page string `json:"page"`
		} `json:"desktop"`
	} `json:"content_urls"`
}

func summaryToResponse(requested string, s wikiSummary) AuthorInfoResponse {
	photo := s.Thumbnail.Source
	if photo == "" {
		photo = s.OriginalImage.Source
	}
	name := s.Title
	if name == "" {
		name = requested
	}
	return AuthorInfoResponse{
		Found:        true,
		Name:         name,
		Description:  s.Description,
		Extract:      s.Extract,
		PhotoURL:     photo,
		WikipediaURL: s.ContentURLs.Desktop.Page,
	}
}

func (h *AuthorInfoHandler) fetchSummary(r *http.Request, title string) (wikiSummary, bool) {
	endpoint := wikiSummaryURL + url.PathEscape(title)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		return wikiSummary{}, false
	}
	req.Header.Set("User-Agent", wikiUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := h.Client.Do(req)
	if err != nil {
		slog.Warn("wiki summary fetch", "error", err, "title", title)
		return wikiSummary{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return wikiSummary{}, false
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return wikiSummary{}, false
	}
	var summary wikiSummary
	if err := json.Unmarshal(body, &summary); err != nil {
		return wikiSummary{}, false
	}
	return summary, true
}

func (h *AuthorInfoHandler) searchTitle(r *http.Request, name string) (string, error) {
	q := url.Values{}
	q.Set("action", "query")
	q.Set("list", "search")
	q.Set("srsearch", name+" author")
	q.Set("srlimit", "1")
	q.Set("format", "json")

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, wikiSearchURL+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", wikiUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := h.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil
	}

	var payload struct {
		Query struct {
			Search []struct {
				Title string `json:"title"`
			} `json:"search"`
		} `json:"query"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if len(payload.Query.Search) == 0 {
		return "", nil
	}
	return payload.Query.Search[0].Title, nil
}
