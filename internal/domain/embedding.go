package domain

import (
	"time"

	"github.com/google/uuid"
)

// EmbeddingEntityType restricts the entity_type column to known values.
type EmbeddingEntityType string

const (
	// EmbeddingEntityTypeTender identifies a tender listing entity.
	EmbeddingEntityTypeTender EmbeddingEntityType = "tender"
	// EmbeddingEntityTypeVendor identifies a vendor entity.
	EmbeddingEntityTypeVendor EmbeddingEntityType = "vendor"
)

// Embedding holds a 1536-dimension vector embedding for a marketplace entity.
// entity_id is a soft reference (no FK) — integrity enforced in the service layer.
type Embedding struct {
	ID           uuid.UUID
	EntityType   EmbeddingEntityType
	EntityID     uuid.UUID
	Embedding    []float32
	ModelVersion string
	CreatedAt    time.Time
}
