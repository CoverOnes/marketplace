// Package service implements the marketplace business logic.
package service

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ListingService handles listing business logic.
type ListingService struct {
	listings store.ListingStore
}

// NewListingService returns a ListingService.
func NewListingService(listings store.ListingStore) *ListingService {
	return &ListingService{listings: listings}
}

// CreateListingInput carries validated input for creating a listing.
type CreateListingInput struct {
	OwnerUserID uuid.UUID
	Title       string
	Description string
	BudgetMin   *decimal.Decimal
	BudgetMax   *decimal.Decimal
	Currency    string
}

// CreateListing creates a new listing owned by the calling user.
// The OwnerUserID MUST be set exclusively from the X-User-Id header — never from the request body.
func (s *ListingService) CreateListing(ctx context.Context, in *CreateListingInput) (*domain.Listing, error) {
	if err := validateListingInput(in.Title, in.Description, in.BudgetMin, in.BudgetMax, in.Currency); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	l := &domain.Listing{
		ID:          uuid.New(),
		OwnerUserID: in.OwnerUserID,
		Title:       in.Title,
		Description: in.Description,
		BudgetMin:   in.BudgetMin,
		BudgetMax:   in.BudgetMax,
		Currency:    in.Currency,
		Status:      domain.ListingStatusOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.listings.Create(ctx, l); err != nil {
		return nil, fmt.Errorf("create listing: %w", err)
	}

	return l, nil
}

// GetListing returns a listing by ID. Returns ErrListingNotFound if not found.
func (s *ListingService) GetListing(ctx context.Context, id uuid.UUID) (*domain.Listing, error) {
	return s.listings.GetByID(ctx, id)
}

// ListListings returns paginated listings, optionally filtered by status or owner.
func (s *ListingService) ListListings(ctx context.Context, filter store.ListingFilter) ([]*domain.Listing, error) {
	return s.listings.List(ctx, filter)
}

// UpdateListingInput carries validated input for updating a listing.
type UpdateListingInput struct {
	ID          uuid.UUID
	CallerID    uuid.UUID // must equal listing.OwnerUserID
	Title       *string
	Description *string
	BudgetMin   *decimal.Decimal
	BudgetMax   *decimal.Decimal
	Currency    *string
}

// UpdateListing applies a partial update to a listing.
// Returns ErrListingNotFound if not found, ErrForbidden if caller is not owner,
// ErrListingNotOpen if listing is not in OPEN status.
func (s *ListingService) UpdateListing(ctx context.Context, in UpdateListingInput) (*domain.Listing, error) {
	l, err := s.listings.GetByID(ctx, in.ID)
	if err != nil {
		return nil, err
	}

	// IDOR: re-check ownership from DB, never from caller-supplied data.
	if l.OwnerUserID != in.CallerID {
		return nil, domain.ErrListingNotFound // 404 to avoid resource enumeration
	}

	if l.Status != domain.ListingStatusOpen {
		return nil, fmt.Errorf("%w: only OPEN listings can be updated", domain.ErrListingNotOpen)
	}

	// Apply partial fields.
	if in.Title != nil {
		l.Title = *in.Title
	}

	if in.Description != nil {
		l.Description = *in.Description
	}

	if in.BudgetMin != nil {
		l.BudgetMin = in.BudgetMin
	}

	if in.BudgetMax != nil {
		l.BudgetMax = in.BudgetMax
	}

	if in.Currency != nil {
		l.Currency = *in.Currency
	}

	if err := validateListingInput(l.Title, l.Description, l.BudgetMin, l.BudgetMax, l.Currency); err != nil {
		return nil, err
	}

	if err := s.listings.Update(ctx, l); err != nil {
		return nil, err
	}

	return l, nil
}

// maxNumeric14_2 is the maximum value representable by numeric(14,2): 999999999999.99.
// Any budget value exceeding this would overflow the DB column (MK-M2).
var maxNumeric14_2 = decimal.NewFromFloat(999999999999.99) //nolint:gochecknoglobals // package-level sentinel; immutable after init

// validateListingInput enforces field-level constraints.
// description may be empty (0..10000 runes) — intentional; see domain design.
func validateListingInput(title, description string, budgetMin, budgetMax *decimal.Decimal, currency string) error {
	if err := sanitizeText(title); err != nil {
		return fmt.Errorf("%w: title: %s", domain.ErrValidation, err)
	}

	if utf8.RuneCountInString(title) < 1 || utf8.RuneCountInString(title) > 200 {
		return fmt.Errorf("%w: title must be 1-200 characters", domain.ErrValidation)
	}

	if err := sanitizeText(description); err != nil {
		return fmt.Errorf("%w: description: %s", domain.ErrValidation, err)
	}

	if utf8.RuneCountInString(description) > 10000 {
		return fmt.Errorf("%w: description exceeds 10000 characters", domain.ErrValidation)
	}

	if len(currency) != 3 {
		return fmt.Errorf("%w: currency must be a 3-letter code", domain.ErrValidation)
	}

	return validateBudget(budgetMin, budgetMax)
}

// validateBudget checks budget_min and budget_max range constraints.
// Both fields are optional (nil = unset). Extracted to keep validateListingInput
// within the cyclomatic complexity limit.
func validateBudget(budgetMin, budgetMax *decimal.Decimal) error {
	zero := decimal.Zero

	if budgetMin != nil && budgetMin.LessThan(zero) {
		return fmt.Errorf("%w: budget_min must be >= 0", domain.ErrValidation)
	}

	if budgetMin != nil && budgetMin.GreaterThan(maxNumeric14_2) {
		return fmt.Errorf("%w: budget_min exceeds maximum allowed value", domain.ErrValidation)
	}

	if budgetMax != nil && budgetMax.LessThan(zero) {
		return fmt.Errorf("%w: budget_max must be >= 0", domain.ErrValidation)
	}

	if budgetMax != nil && budgetMax.GreaterThan(maxNumeric14_2) {
		return fmt.Errorf("%w: budget_max exceeds maximum allowed value", domain.ErrValidation)
	}

	if budgetMin != nil && budgetMax != nil && budgetMax.LessThan(*budgetMin) {
		return fmt.Errorf("%w: budget_max must be >= budget_min", domain.ErrValidation)
	}

	return nil
}

// sanitizeText rejects null bytes, carriage returns, newlines, and ASCII control chars
// in user-supplied strings (backend-security §5.4).
func sanitizeText(s string) error {
	for _, r := range s {
		if r == '\x00' || r == '\r' || r == '\n' {
			return fmt.Errorf("contains illegal control characters (null, CR, LF)")
		}

		if r < 0x20 && r != '\t' {
			return fmt.Errorf("contains ASCII control characters")
		}
	}

	return nil
}

// sanitizeMessage validates bid message text.
// Rejects control characters and caps at 5000 runes.
func sanitizeMessage(s string) error {
	if utf8.RuneCountInString(s) > 5000 {
		return fmt.Errorf("exceeds 5000 character limit")
	}

	return sanitizeText(s)
}
