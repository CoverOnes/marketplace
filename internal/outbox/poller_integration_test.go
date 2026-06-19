package outbox_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/outbox"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	migrations "github.com/CoverOnes/marketplace/migrations"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// newPollerTestStore spins up a dedicated Postgres container, applies migrations,
// and returns an OutboxStore backed by that container.
// The container is cleaned up via t.Cleanup.
func newPollerTestStore(t *testing.T) *pgstore.OutboxStore {
	t.Helper()

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"pgvector/pgvector:pg17",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err, "start poller test container")

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate poller container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "get poller container DSN")

	pool, err := pgstore.NewPool(ctx, dsn, "", pgstore.PoolOptions{})
	require.NoError(t, err, "create outbox pool")

	t.Cleanup(func() { pool.Close() })

	// Apply all embedded migrations.
	var upFiles []string

	walkErr := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})

	require.NoError(t, walkErr, "walk migrations FS")
	require.NotEmpty(t, upFiles, "must find .up.sql migration files")

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		require.NoError(t, readErr, "read migration %s", file)

		_, execErr := pool.Exec(ctx, string(data))
		require.NoError(t, execErr, "apply migration %s", file)
	}

	return pgstore.NewOutboxStore(pool)
}

// newMiniredisClient starts an in-process Redis server and returns a connected client.
func newMiniredisClient(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()

	mr := miniredis.RunT(t)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	t.Cleanup(func() { _ = rdb.Close() })

	return mr, rdb
}

// pollerTestEvent creates a minimal OutboxEvent for poller tests.
func pollerTestEvent(channel string) *domain.OutboxEvent {
	now := time.Now().UTC().Truncate(time.Millisecond)

	return &domain.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: "tender",
		AggregateID:   uuid.New(),
		EventID:       uuid.New(),
		Channel:       channel,
		Payload:       []byte(fmt.Sprintf(`{"test":true,"ts":%d}`, now.UnixNano())),
		CreatedAt:     now,
		NextAttemptAt: now,
	}
}

// recordingRawPublisher satisfies outbox.Publisher and records which channels
// were published to. It also forwards to a real Redis client so the full
// publish path (including miniredis) is exercised.
type recordingRawPublisher struct {
	mu        sync.Mutex
	published []string
	rdb       *redis.Client
}

func (p *recordingRawPublisher) PublishRaw(ctx context.Context, channel string, payload []byte) error {
	if err := p.rdb.Publish(ctx, channel, payload).Err(); err != nil {
		return fmt.Errorf("redis publish %s: %w", channel, err)
	}

	p.mu.Lock()
	p.published = append(p.published, channel)
	p.mu.Unlock()

	return nil
}

func (p *recordingRawPublisher) deliveries() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]string, len(p.published))
	copy(out, p.published)

	return out
}

// TestPoller_AtLeastOnceDelivery verifies that an event is published by the poller.
// At-least-once guarantee: if the poller crashes after publish but before MarkPublished,
// the event is re-delivered on the next tick. We test the delivery side here; the
// MarkFailed re-delivery path is covered by TestOutboxStore_MarkFailed_Integration.
func TestPoller_AtLeastOnceDelivery_Integration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newPollerTestStore(t)

	_, rdb := newMiniredisClient(t)
	pub := &recordingRawPublisher{rdb: rdb}

	// Enqueue one event.
	evt := pollerTestEvent("marketplace.at_least_once_test")
	require.NoError(t, store.Enqueue(ctx, evt))

	// Run the poller with a short interval.
	poller := outbox.NewPoller(store, pub, 50*time.Millisecond)

	pollerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	go func() { poller.Run(pollerCtx) }()

	// Wait until the event has been delivered at least once.
	require.Eventually(t, func() bool {
		for _, ch := range pub.deliveries() {
			if ch == "marketplace.at_least_once_test" {
				return true
			}
		}

		return false
	}, 5*time.Second, 50*time.Millisecond, "event must be delivered within 5s")
}

