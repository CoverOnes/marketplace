// Package client provides typed HTTP clients for inter-service communication.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/google/uuid"
)

// FileClient is the interface for calling the file service from marketplace.
// Defined as an interface so tests can inject a fake without starting a real server.
type FileClient interface {
	// RegisterAttachment calls POST /internal/v1/attachments on the file service.
	// It verifies that the file exists, is owned by ownerUserID, and is in STORED
	// status. Idempotent on duplicate (file already attached to that entity).
	// Returns ErrUpstreamFile on non-2xx responses (raw downstream body is never leaked).
	RegisterAttachment(ctx context.Context, fileID, listingID, ownerUserID uuid.UUID) error

	// PresignAttachment calls POST /internal/v1/attachments/presign on the file service.
	// It verifies that the file is attached to the listing AND in STORED status.
	// Returns the presigned download URL string.
	// Returns ErrUpstreamFile on non-2xx responses.
	PresignAttachment(ctx context.Context, fileID, listingID uuid.UUID) (string, error)
}

// HTTPFileClient is the real implementation backed by net/http.
// It mirrors the pattern of HTTPWorkspaceClient exactly.
type HTTPFileClient struct {
	baseURL      string
	serviceID    string
	serviceToken string
	httpClient   *http.Client
}

// NewHTTPFileClient returns an HTTPFileClient.
// baseURL must NOT have a trailing slash (e.g. "http://file:8083").
// serviceID is the marketplace service identifier (e.g. "marketplace").
// serviceToken is the shared secret expected by the file service's RequireServiceIdentity.
func NewHTTPFileClient(baseURL, serviceID, serviceToken string) *HTTPFileClient {
	return &HTTPFileClient{
		baseURL:      baseURL,
		serviceID:    serviceID,
		serviceToken: serviceToken,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// attachRegisterRequest is the JSON body for POST /internal/v1/attachments.
// entityType is always "listing" for marketplace attachments.
type attachRegisterRequest struct {
	FileID     string `json:"fileId"`
	EntityType string `json:"entityType"`
	EntityID   string `json:"entityId"`
}

// attachPresignRequest is the JSON body for POST /internal/v1/attachments/presign.
type attachPresignRequest struct {
	FileID     string `json:"fileId"`
	EntityType string `json:"entityType"`
	EntityID   string `json:"entityId"`
}

// presignResponseData is the "data" field of the presign response envelope.
// Shape confirmed from ../file/internal/handler/attachment_handler.go Presign:
//
//	{"data":{"url":"...","ttlSeconds":<int>}}
type presignResponseData struct {
	URL        string `json:"url"`
	TTLSeconds int    `json:"ttlSeconds"`
}

// presignEnvelope is the outer envelope returned by the file service presign endpoint.
type presignEnvelope struct {
	Data presignResponseData `json:"data"`
}

// doPost marshals body to JSON, POSTs it to path with S2S identity headers,
// drains and closes the response body, and returns the status code and body bytes.
// The X-Service-Token is sent in the header — NEVER in the URL.
func (c *HTTPFileClient) doPost(ctx context.Context, path string, body any, ownerUserID *uuid.UUID) (statusCode int, responseBody []byte, err error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal file client request to %s: %w", path, err)
	}

	url := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, fmt.Errorf("build file client request to %s: %w", path, err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Id", c.serviceID)       // service identity, not in URL
	req.Header.Set("X-Service-Token", c.serviceToken) // shared secret, not in URL

	// Register passes the attachment owner's user ID as required by the file service.
	// Presign intentionally does NOT send X-User-Id (auth is via IsAttached check).
	if ownerUserID != nil {
		req.Header.Set("X-User-Id", ownerUserID.String())
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("call file service %s: %w", path, err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // drain so connection is reusable; error intentionally discarded
		resp.Body.Close()                     //nolint:errcheck // best-effort close after drain
	}()

	// Read up to 64 KiB for presign response body; for register we discard it.
	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read file service response from %s: %w", path, err)
	}

	return resp.StatusCode, rawBody, nil
}

// RegisterAttachment calls POST /internal/v1/attachments.
// 204 = success (idempotent on duplicate). Any non-204 status is mapped to
// domain.ErrUpstreamFile — the raw downstream body is NEVER forwarded to the caller
// (prevents error-body leakage from the file service to API consumers).
func (c *HTTPFileClient) RegisterAttachment(ctx context.Context, fileID, listingID, ownerUserID uuid.UUID) error {
	status, _, err := c.doPost(ctx, "/internal/v1/attachments", attachRegisterRequest{
		FileID:     fileID.String(),
		EntityType: "listing",
		EntityID:   listingID.String(),
	}, &ownerUserID)
	if err != nil {
		return fmt.Errorf("call file register-attachment: %w", err)
	}

	// 204 No Content = success (file service returns 204 on both first-register and idempotent duplicate).
	if status == http.StatusNoContent {
		return nil
	}

	// All non-204 responses are collapsed to a generic upstream error.
	// We never leak the raw downstream body to avoid confused-deputy information disclosure.
	return fmt.Errorf("file register-attachment returned status %d: %w", status, domain.ErrUpstreamFile)
}

// PresignAttachment calls POST /internal/v1/attachments/presign.
// Returns the presigned URL string on success. Any non-200 status or JSON parse
// failure is mapped to domain.ErrUpstreamFile.
func (c *HTTPFileClient) PresignAttachment(ctx context.Context, fileID, listingID uuid.UUID) (string, error) {
	status, body, err := c.doPost(ctx, "/internal/v1/attachments/presign", attachPresignRequest{
		FileID:     fileID.String(),
		EntityType: "listing",
		EntityID:   listingID.String(),
	}, nil) // Presign: no X-User-Id (auth is IsAttached-gated by file service)
	if err != nil {
		return "", fmt.Errorf("call file presign-attachment: %w", err)
	}

	if status != http.StatusOK {
		return "", fmt.Errorf("file presign-attachment returned status %d: %w", status, domain.ErrUpstreamFile)
	}

	var envelope presignEnvelope
	if jsonErr := json.Unmarshal(body, &envelope); jsonErr != nil {
		return "", fmt.Errorf("decode file presign response: %w: %w", jsonErr, domain.ErrUpstreamFile)
	}

	if envelope.Data.URL == "" {
		return "", fmt.Errorf("file presign response missing url: %w", domain.ErrUpstreamFile)
	}

	return envelope.Data.URL, nil
}
