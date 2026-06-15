package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/events"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- stub implementations ---

type stubListingStore struct {
	listings map[uuid.UUID]*domain.Listing
	updated  []*domain.Listing
	// searchResult, when set, is returned verbatim by Search so service-layer
	// tests can assert visibility filtering / cursor emission deterministically.
	searchResult []*domain.Listing
	lastSearch   *store.SearchFilter
}

func newStubListingStore(listings ...*domain.Listing) *stubListingStore {
	m := &stubListingStore{listings: make(map[uuid.UUID]*domain.Listing)}

	for _, l := range listings {
		m.listings[l.ID] = l
	}

	return m
}

func (s *stubListingStore) Create(_ context.Context, l *domain.Listing) error {
	s.listings[l.ID] = l
	return nil
}

func (s *stubListingStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Listing, error) {
	l, ok := s.listings[id]
	if !ok {
		return nil, domain.ErrListingNotFound
	}

	return l, nil
}

// GetByIDForUpdate simulates SELECT ... FOR UPDATE; in-memory stubs need no locking.
func (s *stubListingStore) GetByIDForUpdate(_ context.Context, id uuid.UUID) (*domain.Listing, error) {
	l, ok := s.listings[id]
	if !ok {
		return nil, domain.ErrListingNotFound
	}

	return l, nil
}

func (s *stubListingStore) List(_ context.Context, _ store.ListingFilter) ([]*domain.Listing, error) {
	return nil, nil
}

func (s *stubListingStore) Search(_ context.Context, filter store.SearchFilter) ([]*domain.Listing, error) {
	f := filter
	s.lastSearch = &f

	return s.searchResult, nil
}

func (s *stubListingStore) Update(_ context.Context, l *domain.Listing) error {
	s.listings[l.ID] = l
	s.updated = append(s.updated, l)

	return nil
}

type stubBidStore struct {
	bids    map[uuid.UUID]*domain.Bid
	updated []*domain.Bid
	created []*domain.Bid
}

func newStubBidStore(bids ...*domain.Bid) *stubBidStore {
	m := &stubBidStore{bids: make(map[uuid.UUID]*domain.Bid)}

	for _, b := range bids {
		m.bids[b.ID] = b
	}

	return m
}

func (s *stubBidStore) Create(_ context.Context, b *domain.Bid) error {
	if _, exists := s.bids[b.ID]; exists {
		return domain.ErrBidAlreadyExists
	}

	s.bids[b.ID] = b
	s.created = append(s.created, b)

	return nil
}

func (s *stubBidStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Bid, error) {
	b, ok := s.bids[id]
	if !ok {
		return nil, domain.ErrBidNotFound
	}

	return b, nil
}

// GetByIDForUpdate simulates SELECT ... FOR UPDATE; in-memory stubs need no locking.
func (s *stubBidStore) GetByIDForUpdate(_ context.Context, id uuid.UUID) (*domain.Bid, error) {
	b, ok := s.bids[id]
	if !ok {
		return nil, domain.ErrBidNotFound
	}

	return b, nil
}

func (s *stubBidStore) ListByListing(_ context.Context, listingID uuid.UUID) ([]*domain.Bid, error) {
	var result []*domain.Bid

	for _, b := range s.bids {
		if b.ListingID == listingID {
			result = append(result, b)
		}
	}

	return result, nil
}

func (s *stubBidStore) ListByBidder(_ context.Context, bidderID uuid.UUID) ([]*domain.Bid, error) {
	var result []*domain.Bid

	for _, b := range s.bids {
		if b.BidderUserID == bidderID {
			result = append(result, b)
		}
	}

	return result, nil
}

func (s *stubBidStore) Update(_ context.Context, b *domain.Bid) error {
	s.bids[b.ID] = b
	s.updated = append(s.updated, b)

	return nil
}

