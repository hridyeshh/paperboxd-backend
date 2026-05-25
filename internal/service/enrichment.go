package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// EnrichableBook holds the fields needed for description enrichment and embedding.
type EnrichableBook struct {
	ID            string
	Title         string
	Subtitle      string
	Authors       []string
	Publisher     string
	Categories    []string
	Description   string
	OpenLibraryID string
	ISBNdbID      string
	ISBN13        string
	GoogleBooksID string
	Metadata      []byte
}

// Enricher fetches missing book descriptions from external sources.
type Enricher struct {
	isbndbAPIKey      string
	googleBooksAPIKey string
	client            *http.Client
}

func NewEnricher(isbndbAPIKey, googleBooksAPIKey string) *Enricher {
	return &Enricher{
		isbndbAPIKey:      isbndbAPIKey,
		googleBooksAPIKey: googleBooksAPIKey,
		client:            &http.Client{Timeout: 10 * time.Second},
	}
}

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

// StripHTML removes HTML tags and trims whitespace. Used for Tiptap diary content.
func StripHTML(s string) string {
	return strings.TrimSpace(htmlTagRe.ReplaceAllString(s, ""))
}

// EnrichBookDescription tries Open Library → ISBNdb → Google Books → metadata jsonb.
// Returns the first description longer than 200 chars and its source name.
// Returns ("", "title_only", nil) when no external source has a good description.
func (e *Enricher) EnrichBookDescription(ctx context.Context, book EnrichableBook) (description, source string, err error) {
	// SOURCE 1: Open Library — free, no API key, high quality descriptions
	if book.OpenLibraryID != "" {
		desc, fetchErr := e.fetchOpenLibraryDescription(ctx, book.OpenLibraryID)
		if fetchErr == nil && len(desc) > 200 {
			return desc, "open_library", nil
		}
		if fetchErr != nil {
			slog.Warn("open library enrichment failed", "book_id", book.ID, "error", fetchErr)
		}
	}

	// SOURCE 2: ISBNdb — synopsis + overview fields
	isbn := book.ISBNdbID
	if isbn == "" {
		isbn = book.ISBN13
	}
	if isbn != "" {
		desc, fetchErr := e.fetchISBNdbDescription(ctx, isbn)
		if fetchErr == nil && len(desc) > 200 {
			return desc, "isbndb", nil
		}
		if fetchErr != nil {
			slog.Warn("isbndb enrichment failed", "book_id", book.ID, "error", fetchErr)
		}
	}

	// SOURCE 3: Google Books — strips HTML from description field
	if book.GoogleBooksID != "" {
		desc, fetchErr := e.fetchGoogleBooksDescription(ctx, book.GoogleBooksID)
		if fetchErr == nil && len(desc) > 200 {
			return desc, "google_books", nil
		}
		if fetchErr != nil {
			slog.Warn("google books enrichment failed", "book_id", book.ID, "error", fetchErr)
		}
	}

	// SOURCE 4: metadata jsonb — may contain unmapped description from migration
	if len(book.Metadata) > 0 {
		var meta map[string]interface{}
		if json.Unmarshal(book.Metadata, &meta) == nil {
			for _, key := range []string{"description", "synopsis", "overview"} {
				if v, ok := meta[key]; ok {
					if s, ok := v.(string); ok && len(s) > 200 {
						return StripHTML(s), "metadata_fallback", nil
					}
				}
			}
		}
	}

	return "", "title_only", nil
}

func (e *Enricher) fetchOpenLibraryDescription(ctx context.Context, openLibraryID string) (string, error) {
	// Normalize: accept "/works/OL123W" or "OL123W"
	id := openLibraryID
	if !strings.HasPrefix(id, "/") {
		id = "/works/" + id
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"https://openlibrary.org"+id+".json", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "PaperBoxd/1.0 (book-enrichment; contact@paperboxd.in)")

	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("open library %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", err
	}

	var result struct {
		Description interface{} `json:"description"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}

	// Open Library description field is either a string or {"type":"...","value":"..."}
	switch v := result.Description.(type) {
	case string:
		return StripHTML(v), nil
	case map[string]interface{}:
		if s, ok := v["value"].(string); ok {
			return StripHTML(s), nil
		}
	}

	return "", fmt.Errorf("no description in open library response")
}

func (e *Enricher) fetchISBNdbDescription(ctx context.Context, isbn string) (string, error) {
	if e.isbndbAPIKey == "" {
		return "", fmt.Errorf("isbndb api key not configured")
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"https://api2.isbndb.com/book/"+isbn, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", e.isbndbAPIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("isbndb %d", resp.StatusCode)
	}

	var result struct {
		Book struct {
			Synopsis string `json:"synopsis"`
			Overview string `json:"overview"`
		} `json:"book"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	synopsis := StripHTML(strings.TrimSpace(result.Book.Synopsis))
	overview := StripHTML(strings.TrimSpace(result.Book.Overview))

	if len(synopsis) > 200 {
		return synopsis, nil
	}
	if len(overview) > 200 {
		return overview, nil
	}
	combined := strings.TrimSpace(synopsis + " " + overview)
	if len(combined) > 200 {
		return combined, nil
	}

	return "", fmt.Errorf("isbndb description too short")
}

func (e *Enricher) fetchGoogleBooksDescription(ctx context.Context, googleBooksID string) (string, error) {
	url := "https://www.googleapis.com/books/v1/volumes/" + googleBooksID
	if e.googleBooksAPIKey != "" {
		url += "?key=" + e.googleBooksAPIKey
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google books %d", resp.StatusCode)
	}

	var result struct {
		VolumeInfo struct {
			Description string `json:"description"`
		} `json:"volumeInfo"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	desc := StripHTML(result.VolumeInfo.Description)
	if desc == "" {
		return "", fmt.Errorf("no description in google books response")
	}
	return desc, nil
}
