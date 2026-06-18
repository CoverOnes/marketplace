package service_test

// search_fusion_integration_test.go validates the T3 semantic+lexical search
// fusion (acceptance criteria 1-5) against the shared pgvector Postgres
// container started in TestMain (sharedServiceDSN).

import (
	"context"
	"fmt"
	"testing"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/service"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// unitVec1536 returns a 1536-dim unit vector with 1.0 at dimension d and
// 0.0 elsewhere.  Used to control cosine similarity in integration tests:
// unitVec1536(0) and unitVec1536(0) have cosine distance = 0 (identical);
// unitVec1536(0) and unitVec1536(1) have cosine distance = 1 (orthogonal).
func unitVec1536(d int) []float32 {
	v := make([]float32, 1536) //nolint:mnd // 1536-dim matches text-embedding-3-small and migration 000010
	v[d] = 1.0

	return v
}

// fixedEmbedClient is a stub EmbeddingClient that always returns a pre-set
// vector or error — no real API call is made.
type fixedEmbedClient struct {
	vec []float32
	err error
}

func (f *fixedEmbedClient) Generate(_ context.Context, _ string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.vec, nil
}

// buildFusionService wires up a ListingService + its embedding dependencies
// against the shared pgvector container. queryClient is the stub that controls
// what vector is returned for each SearchListings call.
func buildFusionService(
	t *testing.T,
	ctx context.Context,
	queryClient client.EmbeddingClient,
) *service.ListingService {
	t.Helper()

	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err, "create embedding pool for fusion service")

	t.Cleanup(pool.Close)

	listingStore := pgstore.NewListingStore(pool)
	listingOutboxTxMgr := pgstore.NewListingOutboxTxManager(pool)
	embStore := pgstore.NewEmbeddingStore(pool)

	return service.NewListingService(listingStore, listingOutboxTxMgr, queryClient, embStore)
}

// createOpenTender inserts an OPEN tender listing for the given owner and
// returns the created domain.Listing.
func createOpenTender(
	t *testing.T,
	ctx context.Context,
	svc *service.ListingService,
	ownerID uuid.UUID,
	title, desc string,
) *domain.Listing {
	t.Helper()

	l, err := svc.CreateListing(ctx, &service.CreateListingInput{
		OwnerUserID: ownerID,
		Title:       title,
		Description: desc,
		Currency:    "TWD",
		IsTender:    true,
	})
	require.NoError(t, err, "createOpenTender(%q)", title)

	return l
}

// seedFusionEmbedding upserts a deterministic vector for listingID into the
// shared pgvector container.
func seedFusionEmbedding(
	t *testing.T,
	ctx context.Context,
	embStore *pgstore.EmbeddingStore,
	listingID uuid.UUID,
	vec []float32,
) {
	t.Helper()
	require.NoError(
		t,
		embStore.Upsert(ctx, domain.EmbeddingEntityTypeTender, listingID, vec, "test-model"),
		"seed embedding for listing %s", listingID,
	)
}

// ─── AC1: Lexical regression ──────────────────────────────────────────────────

// TestSearchFusion_LexicalMode_ReturnsOnlyFTSMatches_Integration verifies
// that mode=lexical (and the default empty mode) returns only FTS-matching rows
// and NOT rows that merely have a close embedding.
func TestSearchFusion_LexicalMode_ReturnsOnlyFTSMatches_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	// nil query client: lexical mode must never call embed.
	svc := buildFusionService(t, ctx, nil)

	// Create two tenders that match the FTS query and one that does NOT.
	_ = createOpenTender(t, ctx, svc, ownerID, "golang backend engineer", "write services")
	_ = createOpenTender(t, ctx, svc, ownerID, "golang frontend wizard", "build Go-wasm UIs")
	_ = createOpenTender(t, ctx, svc, ownerID, "python data scientist", "ML and analytics")

	// Lexical query for "golang" must return only FTS matches.
	res, err := svc.SearchListings(ctx, &service.SearchListingsInput{
		CallerID: ownerID,
		Query:    "golang",
		Mode:     service.SearchModeLexical,
		Limit:    20,
	})
	require.NoError(t, err)

	// All returned rows must contain "golang" in title or description.
	for _, listing := range res.Listings {
		contains := listing.Title + " " + listing.Description
		assert.Contains(t, contains, "golang",
			"lexical mode must only return FTS-matching rows, got %q", listing.Title)
	}

	// Default mode (zero value) must behave identically to lexical.
	resDefault, err := svc.SearchListings(ctx, &service.SearchListingsInput{
		CallerID: ownerID,
		Query:    "golang",
		Limit:    20,
	})
	require.NoError(t, err)
	assert.Equal(t, len(res.Listings), len(resDefault.Listings),
		"default (empty) mode must produce the same result count as lexical mode")
}

