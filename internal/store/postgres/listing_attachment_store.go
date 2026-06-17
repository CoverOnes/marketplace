package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ListingAttachmentStore is a pool-backed listing attachment store.
type ListingAttachmentStore struct {
	q querier
}

// NewListingAttachmentStore returns a ListingAttachmentStore backed by pool.
func NewListingAttachmentStore(pool *pgxpool.Pool) *ListingAttachmentStore {
	return &ListingAttachmentStore{q: pool}
}

// Create inserts a new listing attachment record.
func (s *ListingAttachmentStore) Create(ctx context.Context, a *domain.ListingAttachment) error {
	const query = `
INSERT INTO listing_attachments
	(id, listing_id, file_id, uploader_id, filename, content_type, size_bytes, created_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8)
`

	_, err := s.q.Exec(
		ctx, query,
		a.ID, a.ListingID, a.FileID, a.UploaderID,
		a.Filename, a.ContentType, a.SizeBytes, a.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert listing_attachment: %w", err)
	}

	return nil
}

// GetByID returns the ACTIVE (non-detached) attachment with the given id, or
// ErrAttachmentNotFound. Detached (soft-deleted) rows are treated as not found so
// a stale attachment id cannot be used to presign a download URL for content the
// owner has already removed.
func (s *ListingAttachmentStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.ListingAttachment, error) {
	const query = `
SELECT id, listing_id, file_id, uploader_id, filename, content_type, size_bytes,
       detached_at, detached_by, created_at
FROM listing_attachments
WHERE id = $1
  AND detached_at IS NULL
`

	return scanAttachment(s.q.QueryRow(ctx, query, id))
}

// ListByListing returns all non-detached attachments for a listing, ordered by created_at.
func (s *ListingAttachmentStore) ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.ListingAttachment, error) {
	const query = `
SELECT id, listing_id, file_id, uploader_id, filename, content_type, size_bytes,
       detached_at, detached_by, created_at
FROM listing_attachments
WHERE listing_id = $1
  AND detached_at IS NULL
ORDER BY created_at ASC
`

	rows, err := s.q.Query(ctx, query, listingID)
	if err != nil {
		return nil, fmt.Errorf("list listing_attachments: %w", err)
	}

	defer rows.Close()

	var out []*domain.ListingAttachment

	for rows.Next() {
		a, scanErr := scanAttachment(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		out = append(out, a)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate listing_attachments: %w", err)
	}

	return out, nil
}

// CountActiveByListing returns the number of non-detached attachments for a listing.
func (s *ListingAttachmentStore) CountActiveByListing(ctx context.Context, listingID uuid.UUID) (int, error) {
	const query = `
SELECT COUNT(*)
FROM listing_attachments
WHERE listing_id = $1
  AND detached_at IS NULL
`

	var n int

	if err := s.q.QueryRow(ctx, query, listingID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count listing_attachments: %w", err)
	}

	return n, nil
}

// Detach soft-deletes an attachment by setting detached_at / detached_by.
// Returns ErrAttachmentNotFound when the attachment does not exist or is already detached.
func (s *ListingAttachmentStore) Detach(ctx context.Context, id, detachedBy uuid.UUID) error {
	const query = `
UPDATE listing_attachments
SET detached_at = $2,
    detached_by = $3
WHERE id = $1
  AND detached_at IS NULL
`

	tag, err := s.q.Exec(ctx, query, id, time.Now().UTC(), detachedBy)
	if err != nil {
		return fmt.Errorf("detach listing_attachment: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrAttachmentNotFound
	}

	return nil
}

// --- scan helper ---

func scanAttachment(row rowScanner) (*domain.ListingAttachment, error) {
	var a domain.ListingAttachment

	err := row.Scan(
		&a.ID, &a.ListingID, &a.FileID, &a.UploaderID,
		&a.Filename, &a.ContentType, &a.SizeBytes,
		&a.DetachedAt, &a.DetachedBy, &a.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrAttachmentNotFound
		}

		return nil, fmt.Errorf("scan listing_attachment: %w", err)
	}

	return &a, nil
}
