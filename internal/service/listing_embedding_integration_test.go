package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/embedding"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// fake embedding client — deterministic stand-in for OpenRouter API
// ─────────────────────────────────────────────────────────────────────────────

// fakeEmbeddingClient returns a fixed 1536-dim vector to avoid real API calls
// while exercising the full upsert/retrieve path in integration tests.
type fakeEmbeddingClient struct {
	retErr error
	// gen is an optional override; if nil returns zeroVec1536().
	gen func() []float32
}

func (f *fakeEmbeddingClient) Generate(_ context.Context, _ string) ([]float32, error) {
	if f.retErr != nil {
		return nil, f.retErr
	}

	if f.gen != nil {
		return f.gen(), nil
	}

	return zeroVec1536(), nil
}

// zeroVec1536 returns a 1536-dim float32 slice with vec[0]=1, rest 0.
func zeroVec1536() []float32 {
	v := make([]float32, 1536) //nolint:mnd // 1536-dim matches text-embedding-3-small
	v[0] = 1.0

	return v
}

// altVec1536 returns a different 1536-dim normalised vector used to assert that
// re-embedding actually changes the stored value (acceptance §3).
func altVec1536() []float32 {
	v := make([]float32, 1536) //nolint:mnd // 1536-dim matches text-embedding-3-small
	v[1] = 1.0

	return v
}

// ─────────────────────────────────────────────────────────────────────────────
// stub listing outbox tx manager for pure-unit tests
// ─────────────────────────────────────────────────────────────────────────────

// stubListingOutboxTxManager mirrors stubOutboxTxManager (tender_service_test.go)
// for the listing-specific interface.
type stubListingOutboxTxManager struct {
	listings store.ListingStore
	outbox   store.OutboxStore
}

func (m *stubListingOutboxTxManager) WithListingOutboxTx(
	ctx context.Context,
	fn func(context.Context, store.ListingStore, store.OutboxStore) error,
) error {
	ob := m.outbox
	if ob == nil {
		ob = &noopTenderOutboxStore{} // noopTenderOutboxStore defined in tender_service_test.go
	}

	return fn(ctx, m.listings, ob)
}

// ─────────────────────────────────────────────────────────────────────────────
// integration helpers
// ─────────────────────────────────────────────────────────────────────────────

// buildListingEmbeddingSetup creates all stores and returns a ListingService +
// Indexer backed by the shared pgvector test container (sharedServiceDSN).
func buildListingEmbeddingSetup(
	t *testing.T,
	ctx context.Context,
	embClient *fakeEmbeddingClient,
) (*service.ListingService, *embedding.Indexer) {
	t.Helper()

	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err, "create embedding pool from shared service DSN")

	t.Cleanup(pool.Close)

	listingStore := pgstore.NewListingStore(pool)
	listingOutboxTxMgr := pgstore.NewListingOutboxTxManager(pool)
	svc := service.NewListingService(listingStore, listingOutboxTxMgr, nil, nil)

	idx := embedding.NewIndexer(&embedding.IndexerConfig{
		OutboxStore:    pgstore.NewOutboxStore(pool),
		ListingStore:   listingStore,
		EmbeddingStore: pgstore.NewEmbeddingStore(pool),
		EmbClient:      embClient,
		ModelVersion:   "text-embedding-3-small",
	})

	return svc, idx
}

// countEmbeddingRows returns how many embedding rows exist for a given tender ID.
func countEmbeddingRows(t *testing.T, ctx context.Context, tenderID uuid.UUID) int {
	t.Helper()

	pool, err := pgstore.NewPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM embeddings WHERE entity_type='tender' AND entity_id=$1", tenderID,
	).Scan(&count))

	return count
}

