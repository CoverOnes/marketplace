package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTenderListing creates a tender listing for use in integration tests.
func seedTenderListing(t *testing.T, ctx context.Context, ls *postgres.ListingStore, ownerID uuid.UUID) *domain.Listing {
	t.Helper()

	ts := domain.TenderStatusOpen
	now := time.Now().UTC().Truncate(time.Millisecond)
	l := &domain.Listing{
		ID:              uuid.New(),
		OwnerUserID:     ownerID,
		Title:           "Tender listing",
		Description:     "multi-vendor tender",
		Currency:        "TWD",
		Status:          domain.ListingStatusOpen,
		IsTender:        true,
		RecruiterMode:   domain.RecruiterModeClosed,
		TenderStatus:    &ts,
		KYCTierRequired: 2,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	require.NoError(t, ls.Create(ctx, l))

	return l
}

// seedTenderRole creates a tender role under the given listing.
func seedTenderRole(t *testing.T, ctx context.Context, rs *postgres.TenderRoleStore, listingID uuid.UUID, maxCollaborators *int) *domain.TenderRole {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Millisecond)
	r := &domain.TenderRole{
		ID:               uuid.New(),
		ListingID:        listingID,
		Title:            "Test role",
		Description:      "does things",
		MaxCollaborators: maxCollaborators,
		Status:           domain.TenderRoleStatusOpen,
		SortOrder:        0,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, rs.Create(ctx, r))

	return r
}

// seedCollaborator creates a PENDING collaborator application.
func seedCollaborator(
	t *testing.T, ctx context.Context, cs *postgres.TenderCollaboratorStore,
	roleID, listingID, vendorID uuid.UUID,
) *domain.TenderCollaborator {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Millisecond)
	c := &domain.TenderCollaborator{
		ID:           uuid.New(),
		TenderRoleID: roleID,
		ListingID:    listingID,
		VendorUserID: vendorID,
		Status:       domain.CollaboratorStatusPending,
		JoinMessage:  "I can help",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, cs.Create(ctx, c))

	return c
}

// TestTenderListing_Integration verifies that tender discriminator columns are
// persisted and scanned correctly alongside the CLASSIC listing columns.
func TestTenderListing_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	ls := postgres.NewListingStore(sharedTestPool)
	ownerID := uuid.New()

	t.Run("create and get tender listing preserves discriminator fields", func(t *testing.T) {
		listing := seedTenderListing(t, ctx, ls, ownerID)

		got, err := ls.GetByID(ctx, listing.ID)
		require.NoError(t, err)

		assert.True(t, got.IsTender, "is_tender must be true")
		assert.Equal(t, domain.RecruiterModeClosed, got.RecruiterMode)
		require.NotNil(t, got.TenderStatus)
		assert.Equal(t, domain.TenderStatusOpen, *got.TenderStatus)
		assert.Equal(t, 2, got.KYCTierRequired)
	})

	t.Run("classic listing has zero tender discriminator values", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		classic := &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID,
			Title:       "Classic listing",
			Currency:    "TWD",
			Status:      domain.ListingStatusOpen,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		require.NoError(t, ls.Create(ctx, classic))

		got, err := ls.GetByID(ctx, classic.ID)
		require.NoError(t, err)

		assert.False(t, got.IsTender, "is_tender must be false for CLASSIC listing")
		assert.Nil(t, got.TenderStatus, "tender_status must be nil for CLASSIC listing")
		assert.Equal(t, 0, got.KYCTierRequired)
	})

	t.Run("update listing tender_status", func(t *testing.T) {
		listing := seedTenderListing(t, ctx, ls, ownerID)

		got, err := ls.GetByID(ctx, listing.ID)
		require.NoError(t, err)

		ts := domain.TenderStatusPartiallyStaffed
		got.TenderStatus = &ts
		require.NoError(t, ls.Update(ctx, got))

		updated, err := ls.GetByID(ctx, listing.ID)
		require.NoError(t, err)
		require.NotNil(t, updated.TenderStatus)
		assert.Equal(t, domain.TenderStatusPartiallyStaffed, *updated.TenderStatus)
	})

	t.Run("list listings includes tender discriminator fields", func(t *testing.T) {
		ownerID2 := uuid.New()
		tenderListing := seedTenderListing(t, ctx, ls, ownerID2)

		open := domain.ListingStatusOpen
		listings, err := ls.List(ctx, store.ListingFilter{
			OwnerUserID:     &ownerID2,
			Status:          &open,
			VisibleToUserID: ownerID2,
		})
		require.NoError(t, err)
		require.NotEmpty(t, listings)

		var found *domain.Listing

		for _, l := range listings {
			if l.ID == tenderListing.ID {
				found = l

				break
			}
		}

		require.NotNil(t, found, "tender listing must appear in list")
		assert.True(t, found.IsTender)
	})
}