// TestPoller_ConcurrentPollersSafe verifies that two concurrent pollers together
// deliver all events (at-least-once guarantee) and that the outbox table is
// eventually drained (all events marked published, none remaining for re-delivery).
//
// Note on at-least-once semantics: SKIP LOCKED prevents concurrent pollers from
// claiming the same rows only at the exact moment both queries run. If poller1's
// SELECT completes (releasing the lock) before poller2 starts, poller2 will see the
// same rows. Both pollers call MarkPublished; the second call is a no-op (WHERE
// published_at IS NULL). This test verifies the at-least-once property — all channels
// appear in combined deliveries — not exactly-once.
func TestPoller_ConcurrentPollersSafe_Integration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newPollerTestStore(t)

	// Both pollers share the same miniredis instance but use separate clients
	// to simulate two independent poller processes.
	mr, rdb1 := newMiniredisClient(t)
	rdb2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	t.Cleanup(func() { _ = rdb2.Close() })

	pub1 := &recordingRawPublisher{rdb: rdb1}
	pub2 := &recordingRawPublisher{rdb: rdb2}

	// Enqueue 10 events, each with a unique channel.
	const total = 10

	channels := make([]string, total)
	for i := range total {
		ch := fmt.Sprintf("marketplace.concurrent_poller_%d", i)
		channels[i] = ch
		evt := pollerTestEvent(ch)
		require.NoError(t, store.Enqueue(ctx, evt))
	}

	// Run two pollers concurrently.
	poller1 := outbox.NewPoller(store, pub1, 100*time.Millisecond)
	poller2 := outbox.NewPoller(store, pub2, 100*time.Millisecond)

	pollerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	go func() { poller1.Run(pollerCtx) }()
	go func() { poller2.Run(pollerCtx) }()

	// Build the expected channel set.
	expectedChannels := make(map[string]bool, total)
	for _, ch := range channels {
		expectedChannels[ch] = true
	}

	// At-least-once: wait until every channel has been delivered at least once.
	require.Eventually(t, func() bool {
		delivered := make(map[string]bool)

		for _, ch := range append(pub1.deliveries(), pub2.deliveries()...) {
			delivered[ch] = true
		}

		for ch := range expectedChannels {
			if !delivered[ch] {
				return false
			}
		}

		return true
	}, 10*time.Second, 100*time.Millisecond, "all 10 channels must be delivered at least once by concurrent pollers")

	// After all events are delivered, PollReady must return empty (all published).
	// Cancel pollers first so they do not interfere with the final drain check.
	cancel()

	// Give pollers a moment to finish their current tick.
	time.Sleep(300 * time.Millisecond)

	remaining, err := store.PollReady(ctx, total*2)
	require.NoError(t, err)

	channelSet := make(map[string]bool, total)
	for _, ch := range channels {
		channelSet[ch] = true
	}

	for _, e := range remaining {
		if channelSet[e.Channel] {
			t.Errorf("event on channel %q still unpublished after concurrent pollers drained table", e.Channel)
		}
	}
}

// TestPoller_Retention verifies that published rows are deleted by the
// retention cutoff, so the poller table does not grow unbounded.
func TestPoller_Retention_Integration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newPollerTestStore(t)

	// Enqueue and immediately mark published to simulate a row eligible for deletion.
	evt := pollerTestEvent("marketplace.retention_poller_test")
	require.NoError(t, store.Enqueue(ctx, evt))
	require.NoError(t, store.MarkPublished(ctx, evt.ID))

	// Delete with a cutoff well in the future — the row should be deleted.
	cutoff := time.Now().UTC().Add(1 * time.Hour)

	n, err := store.DeletePublishedBefore(ctx, cutoff)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1), "at least the published event must be deleted")

	// The event must not appear in PollReady after deletion.
	events, err := store.PollReady(ctx, 100)
	require.NoError(t, err)

	for _, e := range events {
		assert.NotEqual(t, evt.ID, e.ID, "deleted event must not reappear in PollReady")
	}
}

