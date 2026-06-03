package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/events"
	"github.com/CoverOnes/marketplace/internal/handler"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- stub stores for bid handler tests ---

type stubBidStoreH struct {
	bids map[uuid.UUID]*domain.Bid
}

func newStubBidStoreH(bids ...*domain.Bid) *stubBidStoreH {
	m := &stubBidStoreH{bids: make(map[uuid.UUID]*domain.Bid)}

	for _, b := range bids {
		m.bids[b.ID] = b
	}

	return m
}

func (s *stubBidStoreH) Create(_ context.Context, b *domain.Bid) error {
	s.bids[b.ID] = b
	return nil
}

func (s *stubBidStoreH) GetByID(_ context.Context, id uuid.UUID) (*domain.Bid, error) {
	b, ok := s.bids[id]
	if !ok {
		return nil, domain.ErrBidNotFound
	}

	return b, nil
}

// GetByIDForUpdate simulates SELECT ... FOR UPDATE; in-memory stubs need no locking.
func (s *stubBidStoreH) GetByIDForUpdate(_ context.Context, id uuid.UUID) (*domain.Bid, error) {
	b, ok := s.bids[id]
	if !ok {
		return nil, domain.ErrBidNotFound
	}

	return b, nil
}

func (s *stubBidStoreH) ListByListing(_ context.Context, listingID uuid.UUID) ([]*domain.Bid, error) {
	var result []*domain.Bid

	for _, b := range s.bids {
		if b.ListingID == listingID {
			result = append(result, b)
		}
	}

	return result, nil
}

func (s *stubBidStoreH) ListByBidder(_ context.Context, bidderID uuid.UUID) ([]*domain.Bid, error) {
	var result []*domain.Bid

	for _, b := range s.bids {
		if b.BidderUserID == bidderID {
			result = append(result, b)
		}
	}

	return result, nil
}

func (s *stubBidStoreH) Update(_ context.Context, b *domain.Bid) error {
	s.bids[b.ID] = b
	return nil
}

func (s *stubBidStoreH) RejectSiblingBids(_ context.Context, listingID, acceptedBidID uuid.UUID) error {
	for _, b := range s.bids {
		if b.ListingID == listingID && b.ID != acceptedBidID && b.Status == domain.BidStatusPending {
			b.Status = domain.BidStatusRejected
		}
	}

	return nil
}

type stubAwardStoreH struct {
	awards map[uuid.UUID]*domain.Award
}

func newStubAwardStoreH() *stubAwardStoreH {
	return &stubAwardStoreH{awards: make(map[uuid.UUID]*domain.Award)}
}

func (s *stubAwardStoreH) Create(_ context.Context, a *domain.Award) error {
	s.awards[a.ID] = a
	return nil
}

func (s *stubAwardStoreH) GetByID(_ context.Context, id uuid.UUID) (*domain.Award, error) {
	a, ok := s.awards[id]
	if !ok {
		return nil, domain.ErrAwardNotFound
	}

	return a, nil
}

func (s *stubAwardStoreH) MarkEventPublished(_ context.Context, awardID uuid.UUID) error {
	a, ok := s.awards[awardID]
	if !ok {
		return domain.ErrAwardNotFound
	}

	now := time.Now().UTC()
	a.EventPublishedAt = &now

	return nil
}

type stubTxManagerH struct {
	listings store.ListingStore
	bids     store.BidStore
	awards   store.AwardStore
}

func (m *stubTxManagerH) WithTx(ctx context.Context, fn func(context.Context, store.ListingStore, store.BidStore, store.AwardStore) error) error {
	return fn(ctx, m.listings, m.bids, m.awards)
}

// buildBidRouter builds a test router with BidHandler wired.
func buildBidRouter(listingStore store.ListingStore, bidStore store.BidStore, awardStore store.AwardStore) *gin.Engine {
	gin.SetMode(gin.TestMode)

	txMgr := &stubTxManagerH{listings: listingStore, bids: bidStore, awards: awardStore}
	publisher := events.NewNoopPublisher()

	bidSvc := service.NewBidService(bidStore, listingStore, awardStore, txMgr, publisher, nil)
	bidH := handler.NewBidHandler(bidSvc)

	r := gin.New()

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())
	api.POST("/listings/:id/bids", middleware.RequireTier(2), bidH.CreateBid)
	api.GET("/listings/:id/bids", middleware.RequireTier(2), bidH.ListBidsForListing)
	api.GET("/bids", middleware.RequireTier(2), bidH.ListMyBids)
	api.POST("/bids/:id/accept", middleware.RequireTier(2), bidH.AcceptBid)
	api.POST("/bids/:id/reject", middleware.RequireTier(2), bidH.RejectBid)
	api.POST("/bids/:id/withdraw", middleware.RequireTier(2), bidH.WithdrawBid)

	return r
}

