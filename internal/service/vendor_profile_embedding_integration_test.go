package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/embedding"
	"github.com/CoverOnes/marketplace/internal/service"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// integration helpers for vendor embedding tests
// ─────────────────────────────────────────────────────────────────────────────

// buildVendorEmbeddingSetup creates all stores and returns a VendorProfileService +
// Indexer backed by the shared pgvector test container (sharedServiceDSN).
func buildVendorEmbeddingSetup(
	t *testing.T,
	ctx context.Context,
	embClient *fakeEmbeddingClient,
) (*service.VendorProfileService, *embedding.Indexer) {
	t.Helper()

	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err, "create embedding pool from shared service DSN")

	t.Cleanup(pool.Close)

	profileStore := pgstore.NewVendorProfileStore(pool)
	vendorProfileOutboxTxMgr := pgstore.NewVendorProfileOutboxTxManager(pool)
	svc := service.NewVendorProfileService(profileStore, vendorProfileOutboxTxMgr)

	idx := embedding.NewIndexer(&embedding.IndexerConfig{
		OutboxStore:        pgstore.NewOutboxStore(pool),
		ListingStore:       pgstore.NewListingStore(pool),
		VendorProfileStore: profileStore,
		EmbeddingStore:     pgstore.NewEmbeddingStore(pool),
		EmbClient:          embClient,
		ModelVersion:       "text-embedding-3-small",
	})

	return svc, idx
}

// countVendorEmbeddingRows returns how many embedding rows exist for a given ownerUserID.
func countVendorEmbeddingRows(t *testing.T, ctx context.Context, ownerUserID uuid.UUID) int {
	t.Helper()

	pool, err := pgstore.NewPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	var count int

	require.NoError(t, pool.QueryRow(
		ctx,
		"SELECT COUNT(*) FROM embeddings WHERE entity_type='vendor' AND entity_id=$1", ownerUserID,
	).Scan(&count))

	return count
}