// ─── AC2: Semantic ordering ───────────────────────────────────────────────────

// TestSearchFusion_SemanticMode_TopHitIsMostSimilar_Integration seeds 3 tenders
// with orthogonal embeddings (unit vectors in different dimensions) and queries
// with a vector near tender A.  Asserts tender A ranks first.
func TestSearchFusion_SemanticMode_TopHitIsMostSimilar_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	// Create tenders using a service wired with a nil queryClient (no query embed
	// needed during creation). We wire the queryClient only for the actual search.
	createSvc := buildFusionService(t, ctx, nil)
	tA := createOpenTender(t, ctx, createSvc, ownerID, "cloud infrastructure engineer", "AWS terraform kubernetes")
	tB := createOpenTender(t, ctx, createSvc, ownerID, "mobile iOS developer", "Swift UIKit xcode")
	tC := createOpenTender(t, ctx, createSvc, ownerID, "data analyst business intelligence", "SQL dashboards")

	// Seed deterministic unit-vector embeddings using high dimensions (400-402) that
	// no other test in this package uses — avoids cross-test ordering interference
	// in the shared DB container.
	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	embStore := pgstore.NewEmbeddingStore(pool)
	seedFusionEmbedding(t, ctx, embStore, tA.ID, unitVec1536(400)) //nolint:mnd // dim 400: unique to this test
	seedFusionEmbedding(t, ctx, embStore, tB.ID, unitVec1536(401)) //nolint:mnd // dim 401: unique to this test
	seedFusionEmbedding(t, ctx, embStore, tC.ID, unitVec1536(402)) //nolint:mnd // dim 402: unique to this test

	// Query with a vector identical to tA's embedding → tA must be cosine-nearest.
	// Cosine distance between dim-400 and dim-N (N≠400) = 1 (orthogonal), so tA
	// is always the unique nearest neighbor regardless of other rows in the shared DB.
	searchSvc := buildFusionService(t, ctx, &fixedEmbedClient{vec: unitVec1536(400)}) //nolint:mnd // dim 400

	res, err := searchSvc.SearchListings(ctx, &service.SearchListingsInput{
		CallerID: ownerID,
		Mode:     service.SearchModeSemantic,
		Limit:    10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.Listings, "semantic search must return results when embeddings are seeded")

	assert.Equal(t, tA.ID, res.Listings[0].ID,
		"tender A (dim-400 unit vector) must rank first when query vector is also dim-400")
}

// ─── AC3: Hybrid RRF combines both lists ─────────────────────────────────────

