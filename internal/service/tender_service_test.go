package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/events"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- stub implementations for tender stores ---

type stubTenderRoleStore struct {
	roles   map[uuid.UUID]*domain.TenderRole
	updated []*domain.TenderRole
}

func newStubTenderRoleStore(roles ...*domain.TenderRole) *stubTenderRoleStore {
	m := &stubTenderRoleStore{roles: make(map[uuid.UUID]*domain.TenderRole)}
	for _, r := range roles {
		m.roles[r.ID] = r
	}

	return m
}

func (s *stubTenderRoleStore) Create(_ context.Context, r *domain.TenderRole) error {
	s.roles[r.ID] = r
	return nil
}

func (s *stubTenderRoleStore) GetByID(_ context.Context, id uuid.UUID) (*domain.TenderRole, error) {
	r, ok := s.roles[id]
	if !ok {
		return nil, domain.ErrTenderRoleNotFound
	}

	return r, nil
}

func (s *stubTenderRoleStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderRole, error) {
	return s.GetByID(ctx, id)
}

func (s *stubTenderRoleStore) ListByListing(_ context.Context, listingID uuid.UUID) ([]*domain.TenderRole, error) {
	var result []*domain.TenderRole

	for _, r := range s.roles {
		if r.ListingID == listingID {
			result = append(result, r)
		}
	}

	return result, nil
}

func (s *stubTenderRoleStore) Update(_ context.Context, r *domain.TenderRole) error {
	if _, ok := s.roles[r.ID]; !ok {
		return domain.ErrTenderRoleNotFound
	}

	s.roles[r.ID] = r
	s.updated = append(s.updated, r)

	return nil
}

type stubTenderCollaboratorStore struct {
	collabs map[uuid.UUID]*domain.TenderCollaborator
	updated []*domain.TenderCollaborator
}

func newStubTenderCollaboratorStore(collabs ...*domain.TenderCollaborator) *stubTenderCollaboratorStore {
	m := &stubTenderCollaboratorStore{collabs: make(map[uuid.UUID]*domain.TenderCollaborator)}
	for _, c := range collabs {
		m.collabs[c.ID] = c
	}

	return m
}

func (s *stubTenderCollaboratorStore) Create(_ context.Context, c *domain.TenderCollaborator) error {
	s.collabs[c.ID] = c
	return nil
}

func (s *stubTenderCollaboratorStore) GetByID(_ context.Context, id uuid.UUID) (*domain.TenderCollaborator, error) {
	c, ok := s.collabs[id]
	if !ok {
		return nil, domain.ErrTenderCollaboratorNotFound
	}

	return c, nil
}

func (s *stubTenderCollaboratorStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderCollaborator, error) {
	return s.GetByID(ctx, id)
}

func (s *stubTenderCollaboratorStore) CountApprovedByRole(_ context.Context, roleID uuid.UUID) (int, error) {
	count := 0

	for _, c := range s.collabs {
		if c.TenderRoleID == roleID && c.Status == domain.CollaboratorStatusApproved {
			count++
		}
	}

	return count, nil
}

func (s *stubTenderCollaboratorStore) ListByListing(_ context.Context, listingID uuid.UUID) ([]*domain.TenderCollaborator, error) {
	var result []*domain.TenderCollaborator

	for _, c := range s.collabs {
		if c.ListingID == listingID {
			result = append(result, c)
		}
	}

	return result, nil
}

func (s *stubTenderCollaboratorStore) ListByRole(_ context.Context, roleID uuid.UUID) ([]*domain.TenderCollaborator, error) {
	var result []*domain.TenderCollaborator

	for _, c := range s.collabs {
		if c.TenderRoleID == roleID {
			result = append(result, c)
		}
	}

	return result, nil
}

func (s *stubTenderCollaboratorStore) Update(_ context.Context, c *domain.TenderCollaborator) error {
	if _, ok := s.collabs[c.ID]; !ok {
		return domain.ErrTenderCollaboratorNotFound
	}

	s.collabs[c.ID] = c
	s.updated = append(s.updated, c)

	return nil
}

type stubTenderMilestoneStore struct {
	milestones map[uuid.UUID]*domain.TenderMilestone
}

func newStubTenderMilestoneStore() *stubTenderMilestoneStore {
	return &stubTenderMilestoneStore{milestones: make(map[uuid.UUID]*domain.TenderMilestone)}
}

func (s *stubTenderMilestoneStore) Create(_ context.Context, m *domain.TenderMilestone) error {
	s.milestones[m.ID] = m
	return nil
}

func (s *stubTenderMilestoneStore) GetByID(_ context.Context, id uuid.UUID) (*domain.TenderMilestone, error) {
	m, ok := s.milestones[id]
	if !ok {
		return nil, domain.ErrTenderMilestoneNotFound
	}

	return m, nil
}

func (s *stubTenderMilestoneStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderMilestone, error) {
	return s.GetByID(ctx, id)
}

func (s *stubTenderMilestoneStore) ListByListing(_ context.Context, listingID uuid.UUID) ([]*domain.TenderMilestone, error) {
	var result []*domain.TenderMilestone

	for _, m := range s.milestones {
		if m.ListingID == listingID {
			result = append(result, m)
		}
	}

	return result, nil
}

func (s *stubTenderMilestoneStore) Update(_ context.Context, m *domain.TenderMilestone) error {
	s.milestones[m.ID] = m
	return nil
}

// stubTenderTxManager calls fn synchronously with the provided stores, simulating a transaction.
type stubTenderTxManager struct {
	listings      store.ListingStore
	roles         store.TenderRoleStore
	collaborators store.TenderCollaboratorStore
}

func (m *stubTenderTxManager) WithTenderTx(
	ctx context.Context,
	fn func(context.Context, store.ListingStore, store.TenderRoleStore, store.TenderCollaboratorStore) error,
) error {
	return fn(ctx, m.listings, m.roles, m.collaborators)
}

// stubOutboxTxManager wraps OutboxTxManager with in-memory stores and a configurable outbox
// so unit tests do not need a real DB for the same-tx enqueue path.
type stubOutboxTxManager struct {
	listings      store.ListingStore
	roles         store.TenderRoleStore
	collaborators store.TenderCollaboratorStore
	// outboxStore overrides the outbox used inside the transaction.
	// If nil, a noopTenderOutboxStore is used.
	outboxStore store.OutboxStore
}

func (m *stubOutboxTxManager) WithOutboxTx(
	ctx context.Context,
	fn func(context.Context, store.ListingStore, store.TenderRoleStore, store.TenderCollaboratorStore, store.OutboxStore) error,
) error {
	ob := m.outboxStore
	if ob == nil {
		ob = &noopTenderOutboxStore{}
	}

	return fn(ctx, m.listings, m.roles, m.collaborators, ob)
}