func (s *stubBidStore) RejectSiblingBids(_ context.Context, listingID, acceptedBidID uuid.UUID) error {
	for _, b := range s.bids {
		if b.ListingID == listingID && b.ID != acceptedBidID && b.Status == domain.BidStatusPending {
			b.Status = domain.BidStatusRejected
		}
	}

	return nil
}

type stubAwardStore struct {
	awards  map[uuid.UUID]*domain.Award
	created []*domain.Award
}

func newStubAwardStore() *stubAwardStore {
	return &stubAwardStore{awards: make(map[uuid.UUID]*domain.Award)}
}

func (s *stubAwardStore) Create(_ context.Context, a *domain.Award) error {
	s.awards[a.ID] = a
	s.created = append(s.created, a)

	return nil
}

func (s *stubAwardStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Award, error) {
	a, ok := s.awards[id]
	if !ok {
		return nil, domain.ErrAwardNotFound
	}

	return a, nil
}

func (s *stubAwardStore) MarkEventPublished(_ context.Context, awardID uuid.UUID) error {
	a, ok := s.awards[awardID]
	if !ok {
		return domain.ErrAwardNotFound
	}

	now := time.Now().UTC()
	a.EventPublishedAt = &now

	return nil
}

// stubTxManager calls fn with the same stores — simulates a transaction.
type stubTxManager struct {
	listings store.ListingStore
	bids     store.BidStore
	awards   store.AwardStore
}

func (m *stubTxManager) WithTx(ctx context.Context, fn func(context.Context, store.ListingStore, store.BidStore, store.AwardStore) error) error {
	return fn(ctx, m.listings, m.bids, m.awards)
}

// stubBidOutboxTxManager wraps BidOutboxTxManager with the same in-memory stores,
// using a noopOutboxStore so unit tests do not need a real DB.
type stubBidOutboxTxManager struct {
	listings store.ListingStore
	bids     store.BidStore
	awards   store.AwardStore
}

func (m *stubBidOutboxTxManager) WithBidOutboxTx(
	ctx context.Context,
	fn func(context.Context, store.ListingStore, store.BidStore, store.AwardStore, store.OutboxStore) error,
) error {
	return fn(ctx, m.listings, m.bids, m.awards, &noopOutboxStore{})
}

// noopOutboxStore satisfies store.OutboxStore for unit tests that do not exercise
// the outbox path (all writes are accepted silently).
type noopOutboxStore struct{}