// newPollerTestStoreWithPool is like newPollerTestStore but also returns the
// underlying pool so tests can execute raw SQL (e.g. to pre-set attempts).
func newPollerTestStoreWithPool(t *testing.T) (*pgstore.OutboxStore, *pgxpool.Pool) {
	t.Helper()

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"pgvector/pgvector:pg17",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err, "start poller dead-letter test container")

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate poller dead-letter container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "get poller dead-letter container DSN")

	pool, err := pgstore.NewPool(ctx, dsn, "", pgstore.PoolOptions{})
	require.NoError(t, err, "create poller dead-letter pool")

	t.Cleanup(func() { pool.Close() })

	var upFiles []string

	walkErr := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})

	require.NoError(t, walkErr, "walk migrations FS for poller dead-letter test")
	require.NotEmpty(t, upFiles, "must find .up.sql migration files")

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		require.NoError(t, readErr, "read migration %s", file)

		_, execErr := pool.Exec(ctx, string(data))
		require.NoError(t, execErr, "apply migration %s", file)
	}

	return pgstore.NewOutboxStore(pool), pool
}

// pollerSetAttempts sets attempts on an outbox row and backdates next_attempt_at
// so that PollReady considers it immediately eligible.
func pollerSetAttempts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, attempts int) {
	t.Helper()

	_, err := pool.Exec(
		ctx,
		`UPDATE event_outbox
		 SET attempts = $2, next_attempt_at = now() - interval '1 minute'
		 WHERE id = $1`,
		id, attempts,
	)
	require.NoError(t, err, "set attempts for poller dead-letter test")
}

// failingRawPublisher always returns an error from PublishRaw, driving the
// poller into the MarkFailed / dead-letter path.
type failingRawPublisher struct{ err error }

func (p *failingRawPublisher) PublishRaw(_ context.Context, _ string, _ []byte) error {
	return p.err
}

// successRawPublisher always succeeds, driving the poller into the MarkPublished path.
type successRawPublisher struct{}

func (p *successRawPublisher) PublishRaw(_ context.Context, _ string, _ []byte) error {
	return nil
}