// stubMilestoneTxManager calls fn synchronously with the provided stores, simulating a transaction.
type stubMilestoneTxManager struct {
	listings   store.ListingStore
	milestones store.TenderMilestoneStore
}

func (m *stubMilestoneTxManager) WithMilestoneTx(
	ctx context.Context,
	fn func(context.Context, store.ListingStore, store.TenderMilestoneStore) error,
) error {
	return fn(ctx, m.listings, m.milestones)
}

// noopTenderOutboxStore satisfies store.OutboxStore for tender unit tests.
type noopTenderOutboxStore struct{}

func (*noopTenderOutboxStore) Enqueue(_ context.Context, _ *domain.OutboxEvent) error { return nil }
func (*noopTenderOutboxStore) PollReady(_ context.Context, _ int) ([]*domain.OutboxEvent, error) {
	return nil, nil
}
func (*noopTenderOutboxStore) MarkPublished(_ context.Context, _ uuid.UUID) error { return nil }
func (*noopTenderOutboxStore) MarkFailed(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

func (*noopTenderOutboxStore) DeletePublishedBefore(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// recordingOutboxStore captures Enqueue calls for assertions in unit tests.
type recordingOutboxStore struct {
	mu       sync.Mutex
	enqueued []*domain.OutboxEvent
}

func (r *recordingOutboxStore) Enqueue(_ context.Context, e *domain.OutboxEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueued = append(r.enqueued, e)

	return nil
}

func (*recordingOutboxStore) PollReady(_ context.Context, _ int) ([]*domain.OutboxEvent, error) {
	return nil, nil
}

func (*recordingOutboxStore) MarkPublished(_ context.Context, _ uuid.UUID) error { return nil }
func (*recordingOutboxStore) MarkFailed(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

func (*recordingOutboxStore) DeletePublishedBefore(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func (r *recordingOutboxStore) enqueuedEvents() []*domain.OutboxEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*domain.OutboxEvent, len(r.enqueued))
	copy(out, r.enqueued)

	return out
}

// makeTenderListing creates a test tender listing with the given tender_status.
func makeTenderListing(ownerID uuid.UUID, ts domain.TenderStatus) *domain.Listing {
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
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
}

// newTenderSvc builds a TenderService wired to the given stub stores.
// Uses a noop publisher and nil workspace client (adequate for most unit tests).
func newTenderSvc(
	ls store.ListingStore,
	rs store.TenderRoleStore,
	cs store.TenderCollaboratorStore,
	ms store.TenderMilestoneStore,
) *service.TenderService {
	txm := &stubTenderTxManager{listings: ls, roles: rs, collaborators: cs}
	outboxTxm := &stubOutboxTxManager{listings: ls, roles: rs, collaborators: cs}
	milestoneTxm := &stubMilestoneTxManager{listings: ls, milestones: ms}

	return service.NewTenderService(ls, rs, cs, ms, txm, outboxTxm, milestoneTxm, nil, events.NewNoopPublisher())
}

// newTenderSvcWithWorkspace builds a TenderService with a custom workspace client.
func newTenderSvcWithWorkspace(
	ls store.ListingStore,
	rs store.TenderRoleStore,
	cs store.TenderCollaboratorStore,
	ms store.TenderMilestoneStore,
	wc client.WorkspaceClient,
) *service.TenderService {
	txm := &stubTenderTxManager{listings: ls, roles: rs, collaborators: cs}
	outboxTxm := &stubOutboxTxManager{listings: ls, roles: rs, collaborators: cs}
	milestoneTxm := &stubMilestoneTxManager{listings: ls, milestones: ms}

	return service.NewTenderService(ls, rs, cs, ms, txm, outboxTxm, milestoneTxm, wc, events.NewNoopPublisher())
}

// --- stub workspace client ---

// stubWorkspaceClient is a fake WorkspaceClient that records calls and can be
// configured to return a specific error for AddPartyToContract.
type stubWorkspaceClient struct {
	createContractErr   error
	addPartyErr         error
	addPartyCallCount   int
	createContractCalls int
}

func (s *stubWorkspaceClient) CreateContract(_ context.Context, _ *domain.Award) error {
	s.createContractCalls++
	return s.createContractErr
}

func (s *stubWorkspaceClient) AddPartyToContract(_ context.Context, _ client.AddPartyInput) error {
	s.addPartyCallCount++
	return s.addPartyErr
}

// --- Fix 2: ApplyToRole tender_status gate ---

// TestApplyToRole_TenderStatusGate proves that applications are rejected when the
// tender is not in a status that accepts new applicants (Fix 2 / security M-1).
func TestApplyToRole_TenderStatusGate(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()

	tests := []struct {
		name         string
		tenderStatus domain.TenderStatus
		wantErr      bool
		wantErrIs    error
	}{
		{
			name:         "happy: OPEN tender accepts applications",
			tenderStatus: domain.TenderStatusOpen,
			wantErr:      false,
		},
		{
			name:         "happy: PARTIALLY_STAFFED tender accepts applications",
			tenderStatus: domain.TenderStatusPartiallyStaffed,
			wantErr:      false,
		},
		{
			name:         "happy: EXECUTING tender accepts applications in Phase 4",
			tenderStatus: domain.TenderStatusExecuting,
			wantErr:      false,
		},
		{
			name:         "error: CANCELED tender rejects applications",
			tenderStatus: domain.TenderStatusCancelled,
			wantErr:      true,
			wantErrIs:    domain.ErrValidation,
		},
		{
			name:         "error: COMPLETED tender rejects applications",
			tenderStatus: domain.TenderStatusCompleted,
			wantErr:      true,
			wantErrIs:    domain.ErrValidation,
		},
		{
			name:         "error: SETTLING tender rejects applications",
			tenderStatus: domain.TenderStatusSettling,
			wantErr:      true,
			wantErrIs:    domain.ErrValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listing := makeTenderListing(ownerID, tc.tenderStatus)
			role := &domain.TenderRole{
				ID:        uuid.New(),
				ListingID: listing.ID,
				Title:     "Dev role",
				Status:    domain.TenderRoleStatusOpen,
			}

			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore(role)
			cs := newStubTenderCollaboratorStore()
			ms := newStubTenderMilestoneStore()
			svc := newTenderSvc(ls, rs, cs, ms)

			_, err := svc.ApplyToRole(context.Background(), &service.ApplyToRoleInput{
				RoleID:       role.ID,
				VendorUserID: vendorID,
				KYCTier:      2,
				JoinMessage:  "I want to join",
			})

			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErrIs),
					"expected %v, got %v", tc.wantErrIs, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// --- Fix 3: milestone amount upper bound ---

// TestCreateMilestone_AmountUpperBound verifies that an amount exceeding
// numeric(14,2) max is rejected with ErrValidation, not a DB 500 (Fix 3 / security M-2).
func TestCreateMilestone_AmountUpperBound(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	ts := domain.TenderStatusOpen
	listing := &domain.Listing{
		ID:              uuid.New(),
		OwnerUserID:     ownerID,
		Title:           "Tender",
		Currency:        "TWD",
		Status:          domain.ListingStatusOpen,
		IsTender:        true,
		TenderStatus:    &ts,
		KYCTierRequired: 0,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}

	// Column limit: numeric(14,2) max = 999999999999.99; one cent over → overflow → 400.
	overMax := decimal.NewFromFloat(1000000000000.00)
	atMax := decimal.NewFromFloat(999999999999.99)

	tests := []struct {
		name      string
		amount    *string
		wantErr   bool
		wantErrIs error
	}{
		{
			name:    "happy: amount at max is accepted",
			amount:  strPtr(atMax.String()),
			wantErr: false,
		},
		{
			name:      "error: amount over max returns ErrValidation (not 500)",
			amount:    strPtr(overMax.String()),
			wantErr:   true,
			wantErrIs: domain.ErrValidation,
		},
		{
			name:      "error: negative amount returns ErrValidation",
			amount:    strPtr("-1.00"),
			wantErr:   true,
			wantErrIs: domain.ErrValidation,
		},
		{
			name:    "happy: nil amount is accepted",
			amount:  nil,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore()
			cs := newStubTenderCollaboratorStore()
			ms := newStubTenderMilestoneStore()
			svc := newTenderSvc(ls, rs, cs, ms)

			var currency *string
			if tc.amount != nil {
				c := "TWD"
				currency = &c
			}

			_, err := svc.CreateMilestone(context.Background(), &service.CreateMilestoneInput{
				ListingID: listing.ID,
				CallerID:  ownerID,
				Title:     "Phase 1",
				Amount:    tc.amount,
				Currency:  currency,
			})

			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErrIs),
					"expected %v wrapping, got %v", tc.wantErrIs, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// --- Fix 1: RejectCollaborator TOCTOU ---

// TestRejectCollaborator_AfterConcurrentApprove proves that rejecting a
// collaborator that was concurrently approved does NOT downgrade the APPROVED
// row back to REJECTED — it returns ErrConflict instead (Fix 1 / reviewer MAJOR).
func TestRejectCollaborator_AfterConcurrentApprove(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()

	ts := domain.TenderStatusOpen
	listing := &domain.Listing{
		ID:           uuid.New(),
		OwnerUserID:  ownerID,
		Title:        "Tender",
		Currency:     "TWD",
		Status:       domain.ListingStatusOpen,
		IsTender:     true,
		TenderStatus: &ts,
	}
	role := &domain.TenderRole{
		ID:        uuid.New(),
		ListingID: listing.ID,
		Title:     "Dev role",
		Status:    domain.TenderRoleStatusOpen,
	}

	approvedAt := time.Now().UTC()
	approvedBy := ownerID

	tests := []struct {
		name               string
		collaboratorStatus domain.CollaboratorStatus
		wantErr            bool
		wantErrIs          error
		wantStatusAfter    domain.CollaboratorStatus
	}{
		{
			name:               "happy: PENDING collaborator gets rejected",
			collaboratorStatus: domain.CollaboratorStatusPending,
			wantErr:            false,
			wantStatusAfter:    domain.CollaboratorStatusRejected,
		},
		{
			name:               "error: already-APPROVED row returns ErrConflict, not downgraded",
			collaboratorStatus: domain.CollaboratorStatusApproved,
			wantErr:            true,
			wantErrIs:          domain.ErrConflict,
			wantStatusAfter:    domain.CollaboratorStatusApproved, // must NOT be changed to REJECTED
		},
		{
			name:               "error: already-REJECTED returns ErrConflict",
			collaboratorStatus: domain.CollaboratorStatusRejected,
			wantErr:            true,
			wantErrIs:          domain.ErrConflict,
			wantStatusAfter:    domain.CollaboratorStatusRejected,
		},
		{
			name:               "error: WITHDRAWN returns ErrConflict",
			collaboratorStatus: domain.CollaboratorStatusWithdrawn,
			wantErr:            true,
			wantErrIs:          domain.ErrConflict,
			wantStatusAfter:    domain.CollaboratorStatusWithdrawn,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			collab := &domain.TenderCollaborator{
				ID:           uuid.New(),
				TenderRoleID: role.ID,
				ListingID:    listing.ID,
				VendorUserID: vendorID,
				Status:       tc.collaboratorStatus,
				ApprovedAt: func() *time.Time {
					if tc.collaboratorStatus == domain.CollaboratorStatusApproved {
						return &approvedAt
					}
					return nil
				}(),
				ApprovedByUserID: func() *uuid.UUID {
					if tc.collaboratorStatus == domain.CollaboratorStatusApproved {
						return &approvedBy
					}
					return nil
				}(),
			}

			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore(role)
			cs := newStubTenderCollaboratorStore(collab)
			ms := newStubTenderMilestoneStore()
			svc := newTenderSvc(ls, rs, cs, ms)

			result, err := svc.RejectCollaborator(context.Background(), &service.RejectCollaboratorInput{
				CollaboratorID: collab.ID,
				CallerID:       ownerID,
			})

			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErrIs),
					"expected error wrapping %v, got %v", tc.wantErrIs, err)
			} else {
				require.NoError(t, err)
			}

			// Critical: verify the collaborator status in the store was not downgraded.
			storedCollab, getErr := cs.GetByID(context.Background(), collab.ID)
			require.NoError(t, getErr)
			assert.Equal(t, tc.wantStatusAfter, storedCollab.Status,
				"collaborator status in store must be %s after reject attempt, got %s",
				tc.wantStatusAfter, storedCollab.Status)

			// For the APPROVED case specifically: the returned result (if any)
			// must never have status=REJECTED.
			if tc.collaboratorStatus == domain.CollaboratorStatusApproved && result != nil {
				assert.NotEqual(t, domain.CollaboratorStatusRejected, result.Status,
					"returned result must not show REJECTED for a row that was APPROVED")
			}
		})
	}
}

// --- Finding #2: terminal-state guards for CreateRole / CloseRole / CreateMilestone ---

// TestCreateRole_TerminalStateGuard verifies that CreateRole rejects mutations on
// tenders in SETTLING, COMPLETED, or CANCELED terminal states (Finding #2 fix).
func TestCreateRole_TerminalStateGuard(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	tests := []struct {
		name         string
		tenderStatus domain.TenderStatus
		wantErr      bool
		wantErrIs    error
	}{
		{
			name:         "happy: OPEN tender accepts new roles",
			tenderStatus: domain.TenderStatusOpen,
			wantErr:      false,
		},
		{
			name:         "happy: PARTIALLY_STAFFED tender accepts new roles",
			tenderStatus: domain.TenderStatusPartiallyStaffed,
			wantErr:      false,
		},
		{
			name:         "error: EXECUTING tender rejects new roles (structure frozen)",
			tenderStatus: domain.TenderStatusExecuting,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
		{
			name:         "error: SETTLING tender rejects new roles",
			tenderStatus: domain.TenderStatusSettling,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
		{
			name:         "error: COMPLETED tender rejects new roles",
			tenderStatus: domain.TenderStatusCompleted,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
		{
			name:         "error: CANCELED tender rejects new roles",
			tenderStatus: domain.TenderStatusCancelled,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listing := makeTenderListing(ownerID, tc.tenderStatus)
			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore()
			cs := newStubTenderCollaboratorStore()
			ms := newStubTenderMilestoneStore()
			svc := newTenderSvc(ls, rs, cs, ms)

			_, err := svc.CreateRole(context.Background(), &service.CreateRoleInput{
				ListingID:   listing.ID,
				CallerID:    ownerID,
				Title:       "New role",
				Description: "desc",
			})

			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErrIs),
					"expected %v, got %v", tc.wantErrIs, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestCloseRole_TerminalStateGuard verifies that CloseRole rejects mutations on
// tenders in SETTLING, COMPLETED, or CANCELED terminal states (Finding #2 fix).
func TestCloseRole_TerminalStateGuard(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	tests := []struct {
		name         string
		tenderStatus domain.TenderStatus
		wantErr      bool
		wantErrIs    error
	}{
		{
			name:         "happy: OPEN tender allows closing a role",
			tenderStatus: domain.TenderStatusOpen,
			wantErr:      false,
		},
		{
			name:         "happy: PARTIALLY_STAFFED tender allows closing a role",
			tenderStatus: domain.TenderStatusPartiallyStaffed,
			wantErr:      false,
		},
		{
			name:         "error: EXECUTING tender rejects CloseRole",
			tenderStatus: domain.TenderStatusExecuting,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
		{
			name:         "error: SETTLING tender rejects CloseRole",
			tenderStatus: domain.TenderStatusSettling,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
		{
			name:         "error: COMPLETED tender rejects CloseRole",
			tenderStatus: domain.TenderStatusCompleted,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
		{
			name:         "error: CANCELED tender rejects CloseRole",
			tenderStatus: domain.TenderStatusCancelled,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listing := makeTenderListing(ownerID, tc.tenderStatus)
			role := &domain.TenderRole{
				ID:        uuid.New(),
				ListingID: listing.ID,
				Title:     "Role to close",
				Status:    domain.TenderRoleStatusOpen,
			}
			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore(role)
			cs := newStubTenderCollaboratorStore()
			ms := newStubTenderMilestoneStore()
			svc := newTenderSvc(ls, rs, cs, ms)

			_, err := svc.CloseRole(context.Background(), &service.CloseRoleInput{
				RoleID:   role.ID,
				CallerID: ownerID,
			})

			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErrIs),
					"expected %v, got %v", tc.wantErrIs, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestCreateMilestone_TerminalStateGuard verifies that CreateMilestone rejects mutations on
// tenders in SETTLING, COMPLETED, or CANCELED terminal states (Finding #2 fix).
func TestCreateMilestone_TerminalStateGuard(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	tests := []struct {
		name         string
		tenderStatus domain.TenderStatus
		wantErr      bool
		wantErrIs    error
	}{
		{
			name:         "happy: OPEN tender accepts new milestones",
			tenderStatus: domain.TenderStatusOpen,
			wantErr:      false,
		},
		{
			name:         "happy: PARTIALLY_STAFFED tender accepts new milestones",
			tenderStatus: domain.TenderStatusPartiallyStaffed,
			wantErr:      false,
		},
		{
			name:         "error: EXECUTING tender rejects new milestones",
			tenderStatus: domain.TenderStatusExecuting,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
		{
			name:         "error: SETTLING tender rejects new milestones",
			tenderStatus: domain.TenderStatusSettling,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
		{
			name:         "error: COMPLETED tender rejects new milestones",
			tenderStatus: domain.TenderStatusCompleted,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
		{
			name:         "error: CANCELED tender rejects new milestones",
			tenderStatus: domain.TenderStatusCancelled,
			wantErr:      true,
			wantErrIs:    domain.ErrInvalidTenderTransition,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listing := makeTenderListing(ownerID, tc.tenderStatus)
			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore()
			cs := newStubTenderCollaboratorStore()
			ms := newStubTenderMilestoneStore()
			svc := newTenderSvc(ls, rs, cs, ms)

			_, err := svc.CreateMilestone(context.Background(), &service.CreateMilestoneInput{
				ListingID: listing.ID,
				CallerID:  ownerID,
				Title:     "Milestone 1",
			})

			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErrIs),
					"expected %v, got %v", tc.wantErrIs, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// --- Finding #5: AcceptCollaborator idempotency when already APPROVED ---

// TestAcceptCollaborator_AlreadyApproved_Idempotent verifies that calling
// AcceptCollaborator on an already-APPROVED collaborator returns success (200)
// instead of ErrValidation/400 (Finding #5 fix).
func TestAcceptCollaborator_AlreadyApproved_Idempotent(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()
	approvedAt := time.Now().UTC()
	approvedBy := ownerID

	tests := []struct {
		name         string
		tenderStatus domain.TenderStatus
		// callWS controls whether a workspace client is provided (EXECUTING path).
		callWS bool
	}{
		{
			name:         "OPEN tender: already-APPROVED is idempotent",
			tenderStatus: domain.TenderStatusOpen,
			callWS:       false,
		},
		{
			name:         "EXECUTING tender: already-APPROVED is idempotent (workspace not re-called on tx path)",
			tenderStatus: domain.TenderStatusExecuting,
			callWS:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listing := makeTenderListing(ownerID, tc.tenderStatus)
			role := &domain.TenderRole{
				ID:        uuid.New(),
				ListingID: listing.ID,
				Title:     "Dev role",
				Status:    domain.TenderRoleStatusOpen,
			}
			collab := &domain.TenderCollaborator{
				ID:               uuid.New(),
				TenderRoleID:     role.ID,
				ListingID:        listing.ID,
				VendorUserID:     vendorID,
				Status:           domain.CollaboratorStatusApproved, // already approved
				ApprovedAt:       &approvedAt,
				ApprovedByUserID: &approvedBy,
				CreatedAt:        time.Now().UTC(),
				UpdatedAt:        time.Now().UTC(),
			}

			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore(role)
			cs := newStubTenderCollaboratorStore(collab)
			ms := newStubTenderMilestoneStore()

			wc := &stubWorkspaceClient{}
			svc := newTenderSvcWithWorkspace(ls, rs, cs, ms, wc)

			result, err := svc.AcceptCollaborator(context.Background(), &service.AcceptCollaboratorInput{
				CollaboratorID: collab.ID,
				CallerID:       ownerID,
			})

			// Must succeed (idempotent) rather than returning ErrValidation/400.
			require.NoError(t, err, "already-APPROVED accept must be idempotent, got: %v", err)
			require.NotNil(t, result)
			assert.Equal(t, domain.CollaboratorStatusApproved, result.Status)
		})
	}
}

// --- Finding #7: ExitCollaborator APPROVED→EXITED deterministic unit test ---

// TestExitCollaborator_ApprovedToExited verifies the deterministic path
// APPROVED→EXITED in ExitCollaborator (Finding #7 test requirement).
func TestExitCollaborator_ApprovedToExited(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()
	approvedAt := time.Now().UTC()
	approvedBy := ownerID

	listing := makeTenderListing(ownerID, domain.TenderStatusExecuting)
	role := &domain.TenderRole{
		ID:        uuid.New(),
		ListingID: listing.ID,
		Title:     "Dev role",
		Status:    domain.TenderRoleStatusOpen,
	}

	tests := []struct {
		name            string
		initialStatus   domain.CollaboratorStatus
		wantErr         bool
		wantErrIs       error
		wantStatusAfter domain.CollaboratorStatus
	}{
		{
			name:            "APPROVED → EXITED (deterministic)",
			initialStatus:   domain.CollaboratorStatusApproved,
			wantErr:         false,
			wantStatusAfter: domain.CollaboratorStatusExited,
		},
		{
			name:            "PENDING → WITHDRAWN",
			initialStatus:   domain.CollaboratorStatusPending,
			wantErr:         false,
			wantStatusAfter: domain.CollaboratorStatusWithdrawn,
		},
		{
			name:          "EXITED → error (not active state)",
			initialStatus: domain.CollaboratorStatusExited,
			wantErr:       true,
			wantErrIs:     domain.ErrValidation,
		},
		{
			name:          "WITHDRAWN → error (not active state)",
			initialStatus: domain.CollaboratorStatusWithdrawn,
			wantErr:       true,
			wantErrIs:     domain.ErrValidation,
		},
		{
			name:          "REJECTED → error (not active state)",
			initialStatus: domain.CollaboratorStatusRejected,
			wantErr:       true,
			wantErrIs:     domain.ErrValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			collab := &domain.TenderCollaborator{
				ID:           uuid.New(),
				TenderRoleID: role.ID,
				ListingID:    listing.ID,
				VendorUserID: vendorID,
				Status:       tc.initialStatus,
				ApprovedAt: func() *time.Time {
					if tc.initialStatus == domain.CollaboratorStatusApproved {
						return &approvedAt
					}
					return nil
				}(),
				ApprovedByUserID: func() *uuid.UUID {
					if tc.initialStatus == domain.CollaboratorStatusApproved {
						return &approvedBy
					}
					return nil
				}(),
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}

			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore(role)
			cs := newStubTenderCollaboratorStore(collab)
			ms := newStubTenderMilestoneStore()
			svc := newTenderSvc(ls, rs, cs, ms)

			result, err := svc.ExitCollaborator(context.Background(), &service.ExitCollaboratorInput{
				CollaboratorID: collab.ID,
				CallerID:       vendorID,
				Reason:         "leaving project",
			})

			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErrIs),
					"expected error wrapping %v, got %v", tc.wantErrIs, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, tc.wantStatusAfter, result.Status)

				// Verify the DB store was updated correctly.
				stored, getErr := cs.GetByID(context.Background(), collab.ID)
				require.NoError(t, getErr)
				assert.Equal(t, tc.wantStatusAfter, stored.Status,
					"store must show %s after exit, got %s", tc.wantStatusAfter, stored.Status)
				assert.NotNil(t, stored.ExitedAt, "exited_at must be set after exit")
				assert.Equal(t, "leaving project", stored.ExitReason)
			}
		})
	}
}

// strPtr is declared in listing_service_test.go (same package); used here without re-declaration.

// --- Phase 4 tests ---

// TestApplyToRole_WhileExecuting verifies that a vendor can apply to a role on an
// EXECUTING tender (Phase 4: join-while-EXECUTING enabled).
func TestApplyToRole_WhileExecuting(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()

	listing := makeTenderListing(ownerID, domain.TenderStatusExecuting)
	listing.RecruiterMode = domain.RecruiterModeClosed

	role := &domain.TenderRole{
		ID:        uuid.New(),
		ListingID: listing.ID,
		Title:     "Dev role",
		Status:    domain.TenderRoleStatusOpen,
	}

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore(role)
	cs := newStubTenderCollaboratorStore()
	ms := newStubTenderMilestoneStore()
	svc := newTenderSvc(ls, rs, cs, ms)

	collab, err := svc.ApplyToRole(context.Background(), &service.ApplyToRoleInput{
		RoleID:       role.ID,
		VendorUserID: vendorID,
		KYCTier:      2,
		JoinMessage:  "Want to join while executing",
	})
	require.NoError(t, err, "ApplyToRole must succeed on EXECUTING tender in Phase 4")
	assert.Equal(t, domain.CollaboratorStatusPending, collab.Status)
	assert.Equal(t, vendorID, collab.VendorUserID)
}

// TestApplyToRole_OpenRecruiterMode verifies that OPEN recruiter mode tenders are accepted
// in Phase 4 (the Phase 1 rejection block has been lifted).
func TestApplyToRole_OpenRecruiterMode(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()

	listing := makeTenderListing(ownerID, domain.TenderStatusOpen)
	listing.RecruiterMode = domain.RecruiterModeOpen

	role := &domain.TenderRole{
		ID:        uuid.New(),
		ListingID: listing.ID,
		Title:     "Open role",
		Status:    domain.TenderRoleStatusOpen,
	}

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore(role)
	cs := newStubTenderCollaboratorStore()
	ms := newStubTenderMilestoneStore()
	svc := newTenderSvc(ls, rs, cs, ms)

	collab, err := svc.ApplyToRole(context.Background(), &service.ApplyToRoleInput{
		RoleID:       role.ID,
		VendorUserID: vendorID,
		KYCTier:      2,
		JoinMessage:  "Open mode apply",
	})
	require.NoError(t, err, "ApplyToRole must succeed for OPEN recruiter mode in Phase 4")
	assert.Equal(t, domain.CollaboratorStatusPending, collab.Status)
}

// TestApplyToRole_SettlingRejected verifies SETTLING still rejects applications.
func TestApplyToRole_SettlingRejected(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()

	listing := makeTenderListing(ownerID, domain.TenderStatusSettling)
	role := &domain.TenderRole{
		ID:        uuid.New(),
		ListingID: listing.ID,
		Title:     "Dev role",
		Status:    domain.TenderRoleStatusOpen,
	}

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore(role)
	cs := newStubTenderCollaboratorStore()
	ms := newStubTenderMilestoneStore()
	svc := newTenderSvc(ls, rs, cs, ms)

	_, err := svc.ApplyToRole(context.Background(), &service.ApplyToRoleInput{
		RoleID:       role.ID,
		VendorUserID: vendorID,
		KYCTier:      2,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation), "SETTLING must reject applications, got: %v", err)
}

// TestIsTenderAcceptingApplications verifies the Phase 4 status gate.
func TestIsTenderAcceptingApplications(t *testing.T) {
	t.Parallel()

	// We test via ApplyToRole since isTenderAcceptingApplications is unexported.
	// This mirrors the acceptance criteria check.
	ownerID := uuid.New()
	vendorID := uuid.New()

	tests := []struct {
		name         string
		tenderStatus domain.TenderStatus
		wantAccept   bool
	}{
		{"OPEN accepts", domain.TenderStatusOpen, true},
		{"PARTIALLY_STAFFED accepts", domain.TenderStatusPartiallyStaffed, true},
		{"EXECUTING accepts", domain.TenderStatusExecuting, true},
		{"SETTLING rejects", domain.TenderStatusSettling, false},
		{"COMPLETED rejects", domain.TenderStatusCompleted, false},
		{"CANCELED rejects", domain.TenderStatusCancelled, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listing := makeTenderListing(ownerID, tc.tenderStatus)
			role := &domain.TenderRole{
				ID:        uuid.New(),
				ListingID: listing.ID,
				Title:     "Role",
				Status:    domain.TenderRoleStatusOpen,
			}

			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore(role)
			cs := newStubTenderCollaboratorStore()
			ms := newStubTenderMilestoneStore()
			svc := newTenderSvc(ls, rs, cs, ms)

			_, err := svc.ApplyToRole(context.Background(), &service.ApplyToRoleInput{
				RoleID:       role.ID,
				VendorUserID: vendorID,
				KYCTier:      2,
			})

			if tc.wantAccept {
				require.NoError(t, err, "status %s should accept, got: %v", tc.tenderStatus, err)
			} else {
				require.Error(t, err, "status %s should reject", tc.tenderStatus)
				assert.True(t, errors.Is(err, domain.ErrValidation))
			}
		})
	}
}

// TestAcceptCollaborator_WhileExecuting_WorkspaceSuccess verifies that accepting a
// collaborator on an EXECUTING tender calls workspace and returns the approved collaborator.
func TestAcceptCollaborator_WhileExecuting_WorkspaceSuccess(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()

	listing := makeTenderListing(ownerID, domain.TenderStatusExecuting)
	role := &domain.TenderRole{
		ID:        uuid.New(),
		ListingID: listing.ID,
		Title:     "Dev role",
		Status:    domain.TenderRoleStatusOpen,
	}
	collab := &domain.TenderCollaborator{
		ID:           uuid.New(),
		TenderRoleID: role.ID,
		ListingID:    listing.ID,
		VendorUserID: vendorID,
		Status:       domain.CollaboratorStatusPending,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	wc := &stubWorkspaceClient{}

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore(role)
	cs := newStubTenderCollaboratorStore(collab)
	ms := newStubTenderMilestoneStore()
	svc := newTenderSvcWithWorkspace(ls, rs, cs, ms, wc)

	result, err := svc.AcceptCollaborator(context.Background(), &service.AcceptCollaboratorInput{
		CollaboratorID: collab.ID,
		CallerID:       ownerID,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.CollaboratorStatusApproved, result.Status)
	assert.Equal(t, 1, wc.addPartyCallCount, "AddPartyToContract must be called once")
}

// TestAcceptCollaborator_WhileExecuting_WorkspaceFailure verifies that when the workspace
// call fails, the service returns a 502-mappable error. The collaborator row stays APPROVED.
func TestAcceptCollaborator_WhileExecuting_WorkspaceFailure(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()

	listing := makeTenderListing(ownerID, domain.TenderStatusExecuting)
	role := &domain.TenderRole{
		ID:        uuid.New(),
		ListingID: listing.ID,
		Title:     "Dev role",
		Status:    domain.TenderRoleStatusOpen,
	}
	collab := &domain.TenderCollaborator{
		ID:           uuid.New(),
		TenderRoleID: role.ID,
		ListingID:    listing.ID,
		VendorUserID: vendorID,
		Status:       domain.CollaboratorStatusPending,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	wc := &stubWorkspaceClient{addPartyErr: errors.New("workspace 500")}

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore(role)
	cs := newStubTenderCollaboratorStore(collab)
	ms := newStubTenderMilestoneStore()
	svc := newTenderSvcWithWorkspace(ls, rs, cs, ms, wc)

	result, err := svc.AcceptCollaborator(context.Background(), &service.AcceptCollaboratorInput{
		CollaboratorID: collab.ID,
		CallerID:       ownerID,
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUpstreamWorkspace), "must return ErrUpstreamWorkspace, got: %v", err)

	// The collaborator must still be APPROVED in the store (tx already committed).
	stored, getErr := cs.GetByID(context.Background(), collab.ID)
	require.NoError(t, getErr)
	assert.Equal(t, domain.CollaboratorStatusApproved, stored.Status,
		"collaborator must remain APPROVED even when workspace call fails")

	// The returned result (partial result before error) must reflect APPROVED.
	require.NotNil(t, result)
	assert.Equal(t, domain.CollaboratorStatusApproved, result.Status)
}

// TestAcceptCollaborator_TerminalStates verifies that accepting on SETTLING, COMPLETED,
// or CANCELED returns ErrInvalidTenderTransition (409).
// M-1 fix: SETTLING was previously missing from the guard, allowing an owner to approve
// a collaborator on a winding-down tender and producing inconsistent DB state.
func TestAcceptCollaborator_TerminalStates(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()

	tests := []struct {
		name         string
		tenderStatus domain.TenderStatus
	}{
		{"SETTLING → 409", domain.TenderStatusSettling},
		{"COMPLETED → 409", domain.TenderStatusCompleted},
		{"CANCELED → 409", domain.TenderStatusCancelled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listing := makeTenderListing(ownerID, tc.tenderStatus)
			role := &domain.TenderRole{
				ID:        uuid.New(),
				ListingID: listing.ID,
				Title:     "Role",
				Status:    domain.TenderRoleStatusOpen,
			}
			collab := &domain.TenderCollaborator{
				ID:           uuid.New(),
				TenderRoleID: role.ID,
				ListingID:    listing.ID,
				VendorUserID: vendorID,
				Status:       domain.CollaboratorStatusPending,
				CreatedAt:    time.Now().UTC(),
				UpdatedAt:    time.Now().UTC(),
			}

			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore(role)
			cs := newStubTenderCollaboratorStore(collab)
			ms := newStubTenderMilestoneStore()
			svc := newTenderSvc(ls, rs, cs, ms)

			_, err := svc.AcceptCollaborator(context.Background(), &service.AcceptCollaboratorInput{
				CollaboratorID: collab.ID,
				CallerID:       ownerID,
			})
			require.Error(t, err)
			assert.True(t, errors.Is(err, domain.ErrInvalidTenderTransition),
				"expected ErrInvalidTenderTransition, got: %v", err)
		})
	}
}

// TestAcceptCollaborator_NonExecuting_NoWorkspaceCall verifies workspace is NOT called
// when tender is in OPEN or PARTIALLY_STAFFED status.
func TestAcceptCollaborator_NonExecuting_NoWorkspaceCall(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()

	tests := []struct {
		name         string
		tenderStatus domain.TenderStatus
	}{
		{"OPEN", domain.TenderStatusOpen},
		{"PARTIALLY_STAFFED", domain.TenderStatusPartiallyStaffed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listing := makeTenderListing(ownerID, tc.tenderStatus)
			role := &domain.TenderRole{
				ID:        uuid.New(),
				ListingID: listing.ID,
				Title:     "Role",
				Status:    domain.TenderRoleStatusOpen,
			}
			collab := &domain.TenderCollaborator{
				ID:           uuid.New(),
				TenderRoleID: role.ID,
				ListingID:    listing.ID,
				VendorUserID: vendorID,
				Status:       domain.CollaboratorStatusPending,
				CreatedAt:    time.Now().UTC(),
				UpdatedAt:    time.Now().UTC(),
			}

			wc := &stubWorkspaceClient{}

			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore(role)
			cs := newStubTenderCollaboratorStore(collab)
			ms := newStubTenderMilestoneStore()
			svc := newTenderSvcWithWorkspace(ls, rs, cs, ms, wc)

			_, err := svc.AcceptCollaborator(context.Background(), &service.AcceptCollaboratorInput{
				CollaboratorID: collab.ID,
				CallerID:       ownerID,
			})
			require.NoError(t, err)
			assert.Equal(t, 0, wc.addPartyCallCount,
				"workspace must NOT be called for non-EXECUTING tenders")
		})
	}
}

// TestCollaboratorJoined_EnqueuedToOutbox verifies that AcceptCollaborator enqueues
// a collaborator_joined event into the outbox store (same-tx outbox pattern).
// Event delivery to Redis is handled by the outbox poller; this test covers the enqueue side.
func TestCollaboratorJoined_EnqueuedToOutbox(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendorID := uuid.New()

	listing := makeTenderListing(ownerID, domain.TenderStatusExecuting)
	role := &domain.TenderRole{
		ID:        uuid.New(),
		ListingID: listing.ID,
		Title:     "Dev role",
		Status:    domain.TenderRoleStatusOpen,
	}
	collab := &domain.TenderCollaborator{
		ID:           uuid.New(),
		TenderRoleID: role.ID,
		ListingID:    listing.ID,
		VendorUserID: vendorID,
		Status:       domain.CollaboratorStatusPending,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	wc := &stubWorkspaceClient{}
	recOutbox := &recordingOutboxStore{}

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore(role)
	cs := newStubTenderCollaboratorStore(collab)
	ms := newStubTenderMilestoneStore()

	// Wire the recording outbox store into the outbox tx manager.
	txm := &stubTenderTxManager{listings: ls, roles: rs, collaborators: cs}
	outboxTxm := &stubOutboxTxManager{listings: ls, roles: rs, collaborators: cs, outboxStore: recOutbox}
	milestoneTxm := &stubMilestoneTxManager{listings: ls, milestones: ms}
	svc := service.NewTenderService(ls, rs, cs, ms, txm, outboxTxm, milestoneTxm, wc, events.NewNoopPublisher())

	_, err := svc.AcceptCollaborator(context.Background(), &service.AcceptCollaboratorInput{
		CollaboratorID: collab.ID,
		CallerID:       ownerID,
	})
	require.NoError(t, err)

	// AcceptCollaborator enqueues synchronously inside the transaction closure.
	// No goroutine needed — check immediately.
	enqueued := recOutbox.enqueuedEvents()
	require.Len(t, enqueued, 1, "collaborator_joined must be enqueued to the outbox exactly once")

	evt := enqueued[0]
	assert.Equal(t, "tender", evt.AggregateType)
	assert.Equal(t, listing.ID, evt.AggregateID)
	assert.Equal(t, "marketplace.collaborator_joined", evt.Channel)
	assert.NotEmpty(t, evt.Payload, "outbox payload must not be empty")
	assert.Nil(t, evt.PublishedAt, "freshly enqueued event must not yet be published")
}

// --- Milestone transition unit tests ---

// TestUpdateMilestone_Transitions verifies legal and illegal milestone status transitions.
func TestUpdateMilestone_Transitions(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	tests := []struct {
		name          string
		initialStatus domain.MilestoneStatus
		targetStatus  domain.MilestoneStatus
		tenderStatus  domain.TenderStatus
		wantErr       bool
		wantErrIs     error
		wantReachedAt bool // true = reached_at must be set after transition
	}{
		{
			name:          "happy: PENDING → REACHED (sets reached_at)",
			initialStatus: domain.MilestoneStatusPending,
			targetStatus:  domain.MilestoneStatusReached,
			tenderStatus:  domain.TenderStatusOpen,
			wantErr:       false,
			wantReachedAt: true,
		},
		{
			name:          "happy: PENDING → SKIPPED",
			initialStatus: domain.MilestoneStatusPending,
			targetStatus:  domain.MilestoneStatusSkipped,
			tenderStatus:  domain.TenderStatusOpen,
			wantErr:       false,
			wantReachedAt: false,
		},
		{
			name:          "happy: PENDING → REACHED on EXECUTING tender",
			initialStatus: domain.MilestoneStatusPending,
			targetStatus:  domain.MilestoneStatusReached,
			tenderStatus:  domain.TenderStatusExecuting,
			wantErr:       false,
			wantReachedAt: true,
		},
		{
			name:          "error: REACHED → PENDING (revert not allowed)",
			initialStatus: domain.MilestoneStatusReached,
			targetStatus:  domain.MilestoneStatusPending,
			tenderStatus:  domain.TenderStatusOpen,
			wantErr:       true,
			wantErrIs:     domain.ErrValidation, // caught by input validation before transition check
		},
		{
			name:          "error: REACHED → SKIPPED (cross-transition not allowed)",
			initialStatus: domain.MilestoneStatusReached,
			targetStatus:  domain.MilestoneStatusSkipped,
			tenderStatus:  domain.TenderStatusOpen,
			wantErr:       true,
			wantErrIs:     domain.ErrInvalidTenderTransition,
		},
		{
			name:          "error: SKIPPED → REACHED (cross-transition not allowed)",
			initialStatus: domain.MilestoneStatusSkipped,
			targetStatus:  domain.MilestoneStatusReached,
			tenderStatus:  domain.TenderStatusOpen,
			wantErr:       true,
			wantErrIs:     domain.ErrInvalidTenderTransition,
		},
		{
			name:          "error: PENDING → REACHED on SETTLING tender (terminal reject)",
			initialStatus: domain.MilestoneStatusPending,
			targetStatus:  domain.MilestoneStatusReached,
			tenderStatus:  domain.TenderStatusSettling,
			wantErr:       true,
			wantErrIs:     domain.ErrInvalidTenderTransition,
		},
		{
			name:          "error: PENDING → REACHED on COMPLETED tender (terminal reject)",
			initialStatus: domain.MilestoneStatusPending,
			targetStatus:  domain.MilestoneStatusReached,
			tenderStatus:  domain.TenderStatusCompleted,
			wantErr:       true,
			wantErrIs:     domain.ErrInvalidTenderTransition,
		},
		{
			name:          "error: PENDING → REACHED on CANCELED tender (terminal reject)",
			initialStatus: domain.MilestoneStatusPending,
			targetStatus:  domain.MilestoneStatusReached,
			tenderStatus:  domain.TenderStatusCancelled,
			wantErr:       true,
			wantErrIs:     domain.ErrInvalidTenderTransition,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			listing := makeTenderListing(ownerID, tc.tenderStatus)
			ms := newStubTenderMilestoneStore()

			m := &domain.TenderMilestone{
				ID:        uuid.New(),
				ListingID: listing.ID,
				Title:     "Test milestone",
				Status:    tc.initialStatus,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}
			ms.milestones[m.ID] = m

			ls := newStubListingStore(listing)
			rs := newStubTenderRoleStore()
			cs := newStubTenderCollaboratorStore()
			svc := newTenderSvc(ls, rs, cs, ms)

			result, err := svc.UpdateMilestone(context.Background(), &service.UpdateMilestoneInput{
				MilestoneID: m.ID,
				CallerID:    ownerID,
				Status:      tc.targetStatus,
			})

			if tc.wantErr {
				require.Error(t, err)
				if tc.wantErrIs != nil {
					assert.True(t, errors.Is(err, tc.wantErrIs),
						"expected %v, got %v", tc.wantErrIs, err)
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, tc.targetStatus, result.Status)

				if tc.wantReachedAt {
					assert.NotNil(t, result.ReachedAt, "reached_at must be set for REACHED transition")
				} else {
					assert.Nil(t, result.ReachedAt, "reached_at must not be set for SKIPPED transition")
				}

				// Verify the milestone in the store was updated.
				stored := ms.milestones[m.ID]
				assert.Equal(t, tc.targetStatus, stored.Status,
					"store must reflect new status %s, got %s", tc.targetStatus, stored.Status)
			}
		})
	}
}

// TestUpdateMilestone_OwnerGuard verifies that a non-owner gets 404 (enumeration guard).
func TestUpdateMilestone_OwnerGuard(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	strangerID := uuid.New()
	listing := makeTenderListing(ownerID, domain.TenderStatusOpen)

	ms := newStubTenderMilestoneStore()
	m := &domain.TenderMilestone{
		ID:        uuid.New(),
		ListingID: listing.ID,
		Title:     "Guarded milestone",
		Status:    domain.MilestoneStatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	ms.milestones[m.ID] = m

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore()
	cs := newStubTenderCollaboratorStore()
	svc := newTenderSvc(ls, rs, cs, ms)

	_, err := svc.UpdateMilestone(context.Background(), &service.UpdateMilestoneInput{
		MilestoneID: m.ID,
		CallerID:    strangerID, // not the owner
		Status:      domain.MilestoneStatusReached,
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrTenderMilestoneNotFound),
		"non-owner must receive ErrTenderMilestoneNotFound (404 guard), got: %v", err)
}

// TestUpdateMilestone_MilestoneNotFound verifies that a missing milestone returns 404.
func TestUpdateMilestone_MilestoneNotFound(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	listing := makeTenderListing(ownerID, domain.TenderStatusOpen)

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore()
	cs := newStubTenderCollaboratorStore()
	ms := newStubTenderMilestoneStore() // empty: milestone not found
	svc := newTenderSvc(ls, rs, cs, ms)

	_, err := svc.UpdateMilestone(context.Background(), &service.UpdateMilestoneInput{
		MilestoneID: uuid.New(), // non-existent
		CallerID:    ownerID,
		Status:      domain.MilestoneStatusReached,
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrTenderMilestoneNotFound),
		"missing milestone must return ErrTenderMilestoneNotFound, got: %v", err)
}

// TestGetMilestoneProgress_Counts verifies progress aggregation across statuses.
func TestGetMilestoneProgress_Counts(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	listing := makeTenderListing(ownerID, domain.TenderStatusExecuting)

	ms := newStubTenderMilestoneStore()

	statuses := []domain.MilestoneStatus{
		domain.MilestoneStatusPending,
		domain.MilestoneStatusPending,
		domain.MilestoneStatusReached,
		domain.MilestoneStatusSkipped,
	}
	for _, st := range statuses {
		id := uuid.New()
		ms.milestones[id] = &domain.TenderMilestone{
			ID:        id,
			ListingID: listing.ID,
			Title:     "m",
			Status:    st,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}
	}

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore()
	cs := newStubTenderCollaboratorStore()
	svc := newTenderSvc(ls, rs, cs, ms)

	progress, err := svc.GetMilestoneProgress(context.Background(), listing.ID, ownerID)
	require.NoError(t, err)
	require.NotNil(t, progress)
	assert.Equal(t, 4, progress.Total)
	assert.Equal(t, 2, progress.Pending)
	assert.Equal(t, 1, progress.Reached)
	assert.Equal(t, 1, progress.Skipped)
}

// TestGetMilestoneProgress_OwnerGuard verifies that non-owner gets 404.
func TestGetMilestoneProgress_OwnerGuard(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	strangerID := uuid.New()
	listing := makeTenderListing(ownerID, domain.TenderStatusOpen)

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore()
	cs := newStubTenderCollaboratorStore()
	ms := newStubTenderMilestoneStore()
	svc := newTenderSvc(ls, rs, cs, ms)

	_, err := svc.GetMilestoneProgress(context.Background(), listing.ID, strangerID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrListingNotFound),
		"non-owner must receive ErrListingNotFound (404 guard), got: %v", err)
}

// TestGetMilestoneProgress_EmptyList verifies progress on a listing with no milestones.
func TestGetMilestoneProgress_EmptyList(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	listing := makeTenderListing(ownerID, domain.TenderStatusOpen)

	ls := newStubListingStore(listing)
	rs := newStubTenderRoleStore()
	cs := newStubTenderCollaboratorStore()
	ms := newStubTenderMilestoneStore()
	svc := newTenderSvc(ls, rs, cs, ms)

	progress, err := svc.GetMilestoneProgress(context.Background(), listing.ID, ownerID)
	require.NoError(t, err)
	require.NotNil(t, progress)
	assert.Equal(t, 0, progress.Total)
	assert.Equal(t, 0, progress.Pending)
	assert.Equal(t, 0, progress.Reached)
	assert.Equal(t, 0, progress.Skipped)
}
