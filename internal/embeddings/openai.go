package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const openAIEmbeddingsURL = "https://api.openai.com/v1/embeddings"

// ErrRejectedInput marks a request the provider will refuse just as firmly the
// next time: input it will not accept, not a rate limit, an authentication
// problem or a server fault. Retrying cannot help, so the indexer isolates the
// offending messages instead of stalling every batch behind them. It lives
// here rather than with the other sentinels because only a provider client can
// decide that a refusal is permanent.
var ErrRejectedInput = errors.New("openai embeddings: input rejected")

type embedder interface {
	Embed(context.Context, []string) ([][]float32, error)
}

type openAIClient struct {
	apiKey     string
	endpoint   string
	httpClient *http.Client
}

func newOpenAIClient(apiKey string) *openAIClient {
	return &openAIClient{
		apiKey:   apiKey,
		endpoint: openAIEmbeddingsURL,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

type embeddingRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	Dimensions     int      `json:"dimensions"`
	EncodingFormat string   `json:"encoding_format"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

func (c *openAIClient) Embed(ctx context.Context, input []string) ([][]float32, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, ErrNotConfigured
	}
	if len(input) == 0 {
		return [][]float32{}, nil
	}
	for _, value := range input {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%w: empty input", ErrRejectedInput)
		}
	}
	body, err := json.Marshal(embeddingRequest{
		Input:          input,
		Model:          ModelSmall,
		Dimensions:     Dimensions,
		EncodingFormat: "float",
	})
	if err != nil {
		return nil, fmt.Errorf("openai embeddings: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai embeddings: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "openconvo")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embeddings: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Provider responses are untrusted and could echo submitted content.
		// Keep job errors useful without persisting any response body in logs.
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
			// The request itself is unacceptable — too long, malformed, refused
			// on policy. Sending it again changes nothing.
			return nil, fmt.Errorf("%w: HTTP %d", ErrRejectedInput, resp.StatusCode)
		}
		return nil, fmt.Errorf("openai embeddings: HTTP %d", resp.StatusCode)
	}
	var decoded embeddingResponse
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("openai embeddings: decode response: %w", err)
	}
	if len(decoded.Data) != len(input) {
		return nil, fmt.Errorf("openai embeddings: returned %d vectors for %d inputs", len(decoded.Data), len(input))
	}
	out := make([][]float32, len(input))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(out) {
			return nil, fmt.Errorf("openai embeddings: invalid response index %d", item.Index)
		}
		if len(item.Embedding) != Dimensions {
			return nil, fmt.Errorf("openai embeddings: vector %d has %d dimensions, expected %d", item.Index, len(item.Embedding), Dimensions)
		}
		if out[item.Index] != nil {
			return nil, fmt.Errorf("openai embeddings: duplicate response index %d", item.Index)
		}
		out[item.Index] = item.Embedding
	}
	return out, nil
}
