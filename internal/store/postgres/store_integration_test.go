package postgres_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/CoverOnes/marketplace/internal/store/postgres"
	migrations "github.com/CoverOnes/marketplace/migrations"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// startTestDB spins up a real Postgres container via testcontainers.
func startTestDB(t *testing.T) string {
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

// runMigrations applies embedded *.up.sql files against the test DB.
func runMigrations(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{}) // empty schema = public (test default)
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

// TestSchemaIsolation_Integration verifies that NewPool with a non-empty schema
// creates the schema and isolates migrations within it. It builds a pool with
// schema="dev_test_schema", runs migrations against that schema, then asserts
// that the service's main table (listings) exists in information_schema.tables
// under table_schema='dev_test_schema' and NOT in 'public'.
func TestSchemaIsolation_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	const testSchema = "dev_test_schema"

	ctx := context.Background()
	dsn := startTestDB(t)

	// Build pool with non-empty schema — this should CREATE SCHEMA IF NOT EXISTS
	// and SET search_path on every connection.
	pool, err := postgres.NewPool(ctx, dsn, testSchema, postgres.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	// Run migrations through the schema-aware pool so all tables land in testSchema.
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
		require.NoError(t, execErr, "apply migration %s in schema %s", file, testSchema)
	}

	t.Run("listings table exists in dev_test_schema", func(t *testing.T) {
		var count int

		err := pool.QueryRow(
			ctx,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2",
			testSchema, "listings",
		).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "listings table must exist in schema %q", testSchema)
	})

	t.Run("listings table does NOT exist in public schema", func(t *testing.T) {
		var count int

		err := pool.QueryRow(
			ctx,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'listings'",
		).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 0, count, "listings table must NOT exist in public schema when using schema isolation")
	})
}

// TestReservedWordSchema_Integration verifies that NewPool correctly double-quotes PG
// reserved-word schema names (e.g. "user") so they do not cause a 42601 syntax error.
// The existing TestSchemaIsolation_Integration used "dev_test_schema" which is not a
// reserved word and therefore did not exercise the quoting path.
func TestReservedWordSchema_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// "user" is a PG reserved word; without double-quoting it causes:
	//   ERROR 42601: syntax error at or near "user"
	const reservedSchema = "user"

	ctx := context.Background()
	dsn := startTestDB(t)

	// NewPool must CREATE SCHEMA IF NOT EXISTS "user" and SET search_path = "user", public
	// without hitting syntax error 42601.
	pool, err := postgres.NewPool(ctx, dsn, reservedSchema, postgres.PoolOptions{})
	require.NoError(t, err, "NewPool with reserved-word schema %q must not fail", reservedSchema)

	defer pool.Close()

	// Run migrations through the reserved-word schema pool so all tables land in "user".
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
		require.NoError(t, execErr, "apply migration %s in schema %q", file, reservedSchema)
	}

	t.Run("listings table exists in user schema", func(t *testing.T) {
		var count int

		err := pool.QueryRow(
			ctx,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2",
			reservedSchema, "listings",
		).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "listings table must exist in reserved-word schema %q", reservedSchema)
	})

	t.Run("listings table does NOT exist in public schema", func(t *testing.T) {
		var count int

		err := pool.QueryRow(
			ctx,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'listings'",
		).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 0, count, "listings table must NOT exist in public schema when using reserved-word schema %q", reservedSchema)
	})
}

