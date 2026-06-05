package client_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testServiceToken is a ≥32-char placeholder token used in tests only — not a real secret.
const testServiceToken = "test-service-token-0123456789abcdef"

// decodedContractBody mirrors the JSON wire shape of the workspace create-contract
// request so the test can decode and assert the authoritative field mapping without
// the production struct being exported.
type decodedContractBody struct {
	ListingID        uuid.UUID `json:"listingId"`
	AwardBidID       uuid.UUID `json:"awardBidId"`
	ClientUserID     uuid.UUID `json:"clientUserId"`
	FreelancerUserID uuid.UUID `json:"freelancerUserId"`
	Amount           string    `json:"amount"`
	Currency         string    `json:"currency"`
}

// capturedRequest records what the fake workspace server received so the test can
// assert the outgoing request shape (method, path, header, body) deterministically.
type capturedRequest struct {
	method      string
	path        string
	headerToken string
	rawURL      string
	body        decodedContractBody
}

// newWorkspaceTestServer returns an httptest.Server that captures the incoming
// request into capture and responds with the given status code.
func newWorkspaceTestServer(t *testing.T, status int, capture *capturedRequest) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.method = r.Method
		capture.path = r.URL.Path
		capture.headerToken = r.Header.Get("X-Service-Token")
		capture.rawURL = r.URL.String()

		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil && len(bodyBytes) > 0 {
			_ = json.Unmarshal(bodyBytes, &capture.body)
		}

		w.WriteHeader(status)
	}))
}

func newTestAward() *domain.Award {
	amount, _ := decimal.NewFromString("1234.5")

	return &domain.Award{
		ID:           uuid.New(),
		ListingID:    uuid.New(),
		BidID:        uuid.New(),
		OwnerUserID:  uuid.New(),
		BidderUserID: uuid.New(),
		Amount:       amount,
		Currency:     "TWD",
	}
}

// TestHTTPWorkspaceClient_CreateContract_Success asserts that on a 201 response the
// client returns nil AND sends the authoritative request shape: POST to the correct
// path, token in the X-Service-Token header (never in the URL), correct JSON body
// with the authoritative field mapping (owner→client, bidder→freelancer, bid→awardBid).
func TestHTTPWorkspaceClient_CreateContract_Success(t *testing.T) {
	t.Parallel()

	award := newTestAward()

	var capture capturedRequest

	srv := newWorkspaceTestServer(t, http.StatusCreated, &capture)
	defer srv.Close()

	c := client.NewHTTPWorkspaceClient(srv.URL, testServiceToken)

	err := c.CreateContract(context.Background(), award)
	require.NoError(t, err)

	// Method + path.
	assert.Equal(t, http.MethodPost, capture.method)
	assert.Equal(t, "/internal/v1/contracts", capture.path)

	// Token in header, NEVER in the URL.
	assert.Equal(t, testServiceToken, capture.headerToken, "token must be sent in X-Service-Token header")
	assert.NotContains(t, capture.rawURL, testServiceToken, "token must NOT appear anywhere in the URL")

	// Authoritative field mapping: owner→client, bidder→freelancer, bid→awardBid.
	assert.Equal(t, award.OwnerUserID, capture.body.ClientUserID, "OwnerUserID must map to clientUserId")
	assert.Equal(t, award.BidderUserID, capture.body.FreelancerUserID, "BidderUserID must map to freelancerUserId")
	assert.Equal(t, award.BidID, capture.body.AwardBidID, "BidID must map to awardBidId")
	assert.Equal(t, award.ListingID, capture.body.ListingID)
	assert.Equal(t, "1234.50", capture.body.Amount, "amount must be StringFixed(2)")
	assert.Equal(t, "TWD", capture.body.Currency)
}

// TestHTTPWorkspaceClient_CreateContract_StatusHandling table-drives the status-code
// contract: 201 and 409 are success (idempotent); everything else is an error.
func TestHTTPWorkspaceClient_CreateContract_StatusHandling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "201 Created is success", status: http.StatusCreated, wantErr: false},
		{name: "409 Conflict is idempotent success", status: http.StatusConflict, wantErr: false},
		{name: "400 Bad Request is an error", status: http.StatusBadRequest, wantErr: true},
		{name: "401 Unauthorized is an error", status: http.StatusUnauthorized, wantErr: true},
		{name: "500 Internal Server Error is an error", status: http.StatusInternalServerError, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var capture capturedRequest

			srv := newWorkspaceTestServer(t, tc.status, &capture)
			defer srv.Close()

			c := client.NewHTTPWorkspaceClient(srv.URL, testServiceToken)

			err := c.CreateContract(context.Background(), newTestAward())

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestHTTPWorkspaceClient_CreateContract_TransportError asserts that a transport-level
// failure (server closed before the request) surfaces as a non-nil error.
func TestHTTPWorkspaceClient_CreateContract_TransportError(t *testing.T) {
	t.Parallel()

	var capture capturedRequest

	srv := newWorkspaceTestServer(t, http.StatusCreated, &capture)
	// Close immediately so the dial fails.
	srv.Close()

	c := client.NewHTTPWorkspaceClient(srv.URL, testServiceToken)

	err := c.CreateContract(context.Background(), newTestAward())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "call workspace create-contract"),
		"transport error should be wrapped with call context, got: %v", err)
}

