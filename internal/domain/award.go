package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Award is the authoritative deal-closed record written in the same transaction
// that accepts the bid and flips the listing status to AWARDED.
// All IDs are soft references — NO foreign key constraints.
type Award struct {
	ID               uuid.UUID       `json:"id"`
	ListingID        uuid.UUID       `json:"listingId"`
	BidID            uuid.UUID       `json:"bidId"`
	OwnerUserID      uuid.UUID       `json:"ownerUserId"`
	BidderUserID     uuid.UUID       `json:"bidderUserId"`
	Amount           decimal.Decimal `json:"amount"`
	Currency         string          `json:"currency"`
	EventPublishedAt *time.Time      `json:"eventPublishedAt,omitempty"`
	CreatedAt        time.Time       `json:"createdAt"`
}
