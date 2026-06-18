// Package store defines the storage interfaces for the marketplace domain.
package store

import (
	"context"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ListingStore defines persistence operations for listings.
type ListingStore interface {
	Create(ctx context.Context, l *domain.Listing) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Listing, error)
	// GetByIDForUpdate fetches a listing by ID with SELECT ... FOR UPDATE row-lock.
	// Must be called inside an active transaction (txListingStore).
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Listing, error)
	List(ctx context.Context, filter ListingFilter) ([]*domain.Listing, error)
	// Search runs a full-text + structured filter query with keyset pagination.
	Search(ctx context.Context, filter SearchFilter) ([]*domain.Listing, error)
	Update(ctx context.Context, l *domain.Listing) error
}

// ListingFilter carries optional filters for listing queries.
//
// VisibleToUserID enforces the listing visibility rule IN SQL (P0 IDOR fix):
// only OPEN listings, plus any listing owned by this user, are returned. When
// the zero UUID is supplied no caller owns a private listing, so only OPEN rows
// are returned. Pushing this into the query (rather than post-filtering in the
// service) prevents ?status=AWARDED|CLOSED from enumerating other users' rows.
type ListingFilter struct {
	Status          *domain.ListingStatus
	OwnerUserID     *uuid.UUID
	VisibleToUserID uuid.UUID
	Limit           int
	Offset          int
}

// SearchCursor is the keyset cursor for stable search pagination.
// It encodes the (created_at, id) of the last row of the previous page so the
// next page selects rows strictly older than this position. id is the
// tiebreaker for rows sharing an identical created_at.
type SearchCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        uuid.UUID `json:"i"`
}

// SearchFilter carries the parameters for ListingStore.Search.
//
// Query, when non-empty, is matched against to_tsvector(title || description)
// via plainto_tsquery. Status/BudgetMin/BudgetMax are optional structured
// filters. After is the keyset cursor for pagination (nil = first page).
// Limit caps the page size (defaulted/clamped by the service layer).
//
// VisibleToUserID enforces the visibility rule IN SQL: only OPEN listings, plus
// any listing owned by this user, are returned. Pushing this into the query
// (rather than post-filtering in the service) keeps keyset pagination correct —
// post-filtering could silently shorten a page and break the next cursor.
type SearchFilter struct {
	Query           string
	Status          *domain.ListingStatus
	BudgetMin       *decimal.Decimal
	BudgetMax       *decimal.Decimal
	After           *SearchCursor
	VisibleToUserID uuid.UUID
	Limit           int
}

// BidStore defines persistence operations for bids.
type BidStore interface {
	Create(ctx context.Context, b *domain.Bid) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Bid, error)
	// GetByIDForUpdate fetches a bid by ID with SELECT ... FOR UPDATE row-lock.
	// Must be called inside an active transaction (txBidStore).
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Bid, error)
	ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.Bid, error)
	ListByBidder(ctx context.Context, bidderUserID uuid.UUID) ([]*domain.Bid, error)
	Update(ctx context.Context, b *domain.Bid) error
	// RejectSiblingBids sets all PENDING bids for a listing (except acceptedBidID) to REJECTED.
	// Called within an accept transaction.
	RejectSiblingBids(ctx context.Context, listingID, acceptedBidID uuid.UUID) error
}

// AwardStore defines persistence operations for awards.
type AwardStore interface {
	Create(ctx context.Context, a *domain.Award) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Award, error)
	MarkEventPublished(ctx context.Context, awardID uuid.UUID) error
}

// TenderRoleStore defines persistence operations for tender roles.
type TenderRoleStore interface {
	Create(ctx context.Context, r *domain.TenderRole) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.TenderRole, error)
	// GetByIDForUpdate fetches a tender role with SELECT ... FOR UPDATE row-lock.
	// Must be called inside an active transaction.
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderRole, error)
	ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.TenderRole, error)
	Update(ctx context.Context, r *domain.TenderRole) error
}

