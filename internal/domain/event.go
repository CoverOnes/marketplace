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

// CollaboratorJoinedEvent is the payload for marketplace.collaborator_joined.
// Published (best-effort) when AcceptCollaborator succeeds on an EXECUTING tender.
type CollaboratorJoinedEvent struct {
	EventID    string                 `json:"eventId"`
	OccurredAt time.Time              `json:"occurredAt"`
	Version    int                    `json:"version"`
	Data       CollaboratorJoinedData `json:"data"`
}

// CollaboratorJoinedData carries the structured data for the collaborator_joined event.
type CollaboratorJoinedData struct {
	TenderID       uuid.UUID  `json:"tenderId"`
	CollaboratorID uuid.UUID  `json:"collaboratorId"`
	VendorUserID   uuid.UUID  `json:"vendorUserId"`
	RoleID         *uuid.UUID `json:"roleId,omitempty"`
	TenderStatus   string     `json:"tenderStatus"`
}
