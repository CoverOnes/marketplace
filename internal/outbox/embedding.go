package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
)

// embeddingReindexChannel is the Redis pub/sub channel used for tender embedding
// reindex events. The indexer worker subscribes to rows on this channel via the
// event_outbox table (same-tx outbox pattern).
const embeddingReindexChannel = "marketplace.embedding_reindex"

// vendorEmbeddingReindexChannel is the separate channel for vendor profile
// embedding reindex events. Using a distinct channel (not overloading the tender
// channel) lets the indexer branch cleanly on evt.Channel without payload inspection.
const vendorEmbeddingReindexChannel = "marketplace.vendor_embedding_reindex"

// vendorEmbeddingReindexEvent is the JSON payload stored in event_outbox for a
// vendor profile embedding reindex. The indexer fetches the current profile by
// owner_user_id at processing time.
//
// IMPORTANT: entity_id for vendor embeddings is owner_user_id (NOT the
// vendor_profile row id). T5 NearestNeighbors results map back to user IDs.
type vendorEmbeddingReindexEvent struct {
	EventID    string    `json:"eventId"`
	OccurredAt time.Time `json:"occurredAt"`
	Version    int       `json:"version"`
	Data       struct {
		OwnerUserID uuid.UUID `json:"ownerUserId"`
	} `json:"data"`
}

// embeddingReindexEvent is the JSON payload stored in event_outbox for a tender
// embedding reindex. Only the tender ID is needed; the indexer fetches the current
// title/description from the listing store at processing time.
type embeddingReindexEvent struct {
	EventID    string    `json:"eventId"`
	OccurredAt time.Time `json:"occurredAt"`
	Version    int       `json:"version"`
	Data       struct {
		TenderID uuid.UUID `json:"tenderId"`
	} `json:"data"`
}

// EnqueueTenderEmbeddingReindex inserts an embedding_reindex event into the
// event_outbox table for the given tender listing. MUST be called inside an active
// transaction (same-tx outbox pattern — mirrors EnqueueTenderStatusChanged).
//
// The event is idempotent on event_id: if the same call is retried within the same
// transaction, the ON CONFLICT DO NOTHING clause prevents duplicates.
func EnqueueTenderEmbeddingReindex(ctx context.Context, ob store.OutboxStore, tenderID uuid.UUID) error {
	now := time.Now().UTC()
	eventUUID := uuid.New()

	var evt embeddingReindexEvent
	evt.EventID = eventUUID.String()
	evt.OccurredAt = now
	evt.Version = 1
	evt.Data.TenderID = tenderID

	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal embedding_reindex event: %w", err)
	}

	row := &domain.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: "tender",
		AggregateID:   tenderID,
		EventID:       eventUUID,
		Channel:       embeddingReindexChannel,
		Payload:       payload,
		CreatedAt:     now,
		NextAttemptAt: now,
	}

	return ob.Enqueue(ctx, row)
}

// EnqueueVendorEmbeddingReindex inserts a vendor_embedding_reindex event into the
// event_outbox table for the given vendor (identified by owner_user_id). MUST be
// called inside an active transaction (same-tx outbox pattern).
//
// entity_id stored in the outbox row is owner_user_id — the same value used by
// EmbeddingStore.Upsert("vendor", ownerUserID, …) — so T5 NearestNeighbors
// results can be mapped directly back to user IDs without an extra profile lookup.
func EnqueueVendorEmbeddingReindex(ctx context.Context, ob store.OutboxStore, ownerUserID uuid.UUID) error {
	now := time.Now().UTC()
	eventUUID := uuid.New()

	var evt vendorEmbeddingReindexEvent
	evt.EventID = eventUUID.String()
	evt.OccurredAt = now
	evt.Version = 1
	evt.Data.OwnerUserID = ownerUserID

	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal vendor_embedding_reindex event: %w", err)
	}

	row := &domain.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: "vendor",
		AggregateID:   ownerUserID,
		EventID:       eventUUID,
		Channel:       vendorEmbeddingReindexChannel,
		Payload:       payload,
		CreatedAt:     now,
		NextAttemptAt: now,
	}

	return ob.Enqueue(ctx, row)
}
