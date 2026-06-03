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

// GetListing returns a listing by ID, applying the listing visibility rule
// (P0 IDOR fix): OPEN listings are visible to any authenticated caller, while
// non-OPEN listings (AWARDED/CLOSED) are visible only to their owner. A caller
// who is neither sees ErrListingNotFound (404) — never a 403 — so the endpoint
// cannot be used to probe the existence of other users' non-OPEN listings.
//
// callerID is the authenticated user (X-User-Id). Returns ErrListingNotFound if
// the listing does not exist OR the caller is not permitted to see it.
func (s *ListingService) GetListing(ctx context.Context, id, callerID uuid.UUID) (*domain.Listing, error) {
	l, err := s.listings.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Visibility rule (mirrors SearchListings / ListListings): non-OPEN listings
	// are private to their owner. Collapse "forbidden" into "not found" to avoid
	// resource enumeration.
	if l.Status != domain.ListingStatusOpen && l.OwnerUserID != callerID {
		return nil, domain.ErrListingNotFound
	}

	return l, nil
}

// ListListings returns paginated listings, optionally filtered by status or owner.
// The filter's VisibleToUserID MUST be set by the caller (handler) to the
// authenticated user so the store can enforce the listing visibility rule in SQL:
// OPEN listings are public, non-OPEN listings are restricted to their owner. This
// closes the P0 IDOR where ?status=AWARDED|CLOSED enumerated every user's
// non-OPEN listings.
func (s *ListingService) ListListings(ctx context.Context, filter store.ListingFilter) ([]*domain.Listing, error) {
	return s.listings.List(ctx, filter)
}

const (
	// searchDefaultLimit is the page size used when no/invalid limit is supplied.
	searchDefaultLimit = 20
	// searchMaxLimit caps the page size to bound memory + DB work per request.
	searchMaxLimit = 100
)

// SearchListingsInput carries validated input for SearchListings.
//
// CallerID is the authenticated caller (X-User-Id). Visibility: non-OPEN
// listings are only returned to their owner; everyone else sees OPEN only.
type SearchListingsInput struct {
	CallerID  uuid.UUID
	Query     string
	Status    *domain.ListingStatus
	BudgetMin *decimal.Decimal
	BudgetMax *decimal.Decimal
	Cursor    string // opaque, base64-encoded keyset cursor ("" = first page)
	Limit     int
}

// SearchListingsResult is the paginated search response.
type SearchListingsResult struct {
	Listings   []*domain.Listing `json:"listings"`
	NextCursor string            `json:"nextCursor,omitempty"` // empty when no further pages
}

// SearchListings runs a full-text + structured-filter search over listings.
//
// Visibility (MK security): callers may always see OPEN listings. Non-OPEN
// listings (AWARDED/CLOSED) are restricted to their owner, mirroring the
// List/Get visibility rules. When a status filter is supplied it is honored,
// but a non-OPEN status filter is silently scoped to the caller's own listings
// so it cannot be used to enumerate other users' awarded/closed cases.
func (s *ListingService) SearchListings(ctx context.Context, in *SearchListingsInput) (*SearchListingsResult, error) {
	if err := validateSearchInput(in); err != nil {
		return nil, err
	}

	cursor, err := decodeSearchCursor(in.Cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid cursor", domain.ErrValidation)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}

	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}

	filter := store.SearchFilter{
		Query:           in.Query,
		Status:          in.Status,
		BudgetMin:       in.BudgetMin,
		BudgetMax:       in.BudgetMax,
		After:           cursor,
		VisibleToUserID: in.CallerID, // visibility enforced in SQL
		Limit:           limit + 1,   // over-fetch one row to detect a next page
	}

	rows, err := s.listings.Search(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("search listings: %w", err)
	}

	return buildSearchResult(rows, limit), nil
}

// buildSearchResult trims the over-fetched page to limit and emits the next
// cursor (keyed on the last returned row) when more rows remain.
func buildSearchResult(rows []*domain.Listing, limit int) *SearchListingsResult {
	res := &SearchListingsResult{Listings: []*domain.Listing{}}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	res.Listings = rows

	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		res.NextCursor = encodeSearchCursor(store.SearchCursor{
			CreatedAt: last.CreatedAt,
			ID:        last.ID,
		})
	}

	return res
}

// validateSearchInput enforces field-level constraints on search parameters.
func validateSearchInput(in *SearchListingsInput) error {
	if err := sanitizeText(in.Query); err != nil {
		return fmt.Errorf("%w: query: %s", domain.ErrValidation, err)
	}

	if utf8.RuneCountInString(in.Query) > 200 {
		return fmt.Errorf("%w: query exceeds 200 characters", domain.ErrValidation)
	}

	if in.Status != nil {
		switch *in.Status {
		case domain.ListingStatusOpen, domain.ListingStatusAwarded, domain.ListingStatusClosed:
		default:
			return fmt.Errorf("%w: invalid status filter", domain.ErrValidation)
		}
	}

	return validateBudget(in.BudgetMin, in.BudgetMax)
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
