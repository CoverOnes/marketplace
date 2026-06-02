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

func strPtr(s string) *string { return &s }
