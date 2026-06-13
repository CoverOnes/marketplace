package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// EmbeddingStore is a pool-backed embedding store.
// It satisfies store.EmbeddingStore.
type EmbeddingStore struct {
	q querier
}

// NewEmbeddingStore returns an EmbeddingStore backed by pool.
// The pool MUST have pgvector types registered in its AfterConnect hook
// (see NewEmbeddingPool or RegisterEmbeddingTypes).
func NewEmbeddingStore(pool *pgxpool.Pool) *EmbeddingStore {
	return &EmbeddingStore{q: pool}
}

// compile-time interface check.
var _ store.EmbeddingStore = (*EmbeddingStore)(nil)

// Upsert inserts or updates the embedding for (entityType, entityID).
// On conflict on (entity_type, entity_id) the embedding and model_version columns
// are replaced; created_at from the original row is preserved.
func (s *EmbeddingStore) Upsert(
	ctx context.Context,
	entityType domain.EmbeddingEntityType,
	entityID uuid.UUID,
	embedding []float32,
	modelVersion string,
) error {
	return upsertEmbedding(ctx, s.q, entityType, entityID, embedding, modelVersion)
}

// NearestNeighbors returns up to topK embeddings ordered by ascending cosine
// distance (most-similar first). The query uses the <=> operator (cosine distance)
// with the HNSW index.
func (s *EmbeddingStore) NearestNeighbors(
	ctx context.Context,
	queryVec []float32,
	entityType domain.EmbeddingEntityType,
	topK int,
) ([]*domain.Embedding, error) {
	return nearestNeighborEmbeddings(ctx, s.q, queryVec, entityType, topK)
}

// --- helpers ---

// embeddingDimensions is the expected vector length for all embeddings.
// It matches the vector(1536) column definition in migration 000010.
const embeddingDimensions = 1536

// maxModelVersionLen is the maximum rune count for model_version.
// It MUST match the CHECK (char_length(model_version) <= 100) constraint in
// migration 000010. Go-level validation prevents raw pgx check_violation (§5.2).
const maxModelVersionLen = 100

func upsertEmbedding(
	ctx context.Context,
	q querier,
	entityType domain.EmbeddingEntityType,
	entityID uuid.UUID,
	embedding []float32,
	modelVersion string,
) error {
	// M-2: validate entity_type before any DB call.
	if !entityType.IsValid() {
		return fmt.Errorf("upsert embedding: %w %q", domain.ErrInvalidEntityType, entityType)
	}

	// M-3: validate model_version length before DB round-trip.
	if utf8.RuneCountInString(modelVersion) > maxModelVersionLen {
		return fmt.Errorf("upsert embedding: model_version exceeds %d rune limit (got %d)",
			maxModelVersionLen, utf8.RuneCountInString(modelVersion))
	}

	if len(embedding) != embeddingDimensions {
		return fmt.Errorf("upsert embedding: %w (got %d)", domain.ErrInvalidEmbeddingDimension, len(embedding))
	}

	const query = `
INSERT INTO embeddings (entity_type, entity_id, embedding, model_version, created_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (entity_type, entity_id) DO UPDATE
    SET embedding     = EXCLUDED.embedding,
        model_version = EXCLUDED.model_version
`

	_, err := q.Exec(
		ctx, query,
		string(entityType),
		entityID,
		pgvector.NewVector(embedding),
		modelVersion,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("upsert embedding: %w", err)
	}

	return nil
}

func nearestNeighborEmbeddings(
	ctx context.Context,
	q querier,
	queryVec []float32,
	entityType domain.EmbeddingEntityType,
	topK int,
) ([]*domain.Embedding, error) {
	// M-2: validate entity_type before any DB call.
	if !entityType.IsValid() {
		return nil, fmt.Errorf("nearest neighbors: %w %q", domain.ErrInvalidEntityType, entityType)
	}

	if len(queryVec) != embeddingDimensions {
		return nil, fmt.Errorf("nearest neighbors: %w (got %d)", domain.ErrInvalidEmbeddingDimension, len(queryVec))
	}

	const maxTopK = 200

	if topK <= 0 {
		topK = 10
	}

	if topK > maxTopK {
		topK = maxTopK
	}

	const query = `
SELECT id, entity_type, entity_id, embedding, model_version, created_at
FROM embeddings
WHERE entity_type = $1
ORDER BY embedding <=> $2
LIMIT $3
`

	rows, err := q.Query(ctx, query, string(entityType), pgvector.NewVector(queryVec), topK)
	if err != nil {
		return nil, fmt.Errorf("nearest neighbors query: %w", err)
	}

	defer rows.Close()

	var results []*domain.Embedding

	for rows.Next() {
		e, scanErr := scanEmbedding(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		results = append(results, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate embeddings: %w", err)
	}

	return results, nil
}

func scanEmbedding(row rowScanner) (*domain.Embedding, error) {
	var (
		e   domain.Embedding
		et  string
		vec pgvector.Vector
	)

	if err := row.Scan(&e.ID, &et, &e.EntityID, &vec, &e.ModelVersion, &e.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}

		return nil, fmt.Errorf("scan embedding: %w", err)
	}

	e.EntityType = domain.EmbeddingEntityType(et)
	e.Vector = vec.Slice()

	return &e, nil
}
