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
