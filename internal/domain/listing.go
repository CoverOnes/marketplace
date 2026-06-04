package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ListingStatus represents the lifecycle state of a listing (CLASSIC flow).
type ListingStatus string

const (
	ListingStatusOpen    ListingStatus = "OPEN"
	ListingStatusAwarded ListingStatus = "AWARDED"
	ListingStatusClosed  ListingStatus = "CLOSED"
)

// TenderStatus represents the lifecycle state of a tender (separate from listings.status).
// CLASSIC listings always have tender_status = NULL.
type TenderStatus string

const (
	TenderStatusOpen             TenderStatus = "OPEN"
	TenderStatusPartiallyStaffed TenderStatus = "PARTIALLY_STAFFED"
	TenderStatusExecuting        TenderStatus = "EXECUTING"
	TenderStatusSettling         TenderStatus = "SETTLING"
	TenderStatusCompleted        TenderStatus = "COMPLETED"
	TenderStatusCancelled        TenderStatus = "CANCELLED" //nolint:misspell // canonical DB enum value uses British spelling; must match migrations/000005
)

// RecruiterMode controls whether unsolicited vendor join requests are permitted.
// Phase 1: only CLOSED is accepted by the API (OPEN is reserved for Phase 4).
type RecruiterMode string

const (
	RecruiterModeClosed RecruiterMode = "CLOSED"
	RecruiterModeOpen   RecruiterMode = "OPEN"
)

// TenderRoleStatus represents the fill state of a tender role.
type TenderRoleStatus string

const (
	TenderRoleStatusOpen   TenderRoleStatus = "OPEN"
	TenderRoleStatusFilled TenderRoleStatus = "FILLED"
	TenderRoleStatusClosed TenderRoleStatus = "CLOSED"
)

// CollaboratorStatus represents the application state of a tender collaborator.
type CollaboratorStatus string

const (
	CollaboratorStatusPending   CollaboratorStatus = "PENDING"
	CollaboratorStatusApproved  CollaboratorStatus = "APPROVED"
	CollaboratorStatusRejected  CollaboratorStatus = "REJECTED"
	CollaboratorStatusWithdrawn CollaboratorStatus = "WITHDRAWN"
	CollaboratorStatusExited    CollaboratorStatus = "EXITED"
)

// MilestoneStatus represents the reach state of a tender milestone.
type MilestoneStatus string

const (
	MilestoneStatusPending MilestoneStatus = "PENDING"
	MilestoneStatusReached MilestoneStatus = "REACHED"
	MilestoneStatusSkipped MilestoneStatus = "SKIPPED"
)

// Listing represents a marketplace case/project posted by a client (owner).
// owner_user_id is a soft reference to users.id — NO foreign key constraint.
//
// CLASSIC listings: is_tender = false, tender_status = nil (tender_status is NULL in DB).
// TENDER listings:  is_tender = true,  tender_status tracks the tender lifecycle.
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
	// Tender discriminator fields (NULL / zero-value for CLASSIC listings).
	IsTender        bool          `json:"isTender"`
	RecruiterMode   RecruiterMode `json:"recruiterMode,omitempty"`
	TenderStatus    *TenderStatus `json:"tenderStatus,omitempty"`
	KYCTierRequired int           `json:"kycTierRequired,omitempty"`
	DeletedAt       *time.Time    `json:"deletedAt,omitempty"`
	CreatedAt       time.Time     `json:"createdAt"`
	UpdatedAt       time.Time     `json:"updatedAt"`
}

// TenderRole represents a named role within a tender listing.
// listing_id is a soft reference to listings.id — NO foreign key constraint.
type TenderRole struct {
	ID               uuid.UUID        `json:"id"`
	ListingID        uuid.UUID        `json:"listingId"`
	Title            string           `json:"title"`
	Description      string           `json:"description"`
	MaxCollaborators *int             `json:"maxCollaborators,omitempty"` // nil = no cap
	ProfitShareBPS   *int             `json:"profitShareBps,omitempty"`
	ProfitShareNote  string           `json:"profitShareNote"`
	Status           TenderRoleStatus `json:"status"`
	SortOrder        int              `json:"sortOrder"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
}

// TenderCollaborator represents a vendor's application to a tender role.
// tender_role_id, listing_id, vendor_user_id are soft references — NO foreign key constraints.
type TenderCollaborator struct {
	ID                     uuid.UUID          `json:"id"`
	TenderRoleID           uuid.UUID          `json:"tenderRoleId"`
	ListingID              uuid.UUID          `json:"listingId"`
	VendorUserID           uuid.UUID          `json:"vendorUserId"`
	Status                 CollaboratorStatus `json:"status"`
	JoinMessage            string             `json:"joinMessage"`
	ApprovedAt             *time.Time         `json:"approvedAt,omitempty"`
	ApprovedByUserID       *uuid.UUID         `json:"approvedByUserId,omitempty"`
	ExitedAt               *time.Time         `json:"exitedAt,omitempty"`
	ExitReason             string             `json:"exitReason"`
	ProfitShareBPSOverride *int               `json:"profitShareBpsOverride,omitempty"`
	CreatedAt              time.Time          `json:"createdAt"`
	UpdatedAt              time.Time          `json:"updatedAt"`
}

// TenderMilestone represents a deliverable checkpoint for a tender listing.
// listing_id is a soft reference to listings.id — NO foreign key constraint.
// Payout computation against milestones is Phase 3; this type ships with Phase 1
// to support milestone CRUD.
type TenderMilestone struct {
	ID        uuid.UUID        `json:"id"`
	ListingID uuid.UUID        `json:"listingId"`
	Title     string           `json:"title"`
	DueDate   *time.Time       `json:"dueDate,omitempty"`
	Amount    *decimal.Decimal `json:"amount,omitempty"`
	Currency  *string          `json:"currency,omitempty"`
	Status    MilestoneStatus  `json:"status"`
	ReachedAt *time.Time       `json:"reachedAt,omitempty"`
	CreatedAt time.Time        `json:"createdAt"`
	UpdatedAt time.Time        `json:"updatedAt"`
}