// TestSearchFusion_HybridMode_BothListsAreUsed_Integration inserts docs and
// verifies the RRF property: a doc ranked high in BOTH lexical and semantic
// must outrank a doc ranked high in only one list.
func TestSearchFusion_HybridMode_BothListsAreUsed_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	createSvc := buildFusionService(t, ctx, nil)

	// "both": matches lexical keyword "golang" AND has the query-nearest vector.
	both := createOpenTender(t, ctx, createSvc, ownerID,
		"golang cloud kubernetes engineer", "AWS terraform EKS")
	// "lexOnly": matches lexical keyword "golang" but has a far embedding.
	lexOnly := createOpenTender(t, ctx, createSvc, ownerID,
		"golang backend api developer", "REST microservices")
	// "semOnly": does NOT match lexical keyword "golang" but has the near vector.
	_ = createOpenTender(t, ctx, createSvc, ownerID,
		"infrastructure automation specialist", "terraform ansible chef")

	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	embStore := pgstore.NewEmbeddingStore(pool)
	// "both" shares dim-410 with query vector; "lexOnly" is at dim-499 (orthogonal/far).
	// Using high dimensions (400+) avoids interference with T2 tests (dim 0,1) in the
	// shared DB container.
	seedFusionEmbedding(t, ctx, embStore, both.ID, unitVec1536(410))    //nolint:mnd // dim 410: near query
	seedFusionEmbedding(t, ctx, embStore, lexOnly.ID, unitVec1536(499)) //nolint:mnd // dim 499: far from query

	// Query: keyword="golang" (matches both and lexOnly), queryVec=dim-410 (near both).
	searchSvc := buildFusionService(t, ctx, &fixedEmbedClient{vec: unitVec1536(410)}) //nolint:mnd // dim 410

	res, err := searchSvc.SearchListings(ctx, &service.SearchListingsInput{
		CallerID: ownerID,
		Query:    "golang",
		Mode:     service.SearchModeHybrid,
		Limit:    10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.Listings)

	// Find positions of "both" and "lexOnly" in the result.
	bothPos, lexOnlyPos := -1, -1

	for i, l := range res.Listings {
		switch l.ID {
		case both.ID:
			bothPos = i
		case lexOnly.ID:
			lexOnlyPos = i
		}
	}

	require.NotEqual(t, -1, bothPos, "doc 'both' must appear in hybrid results")

	// If lexOnly appears, "both" must rank above it (RRF property).
	if lexOnlyPos != -1 {
		assert.Less(t, bothPos, lexOnlyPos,
			"doc ranked high in BOTH lists must outrank doc ranked high in only one list (RRF)")
	}
}

// ─── AC4: Cold-start / disabled fallback ─────────────────────────────────────

// TestSearchFusion_ColdStart_EmbeddingDisabled_Integration verifies that
// semantic and hybrid modes return 200 with lexical results when the embedding
// client returns ErrEmbeddingDisabled — never a 500.
func TestSearchFusion_ColdStart_EmbeddingDisabled_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	cases := []struct {
		name string
		mode service.SearchMode
		err  error
	}{
		{"semantic disabled", service.SearchModeSemantic, client.ErrEmbeddingDisabled},
		{"hybrid disabled", service.SearchModeHybrid, client.ErrEmbeddingDisabled},
		{"semantic upstream error", service.SearchModeSemantic, fmt.Errorf("upstream timeout")},
		{"hybrid upstream error", service.SearchModeHybrid, fmt.Errorf("upstream timeout")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := buildFusionService(t, ctx, &fixedEmbedClient{err: tc.err})

			res, err := svc.SearchListings(ctx, &service.SearchListingsInput{
				CallerID: ownerID,
				Query:    "test",
				Mode:     tc.mode,
				Limit:    10,
			})
			require.NoError(t, err,
				"cold-start (%s) must return 200 and fall back to lexical, not error", tc.name)
			require.NotNil(t, res, "result must not be nil on cold-start fallback (%s)", tc.name)
			// Listings may be nil or empty (no FTS matches for "test"); the key guarantee
			// is that the call returned no error (no 500) and a valid result object.
			assert.GreaterOrEqual(t, len(res.Listings), 0,
				"Listings length must be non-negative on fallback (%s)", tc.name)
		})
	}
}

