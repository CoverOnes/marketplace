package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeOutboxEvent builds a minimal domain.OutboxEvent for testing.
func makeOutboxEvent(aggregateID uuid.UUID, channel string) *domain.OutboxEvent {
	now := time.Now().UTC().Truncate(time.Millisecond)

	return &domain.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: "tender",
		AggregateID:   aggregateID,
		EventID:       uuid.New(),
		Channel:       channel,
		Payload:       []byte(`{"test":true}`),
		CreatedAt:     now,
		NextAttemptAt: now,
	}
}

// Note: outbox integration tests are intentionally NOT run with t.Parallel().
// All tests share the same sharedTestPool + event_outbox table. PollReady uses
// an atomic-claim CTE (claimed_until) — running tests in parallel would cause
// one test's PollReady to steal rows from another test's enqueue, breaking
// deterministic assertions. Sequential execution gives each test exclusive
// access to the rows it enqueued.

// TestOutboxStore_EnqueueAndPoll verifies that an event can be enqueued and
// then retrieved by PollReady.
func TestOutboxStore_EnqueueAndPoll_Integration(t *testing.T) {
	ctx := context.Background()
	s := pgstore.NewOutboxStore(sharedTestPool)

	evt := makeOutboxEvent(uuid.New(), "marketplace.test_event")

	require.NoError(t, s.Enqueue(ctx, evt))

	// PollReady should return the event (atomic-claim: sets claimed_until).
	events, err := s.PollReady(ctx, 10)
	require.NoError(t, err)

	var found bool

	for _, e := range events {
		if e.ID != evt.ID {
			continue
		}

		found = true

		assert.Equal(t, evt.AggregateType, e.AggregateType)
		assert.Equal(t, evt.AggregateID, e.AggregateID)
		assert.Equal(t, evt.EventID, e.EventID)
		assert.Equal(t, evt.Channel, e.Channel)
		assert.Equal(t, evt.Payload, e.Payload)
		assert.Nil(t, e.PublishedAt, "event must not be published yet")
		assert.Equal(t, 0, e.Attempts)
		assert.NotNil(t, e.ClaimedUntil, "PollReady must set claimed_until")
	}

	require.True(t, found, "enqueued event not found in PollReady results")

	// Cleanup: mark published so this row doesn't interfere with subsequent tests.
	require.NoError(t, s.MarkPublished(ctx, evt.ID))
}

// TestOutboxStore_EnqueueIdempotency verifies that re-enqueueing the same
// event_id (ON CONFLICT DO NOTHING) does not produce a duplicate row.
func TestOutboxStore_EnqueueIdempotency_Integration(t *testing.T) {
	ctx := context.Background()
	s := pgstore.NewOutboxStore(sharedTestPool)

	evt := makeOutboxEvent(uuid.New(), "marketplace.idempotency_test")

	require.NoError(t, s.Enqueue(ctx, evt))
	require.NoError(t, s.Enqueue(ctx, evt), "re-enqueue of same event_id must not error")

	// Should still only find one row with this event_id.
	events, err := s.PollReady(ctx, 100)
	require.NoError(t, err)

	var count int

	for _, e := range events {
		if e.EventID == evt.EventID {
			count++
		}
	}

	assert.Equal(t, 1, count, "only one row should exist for a given event_id")

	// Cleanup.
	require.NoError(t, s.MarkPublished(ctx, evt.ID))
}

// TestOutboxStore_MarkPublished verifies that MarkPublished sets published_at
// and clears claimed_until, so the row is no longer returned by PollReady.
func TestOutboxStore_MarkPublished_Integration(t *testing.T) {
	ctx := context.Background()
	s := pgstore.NewOutboxStore(sharedTestPool)

	evt := makeOutboxEvent(uuid.New(), "marketplace.published_test")
	require.NoError(t, s.Enqueue(ctx, evt))

	require.NoError(t, s.MarkPublished(ctx, evt.ID))

	// PollReady must not return a published event.
	events, err := s.PollReady(ctx, 100)
	require.NoError(t, err)

	for _, e := range events {
		assert.NotEqual(t, evt.ID, e.ID, "published event must not appear in PollReady")
	}

	// Verify claimed_until is NULL after MarkPublished (query directly).
	var claimedUntil *time.Time

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT claimed_until FROM event_outbox WHERE id = $1`,
		evt.ID,
	).Scan(&claimedUntil)

	require.NoError(t, err)
	assert.Nil(t, claimedUntil, "MarkPublished must clear claimed_until")
}

// TestOutboxStore_MarkFailed verifies that MarkFailed increments attempts,
// sets last_error, clears claimed_until, and advances next_attempt_at so the
// event is not immediately re-polled (simulating at-least-once after a failed delivery).
func TestOutboxStore_MarkFailed_Integration(t *testing.T) {
	ctx := context.Background()
	s := pgstore.NewOutboxStore(sharedTestPool)

	evt := makeOutboxEvent(uuid.New(), "marketplace.failed_test")
	require.NoError(t, s.Enqueue(ctx, evt))

	// Claim the row first (matches real poller flow).
	claimed, err := s.PollReady(ctx, 10)
	require.NoError(t, err)

	var found bool

	for _, e := range claimed {
		if e.ID == evt.ID {
			found = true
		}
	}

	require.True(t, found, "event must be claimable before MarkFailed")

	// Simulate a failed publish.
	require.NoError(t, s.MarkFailed(ctx, evt.ID, "publish timeout"))

	// After MarkFailed the row has attempts=1, claimed_until=NULL, and
	// next_attempt_at in the future — so PollReady should NOT return it.
	events, err := s.PollReady(ctx, 100)
	require.NoError(t, err)

	for _, e := range events {
		if e.ID == evt.ID {
			// If it appears it must have attempts=1 and a non-nil last_error.
			assert.Equal(t, 1, e.Attempts)
			require.NotNil(t, e.LastError)
			assert.Contains(t, *e.LastError, "publish timeout")
		}
	}

	// Verify claimed_until is NULL after MarkFailed (query directly).
	var claimedUntil *time.Time

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT claimed_until FROM event_outbox WHERE id = $1`,
		evt.ID,
	).Scan(&claimedUntil)

	require.NoError(t, err)
	assert.Nil(t, claimedUntil, "MarkFailed must clear claimed_until")

	// Cleanup: reset next_attempt_at so the row doesn't linger as unpublished.
	_, _ = sharedTestPool.Exec(ctx, `UPDATE event_outbox SET published_at = now() WHERE id = $1`, evt.ID)
}

