package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// searchListing builds an OPEN listing fixture. The store stub returns the
// configured searchResult verbatim (visibility is enforced in SQL, not the
// service), so status only needs to be OPEN for these service-layer tests.
func searchListing(owner uuid.UUID, createdAt time.Time) *domain.Listing {
	return &domain.Listing{
		ID:          uuid.New(),
		OwnerUserID: owner,
		Title:       "Go developer",
		Description: "build things",
		Currency:    "TWD",
		Status:      domain.ListingStatusOpen,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}
}

// TestListingService_SearchListings_VisibilityWiredToStore verifies the service
// pushes the caller id into SearchFilter.VisibleToUserID so the visibility rule
// is enforced in SQL (post-filtering in the service would break pagination).
func TestListingService_SearchListings_VisibilityWiredToStore(t *testing.T) {
	t.Parallel()

	caller := uuid.New()
	base := time.Now().UTC().Truncate(time.Millisecond)
	openMine := searchListing(caller, base.Add(-1*time.Minute))

	ls := newStubListingStore()
	ls.searchResult = []*domain.Listing{openMine}
	svc := service.NewListingService(ls, nil)

	res, err := svc.SearchListings(context.Background(), &service.SearchListingsInput{
		CallerID: caller,
		Query:    "go",
		Limit:    10,
	})
	require.NoError(t, err)
	assert.Len(t, res.Listings, 1)

	require.NotNil(t, ls.lastSearch)
	assert.Equal(t, caller, ls.lastSearch.VisibleToUserID, "caller id must be wired to VisibleToUserID")
	assert.Equal(t, "go", ls.lastSearch.Query)
}

// TestListingService_SearchListings_Pagination verifies over-fetch trimming and
// nextCursor emission.
func TestListingService_SearchListings_Pagination(t *testing.T) {
	t.Parallel()

	caller := uuid.New()
	base := time.Now().UTC().Truncate(time.Millisecond)

	l1 := searchListing(caller, base.Add(-1*time.Minute))
	l2 := searchListing(caller, base.Add(-2*time.Minute))
	l3 := searchListing(caller, base.Add(-3*time.Minute))

	tests := []struct {
		name        string
		stored      []*domain.Listing
		limit       int
		wantLen     int
		wantHasNext bool
	}{
		{
			name:        "exactly limit rows -> no next cursor",
			stored:      []*domain.Listing{l1, l2},
			limit:       2,
			wantLen:     2,
			wantHasNext: false,
		},
		{
			name:        "more than limit rows -> trimmed + next cursor",
			stored:      []*domain.Listing{l1, l2, l3},
			limit:       2,
			wantLen:     2,
			wantHasNext: true,
		},
		{
			name:        "fewer than limit rows -> no next cursor",
			stored:      []*domain.Listing{l1},
			limit:       2,
			wantLen:     1,
			wantHasNext: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStore()
			ls.searchResult = tc.stored
			svc := service.NewListingService(ls, nil)

			res, err := svc.SearchListings(context.Background(), &service.SearchListingsInput{
				CallerID: caller,
				Query:    "go",
				Limit:    tc.limit,
			})
			require.NoError(t, err)
			assert.Len(t, res.Listings, tc.wantLen)

			if tc.wantHasNext {
				assert.NotEmpty(t, res.NextCursor)
			} else {
				assert.Empty(t, res.NextCursor)
			}

			// Store is always asked for limit+1 rows to detect a next page.
			require.NotNil(t, ls.lastSearch)
			assert.Equal(t, tc.limit+1, ls.lastSearch.Limit)
		})
	}
}

// TestListingService_SearchListings_Validation covers the error / edge cases.
func TestListingService_SearchListings_Validation(t *testing.T) {
	t.Parallel()

	caller := uuid.New()
	bogus := domain.ListingStatus("BOGUS")
	minD := decimal.NewFromInt(500)
	maxD := decimal.NewFromInt(100)

	tests := []struct {
		name string
		in   service.SearchListingsInput
	}{
		{
			name: "control char in query",
			in:   service.SearchListingsInput{CallerID: caller, Query: "bad\x00query"},
		},
		{
			name: "invalid status filter",
			in:   service.SearchListingsInput{CallerID: caller, Status: &bogus},
		},
		{
			name: "budget min > max",
			in:   service.SearchListingsInput{CallerID: caller, BudgetMin: &minD, BudgetMax: &maxD},
		},
		{
			name: "malformed cursor",
			in:   service.SearchListingsInput{CallerID: caller, Cursor: "%%%not-base64%%%"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStore()
			svc := service.NewListingService(ls, nil)

			input := tc.in
			_, err := svc.SearchListings(context.Background(), &input)
			require.Error(t, err)
			require.ErrorIs(t, err, domain.ErrValidation)
		})
	}
}

// TestListingService_SearchListings_LimitClamp asserts that the requested limit
// is clamped to the maximum page size and that the over-fetch is limit+1.
func TestListingService_SearchListings_LimitClamp(t *testing.T) {
	t.Parallel()

	caller := uuid.New()
	ls := newStubListingStore()
	svc := service.NewListingService(ls, nil)

	_, err := svc.SearchListings(context.Background(), &service.SearchListingsInput{
		CallerID: caller,
		Query:    "go",
		Limit:    100000, // way over the cap
	})
	require.NoError(t, err)
	require.NotNil(t, ls.lastSearch)
	// 100 (searchMaxLimit) + 1 over-fetch.
	assert.Equal(t, 101, ls.lastSearch.Limit)
}
