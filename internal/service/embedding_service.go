package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Embedder can embed a batch of texts. inputType is "search_document" for books,
// "search_query" for user queries.
type Embedder interface {
	EmbedTexts(texts []string, inputType string) ([][]float32, error)
}

const (
	cohereEmbedURL  = "https://api.cohere.com/v2/embed"
	cohereModel     = "embed-english-v3.0"
	cohereDims      = 384
	cohereBatchSize = 96
	// 650 ms between batch calls keeps us under the free-tier 100-calls/min cap.
	cohereRateDelay = 650 * time.Millisecond
)

// CohereEmbedder calls the Cohere v2 embed API.
type CohereEmbedder struct {
	apiKey string
	client *http.Client
}

func NewCohereEmbedder(apiKey string) *CohereEmbedder {
	return &CohereEmbedder{
		apiKey: apiKey,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

type cohereEmbedRequest struct {
	Texts          []string `json:"texts"`
	Model          string   `json:"model"`
	InputType      string   `json:"input_type"`
	EmbeddingTypes []string `json:"embedding_types"`
}

type cohereEmbedResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
}

// EmbedTexts sends texts to Cohere in batches of 96 and returns one vector per text.
func (e *CohereEmbedder) EmbedTexts(texts []string, inputType string) ([][]float32, error) {
	result := make([][]float32, 0, len(texts))

	for start := 0; start < len(texts); start += cohereBatchSize {
		end := min(start+cohereBatchSize, len(texts))
		batch := texts[start:end]

		vecs, err := e.embedBatch(batch, inputType)
		if err != nil {
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", start, end, err)
		}
		result = append(result, vecs...)

		if end < len(texts) {
			time.Sleep(cohereRateDelay)
		}
	}

	return result, nil
}

func (e *CohereEmbedder) embedBatch(texts []string, inputType string) ([][]float32, error) {
	body, err := json.Marshal(cohereEmbedRequest{
		Texts:          texts,
		Model:          cohereModel,
		InputType:      inputType,
		EmbeddingTypes: []string{"float"},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, cohereEmbedURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cohere API %d: %s", resp.StatusCode, raw)
	}

	var cohereResp cohereEmbedResponse
	if err := json.Unmarshal(raw, &cohereResp); err != nil {
		return nil, fmt.Errorf("decode cohere response: %w", err)
	}

	return cohereResp.Embeddings.Float, nil
}

// BookEmbedText builds the composite string fed to Cohere for a book.
// Uses all authors, publisher, and full description for richer semantic signal.
func BookEmbedText(title, subtitle string, authors []string, publisher string, categories []string, description string) string {
	var parts []string

	parts = append(parts, "Title: "+title)

	if len(authors) > 0 {
		parts = append(parts, "Authors: "+strings.Join(authors, ", "))
	}
	if publisher != "" {
		parts = append(parts, "Publisher: "+publisher)
	}
	if len(categories) > 0 {
		parts = append(parts, "Categories: "+strings.Join(categories, ", "))
	}
	if subtitle != "" {
		parts = append(parts, "Subtitle: "+subtitle)
	}
	if description != "" {
		parts = append(parts, "Description: "+description)
	}

	return strings.Join(parts, "\n")
}

// NoopEmbedder returns zero vectors — used when COHERE_API_KEY is absent.
type NoopEmbedder struct{}

func (NoopEmbedder) EmbedTexts(texts []string, _ string) ([][]float32, error) {
	slog.Warn("COHERE_API_KEY not set; embeddings disabled")
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, cohereDims)
	}
	return out, nil
}