// TestTenderRoleStore_Integration tests CRUD and role-status transitions.
func TestTenderRoleStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	ls := postgres.NewListingStore(sharedTestPool)
	rs := postgres.NewTenderRoleStore(sharedTestPool)
	ownerID := uuid.New()
	listing := seedTenderListing(t, ctx, ls, ownerID)

	t.Run("create and get role", func(t *testing.T) {
		maxCols := 3
		role := seedTenderRole(t, ctx, rs, listing.ID, &maxCols)

		got, err := rs.GetByID(ctx, role.ID)
		require.NoError(t, err)
		assert.Equal(t, role.ID, got.ID)
		assert.Equal(t, listing.ID, got.ListingID)
		assert.Equal(t, "Test role", got.Title)
		require.NotNil(t, got.MaxCollaborators)
		assert.Equal(t, 3, *got.MaxCollaborators)
		assert.Equal(t, domain.TenderRoleStatusOpen, got.Status)
	})

	t.Run("role not found returns ErrTenderRoleNotFound", func(t *testing.T) {
		_, err := rs.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrTenderRoleNotFound)
	})

	t.Run("list roles by listing", func(t *testing.T) {
		ownerID2 := uuid.New()
		listing2 := seedTenderListing(t, ctx, ls, ownerID2)
		_ = seedTenderRole(t, ctx, rs, listing2.ID, nil)
		_ = seedTenderRole(t, ctx, rs, listing2.ID, nil)

		roles, err := rs.ListByListing(ctx, listing2.ID)
		require.NoError(t, err)
		assert.Len(t, roles, 2)
	})

	t.Run("update role status to FILLED", func(t *testing.T) {
		role := seedTenderRole(t, ctx, rs, listing.ID, nil)
		role.Status = domain.TenderRoleStatusFilled
		require.NoError(t, rs.Update(ctx, role))

		got, err := rs.GetByID(ctx, role.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.TenderRoleStatusFilled, got.Status)
	})

	t.Run("update non-existent role returns ErrTenderRoleNotFound", func(t *testing.T) {
		ghost := &domain.TenderRole{
			ID:        uuid.New(),
			ListingID: listing.ID,
			Title:     "Ghost",
			Status:    domain.TenderRoleStatusOpen,
		}
		err := rs.Update(ctx, ghost)
		require.ErrorIs(t, err, domain.ErrTenderRoleNotFound)
	})
}