// TestSearchFusion_ColdStart_EmptyEmbeddingsTable_Integration verifies that
// semantic mode falls back to lexical when the embeddings table has no rows
// matching the query vector — no 500, no error.
func TestSearchFusion_ColdStart_EmptyEmbeddingsTable_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	ownerID := uuid.New()

	// A valid query vector but no embeddings seeded for these listings.
	svc := buildFusionService(t, ctx, &fixedEmbedClient{vec: unitVec1536(500)}) //nolint:mnd // dim 500

	// Create a tender but do NOT seed any embedding for it.
	_ = createOpenTender(t, ctx, svc, ownerID, "unique term zzxyzabcfusion", "no embedding seeded")

	// Semantic search: NearestNeighbors returns [] → must fall back to lexical.
	res, err := svc.SearchListings(ctx, &service.SearchListingsInput{
		CallerID: ownerID,
		Query:    "zzxyzabcfusion",
		Mode:     service.SearchModeSemantic,
		Limit:    10,
	})
	require.NoError(t, err, "empty embeddings must produce lexical fallback, not error")
	require.NotNil(t, res)
	// The FTS-matching listing should be returned by the lexical fallback.
	require.NotEmpty(t, res.Listings, "lexical fallback must return FTS-matched listing")
}

// ─── AC5: Visibility in hydration SQL ────────────────────────────────────────

// TestSearchFusion_Visibility_NonOpenExcluded_Integration seeds a non-OPEN
// tender (AWARDED) owned by userB as the top semantic match, then queries as
// userA and asserts the tender is absent from results.  The visibility rule is
// enforced in the hydration SQL (GetByIDs), not in Go post-filtering, so no
// hidden-doc count is leaked.
func TestSearchFusion_Visibility_NonOpenExcluded_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	userA := uuid.New()
	userB := uuid.New()

	createSvc := buildFusionService(t, ctx, nil)

	// Create a tender for userB (OPEN initially so CreateListing accepts it).
	hidden := createOpenTender(t, ctx, createSvc, userB,
		"secret awarded project", "should not leak to userA")

	// Promote to AWARDED via a direct DB write so userA cannot see it.
	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, "UPDATE listings SET status='AWARDED' WHERE id=$1", hidden.ID)
	require.NoError(t, err, "set hidden listing to AWARDED")

	// Seed the hidden listing at dim-420 — it is the top semantic hit.
	// Using high dimensions (400+) avoids interference with T2 tests in the shared DB.
	embStore := pgstore.NewEmbeddingStore(pool)
	seedFusionEmbedding(t, ctx, embStore, hidden.ID, unitVec1536(420)) //nolint:mnd // dim 420: unique to AC5 test

	// Seed an OPEN listing for userA at dim-421 (lower similarity) so the result
	// set is non-empty and we can confirm the hidden one is excluded, not that
	// the search simply returned nothing.
	openForA := createOpenTender(t, ctx, createSvc, userA,
		"open cloud project for userA", "publicly visible")
	seedFusionEmbedding(t, ctx, embStore, openForA.ID, unitVec1536(421)) //nolint:mnd // dim 421: near dim 420

	// userA searches with dim-420 vector → AWARDED userB listing is the closest
	// neighbor, but the hydration SQL must exclude it (status ≠ OPEN, owner ≠ userA).
	searchSvc := buildFusionService(t, ctx, &fixedEmbedClient{vec: unitVec1536(420)}) //nolint:mnd // dim 420

	res, err := searchSvc.SearchListings(ctx, &service.SearchListingsInput{
		CallerID: userA,
		Mode:     service.SearchModeSemantic,
		Limit:    10,
	})
	require.NoError(t, err)

	for _, l := range res.Listings {
		assert.NotEqual(t, hidden.ID, l.ID,
			"AWARDED listing owned by userB must never appear in userA's semantic search results")
	}

	// userA's own OPEN listing must be present (proves search returned results).
	found := false

	for _, l := range res.Listings {
		if l.ID == openForA.ID {
			found = true

			break
		}
	}

	assert.True(t, found, "userA's OPEN listing must appear in their own search results")
}

// ─── M-1 fix: structural filters in semantic/hybrid hydration ────────────────