func TestBidHandler_CreateBid(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	bidderID := uuid.New()
	listingID := uuid.New()

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
		name         string
		callerID     string
		tierHeader   string
		listingIDStr string
		body         map[string]any
		wantStatus   int
		wantCode     string
	}{
		{
			name:         "happy path: create bid",
			callerID:     bidderID.String(),
			tierHeader:   "2",
			listingIDStr: listingID.String(),
			body: map[string]any{
				"amount":   "1000.00",
				"currency": "TWD",
				"message":  "I am experienced",
			},
			wantStatus: http.StatusCreated,
		},
		{
			name:         "error: missing identity -> 401",
			callerID:     "",
			tierHeader:   "2",
			listingIDStr: listingID.String(),
			body: map[string]any{
				"amount": "1000.00",
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "UNAUTHORIZED",
		},
		{
			name:         "error: tier 1 -> 403",
			callerID:     bidderID.String(),
			tierHeader:   "1",
			listingIDStr: listingID.String(),
			body: map[string]any{
				"amount": "1000.00",
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "KYC_TIER_REQUIRED",
		},
		{
			name:         "error: owner bids own listing -> 422",
			callerID:     ownerID.String(),
			tierHeader:   "2",
			listingIDStr: listingID.String(),
			body: map[string]any{
				"amount":   "1000.00",
				"currency": "TWD",
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "BID_ON_OWN_LISTING",
		},
		{
			name:         "error: invalid listing id",
			callerID:     bidderID.String(),
			tierHeader:   "2",
			listingIDStr: "not-a-uuid",
			body:         map[string]any{"amount": "1000.00"},
			wantStatus:   http.StatusBadRequest,
			wantCode:     "VALIDATION_ERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listingStore := newStubListingStoreH(openListing)
			bidStore := newStubBidStoreH()
			awardStore := newStubAwardStoreH()
			r := buildBidRouter(listingStore, bidStore, awardStore)

			body, _ := json.Marshal(tc.body)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/listings/"+tc.listingIDStr+"/bids", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			if tc.callerID != "" {
				req.Header.Set("X-User-Id", tc.callerID)
			}

			if tc.tierHeader != "" {
				req.Header.Set("X-Kyc-Tier", tc.tierHeader)
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errBody, ok := resp["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, errBody["code"])
			}
		})
	}
}

func TestBidHandler_AcceptBid_IDOR(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	bidderID := uuid.New()
	strangerID := uuid.New()
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

	pendingBid := &domain.Bid{
		ID:           bidID,
		ListingID:    listingID,
		BidderUserID: bidderID,
		Amount:       decimal.NewFromInt(500),
		Currency:     "TWD",
		Status:       domain.BidStatusPending,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	tests := []struct {
		name       string
		callerID   uuid.UUID
		wantStatus int
		wantCode   string
	}{
		{
			name:       "happy path: owner accepts bid",
			callerID:   ownerID,
			wantStatus: http.StatusOK,
		},
		{
			name:       "error: stranger tries to accept (IDOR) -> 404",
			callerID:   strangerID,
			wantStatus: http.StatusNotFound,
			wantCode:   "LISTING_NOT_FOUND",
		},
		{
			name:       "error: bidder tries to accept own bid (IDOR) -> 404",
			callerID:   bidderID,
			wantStatus: http.StatusNotFound,
			wantCode:   "LISTING_NOT_FOUND",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create fresh copies per subtest so parallel tests don't share mutable state.
			freshListing := &domain.Listing{
				ID: openListing.ID, OwnerUserID: openListing.OwnerUserID,
				Status: openListing.Status, Currency: openListing.Currency,
				Title: openListing.Title, CreatedAt: openListing.CreatedAt, UpdatedAt: openListing.UpdatedAt,
			}
			freshBid := &domain.Bid{
				ID: pendingBid.ID, ListingID: pendingBid.ListingID,
				BidderUserID: pendingBid.BidderUserID, Amount: pendingBid.Amount,
				Currency: pendingBid.Currency, Status: pendingBid.Status,
				CreatedAt: pendingBid.CreatedAt, UpdatedAt: pendingBid.UpdatedAt,
			}

			listingStore := newStubListingStoreH(freshListing)
			bidStore := newStubBidStoreH(freshBid)
			awardStore := newStubAwardStoreH()
			r := buildBidRouter(listingStore, bidStore, awardStore)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/bids/"+bidID.String()+"/accept", nil)
			req.Header.Set("X-User-Id", tc.callerID.String())
			req.Header.Set("X-Kyc-Tier", "2")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errBody, ok := resp["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, errBody["code"])
			}
		})
	}
}

func TestBidHandler_WithdrawBid(t *testing.T) {
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
		name       string
		bidStatus  domain.BidStatus
		callerID   uuid.UUID
		wantStatus int
		wantCode   string
	}{
		{
			name:       "happy path: bidder withdraws",
			bidStatus:  domain.BidStatusPending,
			callerID:   bidderID,
			wantStatus: http.StatusOK,
		},
		{
			name:       "error: owner tries to withdraw (not bidder) -> 404",
			bidStatus:  domain.BidStatusPending,
			callerID:   ownerID,
			wantStatus: http.StatusNotFound,
			wantCode:   "BID_NOT_FOUND",
		},
		{
			name:       "error: already accepted bid -> 409",
			bidStatus:  domain.BidStatusAccepted,
			callerID:   bidderID,
			wantStatus: http.StatusConflict,
			wantCode:   "BID_NOT_PENDING",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bid := &domain.Bid{
				ID:           bidID,
				ListingID:    listingID,
				BidderUserID: bidderID,
				Amount:       decimal.NewFromInt(100),
				Currency:     "TWD",
				Status:       tc.bidStatus,
				CreatedAt:    time.Now().UTC(),
				UpdatedAt:    time.Now().UTC(),
			}

			listingStore := newStubListingStoreH(openListing)
			bidStore := newStubBidStoreH(bid)
			awardStore := newStubAwardStoreH()
			r := buildBidRouter(listingStore, bidStore, awardStore)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/bids/"+bidID.String()+"/withdraw", nil)
			req.Header.Set("X-User-Id", tc.callerID.String())
			req.Header.Set("X-Kyc-Tier", "2")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errBody, ok := resp["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, errBody["code"])
			}
		})
	}
}