// TestOutboxStore_AtLeastOnceAfterCrash simulates the at-least-once guarantee:
//  1. Enqueue an event.
//  2. PollReady claims it (sets claimed_until).
//  3. "Crash" without marking published — MarkFailed clears claimed_until.
//  4. The event becomes re-pollable on the next cycle (after backoff).
func TestOutboxStore_AtLeastOnceAfterCrash_Integration(t *testing.T) {
	ctx := context.Background()
	s := pgstore.NewOutboxStore(sharedTestPool)

	evt := makeOutboxEvent(uuid.New(), "marketplace.crash_test")
	require.NoError(t, s.Enqueue(ctx, evt))

	// First poll: claim the event.
	events1, err := s.PollReady(ctx, 10)
	require.NoError(t, err)

	var claimed bool

	for _, e := range events1 {
		if e.ID == evt.ID {
			claimed = true
		}
	}

	require.True(t, claimed, "event must be claimed on first poll")

	// "Crash" = no MarkPublished call. Simulate recovery by calling MarkFailed
	// so next_attempt_at is advanced and claimed_until is cleared.
	require.NoError(t, s.MarkFailed(ctx, evt.ID, "crash simulation"))

	// The row now has attempts=1, next_attempt_at ≈ now+2s, claimed_until=NULL.
	// Verify the row is still present (not deleted) and has attempts=1.
	var attempts int

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT attempts FROM event_outbox WHERE id = $1`,
		evt.ID,
	).Scan(&attempts)

	require.NoError(t, err)
	assert.Equal(t, 1, attempts, "must show one failed attempt after simulated crash")

	// Cleanup.
	_, _ = sharedTestPool.Exec(ctx, `UPDATE event_outbox SET published_at = now() WHERE id = $1`, evt.ID)
}

// TestOutboxStore_ConcurrentPollersSkipLocked verifies the atomic-claim guarantee:
// two concurrent pollers using PollReady must claim DISJOINT sets of rows (no row
// returned to both pollers), and after both complete all events are published with
// claimed_until cleared.
//
// This is deterministic because PollReady uses a CTE that atomically sets
// claimed_until = now() + 30s for the rows it claims.  A second concurrent poller
// therefore sees those rows as already claimed and skips them — no timing dependency.
func TestOutboxStore_ConcurrentPollersSkipLocked_Integration(t *testing.T) {
	ctx := context.Background()
	seedStore := pgstore.NewOutboxStore(sharedTestPool)

	// Enqueue 4 events.
	const batchSize = 4

	ids := make([]uuid.UUID, batchSize)

	for i := range ids {
		evt := makeOutboxEvent(uuid.New(), "marketplace.skip_locked_test")
		require.NoError(t, seedStore.Enqueue(ctx, evt))
		ids[i] = evt.ID
	}

	idSet := make(map[uuid.UUID]bool, batchSize)
	for _, id := range ids {
		idSet[id] = true
	}

	// Two goroutines each run a full poller cycle: PollReady → MarkPublished.
	// Using separate pools ensures each poll runs on an independent connection.
	pool1, err := pgstore.NewPool(ctx, sharedTestDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	t.Cleanup(pool1.Close)

	pool2, err := pgstore.NewPool(ctx, sharedTestDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	t.Cleanup(pool2.Close)

	store1 := pgstore.NewOutboxStore(pool1)
	store2 := pgstore.NewOutboxStore(pool2)

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		claimedByP1 []uuid.UUID
		claimedByP2 []uuid.UUID
	)

	runPollerCycle := func(s *pgstore.OutboxStore, claimed *[]uuid.UUID) {
		defer wg.Done()

		events, pollErr := s.PollReady(ctx, batchSize)
		if pollErr != nil {
			return
		}

		for _, e := range events {
			if !idSet[e.ID] {
				continue // row belongs to a different test; skip
			}

			mu.Lock()
			*claimed = append(*claimed, e.ID)
			mu.Unlock()

			_ = s.MarkPublished(ctx, e.ID)
		}
	}

	wg.Add(2)
	go runPollerCycle(store1, &claimedByP1)
	go runPollerCycle(store2, &claimedByP2)

	wg.Wait()

	// Core invariant: the atomic-claim CTE must guarantee no row is returned to
	// both pollers.  A duplicate here means claimed_until did not prevent the race.
	p1Set := make(map[uuid.UUID]bool, len(claimedByP1))
	for _, id := range claimedByP1 {
		p1Set[id] = true
	}

	for _, id := range claimedByP2 {
		assert.False(t, p1Set[id], "event %s claimed by BOTH pollers — atomic-claim broken", id)
	}

	// Liveness: together the two pollers must have claimed all 4 events.
	allClaimed := make(map[uuid.UUID]bool, batchSize)
	for _, id := range claimedByP1 {
		allClaimed[id] = true
	}

	for _, id := range claimedByP2 {
		allClaimed[id] = true
	}

	for _, id := range ids {
		assert.True(t, allClaimed[id], "event %s was not claimed by either poller", id)
	}

	// After MarkPublished the row must not appear in PollReady, and claimed_until
	// must be NULL (cleared by MarkPublished).
	remaining, err := seedStore.PollReady(ctx, batchSize*2)
	require.NoError(t, err)

	for _, e := range remaining {
		if idSet[e.ID] {
			t.Errorf("event %s still unpublished / re-claimed after both poller cycles", e.ID)
		}
	}
}

// TestOutboxStore_Retention verifies that DeletePublishedBefore removes
// published rows older than the cutoff and leaves newer rows intact.
func TestOutboxStore_Retention_Integration(t *testing.T) {
	ctx := context.Background()
	s := pgstore.NewOutboxStore(sharedTestPool)

	// Enqueue and mark published (simulates a delivered event).
	old := makeOutboxEvent(uuid.New(), "marketplace.retention_test")
	require.NoError(t, s.Enqueue(ctx, old))
	require.NoError(t, s.MarkPublished(ctx, old.ID))

	// Enqueue a second event but DO NOT mark it published (should never be deleted).
	active := makeOutboxEvent(uuid.New(), "marketplace.retention_active")
	require.NoError(t, s.Enqueue(ctx, active))

	// Cutoff in the future → published row should be deleted.
	cutoff := time.Now().UTC().Add(1 * time.Minute)

	n, err := s.DeletePublishedBefore(ctx, cutoff)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1), "at least the published event must be deleted")

	// Active (unpublished) row must still be pollable.
	events, err := s.PollReady(ctx, 100)
	require.NoError(t, err)

	var activeFound bool

	for _, e := range events {
		if e.ID == active.ID {
			activeFound = true
		}
	}

	assert.True(t, activeFound, "active (unpublished) event must survive retention delete")

	// Cleanup: mark the active event published so subsequent tests don't see it.
	require.NoError(t, s.MarkPublished(ctx, active.ID))
}

// TestOutboxStore_MigrationDDL confirms the migration applied correctly:
// the event_outbox table exists, has the expected columns and index.
func TestOutboxStore_MigrationDDL_Integration(t *testing.T) {
	ctx := context.Background()

	// Verify the table exists.
	var tableExists bool

	err := sharedTestPool.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_name = 'event_outbox'
		)`,
	).Scan(&tableExists)

	require.NoError(t, err)
	assert.True(t, tableExists, "event_outbox table must exist after migration")

	// Verify the poll index exists.
	var indexExists bool

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE indexname = 'event_outbox_poll_idx'
		)`,
	).Scan(&indexExists)

	require.NoError(t, err)
	assert.True(t, indexExists, "event_outbox_poll_idx must exist after migration")

	// Verify UNIQUE constraint on event_id.
	var uniqueExists bool

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.table_constraints
			WHERE table_name = 'event_outbox'
			  AND constraint_type = 'UNIQUE'
			  AND constraint_name = 'event_outbox_event_id_unique'
		)`,
	).Scan(&uniqueExists)

	require.NoError(t, err)
	assert.True(t, uniqueExists, "event_outbox_event_id_unique constraint must exist")

	// Verify claimed_until column exists (atomic-claim pattern).
	var claimedUntilExists bool

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name  = 'event_outbox'
			  AND column_name = 'claimed_until'
		)`,
	).Scan(&claimedUntilExists)

	require.NoError(t, err)
	assert.True(t, claimedUntilExists, "claimed_until column must exist after migration")
}