func (*noopOutboxStore) Enqueue(_ context.Context, _ *domain.OutboxEvent) error { return nil }
func (*noopOutboxStore) PollReady(_ context.Context, _ int) ([]*domain.OutboxEvent, error) {
	return nil, nil
}
func (*noopOutboxStore) MarkPublished(_ context.Context, _ uuid.UUID) error { return nil }
func (*noopOutboxStore) MarkFailed(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

func (*noopOutboxStore) DeletePublishedBefore(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// --- tests ---

func TestBidService_CreateBid(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	bidderID := uuid.New()
	listingID := uuid.New()

	openListing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusOpen,
		Currency:    "TWD",
		Title:       "Test Listing",
		Description: "",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	tests := []struct {
		name      string
		listing   *domain.Listing
		input     service.CreateBidInput
		wantErr   error
		wantNoErr bool
	}{
		{
			name:    "happy path: valid bid on open listing",
			listing: openListing,
			input: service.CreateBidInput{
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(1000),
				Currency:     "TWD",
				Message:      "I can do this",
			},
			wantNoErr: true,
		},
		{
			name: "error: listing not found",
			listing: &domain.Listing{
				ID:          uuid.New(), // different listing
				OwnerUserID: ownerID,
				Status:      domain.ListingStatusOpen,
				Currency:    "TWD",
				Title:       "Other",
				CreatedAt:   time.Now().UTC(),
				UpdatedAt:   time.Now().UTC(),
			},
			input: service.CreateBidInput{
				ListingID:    listingID, // not in store
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(1000),
				Currency:     "TWD",
			},
			wantErr: domain.ErrListingNotFound,
		},
		{
			name: "error: listing not open (AWARDED)",
			listing: &domain.Listing{
				ID:          listingID,
				OwnerUserID: ownerID,
				Status:      domain.ListingStatusAwarded,
				Currency:    "TWD",
				Title:       "Awarded",
				CreatedAt:   time.Now().UTC(),
				UpdatedAt:   time.Now().UTC(),
			},
			input: service.CreateBidInput{
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(1000),
				Currency:     "TWD",
			},
			wantErr: domain.ErrListingNotOpen,
		},
		{
			name:    "error: bidder is listing owner",
			listing: openListing,
			input: service.CreateBidInput{
				ListingID:    listingID,
				BidderUserID: ownerID, // same as listing owner
				Amount:       decimal.NewFromInt(1000),
				Currency:     "TWD",
			},
			wantErr: domain.ErrBidOnOwnListing,
		},
		{
			name:    "error: amount is zero",
			listing: openListing,
			input: service.CreateBidInput{
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.Zero,
				Currency:     "TWD",
			},
			wantErr: domain.ErrValidation,
		},
		{
			name:    "error: message contains null byte",
			listing: openListing,
			input: service.CreateBidInput{
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(100),
				Currency:     "TWD",
				Message:      "bad\x00message",
			},
			wantErr: domain.ErrValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listingStore := newStubListingStore(tc.listing)
			bidStore := newStubBidStore()
			awardStore := newStubAwardStore()
			txMgr := &stubTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
			publisher := events.NewNoopPublisher()

			bidOutboxTxMgr := &stubBidOutboxTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
			svc := service.NewBidService(bidStore, listingStore, awardStore, txMgr, bidOutboxTxMgr, publisher, nil)

			bid, err := svc.CreateBid(context.Background(), &tc.input)

			if tc.wantNoErr {
				require.NoError(t, err)
				require.NotNil(t, bid)
				assert.Equal(t, tc.input.ListingID, bid.ListingID)
				assert.Equal(t, tc.input.BidderUserID, bid.BidderUserID)
				assert.Equal(t, domain.BidStatusPending, bid.Status)
			} else {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErr), "got %v, want %v", err, tc.wantErr)
				assert.Nil(t, bid)
			}
		})
	}
}

