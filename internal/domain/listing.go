package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ListingStatus represents the lifecycle state of a listing.
type ListingStatus string

const (
	ListingStatusOpen    ListingStatus = "OPEN"
	ListingStatusAwarded ListingStatus = "AWARDED"
	ListingStatusClosed  ListingStatus = "CLOSED"
)

// Listing represents a marketplace case/project posted by a client (owner).
// owner_user_id is a soft reference to users.id — NO foreign key constraint.
type Listing struct {
	ID           uuid.UUID        `json:"id"`
	OwnerUserID  uuid.UUID        `json:"ownerUserId"`
	Title        string           `json:"title"`
	Description  string           `json:"description"`
	BudgetMin    *decimal.Decimal `json:"budgetMin,omitempty"`
	BudgetMax    *decimal.Decimal `json:"budgetMax,omitempty"`
	Currency     string           `json:"currency"`
	Status       ListingStatus    `json:"status"`
	AwardedBidID *uuid.UUID       `json:"awardedBidId,omitempty"`
	DeletedAt    *time.Time       `json:"deletedAt,omitempty"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
}
