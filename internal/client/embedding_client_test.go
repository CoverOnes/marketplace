package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// make1536Vec returns a float32 slice of length 1536 filled with the given value.
// Used to build fake API responses without repeating the literal 1536 everywhere.
func make1536Vec(v float32) []float32 {
	vec := make([]float32, 1536)
	for i := range vec {
		vec[i] = v
	}

	return vec
}

// makeVecOfLen returns a float32 slice of arbitrary length filled with 0.1.
func makeVecOfLen(n int) []float32 {
	vec := make([]float32, n)
	for i := range vec {
		vec[i] = 0.1
	}

	return vec
}

// embeddingResponse is the shape returned by the fake server to mirror OpenRouter's wire format.
type embeddingResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
}

// capturedEmbedReq records what the fake embedding server received.
type capturedEmbedReq struct {
	method     string
	path       string
	authHeader string
	rawURL     string
}

// newEmbeddingOKServer starts a test HTTP server that returns HTTP 200 with the given embedding vector.
// If vec is nil, the response data array is empty.
// Named "OK" to avoid the unparam linter warning (status was always 200 across all callers).
func newEmbeddingOKServer(t *testing.T, vec []float32) (*httptest.Server, *capturedEmbedReq) {
	t.Helper()

	captured := &capturedEmbedReq{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.authHeader = r.Header.Get("Authorization")
		captured.rawURL = r.URL.String()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if vec != nil {
			resp := embeddingResponse{
				Object: "list",
				Model:  "text-embedding-3-small",
			}
			resp.Data = append(resp.Data, struct {
				Object    string    `json:"object"`
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{Object: "embedding", Embedding: vec, Index: 0})

			_ = json.NewEncoder(w).Encode(resp)
		}
	}))

	t.Cleanup(srv.Close)

	return srv, captured
}

// --- Noop client tests ---

// TestNoopEmbeddingClient_ReturnsErrEmbeddingDisabled asserts that calling Generate on the noop
// client always returns ErrEmbeddingDisabled and never panics.
func TestNoopEmbeddingClient_ReturnsErrEmbeddingDisabled(t *testing.T) {
	t.Parallel()

	c := &client.NoopEmbeddingClient{}

	vec, err := c.Generate(context.Background(), "some text")

	require.Error(t, err)
	assert.Nil(t, vec)
	assert.True(t, errors.Is(err, client.ErrEmbeddingDisabled),
		"expected ErrEmbeddingDisabled, got: %v", err)
}

// TestNewEmbeddingClientFromConfig_NoopOnEmptyKey asserts that NewEmbeddingClientFromConfig
// returns a noop when the API key is empty, and an HTTP client otherwise.
func TestNewEmbeddingClientFromConfig_NoopOnEmptyKey(t *testing.T) {
	t.Parallel()

	noopClient := client.NewEmbeddingClientFromConfig(client.EmbeddingClientConfig{})

	vec, err := noopClient.Generate(context.Background(), "hello")
	require.Error(t, err)
	assert.True(t, errors.Is(err, client.ErrEmbeddingDisabled))
	assert.Nil(t, vec)
}

// --- HTTP client happy path ---

// TestHTTPEmbeddingClient_Generate_Success asserts that a 200 response with a 1536-dim
// vector is parsed correctly and that the API key appears only in the Authorization header.
func TestHTTPEmbeddingClient_Generate_Success(t *testing.T) {
	t.Parallel()

	vec := make1536Vec(0.42)
	srv, captured := newEmbeddingOKServer(t, vec)

	const testKey = "sk-or-test-placeholder-key-12345"

	c := client.NewHTTPEmbeddingClient(client.EmbeddingClientConfig{
		BaseURL: srv.URL,
		APIKey:  testKey,
		Timeout: 5 * time.Second,
	})

	got, err := c.Generate(context.Background(), "hello world")
	require.NoError(t, err)
	require.Len(t, got, 1536, "Generate must return exactly 1536 dimensions")
	assert.Equal(t, float32(0.42), got[0])

	// Request shape assertions.
	assert.Equal(t, http.MethodPost, captured.method)
	assert.Equal(t, "/api/v1/embeddings", captured.path)

	// API key in Authorization header, NEVER in the URL.
	assert.Equal(t, "Bearer "+testKey, captured.authHeader, "API key must be in Authorization header")
	assert.NotContains(t, captured.rawURL, testKey, "API key must NOT appear in the URL")
}

// --- Dimension enforcement ---

