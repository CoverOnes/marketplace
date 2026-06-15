package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListingAttachmentStore_Integration exercises all ListingAttachmentStore methods
// against a real Postgres container (shared via TestMain).
func TestListingAttachmentStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping listing attachment integration test in short mode")
	}

	ctx := context.Background()
	pool := sharedTestPool
	store := pgstore.NewListingAttachmentStore(pool)

	listingID := uuid.New()
	uploaderID := uuid.New()

	newAttachment := func(listingID, uploaderID uuid.UUID) *domain.ListingAttachment {
		return &domain.ListingAttachment{
			ID:          uuid.New(),
			ListingID:   listingID,
			FileID:      uuid.New(),
			UploaderID:  uploaderID,
			Filename:    "report.pdf",
			ContentType: "application/pdf",
			SizeBytes:   12345,
			CreatedAt:   time.Now().UTC().Truncate(time.Microsecond),
		}
	}

	t.Run("Create and GetByID returns the attachment", func(t *testing.T) {
		a := newAttachment(listingID, uploaderID)
		require.NoError(t, store.Create(ctx, a))

		got, err := store.GetByID(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, a.ID, got.ID)
		assert.Equal(t, a.ListingID, got.ListingID)
		assert.Equal(t, a.FileID, got.FileID)
		assert.Equal(t, a.UploaderID, got.UploaderID)
		assert.Equal(t, a.Filename, got.Filename)
		assert.Equal(t, a.ContentType, got.ContentType)
		assert.Equal(t, a.SizeBytes, got.SizeBytes)
		assert.Nil(t, got.DetachedAt)
		assert.Nil(t, got.DetachedBy)
	})

	t.Run("GetByID returns ErrAttachmentNotFound for unknown id", func(t *testing.T) {
		_, err := store.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrAttachmentNotFound)
	})

	t.Run("ListByListing returns only active attachments in order", func(t *testing.T) {
		lid := uuid.New()

		a1 := newAttachment(lid, uploaderID)
		a1.CreatedAt = time.Now().UTC().Add(-2 * time.Second).Truncate(time.Microsecond)
		a2 := newAttachment(lid, uploaderID)
		a2.CreatedAt = time.Now().UTC().Add(-1 * time.Second).Truncate(time.Microsecond)

		require.NoError(t, store.Create(ctx, a1))
		require.NoError(t, store.Create(ctx, a2))

		list, err := store.ListByListing(ctx, lid)
		require.NoError(t, err)
		require.Len(t, list, 2)
		assert.Equal(t, a1.ID, list[0].ID, "older attachment must come first")
		assert.Equal(t, a2.ID, list[1].ID)
	})

	t.Run("ListByListing excludes detached attachments", func(t *testing.T) {
		lid := uuid.New()
		a := newAttachment(lid, uploaderID)
		require.NoError(t, store.Create(ctx, a))

		// Detach it.
		require.NoError(t, store.Detach(ctx, a.ID, uploaderID))

		list, err := store.ListByListing(ctx, lid)
		require.NoError(t, err)
		assert.Empty(t, list)
	})

	t.Run("CountActiveByListing counts only non-detached rows", func(t *testing.T) {
		lid := uuid.New()
		a1 := newAttachment(lid, uploaderID)
		a2 := newAttachment(lid, uploaderID)
		a3 := newAttachment(lid, uploaderID)
		require.NoError(t, store.Create(ctx, a1))
		require.NoError(t, store.Create(ctx, a2))
		require.NoError(t, store.Create(ctx, a3))

		// Detach one.
		require.NoError(t, store.Detach(ctx, a3.ID, uploaderID))

		count, err := store.CountActiveByListing(ctx, lid)
		require.NoError(t, err)
		assert.Equal(t, 2, count)
	})

	t.Run("Detach sets detached_at and detached_by", func(t *testing.T) {
		a := newAttachment(listingID, uploaderID)
		require.NoError(t, store.Create(ctx, a))

		byUser := uuid.New()
		require.NoError(t, store.Detach(ctx, a.ID, byUser))

		got, err := store.GetByID(ctx, a.ID)
		require.NoError(t, err)
		assert.NotNil(t, got.DetachedAt)
		assert.NotNil(t, got.DetachedBy)
		assert.Equal(t, byUser, *got.DetachedBy)
	})

	t.Run("Detach returns ErrAttachmentNotFound for unknown id", func(t *testing.T) {
		err := store.Detach(ctx, uuid.New(), uploaderID)
		require.ErrorIs(t, err, domain.ErrAttachmentNotFound)
	})

	t.Run("Detach returns ErrAttachmentNotFound for already-detached attachment", func(t *testing.T) {
		a := newAttachment(listingID, uploaderID)
		require.NoError(t, store.Create(ctx, a))
		require.NoError(t, store.Detach(ctx, a.ID, uploaderID))

		// Second detach must be idempotent-fail.
		err := store.Detach(ctx, a.ID, uploaderID)
		require.ErrorIs(t, err, domain.ErrAttachmentNotFound)
	})

	t.Run("migration 000011: table and indexes exist", func(t *testing.T) {
		// Verify the table exists.
		var tableName string
		err := pool.QueryRow(ctx,
			"SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'listing_attachments'",
		).Scan(&tableName)
		require.NoError(t, err, "listing_attachments table must exist after migration 000011")
		assert.Equal(t, "listing_attachments", tableName)

		// Verify partial index for active-by-listing exists.
		var idxName string
		err = pool.QueryRow(ctx,
			"SELECT indexname FROM pg_indexes WHERE tablename = 'listing_attachments' AND indexname = 'listing_attachments_listing_id_idx'",
		).Scan(&idxName)
		require.NoError(t, err, "listing_attachments_listing_id_idx must exist")
		assert.Equal(t, "listing_attachments_listing_id_idx", idxName)

		// Verify pgvector extension is still present (installed by migration 000010).
		var extName string
		err = pool.QueryRow(ctx,
			"SELECT extname FROM pg_extension WHERE extname = 'vector'",
		).Scan(&extName)
		require.NoError(t, err, "vector extension must be present (migration 000010)")
		assert.Equal(t, "vector", extName)
	})
}
