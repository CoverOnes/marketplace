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
type Bid struct {
	ID           uuid.UUID       `json:"id"`
	ListingID    uuid.UUID       `json:"listingId"`
	BidderUserID uuid.UUID       `json:"bidderUserId"`
	Amount       decimal.Decimal `json:"amount"`
	Currency     string          `json:"currency"`
	Message      string          `json:"message"`
	Status       BidStatus       `json:"status"`
	DecidedAt    *time.Time      `json:"decidedAt,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}