// TestHTTPEmbeddingClient_Generate_WrongDimension asserts that when the upstream returns a
// vector of unexpected length, Generate returns an error containing the mismatch info.
func TestHTTPEmbeddingClient_Generate_WrongDimension(t *testing.T) {
	t.Parallel()

	wrongVec := makeVecOfLen(512) // deliberately wrong
	srv, _ := newEmbeddingOKServer(t, wrongVec)

	//nolint:gosec // G101: test-only placeholder value, not a real API key
	c := client.NewHTTPEmbeddingClient(client.EmbeddingClientConfig{
		BaseURL: srv.URL,
		APIKey:  "sk-test-key-00000000000000000",
		Timeout: 5 * time.Second,
	})

	got, err := c.Generate(context.Background(), "text")
	require.Error(t, err)
	assert.Nil(t, got)
	assert.True(t, strings.Contains(err.Error(), "1536"),
		"error must mention expected dimension 1536, got: %v", err)
}

// TestHTTPEmbeddingClient_Generate_EmptyDataArray asserts that when the upstream returns an
// empty data array, Generate returns an error rather than panicking.
func TestHTTPEmbeddingClient_Generate_EmptyDataArray(t *testing.T) {
	t.Parallel()

	// nil vec causes newEmbeddingOKServer to write an empty data array.
	srv, _ := newEmbeddingOKServer(t, nil)

	//nolint:gosec // G101: test-only placeholder value, not a real API key
	c := client.NewHTTPEmbeddingClient(client.EmbeddingClientConfig{
		BaseURL: srv.URL,
		APIKey:  "sk-test-key-00000000000000000",
		Timeout: 5 * time.Second,
	})

	got, err := c.Generate(context.Background(), "text")
	require.Error(t, err)
	assert.Nil(t, got)
}

// --- 429 retry behavior ---

