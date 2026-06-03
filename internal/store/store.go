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

// TxManager runs a function inside a single Postgres transaction.
type TxManager interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, listings ListingStore, bids BidStore, awards AwardStore) error) error
}
