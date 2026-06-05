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

	// AddPartyToContract calls POST /internal/v1/multiparty-contracts on the workspace
	// service to add a late joiner at 0 bps placeholder (Model B).
	// 201 → ok; 409 → idempotent ok; any other status → error.
	AddPartyToContract(ctx context.Context, in AddPartyInput) error
}

// AddPartyInput carries the fields needed for the add-party S2S call.
type AddPartyInput struct {
	TenderID     uuid.UUID
	VendorUserID uuid.UUID
	RoleID       *uuid.UUID
	ShareBps     int        // always 0 for Phase 4 (Model B: 0 bps placeholder)
	Currency     *string    // nil for Phase 4
	PosterUserID *uuid.UUID // the listing owner who accepted the collaborator
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

// addPartyRequest is the JSON body sent to workspace POST /internal/v1/multiparty-contracts.
// shareBps is always 0 for Phase 4 (Model B: workspace handles ADDENDUM_PENDING + re-share + re-sign).
type addPartyRequest struct {
	TenderID     uuid.UUID  `json:"tenderId"`
	VendorUserID uuid.UUID  `json:"vendorUserId"`
	RoleID       *uuid.UUID `json:"roleId,omitempty"`
	ShareBps     int        `json:"shareBps"`
	Currency     *string    `json:"currency,omitempty"`
	PosterUserID *uuid.UUID `json:"posterUserId,omitempty"`
}

// awardToContractRequest is the single authoritative Award→request mapping (M-2 / CWE-915).
// It is the ONE place that defines which award field becomes which contract field:
// owner→client, bidder→freelancer, bid→awardBid. Keeping it as a named function lets
// tests pin the mapping so an accidental client/freelancer transposition is caught.
func awardToContractRequest(award *domain.Award) createContractRequest {
	return createContractRequest{
		ListingID:        award.ListingID,
		AwardBidID:       award.BidID,
		ClientUserID:     award.OwnerUserID,
		FreelancerUserID: award.BidderUserID,
		Amount:           award.Amount.StringFixed(2),
		Currency:         award.Currency,
	}
}

// doPost marshals body to JSON, POSTs it to path with the X-Service-Token header,
// drains and closes the response body, then returns the status code and any error.
// This shared helper eliminates duplication between CreateContract and AddPartyToContract.
func (c *HTTPWorkspaceClient) doPost(ctx context.Context, path string, body any) (int, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("marshal workspace request to %s: %w", path, err)
	}

	url := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return 0, fmt.Errorf("build workspace request to %s: %w", path, err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", c.serviceToken) // token in header, never in URL

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("call workspace %s: %w", path, err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // drain body so connection is reusable; error intentionally discarded
		resp.Body.Close()                     //nolint:errcheck // best-effort close after drain
	}()

	return resp.StatusCode, nil
}

// CreateContract sends the authoritative award data to workspace to create a DRAFT contract.
// The token is transmitted in the X-Service-Token header — NEVER in the URL.
func (c *HTTPWorkspaceClient) CreateContract(ctx context.Context, award *domain.Award) error {
	status, err := c.doPost(ctx, "/internal/v1/contracts", awardToContractRequest(award))
	if err != nil {
		return fmt.Errorf("call workspace create-contract: %w", err)
	}

	// 201 Created = success. 409 Conflict = idempotent duplicate (award already
	// created a contract — treat as success to handle at-least-once retries).
	if status == http.StatusCreated || status == http.StatusConflict {
		return nil
	}

	return fmt.Errorf("workspace create-contract returned unexpected status %d", status)
}

// AddPartyToContract sends an add-party request to workspace to enroll a late-joiner
// collaborator in the multiparty contract at 0 bps placeholder (Model B).
// The token is transmitted in the X-Service-Token header — NEVER in the URL.
// 201 → ok; 409 Conflict → idempotent ok (party already enrolled); any other status → error.
func (c *HTTPWorkspaceClient) AddPartyToContract(ctx context.Context, in AddPartyInput) error {
	status, err := c.doPost(ctx, "/internal/v1/multiparty-contracts", addPartyRequest(in))
	if err != nil {
		return fmt.Errorf("call workspace add-party: %w", err)
	}

	// 201 Created = success. 409 Conflict = idempotent duplicate (already a party).
	if status == http.StatusCreated || status == http.StatusConflict {
		return nil
	}

	return fmt.Errorf("workspace add-party returned unexpected status %d", status)
}
