package service_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/events"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTenderTestService creates a real TenderService backed by a testcontainers PG instance.
// workspaceClient may be nil (nil = workspace call skipped in AcceptCollaborator).
func buildTenderTestService( //nolint:gocritic // unnamedResult: return types are self-documenting
	t *testing.T,
	ctx context.Context,
	dsn string,
	wsClient client.WorkspaceClient,
) (*service.TenderService, func()) {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)

	listingStore := postgres.NewListingStore(pool)
	roleStore := postgres.NewTenderRoleStore(pool)
	collabStore := postgres.NewTenderCollaboratorStore(pool)
	milestoneStore := postgres.NewTenderMilestoneStore(pool)
	txMgr := postgres.NewTenderTxManager(pool)
	outboxTxMgr := postgres.NewOutboxTxManager(pool)
	milestoneTxMgr := postgres.NewMilestoneTxManager(pool)
	publisher := events.NewNoopPublisher()

	svc := service.NewTenderService(
		listingStore,
		roleStore,
		collabStore,
		milestoneStore,
		txMgr,
		outboxTxMgr,
		milestoneTxMgr,
		wsClient,
		publisher,
	)

	return svc, pool.Close
}

// seedTenderListingForService inserts a tender listing directly via the store.
func seedTenderListingForService(
	t *testing.T, ctx context.Context, dsn string,
	ownerID uuid.UUID, tenderStatus domain.TenderStatus,
) *domain.Listing {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)
	defer pool.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	ts := tenderStatus
	l := &domain.Listing{
		ID:              uuid.New(),
		OwnerUserID:     ownerID,
		Title:           "Tender for service test",
		Description:     "concurrent test",
		Currency:        "TWD",
		Status:          domain.ListingStatusOpen,
		IsTender:        true,
		RecruiterMode:   domain.RecruiterModeClosed,
		TenderStatus:    &ts,
		KYCTierRequired: 2,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	require.NoError(t, postgres.NewListingStore(pool).Create(ctx, l))

	return l
}

// seedRoleForService inserts a tender role directly via the store.
func seedRoleForService(
	t *testing.T, ctx context.Context, dsn string,
	listingID uuid.UUID, maxCollaborators *int,
) *domain.TenderRole {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)
	defer pool.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	r := &domain.TenderRole{
		ID:               uuid.New(),
		ListingID:        listingID,
		Title:            "Concurrent test role",
		Description:      "for concurrency test",
		MaxCollaborators: maxCollaborators,
		Status:           domain.TenderRoleStatusOpen,
		SortOrder:        0,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, postgres.NewTenderRoleStore(pool).Create(ctx, r))

	return r
}

// seedCollaboratorForService inserts a PENDING collaborator directly via the store.
func seedCollaboratorForService(
	t *testing.T, ctx context.Context, dsn string,
	roleID, listingID, vendorID uuid.UUID,
) *domain.TenderCollaborator {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)
	defer pool.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	c := &domain.TenderCollaborator{
		ID:           uuid.New(),
		TenderRoleID: roleID,
		ListingID:    listingID,
		VendorUserID: vendorID,
		Status:       domain.CollaboratorStatusPending,
		JoinMessage:  "I can help",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, postgres.NewTenderCollaboratorStore(pool).Create(ctx, c))

	return c
}

// readCollabStatus reads the current collaborator status directly from DB.
func readCollabStatus(t *testing.T, ctx context.Context, dsn string, collabID uuid.UUID) domain.CollaboratorStatus {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)
	defer pool.Close()

	got, err := postgres.NewTenderCollaboratorStore(pool).GetByID(ctx, collabID)
	require.NoError(t, err)

	return got.Status
}

// readRoleStatus reads the current role status directly from DB.
func readRoleStatus(t *testing.T, ctx context.Context, dsn string, roleID uuid.UUID) domain.TenderRoleStatus {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)
	defer pool.Close()

	got, err := postgres.NewTenderRoleStore(pool).GetByID(ctx, roleID)
	require.NoError(t, err)

	return got.Status
}

