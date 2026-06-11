package service_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/events"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTestService creates a real BidService backed by a testcontainers PG instance.
// The returned cleanup function closes the underlying pool.
func buildTestService( //nolint:gocritic // unnamedResult: return types are self-documenting (*BidService, cleanup func)
	t *testing.T,
	ctx context.Context,
	dsn string,
) (*service.BidService, func()) {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)

	bidStore := postgres.NewBidStore(pool)
	listingStore := postgres.NewListingStore(pool)
	awardStore := postgres.NewAwardStore(pool)
	txMgr := postgres.NewTxManager(pool)
	publisher := events.NewNoopPublisher()

	return service.NewBidService(bidStore, listingStore, awardStore, txMgr, publisher, nil), pool.Close
}

// insertTestListing inserts a listing directly via the store.
func insertTestListing(t *testing.T, ctx context.Context, dsn string, l *domain.Listing) {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	require.NoError(t, postgres.NewListingStore(pool).Create(ctx, l))
}

// insertTestBid inserts a bid directly via the store.
func insertTestBid(t *testing.T, ctx context.Context, dsn string, b *domain.Bid) {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	require.NoError(t, postgres.NewBidStore(pool).Create(ctx, b))
}

// readBidStatus fetches the current bid status directly from DB.
func readBidStatus(t *testing.T, ctx context.Context, dsn string, bidID uuid.UUID) domain.BidStatus {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	b, err := postgres.NewBidStore(pool).GetByID(ctx, bidID)
	require.NoError(t, err)

	return b.Status
}

// runConcurrentOps fires two bid operations simultaneously via a shared start gate
// and returns (errA, errB).
func runConcurrentOps(opA, opB func() error) (errA, errB error) {
	var wg sync.WaitGroup

	ready := make(chan struct{})

	wg.Add(2)

	go func() {
		defer wg.Done()
		<-ready
		errA = opA()
	}()

	go func() {
		defer wg.Done()
		<-ready
		errB = opB()
	}()

	close(ready)
	wg.Wait()

	return errA, errB
}

// concurrentBidScenario describes one pair of competing bid operations.
type concurrentBidScenario struct {
	name    string
	opA     func(svc *service.BidService, ctx context.Context, bidID, ownerID, bidderID uuid.UUID) error
	opB     func(svc *service.BidService, ctx context.Context, bidID, ownerID, bidderID uuid.UUID) error
	aStatus domain.BidStatus // expected final status when opA wins
	bStatus domain.BidStatus // expected final status when opB wins
}

// TestBidConcurrency_Integration proves that concurrent accept / reject / withdraw
// on the same bid never produce an inconsistent terminal state. Each scenario fires
// two goroutines simultaneously; exactly ONE must succeed.
func TestBidConcurrency_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN

	ownerID := uuid.New()
	bidderID := uuid.New()
	now := time.Now().UTC().Truncate(time.Millisecond)

	svc, cleanup := buildTestService(t, ctx, dsn)
	defer cleanup()

	makeListing := func(t *testing.T) *domain.Listing {
		t.Helper()

		l := &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID,
			Title:       "Concurrency test listing",
			Description: "",
			Currency:    "TWD",
			Status:      domain.ListingStatusOpen,
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		insertTestListing(t, ctx, dsn, l)

		return l
	}

	makeBid := func(t *testing.T, listingID uuid.UUID) *domain.Bid {
		t.Helper()

		b := &domain.Bid{
			ID:           uuid.New(),
			ListingID:    listingID,
			BidderUserID: bidderID,
			Amount:       decimal.NewFromInt(500),
			Currency:     "TWD",
			Message:      "concurrent test",
			Status:       domain.BidStatusPending,
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		insertTestBid(t, ctx, dsn, b)

		return b
	}

	scenarios := []concurrentBidScenario{
		{
			name: "accept vs reject",
			opA: func(svc *service.BidService, ctx context.Context, bidID, ownerID, _ uuid.UUID) error {
				_, err := svc.AcceptBid(ctx, bidID, ownerID)
				return err
			},
			opB: func(svc *service.BidService, ctx context.Context, bidID, ownerID, _ uuid.UUID) error {
				_, err := svc.RejectBid(ctx, bidID, ownerID)
				return err
			},
			aStatus: domain.BidStatusAccepted,
			bStatus: domain.BidStatusRejected,
		},
		{
			name: "reject vs withdraw",
			opA: func(svc *service.BidService, ctx context.Context, bidID, ownerID, _ uuid.UUID) error {
				_, err := svc.RejectBid(ctx, bidID, ownerID)
				return err
			},
			opB: func(svc *service.BidService, ctx context.Context, bidID, _, bidderID uuid.UUID) error {
				_, err := svc.WithdrawBid(ctx, bidID, bidderID)
				return err
			},
			aStatus: domain.BidStatusRejected,
			bStatus: domain.BidStatusWithdrawn,
		},
		{
			name: "accept vs withdraw",
			opA: func(svc *service.BidService, ctx context.Context, bidID, ownerID, _ uuid.UUID) error {
				_, err := svc.AcceptBid(ctx, bidID, ownerID)
				return err
			},
			opB: func(svc *service.BidService, ctx context.Context, bidID, _, bidderID uuid.UUID) error {
				_, err := svc.WithdrawBid(ctx, bidID, bidderID)
				return err
			},
			aStatus: domain.BidStatusAccepted,
			bStatus: domain.BidStatusWithdrawn,
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			listing := makeListing(t)
			bid := makeBid(t, listing.ID)

			sc := sc // capture

			errA, errB := runConcurrentOps(
				func() error { return sc.opA(svc, ctx, bid.ID, ownerID, bidderID) },
				func() error { return sc.opB(svc, ctx, bid.ID, ownerID, bidderID) },
			)

			successCount := 0
			if errA == nil {
				successCount++
			}

			if errB == nil {
				successCount++
			}

			assert.Equal(
				t, 1, successCount,
				"exactly one operation must succeed; opA err=%v opB err=%v",
				errA, errB,
			)

			finalStatus := readBidStatus(t, ctx, dsn, bid.ID)
			if errA == nil {
				assert.Equal(t, sc.aStatus, finalStatus,
					"opA won but final status does not match expected")
			} else {
				assert.Equal(t, sc.bStatus, finalStatus,
					"opB won but final status does not match expected")
			}
		})
	}
}

