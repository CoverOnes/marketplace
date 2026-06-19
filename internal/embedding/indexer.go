package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
)

// embeddingReindexChannel is the Redis pub/sub channel that the event_outbox poller
// delivers embedding_reindex events on. Kept in sync with outbox.embeddingReindexChannel.
const embeddingReindexChannel = "marketplace.embedding_reindex"

// vendorEmbeddingReindexChannel is the separate channel for vendor profile embedding
// reindex events. Kept in sync with outbox.vendorEmbeddingReindexChannel.
const vendorEmbeddingReindexChannel = "marketplace.vendor_embedding_reindex"

// defaultIndexerInterval is the poll frequency when no custom interval is supplied.
const defaultIndexerInterval = 2 * time.Second

// defaultWorkerTimeout is the per-event processing timeout used by the indexer.
// Allows one embedding API call (up to 30 s) plus DB upsert headroom.
const defaultWorkerTimeout = 45 * time.Second

// maxOutboxAttempts is the dead-letter threshold. At 2^N s backoff capped at 600 s,
// attempts 1-20 span ~1.8 h before dead-lettering — long enough for transient outages,
// finite for true poison events. Duplicated in internal/outbox/poller.go (see comment there).
const maxOutboxAttempts = 20

// reindexPayload is the JSON envelope published by EnqueueTenderEmbeddingReindex.
// The indexer parses only the data.tenderId field; the rest is informational.
type reindexPayload struct {
	Data struct {
		TenderID uuid.UUID `json:"tenderId"`
	} `json:"data"`
}

// vendorReindexPayload is the JSON envelope published by EnqueueVendorEmbeddingReindex.
// entity_id for vendor embeddings is owner_user_id (NOT the vendor_profile row id).
// T5 NearestNeighbors results map back to user IDs using this value.
type vendorReindexPayload struct {
	Data struct {
		OwnerUserID uuid.UUID `json:"ownerUserId"`
	} `json:"data"`
}

// Indexer is a poller that consumes embedding_reindex and vendor_embedding_reindex
// events from the outbox, composes entity text, calls the embedding API, and upserts
// the result.
//
// The worker goroutine MUST NOT inherit the request context (backend-security §5
// "goroutine must not inherit request context"). All DB and API calls use
// context.Background()-derived timeouts.
type Indexer struct {
	outboxStore        store.OutboxStore
	listingStore       store.ListingStore
	vendorProfileStore store.VendorProfileStore
	embeddingStore     store.EmbeddingStore
	embClient          client.EmbeddingClient
	interval           time.Duration
	modelVersion       string
}

// IndexerConfig carries the constructor parameters for NewIndexer.
type IndexerConfig struct {
	OutboxStore        store.OutboxStore
	ListingStore       store.ListingStore
	VendorProfileStore store.VendorProfileStore
	EmbeddingStore     store.EmbeddingStore
	EmbClient          client.EmbeddingClient
	// Interval is the poll frequency. Zero → defaultIndexerInterval (2 s).
	Interval time.Duration
	// ModelVersion is the identifier stored in embeddings.model_version.
	// Defaults to "text-embedding-3-small" when empty.
	ModelVersion string
}

// NewIndexer returns a configured Indexer.
func NewIndexer(cfg *IndexerConfig) *Indexer {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultIndexerInterval
	}

	if cfg.ModelVersion == "" {
		cfg.ModelVersion = "text-embedding-3-small"
	}

	return &Indexer{
		outboxStore:        cfg.OutboxStore,
		listingStore:       cfg.ListingStore,
		vendorProfileStore: cfg.VendorProfileStore,
		embeddingStore:     cfg.EmbeddingStore,
		embClient:          cfg.EmbClient,
		interval:           cfg.Interval,
		modelVersion:       cfg.ModelVersion,
	}
}