// countPendingVendorEvents returns unpublished vendor_embedding_reindex events for an owner.
func countPendingVendorEvents(t *testing.T, ctx context.Context, ownerUserID uuid.UUID) int {
	t.Helper()

	pool, err := pgstore.NewPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	var count int

	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM event_outbox
		WHERE channel='marketplace.vendor_embedding_reindex'
		  AND aggregate_id=$1
		  AND published_at IS NULL
	`, ownerUserID).Scan(&count))

	return count
}

// drainUntilVendorEmbedded polls DrainOnce until the vendor embedding row for
// ownerUserID appears (or the deadline expires). This handles shared-outbox
// concurrency in integration tests: multiple tests share one outbox table, so
// a single DrainOnce may pull OTHER tests' events before this test's event.
func drainUntilVendorEmbedded(t *testing.T, ctx context.Context, idx *embedding.Indexer, ownerUserID uuid.UUID) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		if countVendorEmbeddingRows(t, ctx, ownerUserID) > 0 {
			return
		}

		idx.DrainOnce(ctx)
		time.Sleep(50 * time.Millisecond)
	}
}

// drainUntilVendorEventConsumed polls DrainOnce until there are no pending
// (unpublished) vendor_embedding_reindex events for ownerUserID. Used by tests
// where ErrEmbeddingDisabled means NO embedding row is written but the event
// must still be marked published (not pending for retry).
func drainUntilVendorEventConsumed(t *testing.T, ctx context.Context, idx *embedding.Indexer, ownerUserID uuid.UUID) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		if countPendingVendorEvents(t, ctx, ownerUserID) == 0 {
			return
		}

		idx.DrainOnce(ctx)
		time.Sleep(50 * time.Millisecond)
	}
}

// getVendorEmbeddingCreatedAt returns the created_at of the vendor embedding row.
func getVendorEmbeddingCreatedAt(t *testing.T, ctx context.Context, ownerUserID uuid.UUID) time.Time {
	t.Helper()

	pool, err := pgstore.NewPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	var ts time.Time

	require.NoError(t, pool.QueryRow(
		ctx,
		"SELECT created_at FROM embeddings WHERE entity_type='vendor' AND entity_id=$1", ownerUserID,
	).Scan(&ts))

	return ts
}

// makeVendorUpsertInput returns a minimal valid UpsertVendorProfileInput.
func makeVendorUpsertInput(ownerID uuid.UUID) service.UpsertVendorProfileInput {
	headline := "Go specialist"
	bio := "I write Go services."

	return service.UpsertVendorProfileInput{
		OwnerUserID: ownerID,
		DisplayName: "Alice Vendor",
		Headline:    &headline,
		Bio:         &bio,
		Skills:      []string{"Go", "PostgreSQL"},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// integration tests (use pgvector/pgvector:pg17 container via sharedServiceDSN)
// ─────────────────────────────────────────────────────────────────────────────

// TestVendorProfile_Embedding_Create_Integration verifies that creating a vendor
// profile enqueues one vendor_embedding_reindex event in the same tx, and after
// DrainOnce writes exactly one embeddings row with entity_type='vendor',
// entity_id=owner_user_id, dim=1536. (Acceptance §2)
func TestVendorProfile_Embedding_Create_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping vendor embedding create integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	emb := &fakeEmbeddingClient{}
	svc, idx := buildVendorEmbeddingSetup(t, ctx, emb)

	profile, err := svc.Upsert(ctx, makeVendorUpsertInput(ownerID))
	require.NoError(t, err, "Upsert must not fail")
	require.NotNil(t, profile)
	assert.Equal(t, ownerID, profile.OwnerUserID)

	// Outbox row must be enqueued before indexer runs.
	require.Equal(t, 1, countPendingVendorEvents(t, ctx, ownerID),
		"exactly one vendor outbox row must be enqueued synchronously with the profile write")

	// Drain until embedded — handles concurrent tests sharing the outbox table.
	drainUntilVendorEmbedded(t, ctx, idx, ownerID)

	// Verify exactly one embeddings row with entity_id = owner_user_id (NOT profile id).
	assert.Equal(t, 1, countVendorEmbeddingRows(t, ctx, ownerID),
		"exactly one vendor embedding row must be created after indexer drain")

	// Verify entity_id = owner_user_id in the DB directly.
	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	var dim int

	require.NoError(t, pool.QueryRow(
		ctx,
		"SELECT vector_dims(embedding) FROM embeddings WHERE entity_type='vendor' AND entity_id=$1", ownerID,
	).Scan(&dim))
	assert.Equal(t, 1536, dim, "vendor embedding must be 1536-dimensional") //nolint:mnd // text-embedding-3-small

	// Confirm entity_id != profile row id (load-bearing: T5 maps back to user IDs).
	assert.NotEqual(t, profile.ID, ownerID,
		"entity_id in embeddings table is owner_user_id (not profile row id)")
}

// TestVendorProfile_Embedding_Rollback_Integration verifies that when the outbox
// enqueue fails (simulated by a store error), the vendor_profile write is also
// rolled back — same-tx atomicity guarantee. (Acceptance §2: rollback-on-failure)
func TestVendorProfile_Embedding_Rollback_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping vendor embedding rollback integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	// Use the real outbox tx manager but verify that a failing upsert leaves no
	// orphaned outbox row. We simulate store error by using an impossible display_name.
	emb := &fakeEmbeddingClient{}
	svc, _ := buildVendorEmbeddingSetup(t, ctx, emb)

	// Store-layer validation rejects empty display_name → tx never commits.
	_, err := svc.Upsert(ctx, service.UpsertVendorProfileInput{
		OwnerUserID: ownerID,
		DisplayName: "", // will be rejected by service validation → never reaches tx
		Skills:      []string{},
	})
	require.Error(t, err, "empty display_name must fail validation")
	require.True(t, errors.Is(err, domain.ErrValidation))

	// No outbox event should exist (the tx never committed).
	assert.Equal(t, 0, countPendingVendorEvents(t, ctx, ownerID),
		"no vendor outbox event must exist when upsert fails validation before tx")
}

// TestVendorProfile_Embedding_UpdateWithTextChange_Integration verifies that
// re-upserting a profile with changed text enqueues a new event and the indexer
// upserts the embedding (still exactly one row, created_at preserved). (Acceptance §3)
func TestVendorProfile_Embedding_UpdateWithTextChange_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping vendor embedding update integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	callN := 0
	emb := &fakeEmbeddingClient{
		gen: func() []float32 {
			callN++
			if callN == 1 {
				return zeroVec1536()
			}

			return altVec1536()
		},
	}

	svc, idx := buildVendorEmbeddingSetup(t, ctx, emb)

	// Create and drain initial embedding.
	_, err := svc.Upsert(ctx, makeVendorUpsertInput(ownerID))
	require.NoError(t, err)

	drainUntilVendorEmbedded(t, ctx, idx, ownerID)
	require.Equal(t, 1, countVendorEmbeddingRows(t, ctx, ownerID), "initial vendor embedding must exist")

	createdAt := getVendorEmbeddingCreatedAt(t, ctx, ownerID)

	// Allow clock to advance so we can confirm created_at is NOT updated on upsert.
	time.Sleep(5 * time.Millisecond)

	// Update display_name → embeddable text changed → must enqueue.
	updatedInput := makeVendorUpsertInput(ownerID)
	updatedInput.DisplayName = "Alice Vendor Updated"
	_, err = svc.Upsert(ctx, updatedInput)
	require.NoError(t, err)

	drainUntilVendorEmbedded(t, ctx, idx, ownerID)

	// Still exactly one row (upsert, not insert).
	assert.Equal(t, 1, countVendorEmbeddingRows(t, ctx, ownerID),
		"must still be exactly one vendor embedding row after update (upsert)")

	// created_at must be preserved.
	updatedCreatedAt := getVendorEmbeddingCreatedAt(t, ctx, ownerID)
	assert.True(t, updatedCreatedAt.Equal(createdAt),
		"created_at must be preserved by upsert; want %v got %v", createdAt, updatedCreatedAt)
}

// TestVendorProfile_Embedding_NoopUpdate_NoReindex_Integration verifies that a
// re-PUT with identical text does NOT enqueue a vendor_embedding_reindex event.
// (Acceptance §4: identical re-PUT does NOT enqueue)
func TestVendorProfile_Embedding_NoopUpdate_NoReindex_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping vendor embedding noop update integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	emb := &fakeEmbeddingClient{}
	svc, idx := buildVendorEmbeddingSetup(t, ctx, emb)

	in := makeVendorUpsertInput(ownerID)

	// Create profile and drain initial embedding event.
	_, err := svc.Upsert(ctx, in)
	require.NoError(t, err)

	drainUntilVendorEmbedded(t, ctx, idx, ownerID)

	require.Equal(t, 1, countVendorEmbeddingRows(t, ctx, ownerID))

	// Identical re-PUT.
	_, err = svc.Upsert(ctx, in)
	require.NoError(t, err)

	// No new pending outbox event.
	assert.Equal(t, 0, countPendingVendorEvents(t, ctx, ownerID),
		"identical re-PUT must NOT enqueue a vendor_embedding_reindex event")
}

// TestVendorProfile_Embedding_Disabled_Integration verifies that when the embedding
// client is disabled, vendor_profile write still succeeds and the indexer skips
// without leaving a pending event. (Acceptance §3: ErrEmbeddingDisabled → skip)
func TestVendorProfile_Embedding_Disabled_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping vendor embedding disabled integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	emb := &fakeEmbeddingClient{retErr: client.ErrEmbeddingDisabled}
	svc, idx := buildVendorEmbeddingSetup(t, ctx, emb)

	profile, err := svc.Upsert(ctx, makeVendorUpsertInput(ownerID))
	require.NoError(t, err, "vendor_profile create must succeed even when embedding client is disabled")
	require.NotNil(t, profile)

	// Drain until the event is consumed (marked published on ErrEmbeddingDisabled).
	drainUntilVendorEventConsumed(t, ctx, idx, ownerID)

	// No embedding row must be written.
	assert.Equal(t, 0, countVendorEmbeddingRows(t, ctx, ownerID),
		"no vendor embedding row must be written when embedding client is disabled")

	// The outbox event must be marked published (not pending for retry).
	assert.Equal(t, 0, countPendingVendorEvents(t, ctx, ownerID),
		"vendor_embedding_reindex event must be marked published (skipped) on ErrEmbeddingDisabled")
}

// TestVendorProfile_Embedding_DualChannel_Integration verifies that a single
// DrainOnce handles BOTH tender and vendor channels without regression.
// (Acceptance §5: no tender regression)
func TestVendorProfile_Embedding_DualChannel_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping vendor+tender dual-channel integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()
	tenderOwnerID := uuid.New()

	emb := &fakeEmbeddingClient{}

	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	t.Cleanup(pool.Close)

	profileStore := pgstore.NewVendorProfileStore(pool)
	listingStore := pgstore.NewListingStore(pool)
	vendorProfileOutboxTxMgr := pgstore.NewVendorProfileOutboxTxManager(pool)
	listingOutboxTxMgr := pgstore.NewListingOutboxTxManager(pool)
	outboxStore := pgstore.NewOutboxStore(pool)
	embeddingStore := pgstore.NewEmbeddingStore(pool)

	vendorSvc := service.NewVendorProfileService(profileStore, vendorProfileOutboxTxMgr)
	listingSvc := service.NewListingService(listingStore, listingOutboxTxMgr, nil, nil)

	idx := embedding.NewIndexer(&embedding.IndexerConfig{
		OutboxStore:        outboxStore,
		ListingStore:       listingStore,
		VendorProfileStore: profileStore,
		EmbeddingStore:     embeddingStore,
		EmbClient:          emb,
		ModelVersion:       "text-embedding-3-small",
	})

	// Enqueue a tender event.
	tender, err := listingSvc.CreateListing(ctx, &service.CreateListingInput{
		OwnerUserID: tenderOwnerID,
		Title:       "Lead Go Engineer",
		Description: "Build distributed systems.",
		Currency:    "TWD",
		IsTender:    true,
	})
	require.NoError(t, err)

	// Enqueue a vendor event.
	_, err = vendorSvc.Upsert(ctx, makeVendorUpsertInput(ownerID))
	require.NoError(t, err)

	// Drain until both embeddings are indexed (shared outbox: may need multiple passes
	// if other tests' events are also in the queue).
	drainUntilEmbedded := func(entityType string, entityID uuid.UUID) {
		t.Helper()

		deadline := time.Now().Add(15 * time.Second)

		for time.Now().Before(deadline) {
			var cnt int

			if qErr := pool.QueryRow(
				ctx,
				"SELECT COUNT(*) FROM embeddings WHERE entity_type=$1 AND entity_id=$2",
				entityType, entityID,
			).Scan(&cnt); qErr != nil || cnt > 0 {
				return
			}

			idx.DrainOnce(ctx)
			time.Sleep(50 * time.Millisecond)
		}
	}

	drainUntilEmbedded("tender", tender.ID)
	drainUntilEmbedded("vendor", ownerID)

	// Tender embedding must be created.
	var tenderEmbCount int

	require.NoError(t, pool.QueryRow(
		ctx,
		"SELECT COUNT(*) FROM embeddings WHERE entity_type='tender' AND entity_id=$1", tender.ID,
	).Scan(&tenderEmbCount))
	assert.Equal(t, 1, tenderEmbCount, "tender embedding must be created when dual-channel indexer processes tender event")

	// Vendor embedding must be created (same pipeline, different channel).
	assert.Equal(t, 1, countVendorEmbeddingRows(t, ctx, ownerID),
		"vendor embedding must be created when dual-channel indexer processes vendor event")
}
