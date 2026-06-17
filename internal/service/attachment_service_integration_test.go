package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/service"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAttachmentServiceIntegration runs the attachment service against a real Postgres
// container (started once in TestMain). All stores use the shared testcontainers DSN.
func TestAttachmentServiceIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	pool, err := pgstore.NewPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err, "connect to test container")

	t.Cleanup(func() { pool.Close() })

	listingStore := pgstore.NewListingStore(pool)
	attachStore := pgstore.NewListingAttachmentStore(pool)
	collabStore := pgstore.NewTenderCollaboratorStore(pool)

	// fakeFileClient that always succeeds (file service is not in scope for integration tests).
	fileClient := &fakeFileClient{presignURL: "https://integration-test.example.com/presigned"}

	svc := service.NewAttachmentService(attachStore, listingStore, collabStore, fileClient)

	// --- seed helpers ---

	createListing := func(t *testing.T, ownerID uuid.UUID, status domain.ListingStatus) *domain.Listing {
		t.Helper()

		bmin := decimal.NewFromFloat(100)
		bmax := decimal.NewFromFloat(1000)

		l := &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID,
			Title:       "Integration Test Listing",
			Description: "Test listing for attachment integration tests",
			BudgetMin:   &bmin,
			BudgetMax:   &bmax,
			Currency:    "USD",
			Status:      status,
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}

		require.NoError(t, listingStore.Create(ctx, l))

		return l
	}

	createApprovedCollab := func(t *testing.T, listingID, vendorID uuid.UUID) *domain.TenderCollaborator {
		t.Helper()

		now := time.Now().UTC()
		approvedAt := now

		c := &domain.TenderCollaborator{
			ID:               uuid.New(),
			TenderRoleID:     uuid.New(), // synthetic role ID
			ListingID:        listingID,
			VendorUserID:     vendorID,
			Status:           domain.CollaboratorStatusApproved,
			JoinMessage:      "integration-test collab",
			ApprovedAt:       &approvedAt,
			ApprovedByUserID: &listingID, // use listing ID as the approver user ID placeholder
			CreatedAt:        now,
			UpdatedAt:        now,
		}

		require.NoError(t, collabStore.Create(ctx, c))

		return c
	}

	attachFile := func(t *testing.T, svc *service.AttachmentService, listingID, callerID uuid.UUID) *domain.ListingAttachment {
		t.Helper()

		a, err := svc.Attach(ctx, listingID, callerID, service.AttachInput{
			FileID:      uuid.New(),
			Filename:    "test.pdf",
			ContentType: contentTypePDF,
			SizeBytes:   2048,
		})
		require.NoError(t, err)

		return a
	}

	// --- Test cases ---

	t.Run("attach_happy_path", func(t *testing.T) {
		ownerID := uuid.New()
		listing := createListing(t, ownerID, domain.ListingStatusOpen)

		a := attachFile(t, svc, listing.ID, ownerID)

		assert.NotEqual(t, uuid.Nil, a.ID)
		assert.Equal(t, listing.ID, a.ListingID)
		assert.Equal(t, ownerID, a.UploaderID)
		assert.Equal(t, "test.pdf", a.Filename)
		assert.Equal(t, "application/pdf", a.ContentType)
		assert.Equal(t, int64(2048), a.SizeBytes)
		assert.Nil(t, a.DetachedAt)
	})

	t.Run("cap_10_rejection", func(t *testing.T) {
		ownerID := uuid.New()
		listing := createListing(t, ownerID, domain.ListingStatusOpen)

		// Attach 10 files successfully.
		for i := 0; i < 10; i++ {
			_, err := svc.Attach(ctx, listing.ID, ownerID, service.AttachInput{
				FileID:      uuid.New(),
				Filename:    "file.pdf",
				ContentType: contentTypePDF,
				SizeBytes:   512,
			})
			require.NoError(t, err, "attach %d should succeed", i+1)
		}

		// 11th attachment must be rejected.
		_, err := svc.Attach(ctx, listing.ID, ownerID, service.AttachInput{
			FileID:      uuid.New(),
			Filename:    "overflow.pdf",
			ContentType: contentTypePDF,
			SizeBytes:   512,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrAttachmentCapReached)
	})

	t.Run("non_owner_non_collaborator_403", func(t *testing.T) {
		ownerID := uuid.New()
		strangerID := uuid.New()
		listing := createListing(t, ownerID, domain.ListingStatusOpen)

		_, err := svc.Attach(ctx, listing.ID, strangerID, service.AttachInput{
			FileID:      uuid.New(),
			Filename:    "forbidden.pdf",
			ContentType: contentTypePDF,
			SizeBytes:   512,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrAttachmentForbidden)
	})

	t.Run("detach_by_uploader", func(t *testing.T) {
		ownerID := uuid.New()
		uploaderID := uuid.New()
		listing := createListing(t, ownerID, domain.ListingStatusOpen)

		// Create approved collab for the uploader.
		createApprovedCollab(t, listing.ID, uploaderID)

		a := attachFile(t, svc, listing.ID, uploaderID)

		err := svc.Detach(ctx, listing.ID, a.ID, uploaderID)
		require.NoError(t, err)

		// List should now return zero active attachments.
		attachments, err := svc.List(ctx, listing.ID, ownerID)
		require.NoError(t, err)
		assert.Empty(t, attachments)
	})

	t.Run("detach_by_owner", func(t *testing.T) {
		ownerID := uuid.New()
		uploaderID := uuid.New()
		listing := createListing(t, ownerID, domain.ListingStatusOpen)

		// Create approved collab for the uploader.
		createApprovedCollab(t, listing.ID, uploaderID)

		a := attachFile(t, svc, listing.ID, uploaderID)

		// Owner detaches the uploader's file.
		err := svc.Detach(ctx, listing.ID, a.ID, ownerID)
		require.NoError(t, err)
	})

	t.Run("detach_by_stranger_403", func(t *testing.T) {
		ownerID := uuid.New()
		uploaderID := uuid.New()
		strangerID := uuid.New()
		listing := createListing(t, ownerID, domain.ListingStatusOpen)

		createApprovedCollab(t, listing.ID, uploaderID)

		a := attachFile(t, svc, listing.ID, uploaderID)

		err := svc.Detach(ctx, listing.ID, a.ID, strangerID)
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrAttachmentForbidden)
	})

	t.Run("list_returns_only_active", func(t *testing.T) {
		ownerID := uuid.New()
		listing := createListing(t, ownerID, domain.ListingStatusOpen)

		// Attach 3 files.
		var ids []uuid.UUID
		for i := 0; i < 3; i++ {
			a := attachFile(t, svc, listing.ID, ownerID)
			ids = append(ids, a.ID)
		}

		// Detach one.
		require.NoError(t, svc.Detach(ctx, listing.ID, ids[1], ownerID))

		// List should return only 2 active.
		attachments, err := svc.List(ctx, listing.ID, ownerID)
		require.NoError(t, err)
		assert.Len(t, attachments, 2)
	})

	t.Run("approved_collaborator_may_attach", func(t *testing.T) {
		ownerID := uuid.New()
		vendorID := uuid.New()
		listing := createListing(t, ownerID, domain.ListingStatusOpen)

		// Create APPROVED collaborator.
		createApprovedCollab(t, listing.ID, vendorID)

		a, err := svc.Attach(ctx, listing.ID, vendorID, service.AttachInput{
			FileID:      uuid.New(),
			Filename:    "collab.pdf",
			ContentType: contentTypePDF,
			SizeBytes:   1024,
		})
		require.NoError(t, err)
		assert.Equal(t, vendorID, a.UploaderID)
	})
}
