package external

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// storeCountry is the storefront for Read Now store links (Apple Books, Google
// Play). ponytail: India-only for now; a per-reader country needs a country
// dimension on book_read_links.
const storeCountry = "IN"

// AppleBooksClient looks up ebooks on the keyless iTunes Lookup API.
type AppleBooksClient struct {
	httpClient *http.Client
	baseURL    string
}

type appleLookupResponse struct {
	ResultCount int `json:"resultCount"`
	Results     []struct {
		TrackViewURL string `json:"trackViewUrl"`
	} `json:"results"`
}

func NewAppleBooksClient() *AppleBooksClient {
	return &AppleBooksClient{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://itunes.apple.com",
	}
}

// LookupByISBN returns the Apple Books URL for an ISBN, or "" if the store
// doesn't carry it.
func (c *AppleBooksClient) LookupByISBN(ctx context.Context, isbn string) (string, error) {
	params := url.Values{}
	params.Add("isbn", isbn)
	params.Add("entity", "ebook")
	params.Add("country", storeCountry)

	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/lookup?"+params.Encode(), nil)
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("itunes lookup api error: %d", resp.StatusCode)
	}

	var result appleLookupResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.ResultCount == 0 || len(result.Results) == 0 {
		return "", nil
	}
	return result.Results[0].TrackViewURL, nil
}
