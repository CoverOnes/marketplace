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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- minimal stub stores for TenderHandler tests ---

// stubTenderListingStoreH satisfies store.ListingStore for handler-layer unit tests.
type stubTenderListingStoreH struct {
	listings map[uuid.UUID]*domain.Listing
}

func newStubTenderListingStoreH(listings ...*domain.Listing) *stubTenderListingStoreH {
	m := &stubTenderListingStoreH{listings: make(map[uuid.UUID]*domain.Listing)}

	for _, l := range listings {
		m.listings[l.ID] = l
	}

	return m
}

func (s *stubTenderListingStoreH) Create(_ context.Context, l *domain.Listing) error {
	s.listings[l.ID] = l
	return nil
}

func (s *stubTenderListingStoreH) GetByID(_ context.Context, id uuid.UUID) (*domain.Listing, error) {
	l, ok := s.listings[id]
	if !ok {
		return nil, domain.ErrListingNotFound
	}

	return l, nil
}

func (s *stubTenderListingStoreH) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Listing, error) {
	return s.GetByID(ctx, id)
}

func (s *stubTenderListingStoreH) List(_ context.Context, _ store.ListingFilter) ([]*domain.Listing, error) {
	return nil, nil
}

func (s *stubTenderListingStoreH) Search(_ context.Context, _ store.SearchFilter) ([]*domain.Listing, error) {
	return nil, nil
}

func (s *stubTenderListingStoreH) GetByIDs(_ context.Context, ids []uuid.UUID, _ store.HydrationFilter) ([]*domain.Listing, error) {
	out := make([]*domain.Listing, 0, len(ids))

	for _, id := range ids {
		if l, ok := s.listings[id]; ok {
			out = append(out, l)
		}
	}

	return out, nil
}

func (s *stubTenderListingStoreH) Update(_ context.Context, l *domain.Listing) error {
	s.listings[l.ID] = l
	return nil
}

// stubMilestoneStoreH satisfies store.TenderMilestoneStore for handler-layer unit tests.
type stubMilestoneStoreH struct {
	milestones map[uuid.UUID]*domain.TenderMilestone
}

func newStubMilestoneStoreH(milestones ...*domain.TenderMilestone) *stubMilestoneStoreH {
	m := &stubMilestoneStoreH{milestones: make(map[uuid.UUID]*domain.TenderMilestone)}

	for _, ms := range milestones {
		m.milestones[ms.ID] = ms
	}

	return m
}

func (s *stubMilestoneStoreH) Create(_ context.Context, m *domain.TenderMilestone) error {
	s.milestones[m.ID] = m
	return nil
}

func (s *stubMilestoneStoreH) GetByID(_ context.Context, id uuid.UUID) (*domain.TenderMilestone, error) {
	m, ok := s.milestones[id]
	if !ok {
		return nil, domain.ErrTenderMilestoneNotFound
	}

	return m, nil
}

func (s *stubMilestoneStoreH) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderMilestone, error) {
	return s.GetByID(ctx, id)
}

func (s *stubMilestoneStoreH) ListByListing(_ context.Context, listingID uuid.UUID) ([]*domain.TenderMilestone, error) {
	var result []*domain.TenderMilestone

	for _, m := range s.milestones {
		if m.ListingID == listingID {
			result = append(result, m)
		}
	}

	return result, nil
}

func (s *stubMilestoneStoreH) Update(_ context.Context, m *domain.TenderMilestone) error {
	s.milestones[m.ID] = m
	return nil
}

// stubMilestoneTxManagerH is a no-op MilestoneTxManager for handler tests.
// It calls fn synchronously with the provided stores.
type stubMilestoneTxManagerH struct {
	listings   store.ListingStore
	milestones store.TenderMilestoneStore
}

func (m *stubMilestoneTxManagerH) WithMilestoneTx(
	ctx context.Context,
	fn func(context.Context, store.ListingStore, store.TenderMilestoneStore) error,
) error {
	return fn(ctx, m.listings, m.milestones)
}

// stubTenderTxManagerH is a no-op TenderTxManager for handler tests.
type stubTenderTxManagerH struct {
	listings      store.ListingStore
	roles         store.TenderRoleStore
	collaborators store.TenderCollaboratorStore
}

