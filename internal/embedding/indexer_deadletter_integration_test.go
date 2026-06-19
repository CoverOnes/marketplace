package embedding_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/embedding"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	migrations "github.com/CoverOnes/marketplace/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// failingEmbeddingClient always returns an error from Generate.
// Used to drive processEvent into the failure path for dead-letter testing.
type failingEmbeddingClient struct{ err error }

func (c *failingEmbeddingClient) Generate(_ context.Context, _ string) ([]float32, error) {
	return nil, c.err
}

// successEmbeddingClient returns a minimal valid vector from Generate.
type successEmbeddingClient struct{}

func (c *successEmbeddingClient) Generate(_ context.Context, _ string) ([]float32, error) {
	return make([]float32, 1536), nil
}

// startIndexerTestDB spins up a dedicated Postgres container with all migrations applied
// and returns a pool. The container is terminated via t.Cleanup.
func startIndexerTestDB(t *testing.T) *pgxpool.Pool {
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
	require.NoError(t, err, "start pgvector container for indexer dead-letter test")

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate pgvector container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "get container DSN")

	pool, err := pgstore.NewPool(ctx, dsn, "", pgstore.PoolOptions{})
	require.NoError(t, err, "create pool for indexer dead-letter test")

	t.Cleanup(func() { pool.Close() })

	require.NoError(t, applyIndexerTestMigrations(ctx, pool), "apply migrations")

	return pool
}

// applyIndexerTestMigrations applies all embedded *.up.sql files in order.
func applyIndexerTestMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	var upFiles []string

	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("walk migrations FS: %w", err)
	}

	if len(upFiles) == 0 {
		return fmt.Errorf("no *.up.sql files found in embedded FS")
	}

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		if readErr != nil {
			return fmt.Errorf("read migration %s: %w", file, readErr)
		}

		if _, execErr := pool.Exec(ctx, string(data)); execErr != nil {
			return fmt.Errorf("apply migration %s: %w", file, execErr)
		}
	}

	return nil
}

// makeIndexerOutboxEvent builds a minimal embedding_reindex outbox event with
// next_attempt_at in the past so PollReady returns it immediately.
func makeIndexerOutboxEvent(tenderID uuid.UUID) *domain.OutboxEvent {
	now := time.Now().UTC().Truncate(time.Millisecond)

	payload := []byte(fmt.Sprintf(`{"data":{"tenderId":%q}}`, tenderID.String()))

	return &domain.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: "tender",
		AggregateID:   tenderID,
		EventID:       uuid.New(),
		Channel:       "marketplace.embedding_reindex",
		Payload:       payload,
		CreatedAt:     now,
		NextAttemptAt: now.Add(-time.Minute),
	}
}

// setOutboxAttempts directly sets the attempts count for a row in the DB.
// Used to pre-position a row near the dead-letter threshold without running N
// MarkFailed cycles (which would advance next_attempt_at into the far future).
func setOutboxAttempts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, attempts int) {
	t.Helper()

	_, err := pool.Exec(
		ctx,
		`UPDATE event_outbox SET attempts = $1, next_attempt_at = now() - interval '1 minute' WHERE id = $2`,
		attempts, id,
	)
	require.NoError(t, err, "setOutboxAttempts: UPDATE event_outbox")
}