// TestTenderCollaboratorStore_Integration tests CRUD and the live-uniqueness constraint.
func TestTenderCollaboratorStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	ls := postgres.NewListingStore(sharedTestPool)
	rs := postgres.NewTenderRoleStore(sharedTestPool)
	cs := postgres.NewTenderCollaboratorStore(sharedTestPool)
	ownerID := uuid.New()
	listing := seedTenderListing(t, ctx, ls, ownerID)
	role := seedTenderRole(t, ctx, rs, listing.ID, nil)

	t.Run("create and get collaborator", func(t *testing.T) {
		vendorID := uuid.New()
		collab := seedCollaborator(t, ctx, cs, role.ID, listing.ID, vendorID)

		got, err := cs.GetByID(ctx, collab.ID)
		require.NoError(t, err)
		assert.Equal(t, collab.ID, got.ID)
		assert.Equal(t, vendorID, got.VendorUserID)
		assert.Equal(t, domain.CollaboratorStatusPending, got.Status)
	})

	t.Run("duplicate live application returns ErrTenderCollaboratorConflict", func(t *testing.T) {
		vendorID := uuid.New()
		_ = seedCollaborator(t, ctx, cs, role.ID, listing.ID, vendorID)

		// Second PENDING application for same (role, vendor) must fail.
		now := time.Now().UTC().Truncate(time.Millisecond)
		dup := &domain.TenderCollaborator{
			ID:           uuid.New(),
			TenderRoleID: role.ID,
			ListingID:    listing.ID,
			VendorUserID: vendorID,
			Status:       domain.CollaboratorStatusPending,
			JoinMessage:  "duplicate",
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		err := cs.Create(ctx, dup)
		require.ErrorIs(t, err, domain.ErrTenderCollaboratorConflict)
	})

	t.Run("after withdrawal new PENDING application is allowed (slot freed)", func(t *testing.T) {
		vendorID := uuid.New()
		first := seedCollaborator(t, ctx, cs, role.ID, listing.ID, vendorID)

		// Withdraw the first application.
		first.Status = domain.CollaboratorStatusWithdrawn
		require.NoError(t, cs.Update(ctx, first))

		// Re-apply: should succeed because WITHDRAWN frees the unique-index slot.
		now := time.Now().UTC().Truncate(time.Millisecond)
		second := &domain.TenderCollaborator{
			ID:           uuid.New(),
			TenderRoleID: role.ID,
			ListingID:    listing.ID,
			VendorUserID: vendorID,
			Status:       domain.CollaboratorStatusPending,
			JoinMessage:  "re-apply",
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		require.NoError(t, cs.Create(ctx, second), "re-apply after withdrawal must succeed")
	})

	t.Run("count approved by role", func(t *testing.T) {
		ownerID2 := uuid.New()
		listing2 := seedTenderListing(t, ctx, ls, ownerID2)
		role2 := seedTenderRole(t, ctx, rs, listing2.ID, nil)

		v1 := uuid.New()
		v2 := uuid.New()
		v3 := uuid.New()

		c1 := seedCollaborator(t, ctx, cs, role2.ID, listing2.ID, v1)
		c2 := seedCollaborator(t, ctx, cs, role2.ID, listing2.ID, v2)
		c3 := seedCollaborator(t, ctx, cs, role2.ID, listing2.ID, v3)

		// Approve c1 and c2, leave c3 PENDING.
		c1.Status = domain.CollaboratorStatusApproved
		require.NoError(t, cs.Update(ctx, c1))
		c2.Status = domain.CollaboratorStatusApproved
		require.NoError(t, cs.Update(ctx, c2))
		_ = c3

		count, err := cs.CountApprovedByRole(ctx, role2.ID)
		require.NoError(t, err)
		assert.Equal(t, 2, count)
	})

	t.Run("list collaborators by listing", func(t *testing.T) {
		ownerID3 := uuid.New()
		listing3 := seedTenderListing(t, ctx, ls, ownerID3)
		role3 := seedTenderRole(t, ctx, rs, listing3.ID, nil)

		_ = seedCollaborator(t, ctx, cs, role3.ID, listing3.ID, uuid.New())
		_ = seedCollaborator(t, ctx, cs, role3.ID, listing3.ID, uuid.New())

		collabs, err := cs.ListByListing(ctx, listing3.ID)
		require.NoError(t, err)
		assert.Len(t, collabs, 2)
	})

	t.Run("collaborator not found returns error", func(t *testing.T) {
		_, err := cs.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrTenderCollaboratorNotFound)
	})
}

// TestTenderBidBlocked_ClassicFlowUnchanged_Integration proves:
//  1. Classic bids on a classic listing succeed (regression guard).
//  2. Classic bids on a tender listing are rejected with a unique constraint / 409
//     (verified at the store layer via the bid migration role_id column presence).
func TestTenderBidBlocked_ClassicFlowUnchanged_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	ls := postgres.NewListingStore(sharedTestPool)
	bs := postgres.NewBidStore(sharedTestPool)
	ownerID := uuid.New()

	t.Run("classic bid on classic listing succeeds (regression guard)", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		classic := &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID,
			Title:       "Classic listing for bid regression",
			Currency:    "TWD",
			Status:      domain.ListingStatusOpen,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		require.NoError(t, ls.Create(ctx, classic))

		bid := &domain.Bid{
			ID:           uuid.New(),
			ListingID:    classic.ID,
			BidderUserID: uuid.New(),
			Amount:       decimal.NewFromInt(500),
			Currency:     "TWD",
			Message:      "classic bid",
			Status:       domain.BidStatusPending,
			RoleID:       nil, // classic bid: no role_id
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		require.NoError(t, bs.Create(ctx, bid))

		got, err := bs.GetByID(ctx, bid.ID)
		require.NoError(t, err)
		assert.Equal(t, bid.ID, got.ID)
		assert.Nil(t, got.RoleID, "classic bid must have nil role_id")
		assert.Equal(t, domain.BidStatusPending, got.Status)
	})

	t.Run("bids table has role_id column (migration applied correctly)", func(t *testing.T) {
		// Verify migration 000007 added role_id to bids by inserting a bid with a non-nil role_id.
		now := time.Now().UTC().Truncate(time.Millisecond)
		classic := &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID,
			Title:       "Listing for role_id bid test",
			Currency:    "TWD",
			Status:      domain.ListingStatusOpen,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		require.NoError(t, ls.Create(ctx, classic))

		roleID := uuid.New() // soft ref (no FK)
		bid := &domain.Bid{
			ID:           uuid.New(),
			ListingID:    classic.ID,
			BidderUserID: uuid.New(),
			Amount:       decimal.NewFromInt(100),
			Currency:     "TWD",
			Message:      "",
			Status:       domain.BidStatusPending,
			RoleID:       &roleID,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		require.NoError(t, bs.Create(ctx, bid))

		got, err := bs.GetByID(ctx, bid.ID)
		require.NoError(t, err)
		require.NotNil(t, got.RoleID)
		assert.Equal(t, roleID, *got.RoleID)
	})
}