func TestBidService_AcceptBid(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	bidderID := uuid.New()
	otherBidderID := uuid.New()
	listingID := uuid.New()
	bidID := uuid.New()
	siblingBidID := uuid.New()

	makeListing := func(status domain.ListingStatus) *domain.Listing {
		return &domain.Listing{
			ID:          listingID,
			OwnerUserID: ownerID,
			Status:      status,
			Currency:    "TWD",
			Title:       "Test",
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}
	}

	makeBid := func(status domain.BidStatus) *domain.Bid {
		return &domain.Bid{
			ID:           bidID,
			ListingID:    listingID,
			BidderUserID: bidderID,
			Amount:       decimal.NewFromInt(500),
			Currency:     "TWD",
			Status:       status,
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
	}

	siblingBid := &domain.Bid{
		ID:           siblingBidID,
		ListingID:    listingID,
		BidderUserID: otherBidderID,
		Amount:       decimal.NewFromInt(300),
		Currency:     "TWD",
		Status:       domain.BidStatusPending,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	tests := []struct {
		name      string
		listing   *domain.Listing
		bid       *domain.Bid
		callerID  uuid.UUID
		wantErr   error
		wantNoErr bool
	}{
		{
			name:      "happy path: accept pending bid, listing AWARDED, sibling REJECTED",
			listing:   makeListing(domain.ListingStatusOpen),
			bid:       makeBid(domain.BidStatusPending),
			callerID:  ownerID,
			wantNoErr: true,
		},
		{
			name:     "error: bid not found",
			listing:  makeListing(domain.ListingStatusOpen),
			bid:      makeBid(domain.BidStatusPending),
			callerID: ownerID,
			// set up separately: missing bid
			wantErr: domain.ErrBidNotFound,
		},
		{
			name:     "error: bid is not PENDING (already ACCEPTED)",
			listing:  makeListing(domain.ListingStatusOpen),
			bid:      makeBid(domain.BidStatusAccepted),
			callerID: ownerID,
			wantErr:  domain.ErrBidNotPending,
		},
		{
			name:     "error: bid is not PENDING (WITHDRAWN)",
			listing:  makeListing(domain.ListingStatusOpen),
			bid:      makeBid(domain.BidStatusWithdrawn),
			callerID: ownerID,
			wantErr:  domain.ErrBidNotPending,
		},
		{
			name:     "error: caller is not listing owner (IDOR guard)",
			listing:  makeListing(domain.ListingStatusOpen),
			bid:      makeBid(domain.BidStatusPending),
			callerID: uuid.New(), // stranger
			wantErr:  domain.ErrListingNotFound,
		},
		{
			name:     "error: listing already AWARDED (second accept on same listing)",
			listing:  makeListing(domain.ListingStatusAwarded),
			bid:      makeBid(domain.BidStatusPending),
			callerID: ownerID,
			wantErr:  domain.ErrListingNotOpen,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listingStore := newStubListingStore(tc.listing)
			bidStore := newStubBidStore(siblingBid)
			awardStore := newStubAwardStore()

			var targetBidID uuid.UUID

			// For the "bid not found" case, don't add the bid to the store.
			if tc.name == "error: bid not found" {
				targetBidID = uuid.New() // non-existent
			} else {
				bidStore.bids[tc.bid.ID] = tc.bid
				targetBidID = tc.bid.ID
			}

			txMgr := &stubTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
			publisher := events.NewNoopPublisher()

			bidOutboxTxMgr := &stubBidOutboxTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
			svc := service.NewBidService(bidStore, listingStore, awardStore, txMgr, bidOutboxTxMgr, publisher, nil)

			award, err := svc.AcceptBid(context.Background(), targetBidID, tc.callerID)

			if tc.wantNoErr {
				require.NoError(t, err)
				require.NotNil(t, award)
				assert.Equal(t, listingID, award.ListingID)
				assert.Equal(t, bidID, award.BidID)
				assert.Equal(t, ownerID, award.OwnerUserID)
				assert.Equal(t, bidderID, award.BidderUserID)

				// Verify listing flipped to AWARDED.
				updatedListing, _ := listingStore.GetByID(context.Background(), listingID)
				assert.Equal(t, domain.ListingStatusAwarded, updatedListing.Status)

				// Verify sibling bid rejected.
				sibling, _ := bidStore.GetByID(context.Background(), siblingBidID)
				assert.Equal(t, domain.BidStatusRejected, sibling.Status)
			} else {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErr), "got %v, want %v", err, tc.wantErr)
				assert.Nil(t, award)
			}
		})
	}
}

