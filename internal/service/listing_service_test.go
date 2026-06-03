package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListingService_CreateListing verifies field-level validation including the
// numeric(14,2) upper-bound guard (MK-M2).
func TestListingService_CreateListing(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	// 999999999999.99 is the max representable by numeric(14,2).
	maxAllowed, _ := decimal.NewFromString("999999999999.99")
	justOver, _ := decimal.NewFromString("1000000000000.00") // one cent over column max

	tests := []struct {
		name      string
		input     service.CreateListingInput
		wantErr   error
		wantNoErr bool
	}{
		{
			name: "happy path: minimal valid listing",
			input: service.CreateListingInput{
				OwnerUserID: ownerID,
				Title:       "Go developer needed",
				Currency:    "TWD",
			},
			wantNoErr: true,
		},
		{
			name: "happy path: budget at column max is accepted",
			input: service.CreateListingInput{
				OwnerUserID: ownerID,
				Title:       "Max Budget Listing",
				Currency:    "TWD",
				BudgetMin:   &maxAllowed,
				BudgetMax:   &maxAllowed,
			},
			wantNoErr: true,
		},
		{
			name: "error: budget_min over numeric(14,2) max",
			input: service.CreateListingInput{
				OwnerUserID: ownerID,
				Title:       "Over Budget",
				Currency:    "TWD",
				BudgetMin:   &justOver,
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "error: budget_max over numeric(14,2) max",
			input: service.CreateListingInput{
				OwnerUserID: ownerID,
				Title:       "Over Budget Max",
				Currency:    "TWD",
				BudgetMax:   &justOver,
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "error: astronomically large budget_max",
			input: service.CreateListingInput{
				OwnerUserID: ownerID,
				Title:       "Huge Budget",
				Currency:    "TWD",
				BudgetMax:   func() *decimal.Decimal { d := decimal.NewFromFloat(1e20); return &d }(),
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "error: empty title",
			input: service.CreateListingInput{
				OwnerUserID: ownerID,
				Title:       "",
				Currency:    "TWD",
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "error: budget_max less than budget_min",
			input: service.CreateListingInput{
				OwnerUserID: ownerID,
				Title:       "Budget Order",
				Currency:    "TWD",
				BudgetMin:   func() *decimal.Decimal { d := decimal.NewFromInt(500); return &d }(),
				BudgetMax:   func() *decimal.Decimal { d := decimal.NewFromInt(100); return &d }(),
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "error: currency not 3 chars",
			input: service.CreateListingInput{
				OwnerUserID: ownerID,
				Title:       "Bad Currency",
				Currency:    "TWDD",
			},
			wantErr: domain.ErrValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStore()
			ls.listings = make(map[uuid.UUID]*domain.Listing)

			svc := service.NewListingService(ls)

			listing, err := svc.CreateListing(context.Background(), &tc.input)

			if tc.wantNoErr {
				require.NoError(t, err)
				require.NotNil(t, listing)
				assert.Equal(t, tc.input.OwnerUserID, listing.OwnerUserID)
				assert.Equal(t, domain.ListingStatusOpen, listing.Status)
			} else {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErr), "got %v, want %v", err, tc.wantErr)
				assert.Nil(t, listing)
			}
		})
	}
}

// TestListingService_UpdateListing verifies IDOR guard and budget upper-bound validation.
func TestListingService_UpdateListing(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	strangerID := uuid.New()
	listingID := uuid.New()

	justOver, _ := decimal.NewFromString("1000000000000.00")

	baseOpenListing := func() *domain.Listing {
		return &domain.Listing{
			ID:          listingID,
			OwnerUserID: ownerID,
			Title:       "Original",
			Currency:    "TWD",
			Status:      domain.ListingStatusOpen,
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}
	}

	tests := []struct {
		name      string
		input     service.UpdateListingInput
		wantErr   error
		wantNoErr bool
	}{
		{
			name: "happy path: owner updates title",
			input: service.UpdateListingInput{
				ID:       listingID,
				CallerID: ownerID,
				Title:    strPtr("Updated Title"),
			},
			wantNoErr: true,
		},
		{
			name: "error: stranger cannot update (IDOR guard)",
			input: service.UpdateListingInput{
				ID:       listingID,
				CallerID: strangerID,
				Title:    strPtr("Hijacked Title"),
			},
			wantErr: domain.ErrListingNotFound,
		},
		{
			name: "error: budget_max over numeric(14,2) max",
			input: service.UpdateListingInput{
				ID:        listingID,
				CallerID:  ownerID,
				BudgetMax: &justOver,
			},
			wantErr: domain.ErrValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStore(baseOpenListing())
			svc := service.NewListingService(ls)

			listing, err := svc.UpdateListing(context.Background(), tc.input)

			if tc.wantNoErr {
				require.NoError(t, err)
				require.NotNil(t, listing)
			} else {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErr), "got %v, want %v", err, tc.wantErr)
				assert.Nil(t, listing)
			}
		})
	}
}

// TestListingService_GetListing_Visibility verifies the P0 IDOR fix: OPEN
// listings are visible to any caller, non-OPEN listings only to their owner, and
// a non-owner of a non-OPEN listing gets ErrListingNotFound (404), never the row.
func TestListingService_GetListing_Visibility(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	strangerID := uuid.New()

	mk := func(status domain.ListingStatus) *domain.Listing {
		return &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID,
			Title:       "Listing",
			Currency:    "TWD",
			Status:      status,
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}
	}

	tests := []struct {
		name      string
		listing   *domain.Listing
		callerID  uuid.UUID
		wantErr   error
		wantNoErr bool
	}{
		{
			name:      "OPEN listing visible to owner",
			listing:   mk(domain.ListingStatusOpen),
			callerID:  ownerID,
			wantNoErr: true,
		},
		{
			name:      "OPEN listing visible to stranger",
			listing:   mk(domain.ListingStatusOpen),
			callerID:  strangerID,
			wantNoErr: true,
		},
		{
			name:      "AWARDED listing visible to owner",
			listing:   mk(domain.ListingStatusAwarded),
			callerID:  ownerID,
			wantNoErr: true,
		},
		{
			name:     "AWARDED listing hidden from stranger -> 404",
			listing:  mk(domain.ListingStatusAwarded),
			callerID: strangerID,
			wantErr:  domain.ErrListingNotFound,
		},
		{
			name:     "CLOSED listing hidden from stranger -> 404",
			listing:  mk(domain.ListingStatusClosed),
			callerID: strangerID,
			wantErr:  domain.ErrListingNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStore(tc.listing)
			svc := service.NewListingService(ls)

			got, err := svc.GetListing(context.Background(), tc.listing.ID, tc.callerID)

			if tc.wantNoErr {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Equal(t, tc.listing.ID, got.ID)
			} else {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tc.wantErr), "got %v, want %v", err, tc.wantErr)
				assert.Nil(t, got)
			}
		})
	}
}

// TestListingService_GetListing_NotFound verifies a missing listing surfaces
// ErrListingNotFound regardless of caller.
func TestListingService_GetListing_NotFound(t *testing.T) {
	t.Parallel()

	ls := newStubListingStore()
	svc := service.NewListingService(ls)

	got, err := svc.GetListing(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrListingNotFound))
	assert.Nil(t, got)
}

func strPtr(s string) *string { return &s }
