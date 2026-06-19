package domain

import (
	"time"

	"github.com/google/uuid"
)

// MaxOutboxAttempts is the dead-letter threshold for outbox event processing.
// At 2^N seconds exponential backoff capped at 600 s, attempts 1–20 span ~1.8 h
// before a poison event is dead-lettered — long enough for transient outages,
// finite for true poison events. Used by both the embedding indexer and the
// main outbox poller so drift between the two is impossible.
const MaxOutboxAttempts = 20

// OutboxEvent is a row in the event_outbox table.
// It is enqueued inside the same DB transaction as the business operation and
// delivered by the in-process poller (at-least-once; consumer MUST dedup on EventID).
//
// Consumer-side dedup: EventID is a unique key. Consumers must record seen event_ids
// in their own idempotency table and skip duplicates. The outbox guarantees
// at-least-once delivery; exactly-once is NOT guaranteed (a crash between publish
// and mark can cause a re-deliver).
//
// ClaimedUntil is set atomically by PollReady to prevent concurrent pollers from
// picking up the same row. MarkPublished and MarkFailed both clear it.
//
// DeadLetteredAt is set by MarkDeadLettered when Attempts reaches MaxOutboxAttempts.
// Once set, the row is permanently excluded from PollReady and is retained for
// manual inspection. Requeue by setting dead_lettered_at = NULL.
type OutboxEvent struct {
	ID             uuid.UUID
	AggregateType  string
	AggregateID    uuid.UUID
	EventID        uuid.UUID
	Channel        string
	Payload        []byte
	CreatedAt      time.Time
	PublishedAt    *time.Time
	Attempts       int
	LastError      *string
	NextAttemptAt  time.Time
	ClaimedUntil   *time.Time
	DeadLetteredAt *time.Time
}