// TestTenderAcceptCollaborator_TOCTOU_Integration verifies that concurrent
// AcceptCollaborator calls on the real TenderService respect max_collaborators:
// only the exact cap number of collaborators are approved, even under concurrent
// load. This replaces the old toctouAcceptTx reimplementation in the postgres
// package (which exercised a hand-copied copy of the production accept path, not
// the real service method).
func TestTenderAcceptCollaborator_TOCTOU_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN

	ownerID := uuid.New()

	t.Run("max_collaborators=1 with 3 concurrent accepts: exactly 1 APPROVED", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusOpen)

		maxCols := 1
		role := seedRoleForService(t, ctx, dsn, listing.ID, &maxCols)

		v1, v2, v3 := uuid.New(), uuid.New(), uuid.New()
		c1 := seedCollaboratorForService(t, ctx, dsn, role.ID, listing.ID, v1)
		c2 := seedCollaboratorForService(t, ctx, dsn, role.ID, listing.ID, v2)
		c3 := seedCollaboratorForService(t, ctx, dsn, role.ID, listing.ID, v3)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		var wg sync.WaitGroup

		errs := make([]error, 3)
		collabIDs := []uuid.UUID{c1.ID, c2.ID, c3.ID}

		for i, collabID := range collabIDs {
			wg.Add(1)

			go func(idx int, id uuid.UUID) {
				defer wg.Done()

				_, errs[idx] = svc.AcceptCollaborator(ctx, &service.AcceptCollaboratorInput{
					CollaboratorID: id,
					CallerID:       ownerID,
				})
			}(i, collabID)
		}

		wg.Wait()

		approvedCount := 0

		for _, collabID := range collabIDs {
			if readCollabStatus(t, ctx, dsn, collabID) == domain.CollaboratorStatusApproved {
				approvedCount++
			}
		}

		assert.Equal(t, 1, approvedCount, "exactly max_collaborators=1 must be APPROVED")
		assert.Equal(t, domain.TenderRoleStatusFilled, readRoleStatus(t, ctx, dsn, role.ID),
			"role must be FILLED when cap is reached")
	})

	t.Run("max_collaborators=1 EXECUTING tender: exactly 1 APPROVED", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusExecuting)

		maxCols := 1
		role := seedRoleForService(t, ctx, dsn, listing.ID, &maxCols)

		v1, v2 := uuid.New(), uuid.New()
		c1 := seedCollaboratorForService(t, ctx, dsn, role.ID, listing.ID, v1)
		c2 := seedCollaboratorForService(t, ctx, dsn, role.ID, listing.ID, v2)

		// nil workspace client: add-party call is skipped (AcceptCollaborator continues)
		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		var wg sync.WaitGroup

		errs := make([]error, 2)

		for i, collabID := range []uuid.UUID{c1.ID, c2.ID} {
			wg.Add(1)

			go func(idx int, id uuid.UUID) {
				defer wg.Done()

				_, errs[idx] = svc.AcceptCollaborator(ctx, &service.AcceptCollaboratorInput{
					CollaboratorID: id,
					CallerID:       ownerID,
				})
			}(i, collabID)
		}

		wg.Wait()

		approvedCount := 0

		for _, collabID := range []uuid.UUID{c1.ID, c2.ID} {
			if readCollabStatus(t, ctx, dsn, collabID) == domain.CollaboratorStatusApproved {
				approvedCount++
			}
		}

		assert.Equal(t, 1, approvedCount, "exactly 1 APPROVED on EXECUTING tender")
		assert.Equal(t, domain.TenderRoleStatusFilled, readRoleStatus(t, ctx, dsn, role.ID),
			"role must be FILLED after cap reached on EXECUTING tender")
	})
}

