package service

import (
	"context"
	"fmt"
	"time"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
)

// allowedContentTypes is the explicit MIME allowlist for listing attachments.
// Only common document and image types are permitted. Any type not in this list
// is rejected with ErrContentTypeNotAllowed before calling the file service.
//
// This mirrors the spirit of the file service's allowedContentTypes list and
// provides defense-in-depth: we validate at our boundary before the S2S call.
var allowedContentTypes = map[string]bool{
	// Images
	"image/jpeg":    true,
	"image/png":     true,
	"image/gif":     true,
	"image/webp":    true,
	"image/svg+xml": true,
	// Documents
	"application/pdf":    true,
	"application/msword": true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
	"application/vnd.ms-excel": true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
	"application/vnd.ms-powerpoint":                                             true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
	// Plain text
	"text/plain":    true,
	"text/csv":      true,
	"text/markdown": true,
	// Archives (common for design assets / deliverables)
	"application/zip": true,
}

// maxAttachmentsPerListing is the maximum number of active attachments allowed
// per listing. The cap is enforced at service layer via CountActiveByListing.
//
// Race note: there is a TOCTOU window between CountActiveByListing and Create.
// Under concurrent requests a listing could momentarily exceed 10 attachments.
// A full fix would require a database-level unique partial index or an advisory lock.
// This is documented and accepted as best-effort enforcement; a follow-up task
// can add a transactional cap (e.g. advisory lock or DB CHECK via trigger).
const maxAttachmentsPerListing = 10

// AttachInput carries validated client-provided metadata for the Attach operation.
// Metadata (Filename, ContentType, SizeBytes) is client-asserted display metadata
// that is copied at attach time — the file service owns the authoritative file record.
type AttachInput struct {
	FileID      uuid.UUID
	Filename    string
	ContentType string
	SizeBytes   int64
}

// AttachmentService handles listing attachment business logic.
// Authorization rules:
//   - Attach: caller is listing owner OR an APPROVED tender collaborator on the listing.
//   - List / DownloadURL: listing is OPEN, or caller is owner, or caller is an APPROVED collaborator.
//   - Detach: caller is the original uploader OR the listing owner.
type AttachmentService struct {
	attachments   store.ListingAttachmentStore
	listings      store.ListingStore
	collaborators store.TenderCollaboratorStore
	fileClient    client.FileClient // nil = file service call skipped (dev no-op)
}

// NewAttachmentService returns an AttachmentService.
// fileClient may be nil; when nil the file service S2S call is skipped (local dev).
func NewAttachmentService(
	attachments store.ListingAttachmentStore,
	listings store.ListingStore,
	collaborators store.TenderCollaboratorStore,
	fileClient client.FileClient,
) *AttachmentService {
	return &AttachmentService{
		attachments:   attachments,
		listings:      listings,
		collaborators: collaborators,
		fileClient:    fileClient,
	}
}

// Attach attaches a file to a listing.
//
// Steps:
//  1. Load listing (404 if missing).
//  2. Authorization: caller must be the listing owner OR an APPROVED collaborator.
//  3. MIME allowlist check on ContentType.
//  4. Cap check: listing must have fewer than maxAttachmentsPerListing active attachments.
//  5. Call FileClient.RegisterAttachment (S2S): file must exist, be STORED, and be owned by caller.
//  6. Insert the listing_attachments row with the client-provided display metadata.
func (s *AttachmentService) Attach(ctx context.Context, listingID, callerUserID uuid.UUID, in AttachInput) (*domain.ListingAttachment, error) {
	// Step 1: load listing.
	listing, err := s.listings.GetByID(ctx, listingID)
	if err != nil {
		return nil, err // ErrListingNotFound propagates as-is
	}

	// Step 2: authorization — listing owner or APPROVED collaborator.
	if err := s.requireOwnerOrApprovedCollaborator(ctx, listing, callerUserID); err != nil {
		return nil, err
	}

	// Step 3: MIME allowlist.
	if !allowedContentTypes[in.ContentType] {
		return nil, fmt.Errorf("%w: %s", domain.ErrContentTypeNotAllowed, in.ContentType)
	}

	// Step 4: cap check (best-effort; TOCTOU race documented on maxAttachmentsPerListing).
	count, err := s.attachments.CountActiveByListing(ctx, listingID)
	if err != nil {
		return nil, fmt.Errorf("count attachments for listing %s: %w", listingID, err)
	}

	if count >= maxAttachmentsPerListing {
		return nil, domain.ErrAttachmentCapReached
	}

	// Step 5: S2S register with file service (skip when fileClient is nil — local dev).
	if s.fileClient != nil {
		if err := s.fileClient.RegisterAttachment(ctx, in.FileID, listingID, callerUserID); err != nil {
			return nil, fmt.Errorf("register attachment with file service: %w", err)
		}
	}

	// Step 6: persist the attachment row. Metadata is client-asserted at attach time.
	now := time.Now().UTC()
	a := &domain.ListingAttachment{
		ID:          uuid.New(),
		ListingID:   listingID,
		FileID:      in.FileID,
		UploaderID:  callerUserID,
		Filename:    in.Filename,
		ContentType: in.ContentType,
		SizeBytes:   in.SizeBytes,
		CreatedAt:   now,
	}

	if err := s.attachments.Create(ctx, a); err != nil {
		return nil, fmt.Errorf("create listing attachment: %w", err)
	}

	return a, nil
}

