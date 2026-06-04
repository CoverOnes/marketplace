package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// BidStatus represents the lifecycle state of a bid.
type BidStatus string

const (
	BidStatusPending   BidStatus = "PENDING"
	BidStatusAccepted  BidStatus = "ACCEPTED"
	BidStatusRejected  BidStatus = "REJECTED"
	BidStatusWithdrawn BidStatus = "WITHDRAWN"
)

// Bid represents a contractor's bid on a listing.
// listing_id and bidder_user_id are soft references — NO foreign key constraints.
// role_id is a soft reference to tender_roles.id — NULL for classic (1:1) bids.
type Bid struct {
	ID           uuid.UUID       `json:"id"`
	ListingID    uuid.UUID       `json:"listingId"`
	BidderUserID uuid.UUID       `json:"bidderUserId"`
	Amount       decimal.Decimal `json:"amount"`
	Currency     string          `json:"currency"`
	Message      string          `json:"message"`
	Status       BidStatus       `json:"status"`
	RoleID       *uuid.UUID      `json:"roleId,omitempty"` // nil = classic bid
	DecidedAt    *time.Time      `json:"decidedAt,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}