// TestTenderAcceptCollaborator_Executing_WorkspaceIntegration verifies that when
// a collaborator is accepted on an EXECUTING tender, the service calls the
// workspace add-party endpoint exactly once with the correct body, and that a 500
// from workspace returns ErrUpstreamWorkspace while leaving the collaborator APPROVED.
func TestTenderAcceptCollaborator_Executing_WorkspaceIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN

	ownerID := uuid.New()

	t.Run("workspace add-party called once on EXECUTING accept", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusExecuting)
		role := seedRoleForService(t, ctx, dsn, listing.ID, nil)
		vendorID := uuid.New()
		collab := seedCollaboratorForService(t, ctx, dsn, role.ID, listing.ID, vendorID)

		var addPartyCalls int64

		fakeWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/internal/v1/multiparty-contracts" {
				atomic.AddInt64(&addPartyCalls, 1)
				w.WriteHeader(http.StatusCreated)

				return
			}

			w.WriteHeader(http.StatusNotFound)
		}))
		defer fakeWS.Close()

		wsClient := client.NewHTTPWorkspaceClient(fakeWS.URL, "test-token")

		svc, cleanup := buildTenderTestService(t, ctx, dsn, wsClient)
		defer cleanup()

		result, err := svc.AcceptCollaborator(ctx, &service.AcceptCollaboratorInput{
			CollaboratorID: collab.ID,
			CallerID:       ownerID,
		})
		require.NoError(t, err)
		assert.Equal(t, domain.CollaboratorStatusApproved, result.Status,
			"collaborator must be APPROVED after workspace accept")
		assert.Equal(t, int64(1), atomic.LoadInt64(&addPartyCalls),
			"workspace add-party must be called exactly once")
	})

	t.Run("workspace 500 returns ErrUpstreamWorkspace; collaborator stays APPROVED", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusExecuting)
		role := seedRoleForService(t, ctx, dsn, listing.ID, nil)
		vendorID := uuid.New()
		collab := seedCollaboratorForService(t, ctx, dsn, role.ID, listing.ID, vendorID)

		fakeWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer fakeWS.Close()

		wsClient := client.NewHTTPWorkspaceClient(fakeWS.URL, "test-token")

		svc, cleanup := buildTenderTestService(t, ctx, dsn, wsClient)
		defer cleanup()

		result, err := svc.AcceptCollaborator(ctx, &service.AcceptCollaboratorInput{
			CollaboratorID: collab.ID,
			CallerID:       ownerID,
		})
		// Service returns ErrUpstreamWorkspace for workspace failures.
		require.ErrorIs(t, err, domain.ErrUpstreamWorkspace,
			"workspace 500 must return ErrUpstreamWorkspace, got: %v", err)
		// result is non-nil and APPROVED: PG tx committed before workspace call.
		require.NotNil(t, result)
		assert.Equal(t, domain.CollaboratorStatusApproved, result.Status,
			"collaborator row must be APPROVED even when workspace 500s (P5 outbox reconciles)")

		// Verify directly from DB: the APPROVED status is durable in Postgres.
		assert.Equal(t, domain.CollaboratorStatusApproved, readCollabStatus(t, ctx, dsn, collab.ID),
			"APPROVED status must be persisted in DB even on workspace 500")
	})
}

// TestTenderAcceptCollaborator_NonOwner_Integration verifies the 404 (IDOR) guard
// on AcceptCollaborator using real Postgres.
func TestTenderAcceptCollaborator_NonOwner_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN

	ownerID := uuid.New()
	strangerID := uuid.New()

	listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusOpen)
	role := seedRoleForService(t, ctx, dsn, listing.ID, nil)
	collab := seedCollaboratorForService(t, ctx, dsn, role.ID, listing.ID, uuid.New())

	svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
	defer cleanup()

	_, err := svc.AcceptCollaborator(ctx, &service.AcceptCollaboratorInput{
		CollaboratorID: collab.ID,
		CallerID:       strangerID, // not the listing owner
	})
	require.ErrorIs(t, err, domain.ErrTenderCollaboratorNotFound,
		"non-owner accept must return ErrTenderCollaboratorNotFound (404), got: %v", err)

	assert.Equal(t, domain.CollaboratorStatusPending, readCollabStatus(t, ctx, dsn, collab.ID),
		"collaborator must remain PENDING after unauthorized accept")
}

