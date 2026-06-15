package domain

import (
	"time"

	"github.com/google/uuid"
)

// ListingAttachment represents a file attached to a listing.
// It is a soft-reference to a file_objects record in the file service:
// metadata is copied at attach time (NO FK — CONVENTIONS §11).
// Referential integrity (file exists, STORED status, owner-match, MIME allowlist)
// is enforced by the service layer on attach.
type ListingAttachment struct {
	ID          uuid.UUID
	ListingID   uuid.UUID
	FileID      uuid.UUID
	UploaderID  uuid.UUID
	Filename    string
	ContentType string
	SizeBytes   int64
	DetachedAt  *time.Time
	DetachedBy  *uuid.UUID
	CreatedAt   time.Time
}