// TenderCollaboratorStore defines persistence operations for tender collaborators.
type TenderCollaboratorStore interface {
	Create(ctx context.Context, c *domain.TenderCollaborator) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.TenderCollaborator, error)
	// GetByIDForUpdate fetches a collaborator with SELECT ... FOR UPDATE row-lock.
	// Must be called inside an active transaction.
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderCollaborator, error)
	// CountApprovedByRole counts APPROVED collaborators for a role, used inside
	// a transaction to enforce max_collaborators cap atomically.
	CountApprovedByRole(ctx context.Context, roleID uuid.UUID) (int, error)
	ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.TenderCollaborator, error)
	ListByRole(ctx context.Context, roleID uuid.UUID) ([]*domain.TenderCollaborator, error)
	Update(ctx context.Context, c *domain.TenderCollaborator) error
}

// TenderMilestoneStore defines persistence operations for tender milestones.
type TenderMilestoneStore interface {
	Create(ctx context.Context, m *domain.TenderMilestone) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.TenderMilestone, error)
	// GetByIDForUpdate fetches a milestone by ID with SELECT ... FOR UPDATE row-lock.
	// Must be called inside an active transaction (txTenderMilestoneStore).
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderMilestone, error)
	ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.TenderMilestone, error)
	Update(ctx context.Context, m *domain.TenderMilestone) error
}

// MilestoneTxManager runs milestone-specific operations inside a single Postgres transaction.
// Used by UpdateMilestone to serialize concurrent status transitions on the same milestone.
type MilestoneTxManager interface {
	WithMilestoneTx(ctx context.Context, fn func(
		ctx context.Context,
		listings ListingStore,
		milestones TenderMilestoneStore,
	) error) error
}

// TxManager runs a function inside a single Postgres transaction.
type TxManager interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, listings ListingStore, bids BidStore, awards AwardStore) error) error
}

// TenderTxManager runs tender-specific operations inside a single Postgres transaction.
// Separate from TxManager to avoid bloating the classic 1:1 transaction interface.
type TenderTxManager interface {
	WithTenderTx(ctx context.Context, fn func(
		ctx context.Context,
		listings ListingStore,
		roles TenderRoleStore,
		collaborators TenderCollaboratorStore,
	) error) error
}

