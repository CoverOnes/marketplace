package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testFileServiceID and testFileServiceToken are test-only placeholders.
const (
	testFileServiceID    = "marketplace"
	testFileServiceToken = "test-file-token-0123456789abcdef0" // ≥32 chars
)

// capturedFileRequest records the headers and body the fake file server received.
type capturedFileRequest struct {
	method       string
	path         string
	serviceID    string
	serviceToken string
	userID       string
	body         map[string]string
}

// newFileTestServer returns an httptest.Server that captures the incoming request
// and responds with the given status code + optional response body.
func newFileTestServer(t *testing.T, status int, respBody string, captured *capturedFileRequest) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.serviceID = r.Header.Get("X-Service-Id")
		captured.serviceToken = r.Header.Get("X-Service-Token")
		captured.userID = r.Header.Get("X-User-Id")

		var bodyMap map[string]string
		_ = json.NewDecoder(r.Body).Decode(&bodyMap)
		captured.body = bodyMap

		if respBody != "" {
			w.Header().Set("Content-Type", "application/json")
		}

		w.WriteHeader(status)

		if respBody != "" {
			_, _ = w.Write([]byte(respBody))
		}
	}))
}

// --- RegisterAttachment tests ---

func TestHTTPFileClient_RegisterAttachment_Success(t *testing.T) {
	fileID := uuid.New()
	listingID := uuid.New()
	ownerID := uuid.New()

	var captured capturedFileRequest

	srv := newFileTestServer(t, http.StatusNoContent, "", &captured)
	defer srv.Close()

	c := client.NewHTTPFileClient(srv.URL, testFileServiceID, testFileServiceToken)
	err := c.RegisterAttachment(context.Background(), fileID, listingID, ownerID)

	require.NoError(t, err)

	// Assert the correct S2S request shape.
	assert.Equal(t, http.MethodPost, captured.method)
	assert.Equal(t, "/internal/v1/attachments", captured.path)
	assert.Equal(t, testFileServiceID, captured.serviceID, "X-Service-Id must be marketplace service ID")
	assert.Equal(t, testFileServiceToken, captured.serviceToken, "X-Service-Token must be set in header, never in URL")
	assert.Equal(t, ownerID.String(), captured.userID, "X-User-Id must be the attachment owner's UUID")
	assert.Equal(t, fileID.String(), captured.body["fileId"])
	assert.Equal(t, "listing", captured.body["entityType"])
	assert.Equal(t, listingID.String(), captured.body["entityId"])
}

func TestHTTPFileClient_RegisterAttachment_404MapsToUpstreamFile(t *testing.T) {
	var captured capturedFileRequest
	srv := newFileTestServer(t, http.StatusNotFound, "", &captured)
	defer srv.Close()

	c := client.NewHTTPFileClient(srv.URL, testFileServiceID, testFileServiceToken)
	err := c.RegisterAttachment(context.Background(), uuid.New(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUpstreamFile),
		"404 from file service must wrap ErrUpstreamFile, not leak the raw response")
}

func TestHTTPFileClient_RegisterAttachment_403MapsToUpstreamFile(t *testing.T) {
	var captured capturedFileRequest
	srv := newFileTestServer(t, http.StatusForbidden, "", &captured)
	defer srv.Close()

	c := client.NewHTTPFileClient(srv.URL, testFileServiceID, testFileServiceToken)
	err := c.RegisterAttachment(context.Background(), uuid.New(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUpstreamFile))
}

func TestHTTPFileClient_RegisterAttachment_400MapsToUpstreamFile(t *testing.T) {
	var captured capturedFileRequest
	srv := newFileTestServer(t, http.StatusBadRequest, "", &captured)
	defer srv.Close()

	c := client.NewHTTPFileClient(srv.URL, testFileServiceID, testFileServiceToken)
	err := c.RegisterAttachment(context.Background(), uuid.New(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUpstreamFile))
}