// TestPoller_DeadLettersAfterMaxAttempts_Integration verifies that the outbox
// poller's processEvent dead-letters a row when attempts+1 >= domain.MaxOutboxAttempts,
// and does NOT dead-letter when the event succeeds or when attempts is below the cap.
func TestPoller_DeadLettersAfterMaxAttempts_Integration(t *testing.T) {
	t.Parallel()

	t.Run("dead-letters when attempt count reaches cap", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s, pool := newPollerTestStoreWithPool(t)

		evt := pollerTestEvent("marketplace.poller_deadletter_cap")
		require.NoError(t, s.Enqueue(ctx, evt))

		// Pre-set attempts to maxOutboxAttempts-1 so the next failure trips the cap.
		pollerSetAttempts(t, ctx, pool, evt.ID, domain.MaxOutboxAttempts-1)

		pub := &failingRawPublisher{err: fmt.Errorf("redis: connection refused")}
		poller := outbox.NewPoller(s, pub, 50*time.Millisecond)

		pollerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		go func() { poller.Run(pollerCtx) }()

		// Wait until dead_lettered_at is set.
		var deadLetteredAt *time.Time

		require.Eventually(t, func() bool {
			err := pool.QueryRow(
				ctx,
				`SELECT dead_lettered_at FROM event_outbox WHERE id = $1`,
				evt.ID,
			).Scan(&deadLetteredAt)

			return err == nil && deadLetteredAt != nil
		}, 5*time.Second, 50*time.Millisecond, "dead_lettered_at must be set after max attempts")

		// Row must not appear in PollReady.
		events, err := s.PollReady(ctx, 100)
		require.NoError(t, err)

		for _, e := range events {
			assert.NotEqual(t, evt.ID, e.ID, "dead-lettered event must not appear in PollReady")
		}
	})

	t.Run("no dead-letter when succeeds on attempt equal to max", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s, pool := newPollerTestStoreWithPool(t)

		evt := pollerTestEvent("marketplace.poller_deadletter_success")
		require.NoError(t, s.Enqueue(ctx, evt))

		// At maxOutboxAttempts-1, a success must NOT dead-letter.
		pollerSetAttempts(t, ctx, pool, evt.ID, domain.MaxOutboxAttempts-1)

		pub := &successRawPublisher{}
		poller := outbox.NewPoller(s, pub, 50*time.Millisecond)

		pollerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		go func() { poller.Run(pollerCtx) }()

		// Wait until published_at is set (event processed successfully).
		require.Eventually(t, func() bool {
			var publishedAt *time.Time

			err := pool.QueryRow(
				ctx,
				`SELECT published_at FROM event_outbox WHERE id = $1`,
				evt.ID,
			).Scan(&publishedAt)

			return err == nil && publishedAt != nil
		}, 5*time.Second, 50*time.Millisecond, "published_at must be set after successful publish")

		// dead_lettered_at must NOT be set.
		var deadLetteredAt *time.Time

		err := pool.QueryRow(
			ctx,
			`SELECT dead_lettered_at FROM event_outbox WHERE id = $1`,
			evt.ID,
		).Scan(&deadLetteredAt)

		require.NoError(t, err)
		assert.Nil(t, deadLetteredAt, "successful event must not be dead-lettered")
	})

	t.Run("no dead-letter when fails fewer than max attempts", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		s, pool := newPollerTestStoreWithPool(t)

		evt := pollerTestEvent("marketplace.poller_deadletter_low")
		require.NoError(t, s.Enqueue(ctx, evt))

		// Set attempts to 5 — well below the cap.
		pollerSetAttempts(t, ctx, pool, evt.ID, 5)

		pub := &failingRawPublisher{err: fmt.Errorf("redis: connection refused")}
		poller := outbox.NewPoller(s, pub, 50*time.Millisecond)

		pollerCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		go func() { poller.Run(pollerCtx) }()

		// Wait until attempts advances to 6 (poller ran at least once).
		require.Eventually(t, func() bool {
			var attempts int

			err := pool.QueryRow(
				ctx,
				`SELECT attempts FROM event_outbox WHERE id = $1`,
				evt.ID,
			).Scan(&attempts)

			return err == nil && attempts == 6
		}, 3*time.Second, 50*time.Millisecond, "attempts must advance to 6 after one failure")

		// dead_lettered_at must NOT be set.
		var deadLetteredAt *time.Time

		err := pool.QueryRow(
			ctx,
			`SELECT dead_lettered_at FROM event_outbox WHERE id = $1`,
			evt.ID,
		).Scan(&deadLetteredAt)

		require.NoError(t, err)
		assert.Nil(t, deadLetteredAt, "event below cap must not be dead-lettered")
	})
}

// TestPoller_DeadLettersAfterMaxAttempts_BoundaryCheck_Integration is a focused
// boundary regression: attempts = maxOutboxAttempts-1 + one more failure → dead-lettered.
func TestPoller_DeadLettersAfterMaxAttempts_BoundaryCheck_Integration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, pool := newPollerTestStoreWithPool(t)

	evt := pollerTestEvent("marketplace.poller_deadletter_boundary")
	require.NoError(t, s.Enqueue(ctx, evt))

	pollerSetAttempts(t, ctx, pool, evt.ID, domain.MaxOutboxAttempts-1)

	pub := &failingRawPublisher{err: fmt.Errorf("publish timeout")}
	poller := outbox.NewPoller(s, pub, 50*time.Millisecond)

	pollerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	go func() { poller.Run(pollerCtx) }()

	var deadLetteredAt *time.Time

	require.Eventually(t, func() bool {
		err := pool.QueryRow(
			ctx,
			`SELECT dead_lettered_at FROM event_outbox WHERE id = $1`,
			evt.ID,
		).Scan(&deadLetteredAt)

		return err == nil && deadLetteredAt != nil
	}, 5*time.Second, 50*time.Millisecond,
		"boundary: dead_lettered_at must be set at attempt %d (== MaxOutboxAttempts)", domain.MaxOutboxAttempts)
}