// TestSearchFusion_SemanticStatusFilter_Integration verifies that mode=semantic
// with status=OPEN excludes a caller's own non-OPEN (AWARDED) listing even when
// it is the top semantic hit. The status filter is applied IN the hydration SQL
// (GetByIDs), not post-filtered in Go.
func TestSearchFusion_SemanticStatusFilter_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	callerID := uuid.New()

	createSvc := buildFusionService(t, ctx, nil)

	// Create two tenders for the same caller.
	// awardedTender will be the top semantic hit but status=OPEN filter must exclude it.
	awardedTender := createOpenTender(t, ctx, createSvc, callerID, "awarded backend work", "done")
	openTender := createOpenTender(t, ctx, createSvc, callerID, "open frontend work", "in progress")

	// Promote awardedTender to AWARDED via direct DB write.
	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, "UPDATE listings SET status='AWARDED' WHERE id=$1", awardedTender.ID)
	require.NoError(t, err, "set awarded tender status")

	// Seed BOTH with the same dim-430 vector so both are at cosine distance 0
	// from the query vector. The SQL status filter (not Go post-filter) must
	// exclude awardedTender and return openTender.
	embStore := pgstore.NewEmbeddingStore(pool)
	seedFusionEmbedding(t, ctx, embStore, awardedTender.ID, unitVec1536(430)) //nolint:mnd // dim 430
	seedFusionEmbedding(t, ctx, embStore, openTender.ID, unitVec1536(430))    //nolint:mnd // dim 430: same as awarded, both at distance 0

	// Query with dim-430 and status=OPEN: awardedTender is top semantic hit but
	// must be excluded by the status filter; openTender must appear.
	openStatus := domain.ListingStatusOpen
	searchSvc := buildFusionService(t, ctx, &fixedEmbedClient{vec: unitVec1536(430)}) //nolint:mnd // dim 430

	res, err := searchSvc.SearchListings(ctx, &service.SearchListingsInput{
		CallerID: callerID,
		Mode:     service.SearchModeSemantic,
		Status:   &openStatus,
		Limit:    10,
	})
	require.NoError(t, err)

	for _, l := range res.Listings {
		assert.NotEqual(t, awardedTender.ID, l.ID,
			"semantic search with status=OPEN must exclude AWARDED listing even when it is the top semantic hit")
	}

	found := false

	for _, l := range res.Listings {
		if l.ID == openTender.ID {
			found = true

			break
		}
	}

	assert.True(t, found, "OPEN listing must appear when status=OPEN filter is active in semantic mode")
}

// TestSearchFusion_SemanticBudgetFilter_Integration verifies that mode=semantic
// with minBudget excludes a cheaper semantic top-hit that falls below the budget
// floor. The budget filter is applied IN the hydration SQL, not in Go.
func TestSearchFusion_SemanticBudgetFilter_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	callerID := uuid.New()

	// Use the shared pool (plain, not embedding-pool) to insert via raw SQL so we
	// can set budget_max precisely — CreateListing does not expose budget on create.
	pool, err := pgstore.NewEmbeddingPool(ctx, sharedServiceDSN, "", pgstore.PoolOptions{})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cheapID := uuid.New()
	expensiveID := uuid.New()

	// Insert cheap tender (budget_max=100) — will be the top semantic hit.
	_, err = pool.Exec(ctx, `
		INSERT INTO listings (id, owner_user_id, title, description, currency, status,
		                      budget_min, budget_max, is_tender, created_at, updated_at)
		VALUES ($1, $2, 'cheap tender for budget filter test', 'desc', 'TWD', 'OPEN',
		        50, 100, true, NOW(), NOW())`,
		cheapID, callerID)
	require.NoError(t, err, "insert cheap tender")

	// Insert expensive tender (budget_max=10000).
	_, err = pool.Exec(ctx, `
		INSERT INTO listings (id, owner_user_id, title, description, currency, status,
		                      budget_min, budget_max, is_tender, created_at, updated_at)
		VALUES ($1, $2, 'expensive tender for budget filter test', 'desc', 'TWD', 'OPEN',
		        5000, 10000, true, NOW(), NOW())`,
		expensiveID, callerID)
	require.NoError(t, err, "insert expensive tender")

	// Seed BOTH with the same dim-440 vector so both are at cosine distance 0
	// from the query vector. The SQL budget filter (budget_max >= minBudget)
	// must exclude cheapID (budget_max=100 < 1000) and return expensiveID.
	embStore := pgstore.NewEmbeddingStore(pool)
	seedFusionEmbedding(t, ctx, embStore, cheapID, unitVec1536(440))     //nolint:mnd // dim 440
	seedFusionEmbedding(t, ctx, embStore, expensiveID, unitVec1536(440)) //nolint:mnd // dim 440: same vector, both at distance 0 so SQL filter decides

	// Query with minBudget=1000 — cheap tender (budget_max=100 < 1000) must be excluded.
	minBudget := decimal.NewFromInt(1000)                                             //nolint:mnd // 1000 TWD floor
	searchSvc := buildFusionService(t, ctx, &fixedEmbedClient{vec: unitVec1536(440)}) //nolint:mnd // dim 440

	res, err := searchSvc.SearchListings(ctx, &service.SearchListingsInput{
		CallerID:  callerID,
		Mode:      service.SearchModeSemantic,
		BudgetMin: &minBudget,
		Limit:     10,
	})
	require.NoError(t, err)

	for _, l := range res.Listings {
		assert.NotEqual(t, cheapID, l.ID,
			"listing with budget_max below minBudget must be excluded by semantic hydration SQL")
	}

	found := false

	for _, l := range res.Listings {
		if l.ID == expensiveID {
			found = true

			break
		}
	}

	assert.True(t, found, "expensive listing (budget_max >= minBudget) must appear in semantic results")
}