func TestBidService_WithdrawBid(t *testing.T) { //nolint:dupl // similar structure to RejectBid but different IDOR actors
	t.Parallel()

	ownerID := uuid.New()
	bidderID := uuid.New()
	listingID := uuid.New()
	bidID := uuid.New()

	openListing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusOpen,
		Currency:    "TWD",
		Title:       "Test",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	tests := []struct {
		name      string
		bid       *domain.Bid
		callerID  uuid.UUID
		wantErr   error
		wantNoErr bool
	}{
		{
			name: "happy path: bidder withdraws own pending bid",
			bid: &domain.Bid{
				ID:           bidID,
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(100),
				Currency:     "TWD",
				Status:       domain.BidStatusPending,
				CreatedAt:    time.Now().UTC(),
				UpdatedAt:    time.Now().UTC(),
			},
			callerID:  bidderID,
			wantNoErr: true,
		},
		{
			name: "error: IDOR — non-bidder tries to withdraw",
			bid: &domain.Bid{
				ID:           bidID,
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(100),
				Currency:     "TWD",
				Status:       domain.BidStatusPending,
				CreatedAt:    time.Now().UTC(),
				UpdatedAt:    time.Now().UTC(),
			},
			callerID: ownerID, // listing owner, not bidder
			wantErr:  domain.ErrBidNotFound,
		},
		{
			name: "error: bid already accepted (terminal state)",
			bid: &domain.Bid{
				ID:           bidID,
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(100),
				Currency:     "TWD",
				Status:       domain.BidStatusAccepted,
				CreatedAt:    time.Now().UTC(),
				UpdatedAt:    time.Now().UTC(),
			},
			callerID: bidderID,
			wantErr:  domain.ErrBidNotPending,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listingStore := newStubListingStore(openListing)
			bidStore := newStubBidStore(tc.bid)
			awardStore := newStubAwardStore()
			txMgr := &stubTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
			publisher := events.NewNoopPublisher()

			bidOutboxTxMgr := &stubBidOutboxTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
			svc := service.NewBidService(bidStore, listingStore, awardStore, txMgr, bidOutboxTxMgr, publisher, nil)

			bid, err := svc.WithdrawBid(context.Background(), bidID, tc.callerID)

			if tc.wantNoErr {
				require.NoError(t, err)
				require.NotNil(t, bid)
				assert.Equal(t, domain.BidStatusWithdrawn, bid.Status)
				assert.NotNil(t, bid.DecidedAt)
			} else {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErr), "got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestBidService_RejectBid(t *testing.T) { //nolint:dupl // similar structure to WithdrawBid but different IDOR actors
	t.Parallel()

	ownerID := uuid.New()
	bidderID := uuid.New()
	listingID := uuid.New()
	bidID := uuid.New()

	openListing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusOpen,
		Currency:    "TWD",
		Title:       "Test",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	tests := []struct {
		name      string
		bid       *domain.Bid
		callerID  uuid.UUID
		wantErr   error
		wantNoErr bool
	}{
		{
			name: "happy path: owner rejects pending bid",
			bid: &domain.Bid{
				ID:           bidID,
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(100),
				Currency:     "TWD",
				Status:       domain.BidStatusPending,
				CreatedAt:    time.Now().UTC(),
				UpdatedAt:    time.Now().UTC(),
			},
			callerID:  ownerID,
			wantNoErr: true,
		},
		{
			name: "error: non-owner tries to reject (IDOR)",
			bid: &domain.Bid{
				ID:           bidID,
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(100),
				Currency:     "TWD",
				Status:       domain.BidStatusPending,
				CreatedAt:    time.Now().UTC(),
				UpdatedAt:    time.Now().UTC(),
			},
			callerID: bidderID, // bidder, not owner
			wantErr:  domain.ErrBidNotFound,
		},
		{
			name: "error: already rejected (terminal state)",
			bid: &domain.Bid{
				ID:           bidID,
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(100),
				Currency:     "TWD",
				Status:       domain.BidStatusRejected,
				CreatedAt:    time.Now().UTC(),
				UpdatedAt:    time.Now().UTC(),
			},
			callerID: ownerID,
			wantErr:  domain.ErrBidNotPending,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listingStore := newStubListingStore(openListing)
			bidStore := newStubBidStore(tc.bid)
			awardStore := newStubAwardStore()
			txMgr := &stubTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
			publisher := events.NewNoopPublisher()

			bidOutboxTxMgr := &stubBidOutboxTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
			svc := service.NewBidService(bidStore, listingStore, awardStore, txMgr, bidOutboxTxMgr, publisher, nil)

			bid, err := svc.RejectBid(context.Background(), bidID, tc.callerID)

			if tc.wantNoErr {
				require.NoError(t, err)
				require.NotNil(t, bid)
				assert.Equal(t, domain.BidStatusRejected, bid.Status)
				assert.NotNil(t, bid.DecidedAt)
			} else {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErr), "got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestBidService_CreateBid_UpperBound verifies that bid amounts exceeding the
// numeric(14,2) column maximum are rejected at the service layer (MK-M2).
func TestBidService_CreateBid_UpperBound(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	bidderID := uuid.New()
	listingID := uuid.New()

	openListing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusOpen,
		Currency:    "TWD",
		Title:       "Upper Bound Test",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	// 999999999999.99 is the max; anything over must be rejected.
	justOver, _ := decimal.NewFromString("1000000000000.00") // 1e12 — one cent over column max
	maxAllowed, _ := decimal.NewFromString("999999999999.99")

	tests := []struct {
		name    string
		amount  decimal.Decimal
		wantErr error
	}{
		{
			name:   "happy path: amount at column max is accepted",
			amount: maxAllowed,
		},
		{
			name:    "error: amount one cent over column max",
			amount:  justOver,
			wantErr: domain.ErrValidation,
		},
		{
			name:    "error: amount astronomically large",
			amount:  decimal.NewFromFloat(1e20),
			wantErr: domain.ErrValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listingStore := newStubListingStore(openListing)
			bidStore := newStubBidStore()
			awardStore := newStubAwardStore()
			txMgr := &stubTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
			publisher := events.NewNoopPublisher()

			bidOutboxTxMgr := &stubBidOutboxTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
			svc := service.NewBidService(bidStore, listingStore, awardStore, txMgr, bidOutboxTxMgr, publisher, nil)

			_, err := svc.CreateBid(context.Background(), &service.CreateBidInput{
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       tc.amount,
				Currency:     "TWD",
			})

			if tc.wantErr != nil {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErr), "got %v, want %v", err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestBidService_AcceptBid_DoubleAccept verifies that a second AcceptBid call
// on a listing that has already been AWARDED fails with ErrListingNotOpen (MK-M1).
func TestBidService_AcceptBid_DoubleAccept(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	bidderID := uuid.New()
	listingID := uuid.New()
	bidID := uuid.New()

	// Initial state: listing OPEN, bid PENDING.
	listing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusOpen,
		Currency:    "TWD",
		Title:       "Double Accept Test",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	bid := &domain.Bid{
		ID:           bidID,
		ListingID:    listingID,
		BidderUserID: bidderID,
		Amount:       decimal.NewFromInt(100),
		Currency:     "TWD",
		Status:       domain.BidStatusPending,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	listingStore := newStubListingStore(listing)
	bidStore := newStubBidStore(bid)
	awardStore := newStubAwardStore()
	txMgr := &stubTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
	bidOutboxTxMgr := &stubBidOutboxTxManager{listings: listingStore, bids: bidStore, awards: awardStore}
	publisher := events.NewNoopPublisher()

	svc := service.NewBidService(bidStore, listingStore, awardStore, txMgr, bidOutboxTxMgr, publisher, nil)

	// First accept: must succeed.
	award, err := svc.AcceptBid(context.Background(), bidID, ownerID)
	require.NoError(t, err)
	require.NotNil(t, award)

	// After first accept the listing is AWARDED and the bid is ACCEPTED in the stub store.
	// Second accept: must fail because the listing is now AWARDED (not OPEN).
	// The stub GetByIDForUpdate returns the in-memory state which is already AWARDED.
	_, err = svc.AcceptBid(context.Background(), bidID, ownerID)
	require.Error(t, err)

	// The error must be ErrListingNotOpen or ErrBidNotPending (either proves the race guard).
	isConflict := errors.Is(err, domain.ErrListingNotOpen) || errors.Is(err, domain.ErrBidNotPending)
	assert.True(t, isConflict, "expected ErrListingNotOpen or ErrBidNotPending, got: %v", err)
}
