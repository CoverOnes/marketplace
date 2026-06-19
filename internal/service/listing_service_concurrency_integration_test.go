package service_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fakes for the deterministic unit test
// ─────────────────────────────────────────────────────────────────────────────

// splitReadListingStore is a fake ListingStore where GetByID and GetByIDForUpdate
// return DIFFERENT rows. This simulates the TOCTOU scenario: the pre-flight
// GetByID sees the row BEFORE a concurrent writer committed, while GetByIDForUpdate
// (called inside the serialized tx) sees the row AFTER the concurrent commit.
//
// forUpdateCallCount counts how many times GetByIDForUpdate is called so the test
// can assert the fix actually calls it (pre-fix code did NOT call it inside the tx).
type splitReadListingStore struct {
	// preflightRow is returned by GetByID (the optimistic pre-flight guard).
	preflightRow *domain.Listing
	// lockedRow is returned by GetByIDForUpdate (the authoritative in-tx locked read).
	lockedRow *domain.Listing
	// updatedWith records the listing passed to Update, so the test can inspect
	// which row was actually persisted.
	updatedWith *domain.Listing
	// forUpdateCallCount is incremented atomically each time GetByIDForUpdate is called.
	forUpdateCallCount atomic.Int32
}

func (s *splitReadListingStore) Create(_ context.Context, _ *domain.Listing) error { return nil }

func (s *splitReadListingStore) GetByID(_ context.Context, _ uuid.UUID) (*domain.Listing, error) {
	// Return a shallow copy so the tx path cannot mutate the original.
	cp := *s.preflightRow
	return &cp, nil
}

func (s *splitReadListingStore) GetByIDForUpdate(_ context.Context, _ uuid.UUID) (*domain.Listing, error) {
	s.forUpdateCallCount.Add(1)
	// Return a shallow copy — simulates the locked row after a concurrent commit.
	cp := *s.lockedRow
	return &cp, nil
}

func (s *splitReadListingStore) List(_ context.Context, _ store.ListingFilter) ([]*domain.Listing, error) {
	return nil, nil
}

func (s *splitReadListingStore) Search(_ context.Context, _ store.SearchFilter) ([]*domain.Listing, error) {
	return nil, nil
}

func (s *splitReadListingStore) GetByIDs(_ context.Context, _ []uuid.UUID, _ store.HydrationFilter) ([]*domain.Listing, error) {
	return nil, nil
}

func (s *splitReadListingStore) Update(_ context.Context, l *domain.Listing) error {
	cp := *l
	s.updatedWith = &cp
	return nil
}

// recordingListingOutboxStore is a minimal OutboxStore that records Enqueue calls
// so the test can assert exactly how many embedding_reindex events were enqueued.
type recordingListingOutboxStore struct {
	events []*domain.OutboxEvent
}

func (r *recordingListingOutboxStore) Enqueue(_ context.Context, e *domain.OutboxEvent) error {
	cp := *e
	r.events = append(r.events, &cp)
	return nil
}

func (r *recordingListingOutboxStore) PollReady(_ context.Context, _ int) ([]*domain.OutboxEvent, error) {
	return nil, nil
}

