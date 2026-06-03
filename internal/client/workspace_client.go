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

// WorkspaceClient is the interface for calling the workspace service from marketplace.
// Defined as an interface so tests can inject a fake without starting a real server.
type WorkspaceClient interface {
	// CreateContract calls POST /internal/v1/contracts on the workspace service.
	// The values in the award are authoritative — workspace stores them verbatim,
	// preventing the client mass-assignment vulnerability (M-2 / CWE-915).
	CreateContract(ctx context.Context, award *domain.Award) error
}

// HTTPWorkspaceClient is the real implementation backed by net/http.
type HTTPWorkspaceClient struct {
	baseURL      string
	serviceToken string
	httpClient   *http.Client
}

// NewHTTPWorkspaceClient returns an HTTPWorkspaceClient.
// baseURL must NOT have a trailing slash (e.g. "http://workspace:8082").
// serviceToken is the shared secret expected by workspace's RequireServiceToken middleware.
func NewHTTPWorkspaceClient(baseURL, serviceToken string) *HTTPWorkspaceClient {
	return &HTTPWorkspaceClient{
		baseURL:      baseURL,
		serviceToken: serviceToken,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// createContractRequest is the JSON body sent to workspace POST /internal/v1/contracts.
type createContractRequest struct {
	ListingID        uuid.UUID `json:"listingId"`
	AwardBidID       uuid.UUID `json:"awardBidId"`
	ClientUserID     uuid.UUID `json:"clientUserId"`
	FreelancerUserID uuid.UUID `json:"freelancerUserId"`
	Amount           string    `json:"amount"` // numeric string to preserve precision
	Currency         string    `json:"currency"`
}

// CreateContract sends the authoritative award data to workspace to create a DRAFT contract.
// The token is transmitted in the X-Service-Token header — NEVER in the URL.
func (c *HTTPWorkspaceClient) CreateContract(ctx context.Context, award *domain.Award) error {
	body := createContractRequest{
		ListingID:        award.ListingID,
		AwardBidID:       award.BidID,
		ClientUserID:     award.OwnerUserID,
		FreelancerUserID: award.BidderUserID,
		Amount:           award.Amount.StringFixed(2),
		Currency:         award.Currency,
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal workspace create-contract request: %w", err)
	}

	url := c.baseURL + "/internal/v1/contracts"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build workspace request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", c.serviceToken) // token in header, never in URL

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call workspace create-contract: %w", err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // drain body so connection is reusable; error intentionally discarded
		resp.Body.Close()                     //nolint:errcheck // best-effort close after drain
	}()

	// 201 Created = success. 409 Conflict = idempotent duplicate (award already
	// created a contract — treat as success to handle at-least-once retries).
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusConflict {
		return nil
	}

	return fmt.Errorf("workspace create-contract returned unexpected status %d", resp.StatusCode)
}