// countPendingEmbeddingEvents returns unpublished embedding_reindex events for a tender.
func countPendingEmbeddingEvents(t *testing.T, ctx context.Context, tenderID uuid.UUID) int {
	t.Helper()

	pool, err := pgstore.NewPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	var count int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM event_outbox
		WHERE channel='marketplace.embedding_reindex'
		  AND aggregate_id=$1
		  AND published_at IS NULL
	`, tenderID).Scan(&count))

	return count
}

// getEmbeddingCreatedAt returns the created_at of the sole embedding row for a tender.
func getEmbeddingCreatedAt(t *testing.T, ctx context.Context, tenderID uuid.UUID) time.Time {
	t.Helper()

	pool, err := pgstore.NewPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	var ts time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT created_at FROM embeddings WHERE entity_type='tender' AND entity_id=$1", tenderID,
	).Scan(&ts))

	return ts
}

// ─────────────────────────────────────────────────────────────────────────────
// integration tests  (use pgvector/pgvector:pg17 container via sharedServiceDSN)
// ─────────────────────────────────────────────────────────────────────────────

// TestListingService_Embedding_CreateTender_Integration verifies that creating a
// tender enqueues one embedding_reindex event and, after DrainOnce, writes exactly
// one embeddings row with dim=1536. (Acceptance §2)
func TestListingService_Embedding_CreateTender_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedding integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	emb := &fakeEmbeddingClient{}
	svc, idx := buildListingEmbeddingSetup(t, ctx, emb)

	listing, err := svc.CreateListing(ctx, &service.CreateListingInput{
		OwnerUserID: ownerID,
		Title:       "Lead Go Engineer",
		Description: "Build distributed systems.",
		Currency:    "TWD",
		IsTender:    true,
	})
	require.NoError(t, err, "CreateListing must not fail")
	require.NotNil(t, listing)

	// Outbox row must be enqueued before indexer runs.
	require.Equal(t, 1, countPendingEmbeddingEvents(t, ctx, listing.ID),
		"exactly one outbox row must be enqueued synchronously with the tender write")

	// Drain indexer.
	idx.DrainOnce(ctx)

	// Verify exactly one embeddings row.
	assert.Equal(t, 1, countEmbeddingRows(t, ctx, listing.ID),
		"exactly one embedding row must be created after indexer drain")

	// Verify dim = 1536 via vector_dims().
	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	var dim int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT vector_dims(embedding) FROM embeddings WHERE entity_type='tender' AND entity_id=$1", listing.ID,
	).Scan(&dim))
	assert.Equal(t, 1536, dim, "embedding must be 1536-dimensional") //nolint:mnd // text-embedding-3-small
}

// TestListingService_Embedding_UpdateTitle_Integration verifies that updating the
// title of a tender re-enqueues an embedding_reindex event, the indexer upserts the
// row (still exactly one), and created_at is preserved. (Acceptance §3)
func TestListingService_Embedding_UpdateTitle_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedding integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	// First call returns zeroVec; second returns altVec to verify vector changes.
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

	svc, idx := buildListingEmbeddingSetup(t, ctx, emb)

	// Create tender and drain initial embedding.
	listing, err := svc.CreateListing(ctx, &service.CreateListingInput{
		OwnerUserID: ownerID,
		Title:       "Backend Engineer",
		Description: "Go + Postgres.",
		Currency:    "TWD",
		IsTender:    true,
	})
	require.NoError(t, err)

	idx.DrainOnce(ctx)
	require.Equal(t, 1, countEmbeddingRows(t, ctx, listing.ID), "initial embedding must exist")

	createdAt := getEmbeddingCreatedAt(t, ctx, listing.ID)

	// Allow clock to advance so we can confirm created_at is NOT updated on upsert.
	time.Sleep(5 * time.Millisecond)

	// Update title → should enqueue a new reindex event.
	newTitle := "Senior Backend Engineer"
	_, err = svc.UpdateListing(ctx, service.UpdateListingInput{
		ID:       listing.ID,
		CallerID: ownerID,
		Title:    &newTitle,
	})
	require.NoError(t, err)

	// Drain again.
	idx.DrainOnce(ctx)

	// Still exactly one row (upsert, not insert).
	assert.Equal(t, 1, countEmbeddingRows(t, ctx, listing.ID),
		"must still be exactly one embedding row after update (upsert)")

	// created_at must be preserved by ON CONFLICT DO UPDATE.
	updatedCreatedAt := getEmbeddingCreatedAt(t, ctx, listing.ID)
	assert.True(t, updatedCreatedAt.Equal(createdAt),
		"created_at must be preserved by upsert; want %v got %v", createdAt, updatedCreatedAt)
}

// TestListingService_Embedding_BudgetUpdate_NoReindex_Integration verifies that a
// budget-only update does NOT enqueue an embedding_reindex event. (Acceptance §4)
func TestListingService_Embedding_BudgetUpdate_NoReindex_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedding integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	emb := &fakeEmbeddingClient{}
	svc, idx := buildListingEmbeddingSetup(t, ctx, emb)

	// Create tender (enqueues one create event).
	listing, err := svc.CreateListing(ctx, &service.CreateListingInput{
		OwnerUserID: ownerID,
		Title:       "Dev Needed",
		Description: "Solve hard problems.",
		Currency:    "TWD",
		IsTender:    true,
	})
	require.NoError(t, err)

	// Drain initial create event.
	idx.DrainOnce(ctx)

	// Budget-only update — title and description unchanged.
	budgetMin := decimal.NewFromInt(100)
	_, err = svc.UpdateListing(ctx, service.UpdateListingInput{
		ID:        listing.ID,
		CallerID:  ownerID,
		BudgetMin: &budgetMin,
	})
	require.NoError(t, err)

	// No new pending outbox event should exist after the budget update.
	assert.Equal(t, 0, countPendingEmbeddingEvents(t, ctx, listing.ID),
		"budget-only update must NOT enqueue an embedding_reindex event")
}

// TestListingService_Embedding_Disabled_Integration verifies that when the
// embedding client is disabled (ErrEmbeddingDisabled), tender creation still
// succeeds and no embeddings row is written. (Acceptance §5)
func TestListingService_Embedding_Disabled_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedding integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	// Client always returns ErrEmbeddingDisabled.
	emb := &fakeEmbeddingClient{retErr: client.ErrEmbeddingDisabled}
	svc, idx := buildListingEmbeddingSetup(t, ctx, emb)

	listing, err := svc.CreateListing(ctx, &service.CreateListingInput{
		OwnerUserID: ownerID,
		Title:       "Disabled Embedding Tender",
		Description: "Embedding API key not configured.",
		Currency:    "TWD",
		IsTender:    true,
	})
	require.NoError(t, err, "tender create must succeed even when embedding client is disabled")
	require.NotNil(t, listing)

	// Drain — indexer must skip gracefully on ErrEmbeddingDisabled.
	idx.DrainOnce(ctx)

	// No embedding row must be written.
	assert.Equal(t, 0, countEmbeddingRows(t, ctx, listing.ID),
		"no embedding row must be written when embedding client is disabled")

	// The outbox event must be marked published (not pending for retry).
	assert.Equal(t, 0, countPendingEmbeddingEvents(t, ctx, listing.ID),
		"embedding_reindex event must be marked published (skipped) on ErrEmbeddingDisabled, not retried")
}

// TestListingService_Embedding_NonTender_NoEnqueue verifies that creating a
// classic (non-tender) listing does NOT enqueue an embedding_reindex event.
func TestListingService_Embedding_NonTender_NoEnqueue(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedding integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	emb := &fakeEmbeddingClient{}
	svc, _ := buildListingEmbeddingSetup(t, ctx, emb)

	listing, err := svc.CreateListing(ctx, &service.CreateListingInput{
		OwnerUserID: ownerID,
		Title:       "Classic Listing",
		Description: "Not a tender.",
		Currency:    "TWD",
		IsTender:    false,
	})
	require.NoError(t, err)
	require.NotNil(t, listing)

	assert.Equal(t, 0, countPendingEmbeddingEvents(t, ctx, listing.ID),
		"classic (non-tender) listing must not enqueue an embedding_reindex event")
}

// ─────────────────────────────────────────────────────────────────────────────
// unit tests — outbox enqueue behavior (no DB required)
// ─────────────────────────────────────────────────────────────────────────────

// TestListingService_Embedding_CreateTender_EnqueuesOutbox_Unit verifies that
// CreateListing for a tender calls the outbox enqueue path synchronously.
func TestListingService_Embedding_CreateTender_EnqueuesOutbox_Unit(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	ls := newStubListingStore()
	ls.listings = make(map[uuid.UUID]*domain.Listing)
	ob := &recordingOutboxStore{}
	svc := service.NewListingService(ls, &stubListingOutboxTxManager{listings: ls, outbox: ob}, nil, nil)

	listing, err := svc.CreateListing(context.Background(), &service.CreateListingInput{
		OwnerUserID: ownerID,
		Title:       "Unit Test Tender",
		Description: "Ensure outbox is enqueued.",
		Currency:    "TWD",
		IsTender:    true,
	})
	require.NoError(t, err)
	require.NotNil(t, listing)

	events := ob.enqueuedEvents()
	require.Len(t, events, 1, "exactly one outbox event must be enqueued on tender create")
	assert.Equal(t, "marketplace.embedding_reindex", events[0].Channel)
}

// TestListingService_Embedding_CreateNonTender_NoEnqueue_Unit verifies that
// CreateListing for a non-tender listing does NOT call the outbox enqueue path.
func TestListingService_Embedding_CreateNonTender_NoEnqueue_Unit(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	ls := newStubListingStore()
	ls.listings = make(map[uuid.UUID]*domain.Listing)
	ob := &recordingOutboxStore{}
	svc := service.NewListingService(ls, &stubListingOutboxTxManager{listings: ls, outbox: ob}, nil, nil)

	listing, err := svc.CreateListing(context.Background(), &service.CreateListingInput{
		OwnerUserID: ownerID,
		Title:       "Classic Listing",
		Description: "Not a tender.",
		Currency:    "TWD",
		IsTender:    false,
	})
	require.NoError(t, err)
	require.NotNil(t, listing)

	assert.Empty(t, ob.enqueuedEvents(), "non-tender listing must not enqueue an embedding_reindex event")
}

// TestListingService_Embedding_UpdateTitle_EnqueuesOutbox_Unit verifies that
// updating the title of a tender calls the outbox enqueue path.
func TestListingService_Embedding_UpdateTitle_EnqueuesOutbox_Unit(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	tenderStatus := domain.TenderStatusOpen

	existingTender := &domain.Listing{
		ID:           uuid.New(),
		OwnerUserID:  ownerID,
		Title:        "Original Title",
		Description:  "Original description.",
		Currency:     "TWD",
		Status:       domain.ListingStatusOpen,
		IsTender:     true,
		TenderStatus: &tenderStatus,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	ls := newStubListingStore(existingTender)
	ob := &recordingOutboxStore{}
	svc := service.NewListingService(ls, &stubListingOutboxTxManager{listings: ls, outbox: ob}, nil, nil)

	newTitle := "Updated Title"
	_, err := svc.UpdateListing(context.Background(), service.UpdateListingInput{
		ID:       existingTender.ID,
		CallerID: ownerID,
		Title:    &newTitle,
	})
	require.NoError(t, err)

	events := ob.enqueuedEvents()
	require.Len(t, events, 1, "title update on tender must enqueue one embedding_reindex event")
	assert.Equal(t, "marketplace.embedding_reindex", events[0].Channel)
}

// TestListingService_Embedding_UpdateBudgetOnly_NoEnqueue_Unit verifies that a
// budget-only update on a tender does NOT enqueue an embedding_reindex event.
func TestListingService_Embedding_UpdateBudgetOnly_NoEnqueue_Unit(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	tenderStatus := domain.TenderStatusOpen

	existingTender := &domain.Listing{
		ID:           uuid.New(),
		OwnerUserID:  ownerID,
		Title:        "Original Title",
		Description:  "Original description.",
		Currency:     "TWD",
		Status:       domain.ListingStatusOpen,
		IsTender:     true,
		TenderStatus: &tenderStatus,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	ls := newStubListingStore(existingTender)
	ob := &recordingOutboxStore{}
	svc := service.NewListingService(ls, &stubListingOutboxTxManager{listings: ls, outbox: ob}, nil, nil)

	budgetMin := decimal.NewFromInt(100)
	_, err := svc.UpdateListing(context.Background(), service.UpdateListingInput{
		ID:        existingTender.ID,
		CallerID:  ownerID,
		BudgetMin: &budgetMin,
	})
	require.NoError(t, err)

	assert.Empty(t, ob.enqueuedEvents(), "budget-only update on tender must NOT enqueue an embedding_reindex event")
}

// TestListingService_Embedding_UpdateTitleOnNonTender_NoEnqueue_Unit verifies
// that updating the title on a non-tender listing does NOT enqueue an embedding event.
func TestListingService_Embedding_UpdateTitleOnNonTender_NoEnqueue_Unit(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	classicListing := &domain.Listing{
		ID:          uuid.New(),
		OwnerUserID: ownerID,
		Title:       "Classic Original",
		Description: "Non-tender.",
		Currency:    "TWD",
		Status:      domain.ListingStatusOpen,
		IsTender:    false,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	ls := newStubListingStore(classicListing)
	ob := &recordingOutboxStore{}
	svc := service.NewListingService(ls, &stubListingOutboxTxManager{listings: ls, outbox: ob}, nil, nil)

	newTitle := "Classic Updated"
	_, err := svc.UpdateListing(context.Background(), service.UpdateListingInput{
		ID:       classicListing.ID,
		CallerID: ownerID,
		Title:    &newTitle,
	})
	require.NoError(t, err)

	assert.Empty(t, ob.enqueuedEvents(), "title update on non-tender listing must NOT enqueue an embedding_reindex event")
}
