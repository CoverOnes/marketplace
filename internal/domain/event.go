package domain

import (
	"time"

	"github.com/google/uuid"
)

// BidAcceptedEvent is the payload for the marketplace.bid_accepted event.
// Amount is serialized as string to preserve numeric precision (never float).
type BidAcceptedEvent struct {
	EventID    string          `json:"eventId"`
	OccurredAt time.Time       `json:"occurredAt"`
	Version    int             `json:"version"`
	Data       BidAcceptedData `json:"data"`
}

// BidAcceptedData carries the structured data for the bid_accepted event.
type BidAcceptedData struct {
	AwardID      uuid.UUID `json:"awardId"`
	ListingID    uuid.UUID `json:"listingId"`
	BidID        uuid.UUID `json:"bidId"`
	OwnerUserID  uuid.UUID `json:"ownerUserId"`
	BidderUserID uuid.UUID `json:"bidderUserId"`
	Amount       string    `json:"amount"` // numeric as string to preserve precision
	Currency     string    `json:"currency"`
}