// TestTenderExitCollaborator_TOCTOU_Integration verifies that a concurrent
// AcceptCollaborator + ExitCollaborator pair on the same collaborator row
// yields a consistent final state. Without the transaction + FOR UPDATE fix
// on ExitCollaborator, a PENDING→APPROVED accept and a PENDING→WITHDRAWN exit
// could interleave and silently downgrade an accepted collaborator.
//
// The test runs both operations concurrently and asserts that the final status
// is one of the two valid terminal states (APPROVED or WITHDRAWN/EXITED), never
// a combination that represents a lost write.
func TestTenderExitCollaborator_TOCTOU_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN

	ownerID := uuid.New()
	vendorID := uuid.New()

	t.Run("concurrent accept+exit yields consistent final state", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusOpen)
		role := seedRoleForService(t, ctx, dsn, listing.ID, nil)
		collab := seedCollaboratorForService(t, ctx, dsn, role.ID, listing.ID, vendorID)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		// Start both goroutines simultaneously to maximize TOCTOU window.
		var (
			wg        sync.WaitGroup
			acceptErr error
			exitErr   error
		)

		wg.Add(2)

		go func() {
			defer wg.Done()
			_, acceptErr = svc.AcceptCollaborator(ctx, &service.AcceptCollaboratorInput{
				CollaboratorID: collab.ID,
				CallerID:       ownerID,
			})
		}()

		go func() {
			defer wg.Done()
			_, exitErr = svc.ExitCollaborator(ctx, &service.ExitCollaboratorInput{
				CollaboratorID: collab.ID,
				CallerID:       vendorID,
				Reason:         "changed my mind",
			})
		}()

		wg.Wait()

		finalStatus := readCollabStatus(t, ctx, dsn, collab.ID)

		// Exactly one operation must succeed; the other may fail or produce a conflict.
		// The final status must be one of the two valid outcomes — never a "ghost" state
		// where the row was written twice in an inconsistent order.
		//
		// Valid outcome A: accept wins → APPROVED (exit sees APPROVED, returns conflict/validation error).
		// Valid outcome B: exit wins → WITHDRAWN (accept sees WITHDRAWN/non-pending, returns error).
		validOutcomes := map[domain.CollaboratorStatus]bool{
			domain.CollaboratorStatusApproved:  true,
			domain.CollaboratorStatusWithdrawn: true,
			domain.CollaboratorStatusExited:    true, // if exit ran after accept promoted to APPROVED
		}

		assert.True(t, validOutcomes[finalStatus],
			"final status must be one of APPROVED/WITHDRAWN/EXITED, got %s", finalStatus)

		// Both cannot succeed while producing WITHDRAWN — that would mean an approved
		// collaborator was silently downgraded (the pre-fix TOCTOU bug).
		if acceptErr == nil {
			assert.NotEqual(t, domain.CollaboratorStatusWithdrawn, finalStatus,
				"if accept succeeded, final status must not be WITHDRAWN (lost-write bug)")
		}

		// At most one error expected; it's valid for one to succeed and one to conflict.
		t.Logf("accept err=%v exit err=%v finalStatus=%s", acceptErr, exitErr, finalStatus)
	})
}