// OutboxStore defines persistence operations for the transactional outbox.
// All Enqueue calls MUST be made inside the same DB transaction as the business
// operation they represent (same-tx enqueue pattern).
type OutboxStore interface {
	// Enqueue inserts a new outbox event row. MUST be called inside an active
	// transaction (via OutboxTxManager.WithOutboxTx) for the same-tx guarantee.
	Enqueue(ctx context.Context, e *domain.OutboxEvent) error

	// PollReady returns up to limit unpublished rows whose next_attempt_at <= now,
	// locking them with SELECT ... FOR UPDATE SKIP LOCKED so concurrent pollers
	// each claim a disjoint set.
	PollReady(ctx context.Context, limit int) ([]*domain.OutboxEvent, error)

	// MarkPublished sets published_at = now() for the given row.
	MarkPublished(ctx context.Context, id uuid.UUID) error

	// MarkFailed increments attempts, sets last_error, and advances next_attempt_at
	// using exponential backoff (2^attempts seconds, capped at 10 minutes).
	MarkFailed(ctx context.Context, id uuid.UUID, lastErr string) error

	// DeletePublishedBefore removes rows with published_at < cutoff.
	// Retention housekeeping: called by the poller on each cycle.
	DeletePublishedBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// OutboxTxManager wraps the tender business operation + outbox Enqueue in a single
// Postgres transaction, fulfilling the same-tx enqueue requirement.
type OutboxTxManager interface {
	WithOutboxTx(ctx context.Context, fn func(
		ctx context.Context,
		listings ListingStore,
		roles TenderRoleStore,
		collaborators TenderCollaboratorStore,
		outbox OutboxStore,
	) error) error
}

// BidOutboxTxManager wraps the bid accept operation + outbox Enqueue in a single
// Postgres transaction, standardizing the ad-hoc bid_service MarkEventPublished
// flag onto the outbox (same-tx enqueue pattern).
type BidOutboxTxManager interface {
	WithBidOutboxTx(ctx context.Context, fn func(
		ctx context.Context,
		listings ListingStore,
		bids BidStore,
		awards AwardStore,
		outbox OutboxStore,
	) error) error
}

// ListingOutboxTxManager wraps a listing create/update operation + outbox Enqueue
// in a single Postgres transaction. Used by ListingService.CreateListing and
// UpdateListing to enqueue embedding_reindex events in the same tx as the listing
// write (same-tx outbox pattern — backend-security §1.3 / outbox §2).
type ListingOutboxTxManager interface {
	WithListingOutboxTx(ctx context.Context, fn func(
		ctx context.Context,
		listings ListingStore,
		outbox OutboxStore,
	) error) error
}

// ListingAttachmentStore defines persistence operations for listing attachments.
// Attachments are soft-references to file_objects in the file service.
// Referential integrity (file exists, STORED status, owner-match, MIME allowlist)
// is enforced in the service layer on attach; the DB stores what was validated.
type ListingAttachmentStore interface {
	// Create inserts a new listing attachment record.
	// Does NOT check file-service state — callers MUST validate before calling.
	Create(ctx context.Context, a *domain.ListingAttachment) error

	// GetByID returns the attachment with the given id, or ErrAttachmentNotFound.
	GetByID(ctx context.Context, id uuid.UUID) (*domain.ListingAttachment, error)

	// ListByListing returns all non-detached attachments for a listing,
	// ordered by created_at ascending.
	ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.ListingAttachment, error)

	// CountActiveByListing returns the number of non-detached attachments for a listing.
	// Used by the service layer to enforce the 10-attachment cap atomically.
	CountActiveByListing(ctx context.Context, listingID uuid.UUID) (int, error)

	// Detach soft-deletes an attachment by setting detached_at and detached_by.
	// Returns ErrAttachmentNotFound if the attachment does not exist or is already detached.
	Detach(ctx context.Context, id, detachedBy uuid.UUID) error
}

// EmbeddingStore defines persistence operations for vector embeddings.
type EmbeddingStore interface {
	// Upsert inserts or updates the embedding for (entityType, entityID).
	// On conflict on (entity_type, entity_id) the embedding and model_version
	// are updated in place; created_at is preserved from the original row.
	Upsert(ctx context.Context, entityType domain.EmbeddingEntityType, entityID uuid.UUID, embedding []float32, modelVersion string) error

	// NearestNeighbors returns up to topK embeddings whose entity_type matches
	// the supplied filter, ordered by ascending cosine distance (most similar first).
	// Uses the HNSW index via the <=> operator.
	// topK is clamped to [1, 200] by the implementation: values ≤ 0 default to 10,
	// values > 200 are reduced to 200 to prevent full-index OOM scans.
	NearestNeighbors(ctx context.Context, queryVec []float32, entityType domain.EmbeddingEntityType, topK int) ([]*domain.Embedding, error)
}

// RecommendationStore defines persistence operations for AI recommendation audit rows.
type RecommendationStore interface {
	// Insert writes one AI recommendation audit row.
	// The Basis field MUST be redacted by the caller before calling Insert
	// (backend-security §3.1): any generated text matching credential patterns
	// must be replaced with [REDACTED:type] before reaching the store layer.
	Insert(ctx context.Context, r *domain.AIRecommendation) error

	// ListBySubject returns up to limit recommendation rows for the given subject
	// user, ordered by created_at descending (most recent first).
	// limit is clamped to 200 when <= 0 the default of 200 is used.
	ListBySubject(ctx context.Context, subjectUserID uuid.UUID, limit int) ([]*domain.AIRecommendation, error)

	// DeleteOlderThan removes ai_recommendation rows with created_at < cutoff.
	// Returns the number of rows deleted.
	// Called by the retention runner (30-day TTL per migration 000013 comment).
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}
