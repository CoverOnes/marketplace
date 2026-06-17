// Package domain contains core domain types and sentinel errors.
package domain

import "errors"

// Sentinel errors for the marketplace domain.
var (
	ErrNotFound         = errors.New("not found")
	ErrUnauthorized     = errors.New("unauthorized")
	ErrForbidden        = errors.New("forbidden")
	ErrValidation       = errors.New("validation error")
	ErrListingNotOpen   = errors.New("listing is not open")
	ErrListingNotFound  = errors.New("listing not found")
	ErrBidNotFound      = errors.New("bid not found")
	ErrAwardNotFound    = errors.New("award not found")
	ErrBidNotPending    = errors.New("bid is not pending")
	ErrBidAlreadyExists = errors.New("a pending bid already exists for this listing and bidder")
	ErrBidOnOwnListing  = errors.New("cannot bid on your own listing")
	ErrConflict         = errors.New("conflict")
	ErrKYCTierRequired  = errors.New("kyc tier required")

	// Tender-specific errors.
	ErrTenderBidNotAllowed        = errors.New("classic bid not allowed on a tender listing")
	ErrTenderRoleNotFound         = errors.New("tender role not found")
	ErrTenderCollaboratorNotFound = errors.New("tender collaborator not found")
	ErrTenderMilestoneNotFound    = errors.New("tender milestone not found")
	ErrTenderRoleFilled           = errors.New("tender role is already filled")
	ErrTenderCollaboratorConflict = errors.New("a live application already exists for this role and vendor")
	ErrInvalidTenderTransition    = errors.New("invalid tender status transition")
	ErrNotTenderListing           = errors.New("listing is not a tender")
	// ErrInvalidEmbeddingDimension is returned when the caller passes an embedding
	// vector whose length is not the expected 1536 dimensions.
	ErrInvalidEmbeddingDimension = errors.New("embedding must be 1536 dimensions")
	// ErrInvalidEntityType is returned when the caller passes an entity_type value
	// that is not in the domain allowlist (tender, vendor).
	// Go-level validation prevents the raw pgx check_violation from surfacing (§5.2).
	ErrInvalidEntityType = errors.New("embedding: unknown entity_type")

	// ErrUpstreamWorkspace is returned when the synchronous S2S call to the workspace
	// service fails. The collaborator row is already APPROVED (tx committed); P5 outbox
	// will reconcile. Callers map this to HTTP 502.
	ErrUpstreamWorkspace = errors.New("upstream workspace service error")

	// ErrAttachmentNotFound is returned when a listing attachment cannot be found
	// or has already been detached.
	ErrAttachmentNotFound = errors.New("attachment not found")

	// ErrAttachmentCapReached is returned when a listing already has the maximum
	// allowed number of active attachments (10).
	ErrAttachmentCapReached = errors.New("listing has reached the maximum attachment limit")

	// ErrContentTypeNotAllowed is returned when the caller supplies a MIME type
	// that is not on the attachment allowlist.
	ErrContentTypeNotAllowed = errors.New("content type not allowed")

	// ErrAttachmentForbidden is returned when the caller is not authorized to
	// attach, detach, or download attachments for the given listing.
	ErrAttachmentForbidden = errors.New("attachment operation forbidden")

	// ErrUpstreamFile is returned when the synchronous S2S call to the file
	// service fails during register or presign. Callers map this to HTTP 502.
	ErrUpstreamFile = errors.New("upstream file service error")
)