// TestUpdateMilestone_TOCTOU_Integration verifies that two concurrent
// UpdateMilestone calls on the same PENDING milestone are serialized by the
// SELECT … FOR UPDATE row-lock inside WithMilestoneTx:
//   - Exactly ONE call succeeds and writes the terminal status.
//   - The other call loses the lock race and returns ErrInvalidTenderTransition
//     (the milestone is no longer PENDING when it reads it under its own lock).
//   - reached_at is written exactly once (not double-written by two winners).
//
// This test is only meaningful against a real Postgres instance, so it
// skips under -short and must be run with -race.
func TestUpdateMilestone_TOCTOU_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN
	ownerID := uuid.New()

	t.Run("concurrent PENDING→REACHED races: exactly one winner", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusOpen)
		m := seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusPending)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		const goroutines = 2

		errs := make([]error, goroutines)
		start := make(chan struct{}) // barrier: fire both goroutines at once

		var wg sync.WaitGroup

		for i := range goroutines {
			wg.Add(1)

			go func(idx int) {
				defer wg.Done()

				<-start // wait until barrier is released

				_, errs[idx] = svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
					MilestoneID: m.ID,
					CallerID:    ownerID,
					Status:      domain.MilestoneStatusReached,
				})
			}(i)
		}

		close(start) // release both goroutines simultaneously
		wg.Wait()

		// Count outcomes.
		successCount := 0
		for _, err := range errs {
			if err == nil {
				successCount++
			} else {
				// The loser must return ErrInvalidTenderTransition — it sees a
				// non-PENDING row (locked by the winner) and the transition is illegal.
				require.ErrorIs(t, err, domain.ErrInvalidTenderTransition,
					"loser must return ErrInvalidTenderTransition, got: %v", err)
			}
		}

		require.Equal(t, 1, successCount,
			"exactly one UpdateMilestone call must succeed; FOR UPDATE serializes both")

		// The DB row must reflect exactly one write: REACHED with reached_at set.
		stored := readMilestone(t, ctx, dsn, m.ID)
		assert.Equal(t, domain.MilestoneStatusReached, stored.Status,
			"final DB status must be REACHED")
		assert.NotNil(t, stored.ReachedAt,
			"reached_at must be set exactly once by the winning transaction")

		t.Logf("goroutine errors: %v %v", errs[0], errs[1])
	})

	t.Run("concurrent PENDING→REACHED vs PENDING→SKIPPED: exactly one winner", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusOpen)
		m := seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusPending)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		var (
			wg       sync.WaitGroup
			errReach error
			errSkip  error
		)

		start := make(chan struct{})

		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			_, errReach = svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
				MilestoneID: m.ID,
				CallerID:    ownerID,
				Status:      domain.MilestoneStatusReached,
			})
		}()

		go func() {
			defer wg.Done()
			<-start
			_, errSkip = svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
				MilestoneID: m.ID,
				CallerID:    ownerID,
				Status:      domain.MilestoneStatusSkipped,
			})
		}()

		close(start)
		wg.Wait()

		// Exactly one must succeed.
		if errReach == nil && errSkip == nil {
			t.Fatal("both goroutines succeeded — TOCTOU bug: FOR UPDATE not serializing")
		}

		if errReach != nil && errSkip != nil {
			t.Fatalf("both goroutines failed — unexpected: errReach=%v errSkip=%v", errReach, errSkip)
		}

		// The loser gets ErrInvalidTenderTransition.
		loserErr := errReach
		if loserErr == nil {
			loserErr = errSkip
		}

		require.ErrorIs(t, loserErr, domain.ErrInvalidTenderTransition,
			"loser must return ErrInvalidTenderTransition, got: %v", loserErr)

		// The final DB row must be one of the two valid terminal statuses,
		// and reached_at correctness is validated per-status.
		stored := readMilestone(t, ctx, dsn, m.ID)
		validFinal := stored.Status == domain.MilestoneStatusReached ||
			stored.Status == domain.MilestoneStatusSkipped
		assert.True(t, validFinal,
			"final status must be REACHED or SKIPPED, got %s", stored.Status)

		if stored.Status == domain.MilestoneStatusReached {
			assert.NotNil(t, stored.ReachedAt,
				"REACHED status must have reached_at set")
		} else {
			assert.Nil(t, stored.ReachedAt,
				"SKIPPED status must not have reached_at set")
		}

		t.Logf("errReach=%v errSkip=%v finalStatus=%s", errReach, errSkip, stored.Status)
	})
}

// TestTenderAcceptCollaborator_TerminalState_Integration verifies that accepts on
// CANCELED/COMPLETED tenders are rejected with ErrInvalidTenderTransition.
func TestTenderAcceptCollaborator_TerminalState_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN

	ownerID := uuid.New()

	tests := []struct {
		name   string
		status domain.TenderStatus
	}{
		{name: "CANCELED tender rejects accept", status: domain.TenderStatusCancelled},
		{name: "COMPLETED tender rejects accept", status: domain.TenderStatusCompleted},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			listing := seedTenderListingForService(t, ctx, dsn, ownerID, tc.status)
			role := seedRoleForService(t, ctx, dsn, listing.ID, nil)
			collab := seedCollaboratorForService(t, ctx, dsn, role.ID, listing.ID, uuid.New())

			svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
			defer cleanup()

			_, err := svc.AcceptCollaborator(ctx, &service.AcceptCollaboratorInput{
				CollaboratorID: collab.ID,
				CallerID:       ownerID,
			})
			require.Error(t, err, fmt.Sprintf("accept on %s tender must fail", tc.status))
			require.ErrorIs(t, err, domain.ErrInvalidTenderTransition,
				"expected ErrInvalidTenderTransition for %s tender, got: %v", tc.status, err)

			assert.Equal(t, domain.CollaboratorStatusPending, readCollabStatus(t, ctx, dsn, collab.ID),
				"collaborator must remain PENDING after rejected accept on terminal tender")
		})
	}
}