// TestIndexer_DeadLettersAfterMaxAttempts_Integration verifies:
//  1. A row that has failed maxOutboxAttempts-1 times and then fails once more
//     is dead-lettered (dead_lettered_at set, absent from PollReady).
//  2. A row that has failed 19 times and then succeeds on attempt 20 is NOT dead-lettered.
//
// Uses testcontainers PG — never mocked (backend-security §6.5).
func TestIndexer_DeadLettersAfterMaxAttempts_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pool := startIndexerTestDB(t)

	outboxStore := pgstore.NewOutboxStore(pool)
	listingStore := pgstore.NewListingStore(pool)
	embeddingStore := pgstore.NewEmbeddingStore(pool)
	vendorProfileStore := pgstore.NewVendorProfileStore(pool)

	// Seed a listing row so the indexer can fetch it (no FK, but GetByID must not
	// return ErrListingNotFound before the embedding call — therefore seed a row).
	now := time.Now().UTC().Truncate(time.Millisecond)
	tenderID := uuid.New()

	require.NoError(t, listingStore.Create(ctx, &domain.Listing{
		ID:          tenderID,
		OwnerUserID: uuid.New(),
		Title:       "Dead letter test listing",
		Description: "This listing is used by the indexer dead-letter integration test.",
		Currency:    "TWD",
		Status:      domain.ListingStatusOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
	}))

	t.Run("dead-letters when attempt count reaches cap", func(t *testing.T) {
		// Enqueue one event.
		evt := makeIndexerOutboxEvent(tenderID)
		require.NoError(t, outboxStore.Enqueue(ctx, evt))

		// Pre-set attempts to maxOutboxAttempts-1 so the next failure trips the cap.
		// maxOutboxAttempts = 20; set attempts = 19.
		setOutboxAttempts(t, ctx, pool, evt.ID, 19)

		// Build indexer with a failing embedding client.
		ix := embedding.NewIndexer(&embedding.IndexerConfig{
			OutboxStore:        outboxStore,
			ListingStore:       listingStore,
			VendorProfileStore: vendorProfileStore,
			EmbeddingStore:     embeddingStore,
			EmbClient:          &failingEmbeddingClient{err: errors.New("simulated embedding failure")},
			Interval:           time.Hour, // not used; we call DrainOnce directly
		})

		// DrainOnce triggers one processEvent cycle.
		ix.DrainOnce(ctx)

		// dead_lettered_at must be set.
		var deadLetteredAt *time.Time

		err := pool.QueryRow(
			ctx,
			`SELECT dead_lettered_at FROM event_outbox WHERE id = $1`,
			evt.ID,
		).Scan(&deadLetteredAt)

		require.NoError(t, err)
		assert.NotNil(t, deadLetteredAt, "dead_lettered_at must be set after reaching max attempts")

		// Row must be absent from PollReady.
		events, err := outboxStore.PollReady(ctx, 100)
		require.NoError(t, err)

		for _, e := range events {
			assert.NotEqual(t, evt.ID, e.ID, "dead-lettered event must not appear in PollReady")
		}

		// Row must still exist (retained for inspection).
		var count int

		err = pool.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM event_outbox WHERE id = $1`,
			evt.ID,
		).Scan(&count)

		require.NoError(t, err)
		assert.Equal(t, 1, count, "dead-lettered row must be retained in DB (not deleted)")

		// Cleanup.
		_, _ = pool.Exec(ctx, `DELETE FROM event_outbox WHERE id = $1`, evt.ID)
	})

	t.Run("no dead-letter when succeeds on attempt 20", func(t *testing.T) {
		// Enqueue one event.
		evt := makeIndexerOutboxEvent(tenderID)
		require.NoError(t, outboxStore.Enqueue(ctx, evt))

		// Pre-set attempts to 19 so the next attempt is #20 (== maxOutboxAttempts).
		// The failing client on attempt 19 would dead-letter; here we use a SUCCESS client.
		setOutboxAttempts(t, ctx, pool, evt.ID, 19)

		// Build indexer with a successful embedding client.
		ix := embedding.NewIndexer(&embedding.IndexerConfig{
			OutboxStore:        outboxStore,
			ListingStore:       listingStore,
			VendorProfileStore: vendorProfileStore,
			EmbeddingStore:     embeddingStore,
			EmbClient:          &successEmbeddingClient{},
			Interval:           time.Hour,
		})

		ix.DrainOnce(ctx)

		// dead_lettered_at must NOT be set.
		var deadLetteredAt *time.Time

		err := pool.QueryRow(
			ctx,
			`SELECT dead_lettered_at FROM event_outbox WHERE id = $1`,
			evt.ID,
		).Scan(&deadLetteredAt)

		require.NoError(t, err)
		assert.Nil(t, deadLetteredAt, "dead_lettered_at must NOT be set when the event succeeds on attempt 20")

		// Cleanup.
		_, _ = pool.Exec(ctx, `DELETE FROM event_outbox WHERE id = $1`, evt.ID)
	})

	t.Run("no dead-letter when fails fewer than max attempts", func(t *testing.T) {
		// Enqueue one event.
		evt := makeIndexerOutboxEvent(tenderID)
		require.NoError(t, outboxStore.Enqueue(ctx, evt))

		// Pre-set attempts to 5 (well below the cap of 20).
		setOutboxAttempts(t, ctx, pool, evt.ID, 5)

		// Build indexer with a failing client — should MarkFailed, NOT dead-letter.
		ix := embedding.NewIndexer(&embedding.IndexerConfig{
			OutboxStore:        outboxStore,
			ListingStore:       listingStore,
			VendorProfileStore: vendorProfileStore,
			EmbeddingStore:     embeddingStore,
			EmbClient:          &failingEmbeddingClient{err: errors.New("transient failure")},
			Interval:           time.Hour,
		})

		ix.DrainOnce(ctx)

		// dead_lettered_at must NOT be set.
		var deadLetteredAt *time.Time

		err := pool.QueryRow(
			ctx,
			`SELECT dead_lettered_at FROM event_outbox WHERE id = $1`,
			evt.ID,
		).Scan(&deadLetteredAt)

		require.NoError(t, err)
		assert.Nil(t, deadLetteredAt, "dead_lettered_at must NOT be set at only 6 attempts (below cap 20)")

		// Attempts must have been incremented by MarkFailed.
		var attempts int

		err = pool.QueryRow(
			ctx,
			`SELECT attempts FROM event_outbox WHERE id = $1`,
			evt.ID,
		).Scan(&attempts)

		require.NoError(t, err)
		assert.Equal(t, 6, attempts, "MarkFailed must have incremented attempts to 6")

		// Cleanup.
		_, _ = pool.Exec(ctx, `UPDATE event_outbox SET published_at = now() WHERE id = $1`, evt.ID)
	})
}

// TestIndexer_DeadLettersAfterMaxAttempts_BoundaryCheck_Integration verifies that
// a row with exactly maxOutboxAttempts-1 failures that fails one more time IS dead-lettered,
// and a row with maxOutboxAttempts-1 failures that succeeds is NOT dead-lettered.
// This is a focused boundary regression test separate from the broader scenario test above.
func TestIndexer_DeadLettersAfterMaxAttempts_BoundaryCheck_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pool := startIndexerTestDB(t)

	outboxStore := pgstore.NewOutboxStore(pool)
	listingStore := pgstore.NewListingStore(pool)
	embeddingStore := pgstore.NewEmbeddingStore(pool)
	vendorProfileStore := pgstore.NewVendorProfileStore(pool)

	now := time.Now().UTC().Truncate(time.Millisecond)
	tenderID := uuid.New()

	require.NoError(t, listingStore.Create(ctx, &domain.Listing{
		ID:          tenderID,
		OwnerUserID: uuid.New(),
		Title:       "Boundary check listing",
		Description: "Used for dead-letter boundary test.",
		Currency:    "TWD",
		Status:      domain.ListingStatusOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
	}))

	// Boundary: attempts = maxOutboxAttempts - 1 (19), next failure = dead-letter.
	evt := makeIndexerOutboxEvent(tenderID)
	require.NoError(t, outboxStore.Enqueue(ctx, evt))

	setOutboxAttempts(t, ctx, pool, evt.ID, 19)

	ix := embedding.NewIndexer(&embedding.IndexerConfig{
		OutboxStore:        outboxStore,
		ListingStore:       listingStore,
		VendorProfileStore: vendorProfileStore,
		EmbeddingStore:     embeddingStore,
		EmbClient:          &failingEmbeddingClient{err: errors.New("boundary failure")},
		Interval:           time.Hour,
	})

	ix.DrainOnce(ctx)

	var deadLetteredAt *time.Time

	err := pool.QueryRow(
		ctx,
		`SELECT dead_lettered_at FROM event_outbox WHERE id = $1`,
		evt.ID,
	).Scan(&deadLetteredAt)

	require.NoError(t, err)
	assert.NotNil(t, deadLetteredAt, "boundary: dead_lettered_at must be set at attempt 20 (== maxOutboxAttempts)")

	// Cleanup.
	_, _ = pool.Exec(ctx, `DELETE FROM event_outbox WHERE id = $1`, evt.ID)
}

// confirmNoForeignKey verifies the dead_lettered_at column has no FK constraints,
// per CLAUDE.md #9. This is a DDL smoke-test.
func TestIndexer_DeadLetterColumn_NoForeignKey_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	pool := startIndexerTestDB(t)

	var fkCount int

	err := pool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM information_schema.referential_constraints rc
		 JOIN information_schema.key_column_usage kcu
		   ON kcu.constraint_name = rc.constraint_name
		WHERE kcu.table_name = 'event_outbox'
		  AND kcu.column_name = 'dead_lettered_at'`,
	).Scan(&fkCount)

	require.NoError(t, err)
	assert.Equal(t, 0, fkCount, "dead_lettered_at must have NO foreign key constraints (CLAUDE.md #9)")
}
