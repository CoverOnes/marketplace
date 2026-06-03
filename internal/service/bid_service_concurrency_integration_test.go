package service_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/events"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store/postgres"
	migrations "github.com/CoverOnes/marketplace/migrations"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// startServiceTestDB spins up a real Postgres container and returns a DSN.
func startServiceTestDB(t *testing.T) string {
	t.Helper()

	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	return dsn
}

// runServiceMigrations applies embedded *.up.sql files against the test DB.
func runServiceMigrations(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	var upFiles []string

	err = fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	require.NoError(t, err, "walk embedded migrations FS")
	require.NotEmpty(t, upFiles, "no *.up.sql files found")

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		require.NoError(t, readErr, "read migration file %s", file)

		_, execErr := pool.Exec(ctx, string(data))
		require.NoError(t, execErr, fmt.Sprintf("apply migration %s", file))
	}
}

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
	dsn := startServiceTestDB(t)
	runServiceMigrations(t, ctx, dsn)

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

// TestBidAuthz_Integration proves that a non-owner reject and a non-bidder withdraw
// each return ErrBidNotFound (404) while leaving the bid PENDING, using real Postgres.
func TestBidAuthz_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping authz integration test in short mode")
	}

	ctx := context.Background()
	dsn := startServiceTestDB(t)
	runServiceMigrations(t, ctx, dsn)

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
