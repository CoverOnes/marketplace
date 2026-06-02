// Package store defines the storage interfaces for the marketplace domain.
package store

import (
	"context"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/google/uuid"
)

// ListingStore defines persistence operations for listings.
type ListingStore interface {
	Create(ctx context.Context, l *domain.Listing) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Listing, error)
	// GetByIDForUpdate fetches a listing by ID with SELECT ... FOR UPDATE row-lock.
	// Must be called inside an active transaction (txListingStore).
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Listing, error)
	List(ctx context.Context, filter ListingFilter) ([]*domain.Listing, error)
	Update(ctx context.Context, l *domain.Listing) error
}

// ListingFilter carries optional filters for listing queries.
type ListingFilter struct {
	Status      *domain.ListingStatus
	OwnerUserID *uuid.UUID
	Limit       int
	Offset      int
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