func (m *stubTenderTxManagerH) WithTenderTx(
	ctx context.Context,
	fn func(context.Context, store.ListingStore, store.TenderRoleStore, store.TenderCollaboratorStore) error,
) error {
	return fn(ctx, m.listings, m.roles, m.collaborators)
}

// stubTenderOutboxTxManagerH is a no-op OutboxTxManager for handler tests.
type stubTenderOutboxTxManagerH struct {
	listings      store.ListingStore
	roles         store.TenderRoleStore
	collaborators store.TenderCollaboratorStore
}

func (m *stubTenderOutboxTxManagerH) WithOutboxTx(
	ctx context.Context,
	fn func(context.Context, store.ListingStore, store.TenderRoleStore, store.TenderCollaboratorStore, store.OutboxStore) error,
) error {
	return fn(ctx, m.listings, m.roles, m.collaborators, &noopTenderOutboxStoreH{})
}

// noopTenderOutboxStoreH discards all outbox operations in handler tender tests.
type noopTenderOutboxStoreH struct{}

func (*noopTenderOutboxStoreH) Enqueue(_ context.Context, _ *domain.OutboxEvent) error { return nil }

func (*noopTenderOutboxStoreH) PollReady(_ context.Context, _ int) ([]*domain.OutboxEvent, error) {
	return nil, nil
}
func (*noopTenderOutboxStoreH) MarkPublished(_ context.Context, _ uuid.UUID) error { return nil }
func (*noopTenderOutboxStoreH) MarkFailed(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

func (*noopTenderOutboxStoreH) DeletePublishedBefore(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// noopRoleStoreH satisfies store.TenderRoleStore with no-op implementations.
type noopRoleStoreH struct{}

func (*noopRoleStoreH) Create(_ context.Context, _ *domain.TenderRole) error { return nil }
func (*noopRoleStoreH) GetByID(_ context.Context, _ uuid.UUID) (*domain.TenderRole, error) {
	return nil, domain.ErrTenderRoleNotFound
}

func (*noopRoleStoreH) GetByIDForUpdate(_ context.Context, _ uuid.UUID) (*domain.TenderRole, error) {
	return nil, domain.ErrTenderRoleNotFound
}

func (*noopRoleStoreH) ListByListing(_ context.Context, _ uuid.UUID) ([]*domain.TenderRole, error) {
	return nil, nil
}
func (*noopRoleStoreH) Update(_ context.Context, _ *domain.TenderRole) error { return nil }

// noopCollabStoreH satisfies store.TenderCollaboratorStore with no-op implementations.
type noopCollabStoreH struct{}

func (*noopCollabStoreH) Create(_ context.Context, _ *domain.TenderCollaborator) error { return nil }

func (*noopCollabStoreH) GetByID(_ context.Context, _ uuid.UUID) (*domain.TenderCollaborator, error) {
	return nil, domain.ErrTenderCollaboratorNotFound
}

func (*noopCollabStoreH) GetByIDForUpdate(_ context.Context, _ uuid.UUID) (*domain.TenderCollaborator, error) {
	return nil, domain.ErrTenderCollaboratorNotFound
}

func (*noopCollabStoreH) CountApprovedByRole(_ context.Context, _ uuid.UUID) (int, error) {
	return 0, nil
}

func (*noopCollabStoreH) ListByListing(_ context.Context, _ uuid.UUID) ([]*domain.TenderCollaborator, error) {
	return nil, nil
}

func (*noopCollabStoreH) ListByRole(_ context.Context, _ uuid.UUID) ([]*domain.TenderCollaborator, error) {
	return nil, nil
}

func (*noopCollabStoreH) Update(_ context.Context, _ *domain.TenderCollaborator) error { return nil }

// buildUpdateMilestoneRouter sets up a minimal Gin router wired to a TenderHandler.
// The listing and milestone stores are pre-loaded with the given objects.
// callers MUST set the X-User-Id and X-Kyc-Tier request headers — the router uses
// RequireValidIdentity() middleware just as the production router does.
func buildUpdateMilestoneRouter(
	listing *domain.Listing,
	milestone *domain.TenderMilestone,
) *gin.Engine {
	ls := newStubTenderListingStoreH(listing)
	ms := newStubMilestoneStoreH(milestone)
	roles := &noopRoleStoreH{}
	collabs := &noopCollabStoreH{}

	txm := &stubTenderTxManagerH{listings: ls, roles: roles, collaborators: collabs}
	outboxTxm := &stubTenderOutboxTxManagerH{listings: ls, roles: roles, collaborators: collabs}
	milestoneTxm := &stubMilestoneTxManagerH{listings: ls, milestones: ms}

	svc := service.NewTenderService(ls, roles, collabs, ms, txm, outboxTxm, milestoneTxm, nil, events.NewNoopPublisher())
	h := handler.NewTenderHandler(svc)

	r := gin.New()
	r.Use(middleware.RequireValidIdentity())
	r.PATCH("/v1/listings/:id/tender/milestones/:milestoneId", h.UpdateMilestone)

	return r
}

// makeTenderListingH builds a minimal tender listing for handler tests.
func makeTenderListingH(ownerID uuid.UUID, ts domain.TenderStatus) *domain.Listing {
	now := time.Now().UTC()

	return &domain.Listing{
		ID:              uuid.New(),
		OwnerUserID:     ownerID,
		Title:           "Test tender",
		Currency:        "TWD",
		Status:          domain.ListingStatusOpen,
		IsTender:        true,
		RecruiterMode:   domain.RecruiterModeClosed,
		TenderStatus:    &ts,
		KYCTierRequired: 2,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// makePendingMilestoneH builds a PENDING milestone linked to the given listing.
func makePendingMilestoneH(listingID uuid.UUID) *domain.TenderMilestone {
	now := time.Now().UTC()

	return &domain.TenderMilestone{
		ID:        uuid.New(),
		ListingID: listingID,
		Title:     "Test milestone",
		Status:    domain.MilestoneStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// --- M-2 regression tests: handler-layer status allowlist ---

// TestUpdateMilestone_StatusAllowlist_Handler verifies that the UpdateMilestone handler
// rejects any status value other than "REACHED" or "SKIPPED" with HTTP 400 BEFORE
// it reaches the service layer (M-2: handler-layer allowlist prevents log injection).
func TestUpdateMilestone_StatusAllowlist_Handler(t *testing.T) {
	ownerID := uuid.New()

	tests := []struct {
		name       string
		status     string
		wantStatus int
		wantCode   string
	}{
		// Happy path: valid values are accepted and forwarded to the service.
		{
			name:       "REACHED is accepted",
			status:     "REACHED",
			wantStatus: http.StatusOK,
		},
		{
			name:       "SKIPPED is accepted",
			status:     "SKIPPED",
			wantStatus: http.StatusOK,
		},
		// Error cases: all non-allowlisted values must be rejected at the handler layer.
		{
			name:       "empty string is rejected",
			status:     "",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:       "PENDING is rejected (cannot revert a milestone)",
			status:     "PENDING",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:       "lowercase reached is rejected (strict allowlist)",
			status:     "reached",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:       "arbitrary string is rejected",
			status:     "INVALID_STATUS",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:       "newline injection is rejected before reaching service",
			status:     "REACHED\nX-Injected: header",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:       "null-byte injection is rejected",
			status:     "REACHED\x00",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			listing := makeTenderListingH(ownerID, domain.TenderStatusExecuting)
			milestone := makePendingMilestoneH(listing.ID)

			router := buildUpdateMilestoneRouter(listing, milestone)

			body, err := json.Marshal(map[string]string{"status": tc.status})
			require.NoError(t, err)

			req := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPatch,
				"/v1/listings/"+listing.ID.String()+"/tender/milestones/"+milestone.ID.String(),
				bytes.NewReader(body),
			)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-Id", ownerID.String())
			req.Header.Set("X-Kyc-Tier", "3")

			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code, "unexpected status for input %q: body=%s", tc.status, w.Body.String())

			if tc.wantCode != "" {
				var resp map[string]interface{}
				require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "response body must be JSON")

				errObj, ok := resp["error"].(map[string]interface{})
				require.True(t, ok, "response must have an 'error' object, got: %s", w.Body.String())
				assert.Equal(t, tc.wantCode, errObj["code"], "unexpected error code for input %q", tc.status)
			}
		})
	}
}

// TestUpdateMilestone_TxPath_Unit verifies that UpdateMilestone exercises the tx path:
// the stubMilestoneTxManager records whether WithMilestoneTx was called, confirming
// the service wraps the read+validate+write in a transaction (M-1 regression).
func TestUpdateMilestone_TxPath_Unit(t *testing.T) {
	ownerID := uuid.New()

	t.Run("UpdateMilestone exercises WithMilestoneTx", func(t *testing.T) {
		listing := makeTenderListingH(ownerID, domain.TenderStatusExecuting)
		milestone := makePendingMilestoneH(listing.ID)

		ls := newStubTenderListingStoreH(listing)
		ms := newStubMilestoneStoreH(milestone)
		roles := &noopRoleStoreH{}
		collabs := &noopCollabStoreH{}

		txCalled := false
		recordingMilestoneTxm := &recordingMilestoneTxManagerH{
			listings:   ls,
			milestones: ms,
			onCall:     func() { txCalled = true },
		}

		txm := &stubTenderTxManagerH{listings: ls, roles: roles, collaborators: collabs}
		outboxTxm := &stubTenderOutboxTxManagerH{listings: ls, roles: roles, collaborators: collabs}

		svc := service.NewTenderService(
			ls, roles, collabs, ms, txm, outboxTxm, recordingMilestoneTxm, nil, events.NewNoopPublisher(),
		)

		result, err := svc.UpdateMilestone(context.Background(), &service.UpdateMilestoneInput{
			MilestoneID: milestone.ID,
			CallerID:    ownerID,
			Status:      domain.MilestoneStatusReached,
		})

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, domain.MilestoneStatusReached, result.Status)
		assert.True(t, txCalled, "WithMilestoneTx must be called for every UpdateMilestone invocation")
	})

	t.Run("UpdateMilestone with invalid status returns ErrValidation before tx", func(t *testing.T) {
		listing := makeTenderListingH(ownerID, domain.TenderStatusExecuting)
		milestone := makePendingMilestoneH(listing.ID)

		ls := newStubTenderListingStoreH(listing)
		ms := newStubMilestoneStoreH(milestone)
		roles := &noopRoleStoreH{}
		collabs := &noopCollabStoreH{}

		txCalled := false
		recordingMilestoneTxm := &recordingMilestoneTxManagerH{
			listings:   ls,
			milestones: ms,
			onCall:     func() { txCalled = true },
		}

		txm := &stubTenderTxManagerH{listings: ls, roles: roles, collaborators: collabs}
		outboxTxm := &stubTenderOutboxTxManagerH{listings: ls, roles: roles, collaborators: collabs}

		svc := service.NewTenderService(
			ls, roles, collabs, ms, txm, outboxTxm, recordingMilestoneTxm, nil, events.NewNoopPublisher(),
		)

		_, err := svc.UpdateMilestone(context.Background(), &service.UpdateMilestoneInput{
			MilestoneID: milestone.ID,
			CallerID:    ownerID,
			Status:      domain.MilestoneStatusPending, // invalid target
		})

		require.Error(t, err)
		require.ErrorIs(t, err, domain.ErrValidation)
		assert.False(t, txCalled, "WithMilestoneTx must NOT be called for pre-tx validation failures")
	})
}

// recordingMilestoneTxManagerH records whether WithMilestoneTx was called.
type recordingMilestoneTxManagerH struct {
	listings   store.ListingStore
	milestones store.TenderMilestoneStore
	onCall     func()
}

func (m *recordingMilestoneTxManagerH) WithMilestoneTx(
	ctx context.Context,
	fn func(context.Context, store.ListingStore, store.TenderMilestoneStore) error,
) error {
	if m.onCall != nil {
		m.onCall()
	}

	return fn(ctx, m.listings, m.milestones)
}
