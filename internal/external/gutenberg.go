package external

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// GutenbergClient matches books against Project Gutenberg through the keyless
// Gutendex API. Gutendex is a shared free instance: callers rate-limit.
type GutenbergClient struct {
	httpClient *http.Client
	baseURL    string
}

type gutendexResponse struct {
	Count   int            `json:"count"`
	Results []GutendexBook `json:"results"`
}

type GutendexBook struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	Authors []struct {
		Name string `json:"name"` // "Stoker, Bram"
	} `json:"authors"`
	// Copyright is null when Gutenberg doesn't know; treated as not free.
	Copyright *bool             `json:"copyright"`
	Formats   map[string]string `json:"formats"`
}

// GutenbergMatch is a public-domain text for a book.
type GutenbergMatch struct {
	ID      int
	Title   string
	HTMLURL string
	EPUBURL string
}

func NewGutenbergClient() *GutenbergClient {
	return &GutenbergClient{
		// Gutendex is slow under load; this only runs from the backfill.
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://gutendex.com",
	}
}

// Match returns the public-domain Gutenberg text for a title and its authors,
// or nil if there is none. Gutendex search is fuzzy, so a result must name the
// same main title and share the first author's surname — otherwise "The Road"
// by Cormac McCarthy could come back as Jack London's.
func (c *GutenbergClient) Match(ctx context.Context, title string, authors []string) (*GutenbergMatch, error) {
	surname := authorSurname(authors)
	mainTitle := normalizeTitle(title)
	if surname == "" || mainTitle == "" {
		return nil, nil
	}

	params := url.Values{}
	params.Add("search", mainTitle+" "+surname)
	// "/books" 301s to "/books/".
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/books/?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gutendex api error: %d", resp.StatusCode)
	}

	var result gutendexResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return pickGutenbergMatch(result.Results, mainTitle, surname), nil
}

// pickGutenbergMatch takes the first result (Gutendex orders by popularity)
// that is out of copyright, has a readable HTML text, and passes the title and
// author guard.
func pickGutenbergMatch(results []GutendexBook, mainTitle, surname string) *GutenbergMatch {
	for _, r := range results {
		if r.Copyright == nil || *r.Copyright {
			continue
		}
		if normalizeTitle(r.Title) != mainTitle || !hasAuthorToken(r, surname) {
			continue
		}
		m := &GutenbergMatch{ID: r.ID, Title: r.Title}
		for mime, link := range r.Formats {
			switch {
			case strings.HasPrefix(mime, "text/html"):
				m.HTMLURL = link
			case strings.HasPrefix(mime, "application/epub+zip"):
				m.EPUBURL = link
			}
		}
		if m.HTMLURL == "" {
			continue
		}
		return m
	}
	return nil
}

func hasAuthorToken(r GutendexBook, surname string) bool {
	for _, a := range r.Authors {
		for _, tok := range strings.Fields(normalizeText(a.Name)) {
			if tok == surname {
				return true
			}
		}
	}
	return false
}

// authorSurname is the last word of the first author: "Mary Wollstonecraft
// Shelley" → "shelley".
func authorSurname(authors []string) string {
	if len(authors) == 0 {
		return ""
	}
	words := strings.Fields(normalizeText(authors[0]))
	if len(words) == 0 {
		return ""
	}
	return words[len(words)-1]
}

// normalizeTitle keeps the main title only — the part before a subtitle or an
// edition note — lowercased with punctuation folded to spaces.
// "Frankenstein; Or, The Modern Prometheus", "Frankenstein, or The Modern
// Prometheus" and "Frankenstein (Penguin Classics)" all become "frankenstein".
func normalizeTitle(title string) string {
	if i := strings.IndexAny(title, ":;(["); i >= 0 {
		title = title[:i]
	}
	if i := strings.Index(strings.ToLower(title), ", or "); i >= 0 {
		title = title[:i]
	}
	return normalizeText(strings.ReplaceAll(title, "&", " and "))
}

func normalizeText(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}
