// Package client provides typed HTTP clients for inter-service communication.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"
)

// ErrEmbeddingDisabled is returned by the noop embedding client when no API key is
// configured. Callers MUST handle this as a non-fatal expected error (local dev path).
var ErrEmbeddingDisabled = errors.New("embedding client: no API key configured (disabled)")

// embeddingOutputDimensions is the expected vector length for text-embedding-3-small via
// OpenRouter. MUST match the vector(1536) column in migration 000010.
const embeddingOutputDimensions = 1536

// maxEmbeddingTextRunes is the character cap applied before sending text to the embedding
// API. text-embedding-3-small supports up to 8191 tokens (~32 KiB chars); we cap at
// 20000 runes to stay well within the limit and bound the outbound request size.
const maxEmbeddingTextRunes = 20_000

// openRouterDefaultBaseURL is the canonical OpenRouter embeddings base URL.
// Overridable via config for testing or alternative routing.
const openRouterDefaultBaseURL = "https://openrouter.ai"

// openRouterEmbeddingsPath is the endpoint path for the embeddings API.
const openRouterEmbeddingsPath = "/api/v1/embeddings"

// defaultEmbeddingModel is the OpenRouter model used for all embeddings.
// This MUST match the dimension constant above (text-embedding-3-small = 1536 dims).
const defaultEmbeddingModel = "text-embedding-3-small"

// EmbeddingClient is the interface for converting text to a fixed-dimension embedding vector.
// Implementations must return exactly embeddingOutputDimensions (1536) float32 values or an error.
type EmbeddingClient interface {
	// Generate returns a 1536-dimensional float32 embedding for the given text.
	// Returns ErrEmbeddingDisabled when the client is disabled (no API key).
	// Text is capped at maxEmbeddingTextRunes before sending; callers do not need to pre-trim.
	Generate(ctx context.Context, text string) ([]float32, error)
}

// NoopEmbeddingClient is returned when no API key is configured.
// Every call returns ErrEmbeddingDisabled so local dev works without a cloud key.
type NoopEmbeddingClient struct{}

// Generate always returns ErrEmbeddingDisabled.
func (*NoopEmbeddingClient) Generate(_ context.Context, _ string) ([]float32, error) {
	return nil, ErrEmbeddingDisabled
}

// HTTPEmbeddingClient calls the OpenRouter embeddings API.
// It applies a bounded retry on HTTP 429 (rate-limited) and does NOT retry on 5xx
// (those indicate upstream failure, not transient quota exhaustion).
type HTTPEmbeddingClient struct {
	baseURL       string
	model         string
	apiKey        string // NEVER logged in full; redacted in all log output
	httpClient    *http.Client
	max429Retries int // bounded number of retries on HTTP 429
}

// EmbeddingClientConfig holds the settings for NewHTTPEmbeddingClient.
type EmbeddingClientConfig struct {
	// BaseURL is the OpenRouter base URL (default: https://openrouter.ai).
	BaseURL string
	// Model is the embedding model name (default: text-embedding-3-small).
	Model string
	// APIKey is the OpenRouter API key; sent in the Authorization header, never in the URL.
	APIKey string
	// Timeout is the per-request HTTP timeout (default: 30s).
	Timeout time.Duration
	// Max429Retries is the maximum number of retries on HTTP 429 (default: 3).
	Max429Retries int
}

