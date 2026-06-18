package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedMilestoneForService inserts a milestone directly via the store for integration tests.
func seedMilestoneForService(
	t *testing.T, ctx context.Context, dsn string,
	listingID uuid.UUID, status domain.MilestoneStatus,
) *domain.TenderMilestone {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)
	defer pool.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	m := &domain.TenderMilestone{
		ID:        uuid.New(),
		ListingID: listingID,
		Title:     "Integration milestone",
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, postgres.NewTenderMilestoneStore(pool).Create(ctx, m))

	return m
}

// readMilestone reads a milestone directly from the DB.
func readMilestone(t *testing.T, ctx context.Context, dsn string, id uuid.UUID) *domain.TenderMilestone {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)
	defer pool.Close()

	m, err := postgres.NewTenderMilestoneStore(pool).GetByID(ctx, id)
	require.NoError(t, err)

	return m
}

// TestMilestoneTransition_Integration tests the full milestone transition flow
// against a real Postgres instance: PENDING→REACHED, PENDING→SKIPPED, owner guard,
// and terminal-tender reject.
func TestMilestoneTransition_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN
	ownerID := uuid.New()

	t.Run("PENDING → REACHED writes reached_at to DB", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusOpen)
		m := seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusPending)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		before := time.Now().UTC().Truncate(time.Second)
		result, err := svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
			MilestoneID: m.ID,
			CallerID:    ownerID,
			Status:      domain.MilestoneStatusReached,
		})
		after := time.Now().UTC().Add(time.Second)

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, domain.MilestoneStatusReached, result.Status)
		require.NotNil(t, result.ReachedAt, "reached_at must be set")
		assert.True(t, result.ReachedAt.After(before) || result.ReachedAt.Equal(before),
			"reached_at must be >= start of test, got %v", result.ReachedAt)
		assert.True(t, result.ReachedAt.Before(after),
			"reached_at must be <= end of test, got %v", result.ReachedAt)

		// Verify durable in DB.
		stored := readMilestone(t, ctx, dsn, m.ID)
		assert.Equal(t, domain.MilestoneStatusReached, stored.Status)
		require.NotNil(t, stored.ReachedAt)
	})

	t.Run("PENDING → SKIPPED, reached_at stays nil", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusExecuting)
		m := seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusPending)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		result, err := svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
			MilestoneID: m.ID,
			CallerID:    ownerID,
			Status:      domain.MilestoneStatusSkipped,
		})
		require.NoError(t, err)
		assert.Equal(t, domain.MilestoneStatusSkipped, result.Status)
		assert.Nil(t, result.ReachedAt, "reached_at must not be set for SKIPPED")

		// Verify durable in DB.
		stored := readMilestone(t, ctx, dsn, m.ID)
		assert.Equal(t, domain.MilestoneStatusSkipped, stored.Status)
		assert.Nil(t, stored.ReachedAt)
	})

	t.Run("non-owner gets 404 (enumeration guard)", func(t *testing.T) {
		strangerID := uuid.New()
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusOpen)
		m := seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusPending)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		_, err := svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
			MilestoneID: m.ID,
			CallerID:    strangerID,
			Status:      domain.MilestoneStatusReached,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, domain.ErrTenderMilestoneNotFound,
			"non-owner must receive ErrTenderMilestoneNotFound, got: %v", err)

		// Milestone must remain PENDING.
		stored := readMilestone(t, ctx, dsn, m.ID)
		assert.Equal(t, domain.MilestoneStatusPending, stored.Status,
			"milestone must remain PENDING after unauthorized update")
	})

	t.Run("terminal tender (SETTLING) rejects transition", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusSettling)
		m := seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusPending)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		_, err := svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
			MilestoneID: m.ID,
			CallerID:    ownerID,
			Status:      domain.MilestoneStatusReached,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, domain.ErrInvalidTenderTransition,
			"SETTLING tender must reject milestone changes, got: %v", err)

		// Milestone must remain PENDING.
		stored := readMilestone(t, ctx, dsn, m.ID)
		assert.Equal(t, domain.MilestoneStatusPending, stored.Status)
	})

	t.Run("terminal tender (COMPLETED) rejects transition", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusCompleted)
		m := seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusPending)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		_, err := svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
			MilestoneID: m.ID,
			CallerID:    ownerID,
			Status:      domain.MilestoneStatusReached,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, domain.ErrInvalidTenderTransition,
			"COMPLETED tender must reject milestone changes, got: %v", err)
	})

	t.Run("illegal transition REACHED→SKIPPED rejected", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusOpen)
		m := seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusReached)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		_, err := svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
			MilestoneID: m.ID,
			CallerID:    ownerID,
			Status:      domain.MilestoneStatusSkipped,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, domain.ErrInvalidTenderTransition,
			"REACHED→SKIPPED must be rejected, got: %v", err)
	})
}

// TestMilestoneProgress_Integration tests the progress query against real Postgres.
func TestMilestoneProgress_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN
	ownerID := uuid.New()

	t.Run("progress aggregation is accurate after transitions", func(t *testing.T) {
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusExecuting)

		// Seed 3 milestones: we'll mark one REACHED and one SKIPPED.
		m1 := seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusPending)
		m2 := seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusPending)
		_ = seedMilestoneForService(t, ctx, dsn, listing.ID, domain.MilestoneStatusPending)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		_, err := svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
			MilestoneID: m1.ID,
			CallerID:    ownerID,
			Status:      domain.MilestoneStatusReached,
		})
		require.NoError(t, err)

		_, err = svc.UpdateMilestone(ctx, &service.UpdateMilestoneInput{
			MilestoneID: m2.ID,
			CallerID:    ownerID,
			Status:      domain.MilestoneStatusSkipped,
		})
		require.NoError(t, err)

		progress, err := svc.GetMilestoneProgress(ctx, listing.ID, ownerID)
		require.NoError(t, err)
		require.NotNil(t, progress)

		assert.Equal(t, 3, progress.Total)
		assert.Equal(t, 1, progress.Pending)
		assert.Equal(t, 1, progress.Reached)
		assert.Equal(t, 1, progress.Skipped)
	})

	t.Run("non-owner gets 404 on progress query", func(t *testing.T) {
		strangerID := uuid.New()
		listing := seedTenderListingForService(t, ctx, dsn, ownerID, domain.TenderStatusOpen)

		svc, cleanup := buildTenderTestService(t, ctx, dsn, nil)
		defer cleanup()

		_, err := svc.GetMilestoneProgress(ctx, listing.ID, strangerID)
		require.Error(t, err)
		require.ErrorIs(t, err, domain.ErrListingNotFound,
			"non-owner must get ErrListingNotFound (404 guard), got: %v", err)
	})
}