func TestHTTPFileClient_RegisterAttachment_TokenNotInURL(t *testing.T) {
	var captured capturedFileRequest
	srv := newFileTestServer(t, http.StatusNoContent, "", &captured)
	defer srv.Close()

	c := client.NewHTTPFileClient(srv.URL, testFileServiceID, testFileServiceToken)
	_ = c.RegisterAttachment(context.Background(), uuid.New(), uuid.New(), uuid.New())

	// Security: the raw URL must not contain the service token.
	// (This is a structural check — the implementation uses req.Header.Set, not URL params.)
	assert.NotContains(t, captured.path, testFileServiceToken,
		"service token must never appear in the URL path")
}

// --- PresignAttachment tests ---

func TestHTTPFileClient_PresignAttachment_Success(t *testing.T) {
	fileID := uuid.New()
	listingID := uuid.New()

	presignResp := `{"data":{"url":"https://example.com/presigned?sig=abc","ttlSeconds":900}}`

	var captured capturedFileRequest

	srv := newFileTestServer(t, http.StatusOK, presignResp, &captured)
	defer srv.Close()

	c := client.NewHTTPFileClient(srv.URL, testFileServiceID, testFileServiceToken)
	url, err := c.PresignAttachment(context.Background(), fileID, listingID)

	require.NoError(t, err)
	assert.Equal(t, "https://example.com/presigned?sig=abc", url)

	// Assert the correct S2S request shape.
	assert.Equal(t, http.MethodPost, captured.method)
	assert.Equal(t, "/internal/v1/attachments/presign", captured.path)
	assert.Equal(t, testFileServiceID, captured.serviceID)
	assert.Equal(t, testFileServiceToken, captured.serviceToken)
	assert.Empty(t, captured.userID, "presign must NOT forward X-User-Id (auth is IsAttached-gated)")
	assert.Equal(t, fileID.String(), captured.body["fileId"])
	assert.Equal(t, "listing", captured.body["entityType"])
	assert.Equal(t, listingID.String(), captured.body["entityId"])
}

func TestHTTPFileClient_PresignAttachment_404MapsToUpstreamFile(t *testing.T) {
	var captured capturedFileRequest
	srv := newFileTestServer(t, http.StatusNotFound, "", &captured)
	defer srv.Close()

	c := client.NewHTTPFileClient(srv.URL, testFileServiceID, testFileServiceToken)
	_, err := c.PresignAttachment(context.Background(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUpstreamFile))
}

func TestHTTPFileClient_PresignAttachment_403MapsToUpstreamFile(t *testing.T) {
	var captured capturedFileRequest
	srv := newFileTestServer(t, http.StatusForbidden, "", &captured)
	defer srv.Close()

	c := client.NewHTTPFileClient(srv.URL, testFileServiceID, testFileServiceToken)
	_, err := c.PresignAttachment(context.Background(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUpstreamFile))
}

func TestHTTPFileClient_PresignAttachment_MalformedJSONMapsToUpstreamFile(t *testing.T) {
	var captured capturedFileRequest
	srv := newFileTestServer(t, http.StatusOK, `not-json`, &captured)
	defer srv.Close()

	c := client.NewHTTPFileClient(srv.URL, testFileServiceID, testFileServiceToken)
	_, err := c.PresignAttachment(context.Background(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUpstreamFile))
}

func TestHTTPFileClient_PresignAttachment_MissingURLMapsToUpstreamFile(t *testing.T) {
	var captured capturedFileRequest
	// Valid JSON envelope but url field is empty.
	srv := newFileTestServer(t, http.StatusOK, `{"data":{"url":"","ttlSeconds":900}}`, &captured)
	defer srv.Close()

	c := client.NewHTTPFileClient(srv.URL, testFileServiceID, testFileServiceToken)
	_, err := c.PresignAttachment(context.Background(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUpstreamFile))
}