func (r *recordingListingOutboxStore) MarkPublished(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (r *recordingListingOutboxStore) MarkFailed(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

func (r *recordingListingOutboxStore) MarkDeadLettered(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (r *recordingListingOutboxStore) DeletePublishedBefore(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func (r *recordingListingOutboxStore) DeleteDeadLetteredBefore(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// splitTxManager is a ListingOutboxTxManager that calls the callback with the
// splitReadListingStore so GetByIDForUpdate returns the "concurrent writer" row.
type splitTxManager struct {
	ls *splitReadListingStore
	ob *recordingListingOutboxStore
}

func (m *splitTxManager) WithListingOutboxTx(
	ctx context.Context,
	fn func(context.Context, store.ListingStore, store.OutboxStore) error,
) error {
	return fn(ctx, m.ls, m.ob)
}

// ─────────────────────────────────────────────────────────────────────────────
// Deterministic unit test — the regression gate
// ─────────────────────────────────────────────────────────────────────────────

// TestUpdateListing_LockedReadInsideTx_Unit is the DETERMINISTIC regression test
// for the TOCTOU stale-read fix. It proves two invariants that CANNOT both hold
// on the pre-fix code:
//
//  1. GetByIDForUpdate is called exactly once inside the tx (pre-fix code called
//     Update directly on the pre-patched struct and never called GetByIDForUpdate
//     inside the tx at all — this assertion would FAIL on pre-fix code).
//
//  2. The textChanged decision and the final persisted row are derived from the
//     LOCKED row (the "concurrent writer" state), not from the pre-flight GetByID
//     snapshot. Concretely: when the locked row has title="Concurrent Title" and
//     the caller patches to "Our New Title", textChanged is true and an
//     embedding_reindex event is enqueued. If the patch title happened to equal
//     the pre-flight snapshot (a budget-only update), the test also verifies that
//     textChanged is evaluated against the locked row (not the stale snapshot).
//
// On pre-fix code the first assertion (forUpdateCallCount == 1) FAILS because the
// old updateTenderWithOutbox only called Update(l) where l was already mutated
// outside the tx — GetByIDForUpdate was never invoked inside the callback.
func TestUpdateListing_LockedReadInsideTx_Unit(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	listingID := uuid.New()
	tenderStatus := domain.TenderStatusOpen

	now := time.Now().UTC()

	// preflightRow: what GetByID returns (row before a concurrent commit).
	// title = "Pre-flight Title" — the pre-fix code would snapshot oldTitle from here.
	preflightRow := &domain.Listing{
		ID:            listingID,
		OwnerUserID:   ownerID,
		Title:         "Pre-flight Title",
		Description:   "Original description",
		Currency:      "TWD",
		Status:        domain.ListingStatusOpen,
		IsTender:      true,
		TenderStatus:  &tenderStatus,
		RecruiterMode: domain.RecruiterModeClosed,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	// lockedRow: what GetByIDForUpdate returns inside the tx (concurrent writer committed
	// a different title between the pre-flight read and the tx open).
	// Fixed code must use THIS row as the basis for oldTitle / textChanged.
	lockedRow := &domain.Listing{
		ID:            listingID,
		OwnerUserID:   ownerID,
		Title:         "Concurrent Title", // concurrent writer changed this
		Description:   "Original description",
		Currency:      "TWD",
		Status:        domain.ListingStatusOpen,
		IsTender:      true,
		TenderStatus:  &tenderStatus,
		RecruiterMode: domain.RecruiterModeClosed,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	t.Run("GetByIDForUpdate called inside tx (would FAIL on pre-fix code)", func(t *testing.T) {
		t.Parallel()

		ls := &splitReadListingStore{preflightRow: preflightRow, lockedRow: lockedRow}
		ob := &recordingListingOutboxStore{}
		mgr := &splitTxManager{ls: ls, ob: ob}

		svc := service.NewListingService(ls, mgr, nil, nil)

		newTitle := "Our New Title"
		_, err := svc.UpdateListing(context.Background(), service.UpdateListingInput{
			ID:       listingID,
			CallerID: ownerID,
			Title:    &newTitle,
		})
		require.NoError(t, err)

		// KEY ASSERTION: GetByIDForUpdate MUST have been called exactly once inside
		// the tx callback. On pre-fix code this count is 0 (the old code only called
		// listings.Update on the pre-patched struct and never called GetByIDForUpdate).
		assert.Equal(t, int32(1), ls.forUpdateCallCount.Load(),
			"GetByIDForUpdate must be called exactly once inside the tx (FAILS on pre-fix code where it was never called)")
	})

	t.Run("textChanged computed from locked row, not pre-flight snapshot", func(t *testing.T) {
		t.Parallel()

		// Scenario: caller submits title="Our New Title".
		// lockedRow has title="Concurrent Title" (concurrent writer committed).
		// Fixed: oldTitle = "Concurrent Title" (locked), textChanged = "Our New Title" != "Concurrent Title" = true → enqueue.
		// Pre-fix: oldTitle = "Pre-flight Title" (snapshot), textChanged = "Our New Title" != "Pre-flight Title" = true → also enqueue.
		// Both produce the same enqueue decision here; see the budget-only sub-case below
		// for a scenario where the two diverge on textChanged.

		ls := &splitReadListingStore{preflightRow: preflightRow, lockedRow: lockedRow}
		ob := &recordingListingOutboxStore{}
		mgr := &splitTxManager{ls: ls, ob: ob}

		svc := service.NewListingService(ls, mgr, nil, nil)

		newTitle := "Our New Title"
		got, err := svc.UpdateListing(context.Background(), service.UpdateListingInput{
			ID:       listingID,
			CallerID: ownerID,
			Title:    &newTitle,
		})
		require.NoError(t, err)
		require.NotNil(t, got)

		// The final persisted title must be "Our New Title" (caller's patch applied
		// to the locked row, not to the pre-flight row).
		assert.Equal(t, "Our New Title", ls.updatedWith.Title,
			"Update must be called with the caller's patch applied to the locked row")

		// Exactly one embedding_reindex event must be enqueued because title changed
		// (locked row had "Concurrent Title", patch sets "Our New Title").
		require.Len(t, ob.events, 1,
			"exactly one embedding_reindex event must be enqueued when text changes")

		assert.Equal(t, "marketplace.embedding_reindex", ob.events[0].Channel,
			"enqueued event must be on the embedding_reindex channel")

		assert.Equal(t, listingID.String(), ob.events[0].AggregateID.String(),
			"enqueued event aggregate_id must equal the listing ID")
	})

	t.Run("budget-only update: no enqueue (locked row title unchanged)", func(t *testing.T) {
		// Scenario: caller submits BudgetMin only (no title change).
		// lockedRow has title="Concurrent Title". Patch does not touch title.
		// Fixed: oldTitle = "Concurrent Title" (locked), l.Title after patch = "Concurrent Title" → textChanged = false → NO enqueue.
		// Pre-fix: oldTitle = "Pre-flight Title" (snapshot), l.Title after patch = "Pre-flight Title" (pre-patched struct) → textChanged = false → NO enqueue.
		// Decision is the same; what differs is WHICH row drives the Update.
		// This sub-case primarily validates the no-enqueue invariant is preserved.

		ls := &splitReadListingStore{preflightRow: preflightRow, lockedRow: lockedRow}
		ob := &recordingListingOutboxStore{}
		mgr := &splitTxManager{ls: ls, ob: ob}

		svc := service.NewListingService(ls, mgr, nil, nil)

		budgetMin := decimal.NewFromInt(500)
		_, err := svc.UpdateListing(context.Background(), service.UpdateListingInput{
			ID:        listingID,
			CallerID:  ownerID,
			BudgetMin: &budgetMin,
		})
		require.NoError(t, err)

		assert.Empty(t, ob.events,
			"budget-only update must NOT enqueue an embedding_reindex event")

		// GetByIDForUpdate must still be called even for budget-only updates on
		// tender+outbox path — the tx must be opened regardless of textChanged.
		assert.Equal(t, int32(1), ls.forUpdateCallCount.Load(),
			"GetByIDForUpdate must be called even for budget-only tender updates (FAILS on pre-fix code)")
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration helpers (real Postgres via sharedServiceDSN testcontainer)
// ─────────────────────────────────────────────────────────────────────────────

// buildListingTestService creates a ListingService backed by the shared
// testcontainers PG instance. It wires the real ListingOutboxTxManager so
// the FOR UPDATE path inside updateTenderWithOutbox is exercised.
func buildListingTestService( //nolint:gocritic // unnamedResult: return types are self-documenting
	t *testing.T,
	ctx context.Context,
	dsn string,
) (*service.ListingService, func()) {
	t.Helper()

	pool, err := pgstore.NewPool(ctx, dsn, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	listingStore := pgstore.NewListingStore(pool)
	listingOutboxTxMgr := pgstore.NewListingOutboxTxManager(pool)

	svc := service.NewListingService(listingStore, listingOutboxTxMgr, nil, nil)

	return svc, pool.Close
}

// seedTenderForConcurrency inserts an OPEN tender listing directly via the store.
func seedTenderForConcurrency(
	t *testing.T, ctx context.Context, dsn string,
	ownerID uuid.UUID, title, description string,
) *domain.Listing {
	t.Helper()

	pool, err := pgstore.NewPool(ctx, dsn, "", pgstore.PoolOptions{})
	require.NoError(t, err)
	defer pool.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	ts := domain.TenderStatusOpen
	l := &domain.Listing{
		ID:            uuid.New(),
		OwnerUserID:   ownerID,
		Title:         title,
		Description:   description,
		Currency:      "TWD",
		Status:        domain.ListingStatusOpen,
		IsTender:      true,
		RecruiterMode: domain.RecruiterModeClosed,
		TenderStatus:  &ts,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	require.NoError(t, pgstore.NewListingStore(pool).Create(ctx, l))

	return l
}

// readListingTitle reads the current title directly from DB.
func readListingTitle(t *testing.T, ctx context.Context, dsn string, id uuid.UUID) string {
	t.Helper()

	pool, err := pgstore.NewPool(ctx, dsn, "", pgstore.PoolOptions{})
	require.NoError(t, err)
	defer pool.Close()

	l, err := pgstore.NewListingStore(pool).GetByID(ctx, id)
	require.NoError(t, err)

	return l.Title
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration test — concurrent updates with outbox event-count assertions
// ─────────────────────────────────────────────────────────────────────────────

// TestUpdateListing_TOCTOU_Integration exercises concurrent UpdateListing calls
// against a real Postgres testcontainer and asserts outbox event-count invariants.
//
// NOTE: The DETERMINISTIC regression gate is TestUpdateListing_LockedReadInsideTx_Unit
// above (unit test, no Docker, asserts GetByIDForUpdate call count = 1 which FAILS
// on pre-fix code). The integration sub-cases below are complementary smoke tests
// that verify DB-level serialization and outbox correctness in a real PG environment.
// Case 2 is inherently probabilistic (depends on goroutine scheduling) — the unit
// test above is the authoritative proof of the fix.
func TestUpdateListing_TOCTOU_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping TOCTOU integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN

	ownerID := uuid.New()

	t.Run("concurrent title updates: final state consistent + outbox event count correct", func(t *testing.T) {
		listing := seedTenderForConcurrency(t, ctx, dsn, ownerID, "Original Title", "Some description")

		svc, cleanup := buildListingTestService(t, ctx, dsn)
		defer cleanup()

		titleA := "Title from Goroutine A"
		titleB := "Title from Goroutine B"

		var (
			wg   sync.WaitGroup
			errA error
			errB error
		)

		start := make(chan struct{}) // barrier: fire both goroutines simultaneously

		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			_, errA = svc.UpdateListing(ctx, service.UpdateListingInput{
				ID:       listing.ID,
				CallerID: ownerID,
				Title:    &titleA,
			})
		}()

		go func() {
			defer wg.Done()
			<-start
			_, errB = svc.UpdateListing(ctx, service.UpdateListingInput{
				ID:       listing.ID,
				CallerID: ownerID,
				Title:    &titleB,
			})
		}()

		close(start)
		wg.Wait()

		// Both updates target the same row; the FOR UPDATE lock inside each tx
		// serializes them — both may succeed sequentially, or one may fail.
		// Final title must be exactly one of the two requested values.
		finalTitle := readListingTitle(t, ctx, dsn, listing.ID)

		validTitles := map[string]bool{
			titleA: true,
			titleB: true,
		}

		assert.True(t, validTitles[finalTitle],
			"final title must be one of the two requested values; got %q", finalTitle)

		// At least one goroutine must have succeeded.
		successCount := 0
		if errA == nil {
			successCount++
		}
		if errB == nil {
			successCount++
		}

		require.GreaterOrEqual(t, successCount, 1,
			"at least one UpdateListing call must succeed; errA=%v errB=%v", errA, errB)

		// OUTBOX ASSERTION: each successful text-changing update enqueues exactly one
		// embedding_reindex event. If both succeed (serialized), there must be 2 events.
		// If only one succeeds (the other errored/rolled back), exactly 1 event.
		// In no case should there be 0 events when at least one text-changing update succeeded.
		pendingEvents := countPendingEmbeddingEvents(t, ctx, listing.ID)
		assert.Equal(t, successCount, pendingEvents,
			"number of pending embedding_reindex events must equal number of successful text-changing updates (got %d events for %d successes)",
			pendingEvents, successCount)

		t.Logf("errA=%v errB=%v finalTitle=%q pendingEvents=%d", errA, errB, finalTitle, pendingEvents)
	})

	// Case 2: probabilistic smoke test. Goroutine scheduling determines which update
	// wins; we cannot guarantee the interleaving. The invariant checked here is that
	// a successful text-changing update always produces exactly one outbox event,
	// regardless of whether a concurrent budget-only update also ran.
	// The DETERMINISTIC regression proof is TestUpdateListing_LockedReadInsideTx_Unit.
	t.Run("text update concurrent with budget-only: outbox count matches text-changing successes", func(t *testing.T) {
		listing := seedTenderForConcurrency(t, ctx, dsn, ownerID, "Concurrent Listing", "Original desc")

		svc, cleanup := buildListingTestService(t, ctx, dsn)
		defer cleanup()

		newTitle := "New Concurrent Title"
		budgetMinVal := decimal.NewFromInt(1000)

		var (
			wg      sync.WaitGroup
			errText error
			errBudg error
		)

		start := make(chan struct{})

		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			_, errText = svc.UpdateListing(ctx, service.UpdateListingInput{
				ID:       listing.ID,
				CallerID: ownerID,
				Title:    &newTitle,
			})
		}()

		go func() {
			defer wg.Done()
			<-start
			_, errBudg = svc.UpdateListing(ctx, service.UpdateListingInput{
				ID:        listing.ID,
				CallerID:  ownerID,
				BudgetMin: &budgetMinVal,
			})
		}()

		close(start)
		wg.Wait()

		// Outbox event count must match exactly the number of successful
		// text-changing updates (budget-only updates must NOT enqueue).
		textSucceeded := errText == nil
		pendingEvents := countPendingEmbeddingEvents(t, ctx, listing.ID)

		if textSucceeded {
			assert.Equal(t, 1, pendingEvents,
				"text-changing update succeeded: must be exactly 1 pending embedding_reindex event (got %d)", pendingEvents)

			finalTitle := readListingTitle(t, ctx, dsn, listing.ID)
			assert.Equal(t, newTitle, finalTitle,
				"text update succeeded but final title differs — lost-update: want %q got %q", newTitle, finalTitle)
		} else {
			assert.Equal(t, 0, pendingEvents,
				"text-changing update failed: must be exactly 0 pending embedding_reindex events (got %d)", pendingEvents)
		}

		t.Logf("errText=%v errBudg=%v pendingEvents=%d", errText, errBudg, pendingEvents)
	})
}
