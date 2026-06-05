package service_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/events"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wsTestServiceToken is a ≥32-char placeholder token for tests only — not a real secret.
//
//nolint:gosec // G101 false positive: inert test fixture string, not a real credential
const wsTestServiceToken = "bid-svc-test-token-0123456789abcdef"

// fakeWorkspaceClient captures the Award passed to CreateContract and signals
// completion through a WaitGroup so the test can await the detached goroutine
// deterministically — NO time.Sleep (reviewer flagged Sleep-based flakiness).
type fakeWorkspaceClient struct {
	wg        sync.WaitGroup
	mu        sync.Mutex
	called    bool
	gotAward  *domain.Award
	gotErr    error
	returnErr error
}

func newFakeWorkspaceClient() *fakeWorkspaceClient {
	f := &fakeWorkspaceClient{}
	f.wg.Add(1)

	return f
}

func (f *fakeWorkspaceClient) CreateContract(ctx context.Context, award *domain.Award) error {
	f.mu.Lock()
	f.called = true
	f.gotAward = award
	// Capture whether the detached goroutine handed us a live, non-canceled context
	// (it must use context.Background()-derived ctx, not the request ctx).
	f.gotErr = ctx.Err()
	f.mu.Unlock()

	defer f.wg.Done()

	return f.returnErr
}

// AddPartyToContract is a no-op stub (BidService tests do not exercise this path).
func (f *fakeWorkspaceClient) AddPartyToContract(_ context.Context, _ client.AddPartyInput) error {
	return nil
}

