package external

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type GoogleBooksClient struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
}

type GoogleBooksResponse struct {
	Items []GoogleBook `json:"items"`
}

type GoogleBook struct {
	ID         string `json:"id"`
	VolumeInfo struct {
		Title         string   `json:"title"`
		Authors       []string `json:"authors"`
		Description   string   `json:"description"`
		PublishedDate string   `json:"publishedDate"`
		PageCount     int      `json:"pageCount"`
		Language      string   `json:"language"`
		Categories    []string `json:"categories"`
		ImageLinks    struct {
			Thumbnail string `json:"thumbnail"`
		} `json:"imageLinks"`
		IndustryIdentifiers []struct {
			Type       string `json:"type"`
			Identifier string `json:"identifier"`
		} `json:"industryIdentifiers"`
	} `json:"volumeInfo"`
}

func NewGoogleBooksClient(apiKey string) *GoogleBooksClient {
	return &GoogleBooksClient{
		apiKey:     apiKey,
		httpClient: &http.Client{},
		baseURL:    "https://www.googleapis.com/books/v1",
	}
}

func (c *GoogleBooksClient) Search(ctx context.Context, query string, maxResults int) ([]GoogleBook, error) {
	params := url.Values{}
	params.Add("q", query)
	params.Add("maxResults", fmt.Sprintf("%d", maxResults))
	if c.apiKey != "" {
		params.Add("key", c.apiKey)
	}

	reqURL := fmt.Sprintf("%s/volumes?%s", c.baseURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google books api error: %d", resp.StatusCode)
	}

	var result GoogleBooksResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Items, nil
}

func (c *GoogleBooksClient) GetByID(ctx context.Context, volumeID string) (*GoogleBook, error) {
	reqURL := fmt.Sprintf("%s/volumes/%s", c.baseURL, volumeID)
	if c.apiKey != "" {
		reqURL += "?key=" + c.apiKey
	}

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google books api error: %d", resp.StatusCode)
	}

	var book GoogleBook
	if err := json.NewDecoder(resp.Body).Decode(&book); err != nil {
		return nil, err
	}

	return &book, nil
}
