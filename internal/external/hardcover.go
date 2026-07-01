package external

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HardcoverClient queries the Hardcover GraphQL API (https://hardcover.app) for
// community rating stats. Hardcover is a modern Goodreads alternative with a free
// token-authenticated API — bigger, more populated numbers than Open Library.
type HardcoverClient struct {
	token      string
	httpClient *http.Client
	endpoint   string
}

// HardcoverStats holds the community numbers surfaced on the scan screen.
type HardcoverStats struct {
	Rating       float64 // average rating, 0–5
	RatingsCount int     // number of ratings
	UsersCount   int     // total users who have the book on a shelf (readers)
}

func NewHardcoverClient(token string) *HardcoverClient {
	return &HardcoverClient{
		token:      token,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		endpoint:   "https://api.hardcover.app/v1/graphql",
	}
}

// GetStatsByISBN13 looks the book up by ISBN-13 and returns its community rating
// stats. Returns (nil, nil) when the book is not on Hardcover so the caller can
// fall back gracefully. Only returns an error for transport/auth failures.
func (c *HardcoverClient) GetStatsByISBN13(ctx context.Context, isbn13 string) (*HardcoverStats, error) {
	if c.token == "" {
		return nil, fmt.Errorf("hardcover token not configured")
	}

	// Match on either ISBN-13 or ISBN-10: ISBNdb sometimes returns the 10-digit form
	// as the primary identifier, and Hardcover indexes editions under both. book.rating
	// is work-level (shared across editions), so any matching edition yields the rating.
	query := `query BookByISBN($isbn: String!) {
  editions(where: {_or: [{isbn_13: {_eq: $isbn}}, {isbn_10: {_eq: $isbn}}]}, limit: 1) {
    book { rating ratings_count users_count }
  }
}`

	reqBody, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]any{"isbn": isbn13},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal hardcover request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build hardcover request: %w", err)
	}
	// The Hardcover token already carries the "Bearer " prefix in some copies; be
	// tolerant and set the header exactly as Hardcover expects.
	auth := c.token
	if len(auth) < 7 || auth[:7] != "Bearer " {
		auth = "Bearer " + auth
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hardcover request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hardcover api error: %d", resp.StatusCode)
	}

	var result struct {
		Data struct {
			Editions []struct {
				Book struct {
					Rating       float64 `json:"rating"`
					RatingsCount int     `json:"ratings_count"`
					UsersCount   int     `json:"users_count"`
				} `json:"book"`
			} `json:"editions"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode hardcover response: %w", err)
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("hardcover graphql error: %s", result.Errors[0].Message)
	}
	if len(result.Data.Editions) == 0 {
		return nil, nil // book not on Hardcover
	}

	b := result.Data.Editions[0].Book
	return &HardcoverStats{
		Rating:       b.Rating,
		RatingsCount: b.RatingsCount,
		UsersCount:   b.UsersCount,
	}, nil
}
