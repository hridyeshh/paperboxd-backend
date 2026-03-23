package external

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type ISBNdbClient struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
}

type ISBNdbResponse struct {
	Books []ISBNdbBook `json:"books"`
	Total int          `json:"total"`
}

type ISBNdbBook struct {
	ISBN13        string   `json:"isbn13"`
	ISBN          string   `json:"isbn"`
	Title         string   `json:"title"`
	TitleLong     string   `json:"title_long"`
	Authors       []string `json:"authors"`
	Image         string   `json:"image"`
	Publisher     string   `json:"publisher"`
	DatePublished string   `json:"date_published"`
	Pages         int      `json:"pages"`
	Language      string   `json:"language"`
	Synopsis      string   `json:"synopsis"`
	Subjects      []string `json:"subjects"`
}

func NewISBNdbClient(apiKey string) *ISBNdbClient {
	return &ISBNdbClient{
		apiKey:     apiKey,
		httpClient: &http.Client{},
		baseURL:    "https://api2.isbndb.com",
	}
}

func (c *ISBNdbClient) Search(ctx context.Context, query string, page int, pageSize int) ([]ISBNdbBook, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("isbndb api key not configured")
	}

	params := url.Values{}
	params.Add("page", fmt.Sprintf("%d", page))
	params.Add("pageSize", fmt.Sprintf("%d", pageSize))

	searchURL := fmt.Sprintf("%s/books/%s?%s", c.baseURL, url.QueryEscape(query), params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("isbndb api error: %d", resp.StatusCode)
	}

	var result ISBNdbResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Books, nil
}

func (c *ISBNdbClient) GetByISBN(ctx context.Context, isbn string) (*ISBNdbBook, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("isbndb api key not configured")
	}

	bookURL := fmt.Sprintf("%s/book/%s", c.baseURL, isbn)

	req, err := http.NewRequestWithContext(ctx, "GET", bookURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("isbndb api error: %d", resp.StatusCode)
	}

	var result struct {
		Book ISBNdbBook `json:"book"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result.Book, nil
}