// TestHTTPEmbeddingClient_Generate_429_Retry asserts that the client retries on 429
// and eventually succeeds when the server starts returning 200.
func TestHTTPEmbeddingClient_Generate_429_Retry(t *testing.T) {
	t.Parallel()

	// Server returns 429 for the first 2 requests, then 200 on the 3rd.
	callCount := 0
	vec := make1536Vec(0.1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		resp := embeddingResponse{
			Object: "list",
			Model:  "text-embedding-3-small",
		}
		resp.Data = append(resp.Data, struct {
			Object    string    `json:"object"`
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}{Object: "embedding", Embedding: vec, Index: 0})

		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	//nolint:gosec // G101: test-only placeholder value, not a real API key
	c := client.NewHTTPEmbeddingClient(client.EmbeddingClientConfig{
		BaseURL:       srv.URL,
		APIKey:        "sk-test-key-00000000000000000",
		Timeout:       5 * time.Second,
		Max429Retries: 3, // enough to get past 2 failures
	})

	got, err := c.Generate(context.Background(), "text")
	require.NoError(t, err, "should succeed after retrying through 429s")
	require.Len(t, got, 1536)
	assert.Equal(t, 3, callCount, "should have been called 3 times (2 × 429 + 1 × 200)")
}

// TestHTTPEmbeddingClient_Generate_429_ExhaustedRetries asserts that when the server
// returns 429 for every attempt, Generate returns an error after the bounded retry limit.
func TestHTTPEmbeddingClient_Generate_429_ExhaustedRetries(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	//nolint:gosec // G101: test-only placeholder value, not a real API key
	c := client.NewHTTPEmbeddingClient(client.EmbeddingClientConfig{
		BaseURL:       srv.URL,
		APIKey:        "sk-test-key-00000000000000000",
		Timeout:       5 * time.Second,
		Max429Retries: 2, // small for fast test
	})

	got, err := c.Generate(context.Background(), "text")
	require.Error(t, err, "must error after exhausting 429 retries")
	assert.Nil(t, got)
	assert.True(t, strings.Contains(err.Error(), "429"),
		"error must mention 429, got: %v", err)
}

// --- 5xx bounded (no infinite retry) ---

// TestHTTPEmbeddingClient_Generate_5xx_NoRetry asserts that a 5xx response is returned as
// an error immediately without retrying (5xx = server error, not rate-limit exhaustion).
func TestHTTPEmbeddingClient_Generate_5xx_NoRetry(t *testing.T) {
	t.Parallel()

	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	//nolint:gosec // G101: test-only placeholder value, not a real API key
	c := client.NewHTTPEmbeddingClient(client.EmbeddingClientConfig{
		BaseURL:       srv.URL,
		APIKey:        "sk-test-key-00000000000000000",
		Timeout:       5 * time.Second,
		Max429Retries: 3,
	})

	got, err := c.Generate(context.Background(), "text")
	require.Error(t, err, "5xx must surface as error")
	assert.Nil(t, got)
	assert.Equal(t, 1, callCount, "5xx must NOT be retried — only 1 attempt expected")
}

// --- Transport error ---

// TestHTTPEmbeddingClient_Generate_TransportError asserts that a connection-level failure
// surfaces as an error without panicking.
func TestHTTPEmbeddingClient_Generate_TransportError(t *testing.T) {
	t.Parallel()

	// Start then immediately close the server so the dial fails.
	srv, _ := newEmbeddingOKServer(t, make1536Vec(0.1))
	srv.Close()

	//nolint:gosec // G101: test-only placeholder value, not a real API key
	c := client.NewHTTPEmbeddingClient(client.EmbeddingClientConfig{
		BaseURL:       srv.URL,
		APIKey:        "sk-test-key-00000000000000000",
		Timeout:       2 * time.Second,
		Max429Retries: 0,
	})

	got, err := c.Generate(context.Background(), "text")
	require.Error(t, err)
	assert.Nil(t, got)
}

// --- Context cancellation ---

// TestHTTPEmbeddingClient_Generate_ContextCanceled asserts that a canceled context
// surfaces as an error before any network call completes.
func TestHTTPEmbeddingClient_Generate_ContextCanceled(t *testing.T) {
	t.Parallel()

	// Server that sleeps longer than the context allows.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	//nolint:gosec // G101: test-only placeholder value, not a real API key
	c := client.NewHTTPEmbeddingClient(client.EmbeddingClientConfig{
		BaseURL: srv.URL,
		APIKey:  "sk-test-key-00000000000000000",
		Timeout: 10 * time.Second,
	})

	got, err := c.Generate(ctx, "text")
	require.Error(t, err)
	assert.Nil(t, got)
}

// --- Text length cap ---

// TestHTTPEmbeddingClient_Generate_TextCap asserts that Generate does not panic on very
// long text and that the actual outbound body is capped (server receives a shorter request).
func TestHTTPEmbeddingClient_Generate_TextCap(t *testing.T) {
	t.Parallel()

	// Record what the server received.
	var receivedInput string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody struct {
			Input string `json:"input"`
		}

		raw := make([]byte, 1<<20)
		n, _ := r.Body.Read(raw)
		_ = json.Unmarshal(raw[:n], &reqBody)
		receivedInput = reqBody.Input

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		vec := make1536Vec(0.1)
		resp := embeddingResponse{Object: "list", Model: "text-embedding-3-small"}
		resp.Data = append(resp.Data, struct {
			Object    string    `json:"object"`
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}{Object: "embedding", Embedding: vec, Index: 0})
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// Build a string longer than maxEmbeddingTextRunes (20 000 runes).
	overlong := strings.Repeat("a", 25_000)

	//nolint:gosec // G101: test-only placeholder value, not a real API key
	c := client.NewHTTPEmbeddingClient(client.EmbeddingClientConfig{
		BaseURL: srv.URL,
		APIKey:  "sk-test-key-00000000000000000",
		Timeout: 5 * time.Second,
	})

	got, err := c.Generate(context.Background(), overlong)
	require.NoError(t, err)
	require.Len(t, got, 1536)
	assert.LessOrEqual(t, len([]rune(receivedInput)), 20_000,
		"server must receive at most 20000 runes")
}

// --- API key redaction ---

// TestHTTPEmbeddingClient_APIKeyNotInURL asserts that the API key never appears in the
// request URL (it must only be in the Authorization header).
func TestHTTPEmbeddingClient_APIKeyNotInURL(t *testing.T) {
	t.Parallel()

	vec := make1536Vec(0.1)
	srv, captured := newEmbeddingOKServer(t, vec)

	const apiKey = "sk-or-test-unique-key-xyzabc123" //nolint:gosec // G101: test-only placeholder value, not a real API key

	c := client.NewHTTPEmbeddingClient(client.EmbeddingClientConfig{
		BaseURL: srv.URL,
		APIKey:  apiKey,
		Timeout: 5 * time.Second,
	})

	_, err := c.Generate(context.Background(), "text")
	require.NoError(t, err)

	assert.NotContains(t, captured.rawURL, apiKey, "API key must NOT appear in URL")
	assert.Contains(t, captured.authHeader, apiKey, "API key must appear in Authorization header")
}