// TestListingStore_Integration tests the ListingStore against a real Postgres instance.
func TestListingStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{}) // empty schema = public (test default)
	require.NoError(t, err)

	defer pool.Close()

	listingStore := postgres.NewListingStore(pool)

	ownerID := uuid.New()

	t.Run("create and get listing", func(t *testing.T) {
		budgetMin := decimal.NewFromInt(100)
		budgetMax := decimal.NewFromInt(500)
		now := time.Now().UTC().Truncate(time.Millisecond)

		l := &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID,
			Title:       "Integration test listing",
			Description: "test description",
			BudgetMin:   &budgetMin,
			BudgetMax:   &budgetMax,
			Currency:    "TWD",
			Status:      domain.ListingStatusOpen,
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		require.NoError(t, listingStore.Create(ctx, l))

		got, err := listingStore.GetByID(ctx, l.ID)
		require.NoError(t, err)
		assert.Equal(t, l.ID, got.ID)
		assert.Equal(t, l.OwnerUserID, got.OwnerUserID)
		assert.Equal(t, l.Title, got.Title)
		assert.Equal(t, l.Status, got.Status)
		require.NotNil(t, got.BudgetMin)
		assert.True(t, budgetMin.Equal(*got.BudgetMin))
	})

	t.Run("get listing not found", func(t *testing.T) {
		_, err := listingStore.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrListingNotFound)
	})

	t.Run("list listings by owner", func(t *testing.T) {
		ownerID2 := uuid.New()
		now := time.Now().UTC().Truncate(time.Millisecond)

		l := &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID2,
			Title:       "Owner2 listing",
			Description: "",
			Currency:    "TWD",
			Status:      domain.ListingStatusOpen,
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		require.NoError(t, listingStore.Create(ctx, l))

		listings, err := listingStore.List(ctx, store.ListingFilter{
			OwnerUserID: &ownerID2,
		})
		require.NoError(t, err)
		assert.Len(t, listings, 1)
		assert.Equal(t, l.ID, listings[0].ID)
	})

	t.Run("update listing status", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		l := &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID,
			Title:       "To update",
			Description: "",
			Currency:    "TWD",
			Status:      domain.ListingStatusOpen,
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		require.NoError(t, listingStore.Create(ctx, l))

		l.Status = domain.ListingStatusAwarded
		bidID := uuid.New()
		l.AwardedBidID = &bidID

		require.NoError(t, listingStore.Update(ctx, l))

		got, err := listingStore.GetByID(ctx, l.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.ListingStatusAwarded, got.Status)
		require.NotNil(t, got.AwardedBidID)
		assert.Equal(t, bidID, *got.AwardedBidID)
	})
}

// seedSearchListing inserts a listing with explicit timestamps for deterministic
// ordering in search integration tests.
func seedSearchListing(
	t *testing.T, ctx context.Context, ls *postgres.ListingStore,
	owner uuid.UUID, title, desc, status string, createdAt time.Time,
	budgetMin, budgetMax *decimal.Decimal,
) *domain.Listing {
	t.Helper()

	l := &domain.Listing{
		ID:          uuid.New(),
		OwnerUserID: owner,
		Title:       title,
		Description: desc,
		BudgetMin:   budgetMin,
		BudgetMax:   budgetMax,
		Currency:    "TWD",
		Status:      domain.ListingStatus(status),
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}
	require.NoError(t, ls.Create(ctx, l))

	return l
}

func dptr(v int64) *decimal.Decimal {
	d := decimal.NewFromInt(v)
	return &d
}