// DrainOnce processes one batch of ready embedding_reindex and vendor_embedding_reindex
// events synchronously. It is exported for use in integration tests that need to
// trigger indexer processing without relying on ticker timing.
func (ix *Indexer) DrainOnce(ctx context.Context) {
	pollCtx, pollCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pollCancel()

	events, err := ix.outboxStore.PollReady(pollCtx, 20)
	if err != nil {
		slog.Warn("embedding indexer DrainOnce: poll failed", "err", err)

		return
	}

	for _, evt := range events {
		switch evt.Channel {
		case embeddingReindexChannel, vendorEmbeddingReindexChannel:
			//nolint:contextcheck // processEvent owns its context.Background()-derived timeouts — worker must not inherit request context (backend-security §5)
			ix.processEvent(evt)
		default:
			// Skip events for other channels; they will be picked up by the main outbox poller.
		}
	}
}

// Run starts the indexer poll loop. It blocks until ctx is canceled.
// Safe to launch in a goroutine; uses context.Background()-derived timeouts for
// each DB and API operation (backend-security §5: worker goroutine must not
// inherit request context so it is not canceled on client disconnect).
func (ix *Indexer) Run(ctx context.Context) {
	ticker := time.NewTicker(ix.interval)
	defer ticker.Stop()

	slog.Info("embedding indexer started", "interval", ix.interval)

	for {
		select {
		case <-ctx.Done():
			slog.Info("embedding indexer stopping")

			return
		case <-ticker.C:
			ix.tick() //nolint:contextcheck // tick intentionally uses context.Background() per backend-security §5: indexer goroutine must not inherit request context
		}
	}
}

// tick polls one batch of embedding_reindex and vendor_embedding_reindex rows and
// processes each.
func (ix *Indexer) tick() {
	pollCtx, pollCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pollCancel()

	events, err := ix.outboxStore.PollReady(pollCtx, 20)
	if err != nil {
		slog.Warn("embedding indexer: poll failed", "err", err)

		return
	}

	for _, evt := range events {
		switch evt.Channel {
		case embeddingReindexChannel, vendorEmbeddingReindexChannel:
			ix.processEvent(evt)
		default:
			// This poller shares the outbox table; skip non-reindex events.
			// They will be picked up by the main outbox poller.
		}
	}
}

// processEvent handles a single embedding_reindex or vendor_embedding_reindex outbox event.
// On ErrEmbeddingDisabled it logs at Info and marks the event published (skip).
// On any other error it marks the event failed so the outbox retries.
func (ix *Indexer) processEvent(evt *domain.OutboxEvent) {
	procCtx, procCancel := context.WithTimeout(context.Background(), defaultWorkerTimeout)
	defer procCancel()

	processErr := ix.index(procCtx, evt)

	markCtx, markCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer markCancel()

	if processErr != nil {
		if evt.Attempts+1 >= maxOutboxAttempts {
			slog.Error(
				"embedding indexer: dead-lettering poison event",
				"outbox_id", evt.ID,
				"event_id", evt.EventID,
				"channel", evt.Channel,
				"attempts", evt.Attempts+1,
				"last_err", processErr.Error(),
			)

			if dlErr := ix.outboxStore.MarkDeadLettered(markCtx, evt.ID); dlErr != nil {
				slog.Warn("embedding indexer: mark-dead-lettered failed", "outbox_id", evt.ID, "err", dlErr)
			}

			return
		}

		slog.Warn(
			"embedding indexer: reindex failed; will retry via outbox backoff",
			"outbox_id", evt.ID,
			"event_id", evt.EventID,
			"channel", evt.Channel,
			"attempts", evt.Attempts+1,
			"err", processErr,
		)

		if markErr := ix.outboxStore.MarkFailed(markCtx, evt.ID, processErr.Error()); markErr != nil {
			slog.Warn("embedding indexer: mark-failed failed", "outbox_id", evt.ID, "err", markErr)
		}

		return
	}

	if markErr := ix.outboxStore.MarkPublished(markCtx, evt.ID); markErr != nil {
		slog.Warn(
			"embedding indexer: mark-published failed; event processed but may be re-processed",
			"outbox_id", evt.ID,
			"event_id", evt.EventID,
			"err", markErr,
		)
	}
}

