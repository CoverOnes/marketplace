package domain

import (
	"time"

	"github.com/google/uuid"
)

// VendorProfile holds a vendor's embeddable self-description.
// There is at most one profile per user (owner_user_id is UNIQUE in the DB).
// V1: plain CRUD. V2 will wrap Upsert in an outbox transaction to trigger
// embedding reindex — no schema change required.
type VendorProfile struct {
	ID          uuid.UUID `json:"id"`
	OwnerUserID uuid.UUID `json:"ownerUserId"`
	DisplayName string    `json:"displayName"`
	Headline    *string   `json:"headline,omitempty"`
	Bio         *string   `json:"bio,omitempty"`
	Skills      []string  `json:"skills"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