// TestListingStore_Search_Integration exercises full-text search, structured
// filters, and keyset pagination against a real Postgres + the 000004 GIN index.
func TestListingStore_Search_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{})
	require.NoError(t, err)

	defer pool.Close()

	ls := postgres.NewListingStore(pool)
	owner := uuid.New()
	stranger := uuid.New()
	base := time.Now().UTC().Truncate(time.Millisecond)

	// Full-text + budget candidates (newest first: go1 > go2 > go3).
	go1 := seedSearchListing(t, ctx, ls, owner, "Senior Go developer wanted", "build a payments API",
		"OPEN", base.Add(-1*time.Minute), dptr(100), dptr(500))
	go2 := seedSearchListing(t, ctx, ls, owner, "Golang backend engineer", "go microservices and gRPC",
		"OPEN", base.Add(-2*time.Minute), dptr(200), dptr(800))
	go3 := seedSearchListing(t, ctx, ls, owner, "Need a Go developer", "concurrency expert",
		"OPEN", base.Add(-3*time.Minute), nil, nil)
	// Non-matching keyword.
	_ = seedSearchListing(t, ctx, ls, owner, "React frontend designer", "tailwind ui work",
		"OPEN", base.Add(-4*time.Minute), nil, nil)
	// Matching keyword but CLOSED + owned by stranger (visibility handled in service; store returns it).
	closedStranger := seedSearchListing(t, ctx, ls, stranger, "Go developer position (closed)", "already filled",
		"CLOSED", base.Add(-30*time.Second), nil, nil)

	t.Run("full-text keyword returns matching listings newest-first", func(t *testing.T) {
		got, err := ls.Search(ctx, store.SearchFilter{Query: "go developer", VisibleToUserID: owner, Limit: 10})
		require.NoError(t, err)

		ids := idsOf(got)
		// plainto_tsquery('simple', 'go developer') requires BOTH lexemes. The
		// 'simple' config does not stem, so go2 ("Golang ... go microservices")
		// lacks the "developer" lexeme and is excluded. closedStranger is CLOSED +
		// owned by the stranger, so it is hidden from the owner via the SQL
		// visibility clause.
		assert.Contains(t, ids, go1.ID)
		assert.NotContains(t, ids, go2.ID)
		assert.Contains(t, ids, go3.ID)
		assert.NotContains(t, ids, closedStranger.ID)
		// Newest-first ordering (created_at DESC): go1 > go3.
		assert.True(t, indexOf(ids, go1.ID) < indexOf(ids, go3.ID), "go1 newer than go3")
	})

	t.Run("visibility: stranger CLOSED listing visible to its owner", func(t *testing.T) {
		got, err := ls.Search(ctx, store.SearchFilter{Query: "go developer", VisibleToUserID: stranger, Limit: 10})
		require.NoError(t, err)
		ids := idsOf(got)
		// From the stranger's perspective their CLOSED listing is visible; the
		// owner's OPEN listings remain public too.
		assert.Contains(t, ids, closedStranger.ID)
		assert.Contains(t, ids, go1.ID)
	})

	t.Run("keyword excludes non-matching listings", func(t *testing.T) {
		got, err := ls.Search(ctx, store.SearchFilter{Query: "react designer", VisibleToUserID: owner, Limit: 10})
		require.NoError(t, err)
		ids := idsOf(got)
		assert.NotContains(t, ids, go1.ID)
		assert.NotContains(t, ids, go2.ID)
	})

	t.Run("status filter restricts to CLOSED (owner of that listing)", func(t *testing.T) {
		closed := domain.ListingStatusClosed
		got, err := ls.Search(ctx, store.SearchFilter{
			Query: "go", Status: &closed, VisibleToUserID: stranger, Limit: 10,
		})
		require.NoError(t, err)
		ids := idsOf(got)
		assert.Equal(t, []uuid.UUID{closedStranger.ID}, ids)
	})

	t.Run("budget range filter", func(t *testing.T) {
		// minBudget=600 keeps listings whose budget_max >= 600 OR budget_max is NULL.
		// go1(max 500) is excluded; go2(max 800) kept; go3(NULL) kept.
		got, err := ls.Search(ctx, store.SearchFilter{
			Query: "go", BudgetMin: dptr(600), VisibleToUserID: owner, Limit: 10,
		})
		require.NoError(t, err)
		ids := idsOf(got)
		assert.NotContains(t, ids, go1.ID)
		assert.Contains(t, ids, go2.ID)
		assert.Contains(t, ids, go3.ID)
	})

	t.Run("keyset pagination walks all pages without overlap", func(t *testing.T) {
		// OPEN listings whose text contains the "go" lexeme, visible to the owner:
		// go1, go2, go3 (3 rows, newest-first), page size 2.
		page1, err := ls.Search(ctx, store.SearchFilter{Query: "go", VisibleToUserID: owner, Limit: 2})
		require.NoError(t, err)
		require.Len(t, page1, 2)
		assert.Equal(t, go1.ID, page1[0].ID)
		assert.Equal(t, go2.ID, page1[1].ID)

		last := page1[len(page1)-1]
		page2, err := ls.Search(ctx, store.SearchFilter{
			Query:           "go",
			VisibleToUserID: owner,
			Limit:           2,
			After:           &store.SearchCursor{CreatedAt: last.CreatedAt, ID: last.ID},
		})
		require.NoError(t, err)
		require.Len(t, page2, 1)
		assert.Equal(t, go3.ID, page2[0].ID)
	})

	t.Run("soft-deleted listings are excluded", func(t *testing.T) {
		del := seedSearchListing(t, ctx, ls, owner, "Go developer to be deleted", "temp",
			"OPEN", base.Add(-10*time.Second), nil, nil)
		// Soft delete via direct update of deleted_at (no FK, no special API needed).
		_, err := pool.Exec(ctx, "UPDATE listings SET deleted_at = now() WHERE id = $1", del.ID)
		require.NoError(t, err)

		got, err := ls.Search(ctx, store.SearchFilter{Query: "go developer", VisibleToUserID: owner, Limit: 50})
		require.NoError(t, err)
		assert.NotContains(t, idsOf(got), del.ID)
	})
}