// --- AddPartyToContract tests ---

// decodedAddPartyBody mirrors the JSON wire shape sent to /internal/v1/multiparty-contracts.
type decodedAddPartyBody struct {
	TenderID     string  `json:"tenderId"`
	VendorUserID string  `json:"vendorUserId"`
	RoleID       *string `json:"roleId,omitempty"`
	ShareBps     int     `json:"shareBps"`
	Currency     *string `json:"currency,omitempty"`
	PosterUserID *string `json:"posterUserId,omitempty"`
}

// capturedAddPartyRequest records add-party request shape for assertions.
type capturedAddPartyRequest struct {
	method      string
	path        string
	headerToken string
	rawURL      string
	body        decodedAddPartyBody
}

// newAddPartyTestServer returns an httptest.Server that captures the incoming add-party
// request and responds with the given status code.
func newAddPartyTestServer(t *testing.T, status int, capture *capturedAddPartyRequest) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.method = r.Method
		capture.path = r.URL.Path
		capture.headerToken = r.Header.Get("X-Service-Token")
		capture.rawURL = r.URL.String()

		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil && len(bodyBytes) > 0 {
			_ = json.Unmarshal(bodyBytes, &capture.body)
		}

		w.WriteHeader(status)
	}))
}

// TestHTTPWorkspaceClient_AddPartyToContract_RequestShape verifies the outgoing request
// shape: POST to the correct path, token in header (never in URL), body has all fields.
func TestHTTPWorkspaceClient_AddPartyToContract_RequestShape(t *testing.T) {
	t.Parallel()

	tenderID := uuid.New()
	vendorID := uuid.New()
	roleID := uuid.New()
	posterID := uuid.New()

	var capture capturedAddPartyRequest

	srv := newAddPartyTestServer(t, http.StatusCreated, &capture)
	defer srv.Close()

	c := client.NewHTTPWorkspaceClient(srv.URL, testServiceToken)

	err := c.AddPartyToContract(context.Background(), client.AddPartyInput{
		TenderID:     tenderID,
		VendorUserID: vendorID,
		RoleID:       &roleID,
		ShareBps:     0,
		Currency:     nil,
		PosterUserID: &posterID,
	})
	require.NoError(t, err)

	// Method + path.
	assert.Equal(t, http.MethodPost, capture.method)
	assert.Equal(t, "/internal/v1/multiparty-contracts", capture.path)

	// Token in header, NEVER in the URL.
	assert.Equal(t, testServiceToken, capture.headerToken, "token must be in X-Service-Token header")
	assert.NotContains(t, capture.rawURL, testServiceToken, "token must NOT appear in the URL")

	// Body fields.
	assert.Equal(t, tenderID.String(), capture.body.TenderID)
	assert.Equal(t, vendorID.String(), capture.body.VendorUserID)
	require.NotNil(t, capture.body.RoleID)
	assert.Equal(t, roleID.String(), *capture.body.RoleID)
	assert.Equal(t, 0, capture.body.ShareBps, "shareBps must be 0 (Model B placeholder)")
	assert.Nil(t, capture.body.Currency)
	require.NotNil(t, capture.body.PosterUserID)
	assert.Equal(t, posterID.String(), *capture.body.PosterUserID)
}

// TestHTTPWorkspaceClient_AddPartyToContract_StatusHandling table-drives the status-code
// contract: 201 and 409 are success; everything else is an error.
func TestHTTPWorkspaceClient_AddPartyToContract_StatusHandling(t *testing.T) {
	t.Parallel()

	tenderID := uuid.New()
	vendorID := uuid.New()

	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "201 Created is success", status: http.StatusCreated, wantErr: false},
		{name: "409 Conflict is idempotent success", status: http.StatusConflict, wantErr: false},
		{name: "400 Bad Request is an error", status: http.StatusBadRequest, wantErr: true},
		{name: "401 Unauthorized is an error", status: http.StatusUnauthorized, wantErr: true},
		{name: "500 Internal Server Error is an error", status: http.StatusInternalServerError, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var capture capturedAddPartyRequest

			srv := newAddPartyTestServer(t, tc.status, &capture)
			defer srv.Close()

			c := client.NewHTTPWorkspaceClient(srv.URL, testServiceToken)

			err := c.AddPartyToContract(context.Background(), client.AddPartyInput{
				TenderID:     tenderID,
				VendorUserID: vendorID,
				ShareBps:     0,
			})

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestHTTPWorkspaceClient_AddPartyToContract_TransportError asserts that a transport-level
// failure surfaces as a non-nil error.
func TestHTTPWorkspaceClient_AddPartyToContract_TransportError(t *testing.T) {
	t.Parallel()

	var capture capturedAddPartyRequest

	srv := newAddPartyTestServer(t, http.StatusCreated, &capture)
	srv.Close() // Close immediately so the dial fails.

	c := client.NewHTTPWorkspaceClient(srv.URL, testServiceToken)

	err := c.AddPartyToContract(context.Background(), client.AddPartyInput{
		TenderID:     uuid.New(),
		VendorUserID: uuid.New(),
		ShareBps:     0,
	})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "call workspace add-party"),
		"transport error should be wrapped with call context, got: %v", err)
}
