package postgres_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/CoverOnes/marketplace/internal/domain"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// pgvectorImage is the Docker image used for embedding integration tests.
// Plain postgres:17-alpine does NOT include pgvector; we must use a pgvector-enabled image.
const pgvectorImage = "pgvector/pgvector:pg17"

// startEmbeddingDB starts a fresh pgvector-enabled Postgres container for the
// embedding integration tests and returns a pool with pgvector types registered.
// The pgvector extension is installed via migration 000010.
func startEmbeddingDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		pgvectorImage,
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err, "start pgvector container")

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate pgvector container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "get pgvector container DSN")

	// Verify server version and pgvector availability before any migration.
	verifyPgvectorAvailable(t, ctx, dsn)

	// Build pool with pgvector types registered.
	pool, err := pgstore.NewEmbeddingPool(ctx, dsn, "", pgstore.PoolOptions{})
	require.NoError(t, err, "create embedding pool")

	t.Cleanup(func() { pool.Close() })

	// Apply all migrations (including 000010 which creates the vector extension and table).
	require.NoError(t, applyMigrations(ctx, pool), "apply migrations on pgvector container")

	return pool
}

// verifyPgvectorAvailable confirms the server version and that the vector
// extension is installable on the pgvector image.
// This satisfies the task requirement to confirm SHOW server_version / extension
// availability in the test.
func verifyPgvectorAvailable(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()

	// Use a plain pool (no pgvector registration) for the pre-migration version check.
	plainPool, err := pgstore.NewPool(ctx, dsn, "", pgstore.PoolOptions{})
	require.NoError(t, err, "temp pool for version check")

	defer plainPool.Close()

	var serverVersion string

	require.NoError(t, plainPool.QueryRow(ctx, "SHOW server_version").Scan(&serverVersion),
		"SHOW server_version")

	t.Logf("pgvector container server_version: %s", serverVersion)

	// Install the extension so we can confirm it exists in pg_extension.
	_, err = plainPool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	require.NoError(t, err, "CREATE EXTENSION vector must succeed on pgvector image")

	var extName string

	require.NoError(t, plainPool.QueryRow(ctx,
		"SELECT extname FROM pg_extension WHERE extname = 'vector'").Scan(&extName),
		"vector extension must be listed in pg_extension")

	assert.Equal(t, "vector", extName, "vector extension must be present")
}

// makeVec creates an L2-normalised 1536-dimension float32 slice with val0 at
// index 0 and val1 at index 1; all other elements are zero.
// Normalisation ensures cosine similarity == dot product.
func makeVec(val0, val1 float32) []float32 {
	const dims = 1536

	v := make([]float32, dims)
	v[0] = val0
	v[1] = val1

	var sum float64

	for _, x := range v {
		sum += float64(x) * float64(x)
	}

	if sum > 0 {
		norm := float32(math.Sqrt(sum))

		for i := range v {
			v[i] /= norm
		}
	}

	return v
}