// List returns all active (non-detached) attachments for a listing.
//
// Visibility rule: OPEN listings are publicly readable; otherwise the caller must
// be the listing owner or an APPROVED collaborator.
func (s *AttachmentService) List(ctx context.Context, listingID, callerUserID uuid.UUID) ([]*domain.ListingAttachment, error) {
	listing, err := s.listings.GetByID(ctx, listingID)
	if err != nil {
		return nil, err
	}

	if listing.Status != domain.ListingStatusOpen {
		// Non-OPEN listings: require owner or APPROVED collaborator.
		if err := s.requireOwnerOrApprovedCollaborator(ctx, listing, callerUserID); err != nil {
			return nil, err
		}
	}

	attachments, err := s.attachments.ListByListing(ctx, listingID)
	if err != nil {
		return nil, fmt.Errorf("list attachments for listing %s: %w", listingID, err)
	}

	return attachments, nil
}

// Detach soft-deletes an attachment from a listing.
//
// Authorization: caller must be the original uploader OR the listing owner.
func (s *AttachmentService) Detach(ctx context.Context, listingID, attachmentID, callerUserID uuid.UUID) error {
	// Load the attachment first to verify it belongs to the listing.
	a, err := s.attachments.GetByID(ctx, attachmentID)
	if err != nil {
		return err // ErrAttachmentNotFound propagates as-is
	}

	if a.ListingID != listingID {
		// Attachment exists but belongs to a different listing — treat as not found
		// to prevent listing-to-attachment cross-enumeration.
		return domain.ErrAttachmentNotFound
	}

	// Authorization: uploader or listing owner.
	listing, err := s.listings.GetByID(ctx, listingID)
	if err != nil {
		return err
	}

	if callerUserID != a.UploaderID && callerUserID != listing.OwnerUserID {
		return domain.ErrAttachmentForbidden
	}

	return s.attachments.Detach(ctx, attachmentID, callerUserID)
}

// DownloadURL returns a presigned download URL for an attachment.
//
// Visibility rule: same as List — OPEN listing or owner/APPROVED-collaborator.
func (s *AttachmentService) DownloadURL(ctx context.Context, listingID, attachmentID, callerUserID uuid.UUID) (string, error) {
	listing, err := s.listings.GetByID(ctx, listingID)
	if err != nil {
		return "", err
	}

	if listing.Status != domain.ListingStatusOpen {
		if err := s.requireOwnerOrApprovedCollaborator(ctx, listing, callerUserID); err != nil {
			return "", err
		}
	}

	a, err := s.attachments.GetByID(ctx, attachmentID)
	if err != nil {
		return "", err
	}

	if a.ListingID != listingID {
		return "", domain.ErrAttachmentNotFound
	}

	if s.fileClient == nil {
		// Dev no-op: return a placeholder URL when file service is not configured.
		return "https://dev-placeholder.example.com/no-file-service-configured", nil
	}

	url, err := s.fileClient.PresignAttachment(ctx, a.FileID, listingID)
	if err != nil {
		return "", fmt.Errorf("presign attachment: %w", err)
	}

	return url, nil
}

// --- Authorization helpers ---

// requireOwnerOrApprovedCollaborator returns nil when callerUserID is the listing owner
// or has an APPROVED collaborator record for the listing. Returns ErrAttachmentForbidden
// otherwise. This is the shared gate for Attach (write) operations.
func (s *AttachmentService) requireOwnerOrApprovedCollaborator(
	ctx context.Context,
	listing *domain.Listing,
	callerUserID uuid.UUID,
) error {
	if callerUserID == listing.OwnerUserID {
		return nil
	}

	// Check APPROVED collaborators for this listing.
	collabs, err := s.collaborators.ListByListing(ctx, listing.ID)
	if err != nil {
		return fmt.Errorf("list collaborators for listing %s: %w", listing.ID, err)
	}

	for _, c := range collabs {
		if c.VendorUserID == callerUserID && c.Status == domain.CollaboratorStatusApproved {
			return nil
		}
	}

	return domain.ErrAttachmentForbidden
}
