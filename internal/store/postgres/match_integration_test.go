package postgres_test

import (
	"context"
	"testing"

	"github.com/CoverOnes/marketplace/internal/domain"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetByEntityID_Integration tests EmbeddingStore.GetByEntityID against a real
// pgvector-enabled Postgres container (shared from TestMain).
func TestGetByEntityID_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedding integration test in short mode")
	}

	ctx := context.Background()

	// Use the shared embedding pool (pgvector codec registered).
	embPool := sharedTestPool
	es := pgstore.NewEmbeddingStore(embPool)

	t.Run("returns embedding when row exists", func(t *testing.T) {
		entityID := uuid.New()
		vec := makeVec(1.0, 0.0)

		require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeTender, entityID, vec, "v1"))

		got, err := es.GetByEntityID(ctx, domain.EmbeddingEntityTypeTender, entityID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, entityID, got.EntityID)
		assert.Equal(t, domain.EmbeddingEntityTypeTender, got.EntityType)
		assert.Equal(t, "v1", got.ModelVersion)
	})

	t.Run("returns ErrNotFound when no row exists", func(t *testing.T) {
		nonExistent := uuid.New()

		_, err := es.GetByEntityID(ctx, domain.EmbeddingEntityTypeTender, nonExistent)
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("returns ErrInvalidEntityType for invalid entity type", func(t *testing.T) {
		_, err := es.GetByEntityID(ctx, domain.EmbeddingEntityType("contract"), uuid.New())
		require.ErrorIs(t, err, domain.ErrInvalidEntityType)
	})
}

// TestNearestNeighborsWithDistance_Integration tests
// EmbeddingStore.NearestNeighborsWithDistance against a real Postgres container.
// It seeds a tender embedding and 3 vendor embeddings with known vectors so
// the cosine ordering is deterministic, then asserts distance ordering and owner exclusion.
func TestNearestNeighborsWithDistance_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedding integration test in short mode")
	}

	ctx := context.Background()
	es := pgstore.NewEmbeddingStore(sharedTestPool)

	// Tender vector: direction (1, 0, ...).
	tenderID := uuid.New()
	tenderVec := makeVec(1.0, 0.0)
	require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeTender, tenderID, tenderVec, "v1"))

	// Three vendor vectors with increasing cosine distance from tender:
	//   vendor1: (1, 0, ...) → distance ~0  (most similar)
	//   vendor2: (0, 1, ...) → distance ~1  (orthogonal)
	//   vendor3: (-1, 0, ...) → distance ~2 (opposite)
	vendor1ID := uuid.New()
	vendor2ID := uuid.New()
	vendor3ID := uuid.New()

	require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeVendor, vendor1ID, makeVec(1.0, 0.0), "v1"))
	require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeVendor, vendor2ID, makeVec(0.0, 1.0), "v1"))
	require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeVendor, vendor3ID, makeVec(-1.0, 0.0), "v1"))

	t.Run("results ordered by ascending cosine distance", func(t *testing.T) {
		results, err := es.NearestNeighborsWithDistance(ctx, tenderVec, domain.EmbeddingEntityTypeVendor, 10)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(results), 3, "must return at least 3 vendor embeddings")

		// Find our 3 vendors in the results.
		distMap := make(map[uuid.UUID]float32)

		for _, r := range results {
			distMap[r.EntityID] = r.CosineDistance
		}

		require.Contains(t, distMap, vendor1ID, "vendor1 must appear in results")
		require.Contains(t, distMap, vendor2ID, "vendor2 must appear in results")
		require.Contains(t, distMap, vendor3ID, "vendor3 must appear in results")

		// Assert distance ordering: vendor1 < vendor2 < vendor3.
		assert.Less(t, distMap[vendor1ID], distMap[vendor2ID],
			"vendor1 (same direction) must be closer than vendor2 (orthogonal)")
		assert.Less(t, distMap[vendor2ID], distMap[vendor3ID],
			"vendor2 (orthogonal) must be closer than vendor3 (opposite)")
	})

	t.Run("results are ordered ascending by distance in the returned slice", func(t *testing.T) {
		results, err := es.NearestNeighborsWithDistance(ctx, tenderVec, domain.EmbeddingEntityTypeVendor, 3)
		require.NoError(t, err)
		require.Len(t, results, 3)

		for i := 1; i < len(results); i++ {
			assert.LessOrEqual(t, results[i-1].CosineDistance, results[i].CosineDistance,
				"results must be in ascending cosine distance order (index %d)", i)
		}
	})

	t.Run("ErrInvalidEntityType for unknown entity type", func(t *testing.T) {
		_, err := es.NearestNeighborsWithDistance(ctx, tenderVec, domain.EmbeddingEntityType("bad"), 5)
		require.ErrorIs(t, err, domain.ErrInvalidEntityType)
	})

	t.Run("ErrInvalidEmbeddingDimension for wrong vector length", func(t *testing.T) {
		shortVec := []float32{0.1, 0.2, 0.3}
		_, err := es.NearestNeighborsWithDistance(ctx, shortVec, domain.EmbeddingEntityTypeVendor, 5)
		require.ErrorIs(t, err, domain.ErrInvalidEmbeddingDimension)
	})

	t.Run("topK=0 defaults to 10 without error", func(t *testing.T) {
		results, err := es.NearestNeighborsWithDistance(ctx, tenderVec, domain.EmbeddingEntityTypeVendor, 0)
		require.NoError(t, err)
		assert.NotNil(t, results)
	})

	t.Run("topK>200 clamped to 200 without error", func(t *testing.T) {
		results, err := es.NearestNeighborsWithDistance(ctx, tenderVec, domain.EmbeddingEntityTypeVendor, 10_000)
		require.NoError(t, err)
		assert.NotNil(t, results)
	})
}