// index dispatches to the correct indexing path based on evt.Channel.
func (ix *Indexer) index(ctx context.Context, evt *domain.OutboxEvent) error {
	switch evt.Channel {
	case embeddingReindexChannel:
		return ix.indexTender(ctx, evt)
	case vendorEmbeddingReindexChannel:
		return ix.indexVendor(ctx, evt)
	default:
		return fmt.Errorf("embedding indexer: unexpected channel %q", evt.Channel)
	}
}

// generateAndUpsert is the shared embedding generate + store path.
// It calls the embedding API with text and upserts the result for entityType/entityID.
// On ErrEmbeddingDisabled it logs at Info level and returns nil (skip, not retry).
// skipLogKey is the slog attribute key used in the disabled-log line (e.g. "tender_id").
func (ix *Indexer) generateAndUpsert(
	ctx context.Context,
	entityType domain.EmbeddingEntityType,
	entityID uuid.UUID,
	text string,
	skipLogKey string,
) error {
	vec, err := ix.embClient.Generate(ctx, text)
	if err != nil {
		if errors.Is(err, client.ErrEmbeddingDisabled) {
			// Embedding disabled (no API key). Log and skip — the entity write is
			// already committed; we must not block or retry on this condition.
			slog.Info(
				"embedding indexer: client disabled; skipping reindex",
				skipLogKey, entityID,
			)

			return nil
		}

		return fmt.Errorf("generate embedding for %s %s: %w", entityType, entityID, err)
	}

	if upsertErr := ix.embeddingStore.Upsert(ctx, entityType, entityID, vec, ix.modelVersion); upsertErr != nil {
		return fmt.Errorf("upsert embedding for %s %s: %w", entityType, entityID, upsertErr)
	}

	slog.Info(
		"embedding indexer: entity reindexed",
		"entity_type", entityType,
		skipLogKey, entityID,
		"model", ix.modelVersion,
	)

	return nil
}

// indexTender handles a single embedding_reindex event: parse payload → fetch listing →
// compose text → call embedding API → upsert result.
func (ix *Indexer) indexTender(ctx context.Context, evt *domain.OutboxEvent) error {
	var payload reindexPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return fmt.Errorf("parse embedding_reindex payload: %w", err)
	}

	tenderID := payload.Data.TenderID
	if tenderID == uuid.Nil {
		return fmt.Errorf("embedding_reindex event has nil tenderID")
	}

	listing, err := ix.listingStore.GetByID(ctx, tenderID)
	if err != nil {
		return fmt.Errorf("fetch tender %s: %w", tenderID, err)
	}

	return ix.generateAndUpsert(ctx, domain.EmbeddingEntityTypeTender, tenderID, ComposeTenderText(listing), "tender_id")
}

// indexVendor handles a single vendor_embedding_reindex event: parse payload →
// fetch vendor profile by owner_user_id → compose text → call embedding API →
// upsert result using entity_id=owner_user_id (NOT the profile row id).
//
// entity_id=owner_user_id is load-bearing: T5 NearestNeighbors results are mapped
// directly back to user IDs without an additional profile lookup.
func (ix *Indexer) indexVendor(ctx context.Context, evt *domain.OutboxEvent) error {
	var payload vendorReindexPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		return fmt.Errorf("parse vendor_embedding_reindex payload: %w", err)
	}

	ownerUserID := payload.Data.OwnerUserID
	if ownerUserID == uuid.Nil {
		return fmt.Errorf("vendor_embedding_reindex event has nil ownerUserId")
	}

	profile, err := ix.vendorProfileStore.GetByOwner(ctx, ownerUserID)
	if err != nil {
		return fmt.Errorf("fetch vendor profile for owner %s: %w", ownerUserID, err)
	}

	// entity_id = owner_user_id (NOT profile row id).
	// T5 NearestNeighbors(vec,"vendor",K) returns owner_user_id values directly.
	return ix.generateAndUpsert(ctx, domain.EmbeddingEntityTypeVendor, ownerUserID, ComposeVendorText(profile), "owner_user_id")
}