// NewHTTPEmbeddingClient returns an HTTPEmbeddingClient using the provided config.
// Callers should prefer NewEmbeddingClientFromConfig which picks the right implementation
// based on whether an API key is present.
func NewHTTPEmbeddingClient(cfg EmbeddingClientConfig) *HTTPEmbeddingClient {
	if cfg.BaseURL == "" {
		cfg.BaseURL = openRouterDefaultBaseURL
	}

	if cfg.Model == "" {
		cfg.Model = defaultEmbeddingModel
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	maxRetries := cfg.Max429Retries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	return &HTTPEmbeddingClient{
		baseURL: cfg.BaseURL,
		model:   cfg.Model,
		apiKey:  cfg.APIKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		max429Retries: maxRetries,
	}
}

// NewEmbeddingClientFromConfig returns a NoopEmbeddingClient when apiKey is empty,
// or a configured HTTPEmbeddingClient otherwise. This is the preferred constructor.
func NewEmbeddingClientFromConfig(cfg EmbeddingClientConfig) EmbeddingClient {
	if cfg.APIKey == "" {
		return &NoopEmbeddingClient{}
	}

	return NewHTTPEmbeddingClient(cfg)
}

// redactAPIKey returns a safe-to-log representation of the API key.
// Only the first 4 characters are shown to allow identification without leaking the secret.
func redactAPIKey(key string) string {
	r := []rune(key)
	if len(r) <= 4 { //nolint:mnd // 4 chars shown = enough to identify key prefix without leaking it
		return "****"
	}

	return string(r[:4]) + "****"
}

// openRouterEmbeddingRequest is the JSON body for POST /api/v1/embeddings.
type openRouterEmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// openRouterEmbeddingData is one element in the "data" array of the embeddings response.
type openRouterEmbeddingData struct {
	Object    string    `json:"object"`
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

// openRouterEmbeddingResponse is the JSON envelope returned by the embeddings endpoint.
type openRouterEmbeddingResponse struct {
	Object string                    `json:"object"`
	Data   []openRouterEmbeddingData `json:"data"`
	Model  string                    `json:"model"`
}

// Generate sends text to the OpenRouter embeddings API and returns a 1536-dim vector.
// It retries up to max429Retries times on HTTP 429 with a short backoff.
// It does NOT retry on 5xx — those represent upstream server errors.
// The API key is transmitted in the Authorization header, NEVER in the URL.
func (c *HTTPEmbeddingClient) Generate(ctx context.Context, text string) ([]float32, error) {
	// Cap text length before sending to bound the outbound payload size.
	if utf8.RuneCountInString(text) > maxEmbeddingTextRunes {
		runes := []rune(text)
		text = string(runes[:maxEmbeddingTextRunes])
	}

	reqBody := openRouterEmbeddingRequest{
		Model: c.model,
		Input: text,
	}

	encoded, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("embedding client: marshal request: %w", err)
	}

	url := c.baseURL + openRouterEmbeddingsPath

	var (
		statusCode int
		respBody   []byte
	)

	for attempt := range c.max429Retries + 1 {
		statusCode, respBody, err = c.doRequest(ctx, url, encoded)
		if err != nil {
			return nil, fmt.Errorf("embedding client: HTTP request (attempt %d): %w", attempt+1, err)
		}

		if statusCode != http.StatusTooManyRequests {
			break
		}

		// 429: rate-limited. Log a warning (with redacted key) and back off before retrying.
		if attempt < c.max429Retries {
			slog.Warn("embedding client: rate-limited (429), retrying",
				"attempt", attempt+1,
				"max_retries", c.max429Retries,
				"api_key_prefix", redactAPIKey(c.apiKey),
			)

			// Simple linear backoff: (attempt+1) * 500ms, capped at 2s.
			backoff := time.Duration(attempt+1) * 500 * time.Millisecond //nolint:mnd // 500ms base backoff, well-known retry pattern
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}

			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, fmt.Errorf("embedding client: context canceled during retry backoff: %w", ctx.Err())
			}
		}
	}

	if statusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("embedding client: rate-limited (429) after %d retries", c.max429Retries)
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding client: unexpected status %d", statusCode)
	}

	var resp openRouterEmbeddingResponse
	if jsonErr := json.Unmarshal(respBody, &resp); jsonErr != nil {
		return nil, fmt.Errorf("embedding client: decode response: %w", jsonErr)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("embedding client: response contained no embedding data")
	}

	vec := resp.Data[0].Embedding
	if len(vec) != embeddingOutputDimensions {
		return nil, fmt.Errorf("embedding client: expected %d dimensions, got %d", embeddingOutputDimensions, len(vec))
	}

	return vec, nil
}

// doRequest executes a single POST to the embeddings endpoint.
// The API key is set in the Authorization header — NEVER in the URL.
// The response body is limited to 1 MiB to prevent unbounded memory growth.
func (c *HTTPEmbeddingClient) doRequest(ctx context.Context, url string, body []byte) (statusCode int, respBody []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	// API key in Authorization header, NEVER in the URL (security red-line).
	req.Header.Set("Authorization", "Bearer "+c.apiKey) // key from config env var, in header not URL
	req.Header.Set("HTTP-Referer", "https://github.com/CoverOnes/marketplace")
	req.Header.Set("X-Title", "CoverOnes-marketplace")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("execute request: %w", err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // drain so connection is reusable; error intentionally discarded
		resp.Body.Close()                     //nolint:errcheck // best-effort close after drain
	}()

	// Limit response body to 1 MiB to bound memory on unexpected large payloads.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response body: %w", err)
	}

	return resp.StatusCode, raw, nil
}