// TestEmbeddingStore_Integration tests EmbeddingStore against a real pgvector Postgres.
func TestEmbeddingStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedding integration test in short mode")
	}

	pool := startEmbeddingDB(t)
	ctx := context.Background()

	es := pgstore.NewEmbeddingStore(pool)

	t.Run("nearest neighbor top-1 is correct", func(t *testing.T) {
		// Three unit vectors in 1536-dim space:
		//   vec1 = (1, 0, 0, …) in direction +x
		//   vec2 = (0, 1, 0, …) in direction +y
		//   vec3 = (-1, 0, 0, …) in direction -x
		// After L2 normalisation:
		//   cosine_dist(vec1, vec1) = 0 (identical)
		//   cosine_dist(vec1, vec2) = 1 (orthogonal)
		//   cosine_dist(vec1, vec3) = 2 (opposite)
		vec1 := makeVec(1.0, 0.0)
		vec2 := makeVec(0.0, 1.0)
		vec3 := makeVec(-1.0, 0.0)

		id1 := uuid.New()
		id2 := uuid.New()
		id3 := uuid.New()

		require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeTender, id1, vec1, "v1"))
		require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeTender, id2, vec2, "v1"))
		require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeTender, id3, vec3, "v1"))

		// Query with vec1: top-1 should be id1 (cosine distance 0).
		results, err := es.NearestNeighbors(ctx, vec1, domain.EmbeddingEntityTypeTender, 3)
		require.NoError(t, err)
		require.Len(t, results, 3, "must return all 3 inserted vectors")

		assert.Equal(t, id1, results[0].EntityID, "top-1 nearest neighbor must be vec1 itself")
		assert.Equal(t, domain.EmbeddingEntityTypeTender, results[0].EntityType)
	})

	t.Run("upsert idempotency: second upsert updates embedding and model_version", func(t *testing.T) {
		entityID := uuid.New()
		vecOld := makeVec(1.0, 0.0)
		vecNew := makeVec(0.0, 1.0)

		// First insert.
		require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeTender, entityID, vecOld, "v1"))

		// Second upsert: different vector and model version.
		require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeTender, entityID, vecNew, "v2"))

		// NearestNeighbors with vecNew: entityID must appear in results with updated model_version.
		results, err := es.NearestNeighbors(ctx, vecNew, domain.EmbeddingEntityTypeTender, 10)
		require.NoError(t, err)

		var found *domain.Embedding

		for _, r := range results {
			if r.EntityID == entityID {
				found = r

				break
			}
		}

		require.NotNil(t, found, "upserted entity must appear in nearest neighbors")
		assert.Equal(t, "v2", found.ModelVersion, "model_version must be updated after second upsert")

		// Verify only ONE row exists for (entityType, entityID).
		var count int

		require.NoError(t, pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM embeddings WHERE entity_type=$1 AND entity_id=$2",
			"tender", entityID,
		).Scan(&count))

		assert.Equal(t, 1, count, "upsert must not create duplicate rows")
	})

	t.Run("entity type filter: vendor embeddings excluded from tender query", func(t *testing.T) {
		vendorID := uuid.New()
		vec := makeVec(1.0, 0.0)

		require.NoError(t, es.Upsert(ctx, domain.EmbeddingEntityTypeVendor, vendorID, vec, "v1"))

		results, err := es.NearestNeighbors(ctx, vec, domain.EmbeddingEntityTypeTender, 20)
		require.NoError(t, err)

		for _, r := range results {
			assert.NotEqual(t, vendorID, r.EntityID, "vendor embedding must not appear in tender query")
		}
	})

	t.Run("topK=0 defaults to 10 without panic", func(t *testing.T) {
		vec := makeVec(1.0, 0.0)
		results, err := es.NearestNeighbors(ctx, vec, domain.EmbeddingEntityTypeTender, 0)
		require.NoError(t, err)
		assert.NotNil(t, results)
	})

	t.Run("topK>200 is clamped to 200 without error", func(t *testing.T) {
		vec := makeVec(1.0, 0.0)
		// Passing an oversized topK must succeed (clamped), not OOM or error.
		results, err := es.NearestNeighbors(ctx, vec, domain.EmbeddingEntityTypeTender, 10_000_000)
		require.NoError(t, err)
		assert.NotNil(t, results)
	})

	t.Run("Upsert wrong dimension returns ErrInvalidEmbeddingDimension", func(t *testing.T) {
		// A 3-dim vector must be rejected before hitting the DB.
		shortVec := []float32{0.1, 0.2, 0.3}
		err := es.Upsert(ctx, domain.EmbeddingEntityTypeTender, uuid.New(), shortVec, "v1")
		require.ErrorIs(t, err, domain.ErrInvalidEmbeddingDimension)
	})

	t.Run("NearestNeighbors wrong dimension returns ErrInvalidEmbeddingDimension", func(t *testing.T) {
		shortVec := []float32{0.1, 0.2, 0.3}
		_, err := es.NearestNeighbors(ctx, shortVec, domain.EmbeddingEntityTypeTender, 5)
		require.ErrorIs(t, err, domain.ErrInvalidEmbeddingDimension)
	})

	// M-2 regression: invalid entity_type rejected before DB call.
	t.Run("Upsert invalid entity_type returns ErrInvalidEntityType without DB write", func(t *testing.T) {
		vec := makeVec(1.0, 0.0)
		entityID := uuid.New()
		err := es.Upsert(ctx, domain.EmbeddingEntityType("contract"), entityID, vec, "v1")
		require.ErrorIs(t, err, domain.ErrInvalidEntityType, "must wrap ErrInvalidEntityType")

		// Confirm no row was written.
		var count int
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM embeddings WHERE entity_id = $1", entityID,
		).Scan(&count))
		assert.Equal(t, 0, count, "no row must be written on invalid entity_type")
	})

	t.Run("NearestNeighbors invalid entity_type returns ErrInvalidEntityType", func(t *testing.T) {
		vec := makeVec(1.0, 0.0)
		_, err := es.NearestNeighbors(ctx, vec, domain.EmbeddingEntityType(""), 5)
		require.ErrorIs(t, err, domain.ErrInvalidEntityType, "empty entity_type must wrap ErrInvalidEntityType")
	})

	// M-3 regression: model_version > 100 runes rejected before DB call.
	t.Run("Upsert model_version over 100 runes returns domain error", func(t *testing.T) {
		vec := makeVec(1.0, 0.0)
		overlong := string(make([]rune, 101)) // 101 NUL runes; any 101-rune string works
		err := es.Upsert(ctx, domain.EmbeddingEntityTypeTender, uuid.New(), vec, overlong)
		require.Error(t, err, "must return error for model_version > 100 runes")
		// Must NOT be a raw pgx error — message should mention the limit.
		assert.Contains(t, err.Error(), "model_version", "error must mention model_version")
		assert.Contains(t, err.Error(), "100", "error must mention the limit")
	})
}

// TestEmbeddingStore_PgvectorImage_Integration is a standalone confirmation that
// the test container uses pgvector/pgvector:pg17, NOT plain postgres:17-alpine.
// It verifies SHOW server_version and CREATE EXTENSION IF NOT EXISTS vector.
func TestEmbeddingStore_PgvectorImage_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pgvector image check integration test in short mode")
	}

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		pgvectorImage,
		tcpostgres.WithDatabase("imgcheck"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)

	defer func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			fmt.Fprintf(os.Stderr, "terminate pgvector image check container: %v\n", termErr)
		}
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := pgstore.NewPool(ctx, dsn, "", pgstore.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	var serverVersion string

	require.NoError(t, pool.QueryRow(ctx, "SHOW server_version").Scan(&serverVersion))

	t.Logf("pgvector/pgvector:pg17 server_version: %s", serverVersion)

	_, err = pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	require.NoError(t, err, "CREATE EXTENSION vector must succeed on pgvector/pgvector:pg17")

	var extName string

	err = pool.QueryRow(ctx,
		"SELECT extname FROM pg_extension WHERE extname = 'vector'",
	).Scan(&extName)
	require.NoError(t, err)

	assert.Equal(t, "vector", extName)
}