// await blocks until CreateContract has fired or the timeout elapses; returns false
// on timeout so the test fails loudly instead of hanging.
func (f *fakeWorkspaceClient) await(timeout time.Duration) bool {
	done := make(chan struct{})

	go func() {
		f.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// decodedWSBody mirrors the workspace create-contract JSON wire shape so the
// end-to-end mapping assertion can decode it.
type decodedWSBody struct {
	ListingID        uuid.UUID `json:"listingId"`
	AwardBidID       uuid.UUID `json:"awardBidId"`
	ClientUserID     uuid.UUID `json:"clientUserId"`
	FreelancerUserID uuid.UUID `json:"freelancerUserId"`
	Amount           string    `json:"amount"`
	Currency         string    `json:"currency"`
}

// acceptFixture bundles the stores, tx manager and the canonical IDs for an
// AcceptBid scenario so tests can set up a service in one line.
type acceptFixture struct {
	listings  *stubListingStore
	bids      *stubBidStore
	awards    *stubAwardStore
	txMgr     *stubTxManager
	ownerID   uuid.UUID
	bidderID  uuid.UUID
	listingID uuid.UUID
	bidID     uuid.UUID
}

func newAcceptFixture(t *testing.T) acceptFixture {
	t.Helper()

	ownerID := uuid.New()
	bidderID := uuid.New()
	listingID := uuid.New()
	bidID := uuid.New()

	listing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusOpen,
		Currency:    "TWD",
		Title:       "WS Test",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	amount, _ := decimal.NewFromString("4321.5")
	bid := &domain.Bid{
		ID:           bidID,
		ListingID:    listingID,
		BidderUserID: bidderID,
		Amount:       amount,
		Currency:     "TWD",
		Status:       domain.BidStatusPending,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	listingStore := newStubListingStore(listing)
	bidStore := newStubBidStore(bid)
	awardStore := newStubAwardStore()
	txMgr := &stubTxManager{listings: listingStore, bids: bidStore, awards: awardStore}

	return acceptFixture{
		listings:  listingStore,
		bids:      bidStore,
		awards:    awardStore,
		txMgr:     txMgr,
		ownerID:   ownerID,
		bidderID:  bidderID,
		listingID: listingID,
		bidID:     bidID,
	}
}

// TestBidService_AcceptBid_FiresWorkspaceContract proves the headline M-2 security fix:
// AcceptBid fires CreateContract with the authoritative Award. The detached goroutine
// is awaited deterministically via WaitGroup (no time.Sleep).
func TestBidService_AcceptBid_FiresWorkspaceContract(t *testing.T) {
	t.Parallel()

	fx := newAcceptFixture(t)

	fake := newFakeWorkspaceClient()
	publisher := events.NewNoopPublisher()

	svc := service.NewBidService(fx.bids, fx.listings, fx.awards, fx.txMgr, publisher, fake)

	award, err := svc.AcceptBid(context.Background(), fx.bidID, fx.ownerID)
	require.NoError(t, err)
	require.NotNil(t, award)

	require.True(t, fake.await(5*time.Second), "CreateContract was not called within timeout")

	fake.mu.Lock()
	defer fake.mu.Unlock()

	require.True(t, fake.called, "CreateContract must be called")
	require.NotNil(t, fake.gotAward)
	require.NoError(t, fake.gotErr, "detached goroutine must pass a live (non-canceled) context")

	// Authoritative Award field mapping at the service boundary.
	assert.Equal(t, fx.ownerID, fake.gotAward.OwnerUserID, "award owner must be the listing owner (→ ClientUserID)")
	assert.Equal(t, fx.bidderID, fake.gotAward.BidderUserID, "award bidder must be the winning bidder (→ FreelancerUserID)")
	assert.Equal(t, fx.bidID, fake.gotAward.BidID, "award bid id (→ AwardBidID)")
	assert.Equal(t, fx.listingID, fake.gotAward.ListingID)
	assert.Equal(t, "4321.50", fake.gotAward.Amount.StringFixed(2), "amount must serialize as StringFixed(2)")
}

// TestBidService_AcceptBid_WorkspaceWireMapping is load-bearing on the real DTO mapping:
// it routes the captured Award through the REAL HTTPWorkspaceClient against an
// httptest.Server and asserts the wire body. Transposing the client/freelancer mapping
// in workspace_client.go makes THIS test fail.
func TestBidService_AcceptBid_WorkspaceWireMapping(t *testing.T) {
	t.Parallel()

	fx := newAcceptFixture(t)

	var (
		mu       sync.Mutex
		gotBody  decodedWSBody
		gotToken string
		gotPath  string
		bodySeen = make(chan struct{})
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		gotToken = r.Header.Get("X-Service-Token")
		gotPath = r.URL.Path

		bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(bodyBytes, &gotBody)

		w.WriteHeader(http.StatusCreated)
		close(bodySeen)
	}))
	defer srv.Close()

	realClient := client.NewHTTPWorkspaceClient(srv.URL, wsTestServiceToken)
	publisher := events.NewNoopPublisher()

	svc := service.NewBidService(fx.bids, fx.listings, fx.awards, fx.txMgr, publisher, realClient)

	_, err := svc.AcceptBid(context.Background(), fx.bidID, fx.ownerID)
	require.NoError(t, err)

	// Deterministic await on the server-received signal (no time.Sleep).
	select {
	case <-bodySeen:
	case <-time.After(5 * time.Second):
		t.Fatal("workspace server never received the create-contract request")
	}

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "/internal/v1/contracts", gotPath)
	assert.Equal(t, wsTestServiceToken, gotToken, "token must be sent in X-Service-Token header")

	// AUTHORITATIVE mapping: owner→client, bidder→freelancer. Transposing fails here.
	assert.Equal(t, fx.ownerID, gotBody.ClientUserID, "OwnerUserID must map to clientUserId")
	assert.Equal(t, fx.bidderID, gotBody.FreelancerUserID, "BidderUserID must map to freelancerUserId")
	assert.Equal(t, fx.bidID, gotBody.AwardBidID, "BidID must map to awardBidId")
	assert.Equal(t, fx.listingID, gotBody.ListingID)
	assert.Equal(t, "4321.50", gotBody.Amount, "amount must be StringFixed(2)")
	assert.Equal(t, "TWD", gotBody.Currency)
}

// TestBidService_AcceptBid_NilWorkspaceClientNoPanic asserts the nil client is a safe
// no-op: AcceptBid still succeeds and no panic occurs in the detached goroutine path.
func TestBidService_AcceptBid_NilWorkspaceClientNoPanic(t *testing.T) {
	t.Parallel()

	fx := newAcceptFixture(t)
	publisher := events.NewNoopPublisher()

	// nil workspace client — createWorkspaceContractAsync must early-return, never panic.
	svc := service.NewBidService(fx.bids, fx.listings, fx.awards, fx.txMgr, publisher, nil)

	var award *domain.Award

	require.NotPanics(t, func() {
		var err error

		award, err = svc.AcceptBid(context.Background(), fx.bidID, fx.ownerID)
		require.NoError(t, err)
		require.NotNil(t, award)
		assert.Equal(t, fx.listingID, award.ListingID)
	})

	// The award is still committed and authoritative even though no workspace contract
	// was created — the nil client is a pure no-op, not a failure.
	stored, err := fx.awards.GetByID(context.Background(), award.ID)
	require.NoError(t, err)
	assert.Equal(t, fx.ownerID, stored.OwnerUserID)
	assert.Equal(t, fx.bidderID, stored.BidderUserID)
}