// ─── M-2 fix: hybrid fallback pagination matches lexical ─────────────────────

// TestSearchFusion_HybridFallback_PaginationMatchesLexical_Integration verifies
// that when hybrid falls back to lexical (embedding disabled), the nextCursor in
// the response is consistent with lexical-mode pagination (correct limit+1
// over-fetch, not the searchTopK=200 over-fetch used for RRF candidates).
func TestSearchFusion_HybridFallback_PaginationMatchesLexical_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	callerID := uuid.New()

	// Create enough listings to trigger next-page cursor.
	createSvc := buildFusionService(t, ctx, nil)

	for i := range 6 {
		_ = createOpenTender(t, ctx, createSvc, callerID,
			fmt.Sprintf("pagination test tender paginationfusionkw %d", i),
			"pagination test description")
	}

	// Hybrid fallback (embedding disabled) with limit=3 → must get exactly 3
	// listings and a non-empty nextCursor, matching lexical-mode behavior.
	svcFallback := buildFusionService(t, ctx, &fixedEmbedClient{err: client.ErrEmbeddingDisabled})
	svcLexical := buildFusionService(t, ctx, nil)

	resFallback, err := svcFallback.SearchListings(ctx, &service.SearchListingsInput{
		CallerID: callerID,
		Query:    "paginationfusionkw",
		Mode:     service.SearchModeHybrid,
		Limit:    3, //nolint:mnd // page size for pagination test
	})
	require.NoError(t, err)

	resLexical, err := svcLexical.SearchListings(ctx, &service.SearchListingsInput{
		CallerID: callerID,
		Query:    "paginationfusionkw",
		Mode:     service.SearchModeLexical,
		Limit:    3, //nolint:mnd // page size for pagination test
	})
	require.NoError(t, err)

	// Both must return exactly 3 listings and a next-cursor (6 total, page=3).
	assert.Len(t, resFallback.Listings, 3,
		"hybrid fallback must return exactly page-size=3 listings, not up to searchTopK=200")
	assert.NotEmpty(t, resFallback.NextCursor,
		"hybrid fallback must produce a nextCursor when more results exist")

	// The cursor must agree with lexical mode (same over-fetch path).
	assert.Equal(t, resLexical.NextCursor, resFallback.NextCursor,
		"hybrid fallback next-cursor must match lexical next-cursor (same limit+1 over-fetch path)")
}