func idsOf(ls []*domain.Listing) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.ID)
	}

	return out
}

func indexOf(ids []uuid.UUID, target uuid.UUID) int {
	for i, id := range ids {
		if id == target {
			return i
		}
	}

	return -1
}

// TestBidStore_Integration tests the BidStore against a real Postgres instance.
func TestBidStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{}) // empty schema = public (test default)
	require.NoError(t, err)

	defer pool.Close()

	bidStore := postgres.NewBidStore(pool)
	listingStore := postgres.NewListingStore(pool)

	ownerID := uuid.New()
	bidderID := uuid.New()
	listingID := uuid.New()

	// Create a listing first (soft ref, no FK needed).
	now := time.Now().UTC().Truncate(time.Millisecond)
	listing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Title:       "Bid test listing",
		Description: "",
		Currency:    "TWD",
		Status:      domain.ListingStatusOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, listingStore.Create(ctx, listing))

	t.Run("create and get bid", func(t *testing.T) {
		b := &domain.Bid{
			ID:           uuid.New(),
			ListingID:    listingID,
			BidderUserID: bidderID,
			Amount:       decimal.NewFromInt(500),
			Currency:     "TWD",
			Message:      "I can deliver",
			Status:       domain.BidStatusPending,
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		require.NoError(t, bidStore.Create(ctx, b))

		got, err := bidStore.GetByID(ctx, b.ID)
		require.NoError(t, err)
		assert.Equal(t, b.ID, got.ID)
		assert.Equal(t, b.ListingID, got.ListingID)
		assert.True(t, b.Amount.Equal(got.Amount))
		assert.Equal(t, domain.BidStatusPending, got.Status)
	})

	t.Run("duplicate pending bid returns ErrBidAlreadyExists", func(t *testing.T) {
		bidderID2 := uuid.New()
		b1 := &domain.Bid{
			ID:           uuid.New(),
			ListingID:    listingID,
			BidderUserID: bidderID2,
			Amount:       decimal.NewFromInt(200),
			Currency:     "TWD",
			Status:       domain.BidStatusPending,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		require.NoError(t, bidStore.Create(ctx, b1))

		b2 := &domain.Bid{
			ID:           uuid.New(),
			ListingID:    listingID,
			BidderUserID: bidderID2, // same bidder
			Amount:       decimal.NewFromInt(300),
			Currency:     "TWD",
			Status:       domain.BidStatusPending,
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		err := bidStore.Create(ctx, b2)
		require.ErrorIs(t, err, domain.ErrBidAlreadyExists)
	})

	t.Run("bid not found", func(t *testing.T) {
		_, err := bidStore.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrBidNotFound)
	})

	t.Run("reject sibling bids", func(t *testing.T) {
		// Use a new listing to avoid state interference.
		newListingID := uuid.New()
		newListing := &domain.Listing{
			ID:          newListingID,
			OwnerUserID: ownerID,
			Title:       "Sibling test",
			Description: "",
			Currency:    "TWD",
			Status:      domain.ListingStatusOpen,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		require.NoError(t, listingStore.Create(ctx, newListing))

		bidder1 := uuid.New()
		bidder2 := uuid.New()
		bidder3 := uuid.New()

		b1 := &domain.Bid{
			ID: uuid.New(), ListingID: newListingID, BidderUserID: bidder1,
			Amount: decimal.NewFromInt(100), Currency: "TWD", Status: domain.BidStatusPending,
			CreatedAt: now, UpdatedAt: now,
		}
		b2 := &domain.Bid{
			ID: uuid.New(), ListingID: newListingID, BidderUserID: bidder2,
			Amount: decimal.NewFromInt(200), Currency: "TWD", Status: domain.BidStatusPending,
			CreatedAt: now, UpdatedAt: now,
		}
		b3 := &domain.Bid{
			ID: uuid.New(), ListingID: newListingID, BidderUserID: bidder3,
			Amount: decimal.NewFromInt(300), Currency: "TWD", Status: domain.BidStatusPending,
			CreatedAt: now, UpdatedAt: now,
		}

		require.NoError(t, bidStore.Create(ctx, b1))
		require.NoError(t, bidStore.Create(ctx, b2))
		require.NoError(t, bidStore.Create(ctx, b3))

		// Accept b1, reject siblings b2 and b3.
		require.NoError(t, bidStore.RejectSiblingBids(ctx, newListingID, b1.ID))

		got2, err := bidStore.GetByID(ctx, b2.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.BidStatusRejected, got2.Status)

		got3, err := bidStore.GetByID(ctx, b3.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.BidStatusRejected, got3.Status)

		// b1 should still be PENDING (it's the accepted one, not yet updated by this call).
		got1, err := bidStore.GetByID(ctx, b1.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.BidStatusPending, got1.Status)
	})
}

// TestAwardStore_Integration tests the AwardStore against a real Postgres instance.
func TestAwardStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	dsn := startTestDB(t)
	runMigrations(t, ctx, dsn)

	pool, err := postgres.NewPool(ctx, dsn, "", postgres.PoolOptions{}) // empty schema = public (test default)
	require.NoError(t, err)

	defer pool.Close()

	awardStore := postgres.NewAwardStore(pool)

	ownerID := uuid.New()
	bidderID := uuid.New()
	listingID := uuid.New()
	bidID := uuid.New()

	now := time.Now().UTC().Truncate(time.Millisecond)

	t.Run("create and get award", func(t *testing.T) {
		a := &domain.Award{
			ID:           uuid.New(),
			ListingID:    listingID,
			BidID:        bidID,
			OwnerUserID:  ownerID,
			BidderUserID: bidderID,
			Amount:       decimal.NewFromInt(500),
			Currency:     "TWD",
			CreatedAt:    now,
		}

		require.NoError(t, awardStore.Create(ctx, a))

		got, err := awardStore.GetByID(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, a.ID, got.ID)
		assert.Equal(t, a.ListingID, got.ListingID)
		assert.True(t, a.Amount.Equal(got.Amount))
		assert.Nil(t, got.EventPublishedAt)
	})

	t.Run("award not found", func(t *testing.T) {
		_, err := awardStore.GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, domain.ErrAwardNotFound)
	})

	t.Run("duplicate award for same listing returns conflict", func(t *testing.T) {
		a1 := &domain.Award{
			ID:           uuid.New(),
			ListingID:    uuid.New(), // unique listing
			BidID:        uuid.New(),
			OwnerUserID:  ownerID,
			BidderUserID: bidderID,
			Amount:       decimal.NewFromInt(100),
			Currency:     "TWD",
			CreatedAt:    now,
		}
		require.NoError(t, awardStore.Create(ctx, a1))

		a2 := &domain.Award{
			ID:           uuid.New(),
			ListingID:    a1.ListingID, // same listing
			BidID:        uuid.New(),
			OwnerUserID:  ownerID,
			BidderUserID: bidderID,
			Amount:       decimal.NewFromInt(200),
			Currency:     "TWD",
			CreatedAt:    now,
		}

		err := awardStore.Create(ctx, a2)
		require.ErrorIs(t, err, domain.ErrConflict)
	})

	t.Run("mark event published", func(t *testing.T) {
		a := &domain.Award{
			ID:           uuid.New(),
			ListingID:    uuid.New(),
			BidID:        uuid.New(),
			OwnerUserID:  ownerID,
			BidderUserID: bidderID,
			Amount:       decimal.NewFromInt(300),
			Currency:     "TWD",
			CreatedAt:    now,
		}

		require.NoError(t, awardStore.Create(ctx, a))
		require.NoError(t, awardStore.MarkEventPublished(ctx, a.ID))

		got, err := awardStore.GetByID(ctx, a.ID)
		require.NoError(t, err)
		assert.NotNil(t, got.EventPublishedAt)
	})
}