// TestCreateBid_NoGhostOnAwarded_Integration proves that a concurrent CreateBid and
// AcceptBid on the same listing never leaves a ghost PENDING bid on an AWARDED listing.
//
// Without the CreateBid transaction fix, the race window was:
//   - CreateBid reads listing (OPEN), then AcceptBid awards it, then CreateBid inserts
//     the bid — resulting in a PENDING bid on an AWARDED listing.
//
// With the fix, CreateBid holds a FOR UPDATE lock on the listing row during its tx,
// so AcceptBid either wins first (CreateBid sees AWARDED → ErrListingNotOpen) or
// CreateBid wins first (AcceptBid sees a committed PENDING bid, listing still OPEN → awards it normally).
func TestCreateBid_NoGhostOnAwarded_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ghost-bid integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN

	ownerID := uuid.New()
	bidderA := uuid.New() // the bidder that will try CreateBid
	bidderB := uuid.New() // the bidder whose bid will be accepted
	now := time.Now().UTC().Truncate(time.Millisecond)

	// Seed a listing and an existing PENDING bid by bidderB (which AcceptBid will accept).
	listing := &domain.Listing{
		ID:          uuid.New(),
		OwnerUserID: ownerID,
		Title:       "Ghost bid race listing",
		Currency:    "TWD",
		Status:      domain.ListingStatusOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	insertTestListing(t, ctx, dsn, listing)

	existingBid := &domain.Bid{
		ID:           uuid.New(),
		ListingID:    listing.ID,
		BidderUserID: bidderB,
		Amount:       decimal.NewFromInt(500),
		Currency:     "TWD",
		Message:      "existing bid",
		Status:       domain.BidStatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	insertTestBid(t, ctx, dsn, existingBid)

	svc, cleanup := buildTestService(t, ctx, dsn)
	defer cleanup()

	const iterations = 10 // run multiple times to flush out the race

	for i := range iterations {
		t.Run(fmt.Sprintf("iteration_%d", i), func(t *testing.T) {
			// Each iteration needs a fresh listing + bid to avoid cross-contamination.
			l := &domain.Listing{
				ID:          uuid.New(),
				OwnerUserID: ownerID,
				Title:       fmt.Sprintf("Race listing %d", i),
				Currency:    "TWD",
				Status:      domain.ListingStatusOpen,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			insertTestListing(t, ctx, dsn, l)

			eb := &domain.Bid{
				ID:           uuid.New(),
				ListingID:    l.ID,
				BidderUserID: bidderB,
				Amount:       decimal.NewFromInt(500),
				Currency:     "TWD",
				Message:      "race bid",
				Status:       domain.BidStatusPending,
				CreatedAt:    now,
				UpdatedAt:    now,
			}
			insertTestBid(t, ctx, dsn, eb)

			// Run CreateBid (bidderA) and AcceptBid (owner accepts bidderB) concurrently.
			errA, errB := runConcurrentOps(
				func() error {
					_, err := svc.CreateBid(ctx, &service.CreateBidInput{
						ListingID:    l.ID,
						BidderUserID: bidderA,
						Amount:       decimal.NewFromInt(300),
						Currency:     "TWD",
						Message:      "racing bid",
					})

					return err
				},
				func() error {
					_, err := svc.AcceptBid(ctx, eb.ID, ownerID)
					return err
				},
			)

			// One of four outcomes is valid:
			// A) CreateBid won first (inserted): listing stays OPEN, bidderA bid is PENDING;
			//    then AcceptBid runs normally — listing AWARDED, bidderB ACCEPTED, bidderA REJECTED.
			// B) AcceptBid won first: listing is AWARDED; CreateBid sees AWARDED → ErrListingNotOpen.
			// C) Both succeed: possible only in case A above.
			// D) CreateBid fails for any reason + AcceptBid succeeds.
			//
			// The invariant to assert: if listing is AWARDED, no PENDING bid must exist from bidderA.

			// Fetch final bids for this listing.
			pool, poolErr := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
			require.NoError(t, poolErr)
			defer pool.Close()

			bidStore := postgres.NewBidStore(pool)
			allBids, listErr := bidStore.ListByListing(ctx, l.ID)
			require.NoError(t, listErr)

			listingStore := postgres.NewListingStore(pool)
			finalListing, lErr := listingStore.GetByID(ctx, l.ID)
			require.NoError(t, lErr)

			_ = errA // either nil (bid created) or ErrListingNotOpen (race lost)
			_ = errB // either nil (accepted) or error

			if finalListing.Status == domain.ListingStatusAwarded {
				for _, b := range allBids {
					if b.BidderUserID == bidderA {
						assert.NotEqual(t, domain.BidStatusPending, b.Status,
							"ghost PENDING bid from bidderA must not exist on AWARDED listing (iter %d)", i)
					}
				}
			}
		})
	}
}

// TestBidAuthz_Integration proves that a non-owner reject and a non-bidder withdraw
// each return ErrBidNotFound (404) while leaving the bid PENDING, using real Postgres.
func TestBidAuthz_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping authz integration test in short mode")
	}

	ctx := context.Background()
	dsn := sharedServiceDSN

	ownerID := uuid.New()
	bidderID := uuid.New()
	strangerID := uuid.New()
	now := time.Now().UTC().Truncate(time.Millisecond)

	listing := &domain.Listing{
		ID:          uuid.New(),
		OwnerUserID: ownerID,
		Title:       "Authz test listing",
		Description: "",
		Currency:    "TWD",
		Status:      domain.ListingStatusOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	insertTestListing(t, ctx, dsn, listing)

	bid := &domain.Bid{
		ID:           uuid.New(),
		ListingID:    listing.ID,
		BidderUserID: bidderID,
		Amount:       decimal.NewFromInt(300),
		Currency:     "TWD",
		Message:      "authz test bid",
		Status:       domain.BidStatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	insertTestBid(t, ctx, dsn, bid)

	svc, cleanup := buildTestService(t, ctx, dsn)
	defer cleanup()

	t.Run("non-owner reject returns ErrBidNotFound and bid stays PENDING", func(t *testing.T) {
		_, err := svc.RejectBid(ctx, bid.ID, strangerID)
		require.ErrorIs(t, err, domain.ErrBidNotFound,
			"non-owner reject must return ErrBidNotFound (404), got: %v", err)

		assert.Equal(t, domain.BidStatusPending, readBidStatus(t, ctx, dsn, bid.ID),
			"bid must remain PENDING after unauthorized reject")
	})

	t.Run("non-bidder withdraw returns ErrBidNotFound and bid stays PENDING", func(t *testing.T) {
		_, err := svc.WithdrawBid(ctx, bid.ID, ownerID)
		require.ErrorIs(t, err, domain.ErrBidNotFound,
			"non-bidder withdraw must return ErrBidNotFound (404), got: %v", err)

		assert.Equal(t, domain.BidStatusPending, readBidStatus(t, ctx, dsn, bid.ID),
			"bid must remain PENDING after unauthorized withdraw")
	})
}