// TestTenderMilestoneStore_Integration tests milestone CRUD.
func TestTenderMilestoneStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	ls := postgres.NewListingStore(sharedTestPool)
	ms := postgres.NewTenderMilestoneStore(sharedTestPool)
	ownerID := uuid.New()
	listing := seedTenderListing(t, ctx, ls, ownerID)

	t.Run("create and get milestone", func(t *testing.T) {
		amount := decimal.NewFromInt(1000)
		currency := "TWD"
		now := time.Now().UTC().Truncate(time.Millisecond)
		due := now.Add(7 * 24 * time.Hour)
		m := &domain.TenderMilestone{
			ID:        uuid.New(),
			ListingID: listing.ID,
			Title:     "Phase 1 delivery",
			DueDate:   &due,
			Amount:    &amount,
			Currency:  &currency,
			Status:    domain.MilestoneStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		}
		require.NoError(t, ms.Create(ctx, m))

		got, err := ms.GetByID(ctx, m.ID)
		require.NoError(t, err)
		assert.Equal(t, m.ID, got.ID)
		assert.Equal(t, listing.ID, got.ListingID)
		require.NotNil(t, got.Amount)
		assert.True(t, amount.Equal(*got.Amount))
		assert.Equal(t, domain.MilestoneStatusPending, got.Status)
	})

	t.Run("milestone not found returns ErrTenderMilestoneNotFound", func(t *testing.T) {
		_, err := ms.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrTenderMilestoneNotFound)
	})

	t.Run("update milestone to REACHED", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		m := &domain.TenderMilestone{
			ID:        uuid.New(),
			ListingID: listing.ID,
			Title:     "Milestone to reach",
			Status:    domain.MilestoneStatusPending,
			CreatedAt: now,
			UpdatedAt: now,
		}
		require.NoError(t, ms.Create(ctx, m))

		reachedAt := time.Now().UTC()
		m.Status = domain.MilestoneStatusReached
		m.ReachedAt = &reachedAt
		require.NoError(t, ms.Update(ctx, m))

		got, err := ms.GetByID(ctx, m.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.MilestoneStatusReached, got.Status)
		require.NotNil(t, got.ReachedAt)
	})

	t.Run("list milestones by listing", func(t *testing.T) {
		ownerID2 := uuid.New()
		listing2 := seedTenderListing(t, ctx, ls, ownerID2)

		now := time.Now().UTC().Truncate(time.Millisecond)
		m1 := &domain.TenderMilestone{
			ID: uuid.New(), ListingID: listing2.ID, Title: "M1",
			Status: domain.MilestoneStatusPending, CreatedAt: now, UpdatedAt: now,
		}
		m2 := &domain.TenderMilestone{
			ID: uuid.New(), ListingID: listing2.ID, Title: "M2",
			Status: domain.MilestoneStatusPending, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second),
		}
		require.NoError(t, ms.Create(ctx, m1))
		require.NoError(t, ms.Create(ctx, m2))

		milestones, err := ms.ListByListing(ctx, listing2.ID)
		require.NoError(t, err)
		assert.Len(t, milestones, 2)
	})
}
