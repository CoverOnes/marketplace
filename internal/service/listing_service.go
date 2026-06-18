// Package service implements the marketplace business logic.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/outbox"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// SearchMode controls which retrieval strategy is used for search.
type SearchMode string

const (
	// SearchModeLexical uses FTS only (keyset-paginated, byte-identical to legacy behavior).
	SearchModeLexical SearchMode = "lexical"
	// SearchModeSemantic uses pgvector cosine similarity only.
	SearchModeSemantic SearchMode = "semantic"
	// SearchModeHybrid fuses lexical + semantic via RRF (k=60).
	SearchModeHybrid SearchMode = "hybrid"
)

// searchTopK is the maximum number of candidates fetched from each list for
// ranked modes. Bounded to prevent unbounded vector scans.
const searchTopK = 200

// ListingService handles listing business logic.
type ListingService struct {
	listings      store.ListingStore
	listingOutbox store.ListingOutboxTxManager // nil → no outbox (non-tender / disabled)
	embClient     client.EmbeddingClient       // nil → semantic/hybrid falls back to lexical
	embStore      store.EmbeddingStore         // nil → semantic/hybrid falls back to lexical
}

// NewListingService returns a ListingService.
// listingOutbox may be nil; when nil, no outbox event is enqueued on create/update.
// Pass a real ListingOutboxTxManager to enable same-tx embedding_reindex enqueue.
// embClient and embStore are optional; when nil, semantic/hybrid modes fall back to lexical.
func NewListingService(
	listings store.ListingStore,
	listingOutbox store.ListingOutboxTxManager,
	embClient client.EmbeddingClient,
	embStore store.EmbeddingStore,
) *ListingService {
	return &ListingService{
		listings:      listings,
		listingOutbox: listingOutbox,
		embClient:     embClient,
		embStore:      embStore,
	}
}

// CreateListingInput carries validated input for creating a listing.
type CreateListingInput struct {
	OwnerUserID uuid.UUID
	Title       string
	Description string
	BudgetMin   *decimal.Decimal
	BudgetMax   *decimal.Decimal
	Currency    string
	// Tender discriminator fields. IsTender=false → CLASSIC 1:1 listing (default).
	IsTender        bool
	KYCTierRequired int // 0..5; only meaningful when IsTender=true
}

// CreateListing creates a new listing owned by the calling user.
// The OwnerUserID MUST be set exclusively from the X-User-Id header — never from the request body.
// When IsTender=true the listing is created with tender_status='OPEN' and recruiter_mode='CLOSED'.
// When IsTender=true and a ListingOutboxTxManager is configured, an embedding_reindex event is
// enqueued in the same transaction as the listing insert (same-tx outbox pattern).
func (s *ListingService) CreateListing(ctx context.Context, in *CreateListingInput) (*domain.Listing, error) {
	if err := validateListingInput(in.Title, in.Description, in.BudgetMin, in.BudgetMax, in.Currency); err != nil {
		return nil, err
	}

	if in.IsTender {
		if in.KYCTierRequired < 0 || in.KYCTierRequired > 5 {
			return nil, fmt.Errorf("%w: kyc_tier_required must be 0..5", domain.ErrValidation)
		}
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

	if in.IsTender {
		ts := domain.TenderStatusOpen
		l.IsTender = true
		l.TenderStatus = &ts
		l.RecruiterMode = domain.RecruiterModeClosed
		l.KYCTierRequired = in.KYCTierRequired
	}

	// When creating a tender and the outbox tx manager is configured, run the insert
	// + outbox enqueue in the same transaction (same-tx outbox pattern). For classic
	// (non-tender) listings no embedding is needed, so fall back to the plain insert.
	if in.IsTender && s.listingOutbox != nil {
		return s.createTenderWithOutbox(ctx, l)
	}

	if err := s.listings.Create(ctx, l); err != nil {
		return nil, fmt.Errorf("create listing: %w", err)
	}

	return l, nil
}

// createTenderWithOutbox inserts the tender listing and enqueues an
// embedding_reindex event in a single Postgres transaction.
func (s *ListingService) createTenderWithOutbox(ctx context.Context, l *domain.Listing) (*domain.Listing, error) {
	var created *domain.Listing

	err := s.listingOutbox.WithListingOutboxTx(ctx, func(txCtx context.Context, listings store.ListingStore, ob store.OutboxStore) error {
		if createErr := listings.Create(txCtx, l); createErr != nil {
			return fmt.Errorf("create listing: %w", createErr)
		}

		if enqErr := outbox.EnqueueTenderEmbeddingReindex(txCtx, ob, l.ID); enqErr != nil {
			return fmt.Errorf("enqueue embedding_reindex: %w", enqErr)
		}

		created = l

		return nil
	})
	if err != nil {
		return nil, err
	}

	return created, nil
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
// Mode controls retrieval strategy: lexical (default), semantic, or hybrid.
// Semantic/hybrid fall back to lexical when embedding is disabled or cold.
type SearchListingsInput struct {
	CallerID  uuid.UUID
	Query     string
	Status    *domain.ListingStatus
	BudgetMin *decimal.Decimal
	BudgetMax *decimal.Decimal
	Cursor    string // opaque, base64-encoded keyset cursor ("" = first page); only used for lexical mode
	Limit     int
	Mode      SearchMode // default: SearchModeLexical
}

// SearchListingsResult is the paginated search response.
type SearchListingsResult struct {
	Listings   []*domain.Listing `json:"listings"`
	NextCursor string            `json:"nextCursor,omitempty"` // empty when no further pages
}

// SearchListings runs a search over listings using the requested mode.
//
// Modes:
//   - lexical (default/empty): FTS filter + keyset pagination — byte-identical to the
//     previous behavior. The Cursor field is honored for pagination.
//   - semantic: generate query embedding → NearestNeighbors → hydrate + visibility SQL.
//   - hybrid: run both lists (up to searchTopK candidates each), fuse with RRF (k=60),
//     hydrate the fused ranking with visibility enforced IN SQL.
//
// Cold-start / disabled fallback: if the embedding client or store is nil, if
// ErrEmbeddingDisabled is returned, or if the embed call errors, semantic/hybrid
// modes silently fall back to lexical-only and return 200.
//
// Visibility (MK security): callers may always see OPEN listings. Non-OPEN
// listings (AWARDED/CLOSED) are restricted to their owner, mirroring the
// List/Get visibility rules.
func (s *ListingService) SearchListings(ctx context.Context, in *SearchListingsInput) (*SearchListingsResult, error) {
	if err := validateSearchInput(in); err != nil {
		return nil, err
	}

	limit := in.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}

	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}

	mode := in.Mode
	if mode == "" {
		mode = SearchModeLexical
	}

	// Ranked modes require an embedding client and store.
	// Fall back to lexical when they are unavailable (cold-start / dev).
	if (mode == SearchModeSemantic || mode == SearchModeHybrid) &&
		(s.embClient == nil || s.embStore == nil) {
		slog.Warn("search: embedding client or store not configured; falling back to lexical",
			"requested_mode", mode)
		mode = SearchModeLexical
	}

	switch mode {
	case SearchModeSemantic:
		return s.searchSemantic(ctx, in, limit)
	case SearchModeHybrid:
		return s.searchHybrid(ctx, in, limit)
	default:
		return s.searchLexical(ctx, in, limit)
	}
}

// searchLexical runs the existing FTS+keyset path — byte-identical to the pre-T3 behavior.
func (s *ListingService) searchLexical(ctx context.Context, in *SearchListingsInput, limit int) (*SearchListingsResult, error) {
	cursor, err := decodeSearchCursor(in.Cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid cursor", domain.ErrValidation)
	}

	filter := store.SearchFilter{
		Query:           in.Query,
		Status:          in.Status,
		BudgetMin:       in.BudgetMin,
		BudgetMax:       in.BudgetMax,
		After:           cursor,
		VisibleToUserID: in.CallerID,
		Limit:           limit + 1, // over-fetch one row to detect a next page
	}

	rows, err := s.listings.Search(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("search listings: %w", err)
	}

	return buildSearchResult(rows, limit), nil
}

// searchSemantic generates a query embedding, finds nearest neighbors, and
// hydrates the ranked ID set with visibility enforced in SQL. Errors in the
// embedding call fall back silently to lexical.
func (s *ListingService) searchSemantic(ctx context.Context, in *SearchListingsInput, limit int) (*SearchListingsResult, error) {
	semIDs, fallback := s.semanticIDs(ctx, in.Query)
	if fallback {
		return s.searchLexical(ctx, in, limit)
	}

	// Cap to page size and hydrate.
	if len(semIDs) > limit {
		semIDs = semIDs[:limit]
	}

	rows, err := s.listings.GetByIDs(ctx, semIDs, in.CallerID)
	if err != nil {
		return nil, fmt.Errorf("hydrate semantic results: %w", err)
	}

	return &SearchListingsResult{Listings: nonNilListings(rows)}, nil
}

// searchHybrid fuses lexical and semantic candidate lists with RRF (k=60),
// hydrates the fused ranking with visibility enforced in SQL. Falls back to
// lexical silently when the embedding call fails.
func (s *ListingService) searchHybrid(ctx context.Context, in *SearchListingsInput, limit int) (*SearchListingsResult, error) {
	// Lexical candidates — fetch up to searchTopK from FTS (no cursor, fresh ranking).
	lexFilter := store.SearchFilter{
		Query:           in.Query,
		Status:          in.Status,
		BudgetMin:       in.BudgetMin,
		BudgetMax:       in.BudgetMax,
		VisibleToUserID: in.CallerID,
		Limit:           searchTopK,
	}

	lexRows, err := s.listings.Search(ctx, lexFilter)
	if err != nil {
		return nil, fmt.Errorf("hybrid: lexical search: %w", err)
	}

	lexIDs := make([]uuid.UUID, len(lexRows))
	for i, r := range lexRows {
		lexIDs[i] = r.ID
	}

	// Semantic candidates — falls back to lexical if embedding unavailable.
	semIDs, fallback := s.semanticIDs(ctx, in.Query)
	if fallback {
		slog.Warn("hybrid: embedding fallback; returning lexical-only results")
		return buildSearchResult(lexRows, limit), nil
	}

	// Fuse via RRF and hydrate.
	fusedIDs := RRFCombine(lexIDs, semIDs)
	if len(fusedIDs) > limit {
		fusedIDs = fusedIDs[:limit]
	}

	rows, err := s.listings.GetByIDs(ctx, fusedIDs, in.CallerID)
	if err != nil {
		return nil, fmt.Errorf("hybrid: hydrate fused results: %w", err)
	}

	return &SearchListingsResult{Listings: nonNilListings(rows)}, nil
}

// semanticIDs generates a query embedding and returns nearest-neighbor IDs
// ordered by ascending cosine distance (most-similar first). The second return
// value is true when the caller should fall back to lexical (disabled, cold, or error).
func (s *ListingService) semanticIDs(ctx context.Context, query string) (ids []uuid.UUID, fallback bool) {
	vec, err := s.embClient.Generate(ctx, query)
	if err != nil {
		if errors.Is(err, client.ErrEmbeddingDisabled) {
			slog.Warn("search: embedding disabled; falling back to lexical")
		} else {
			slog.Warn("search: embedding generate failed; falling back to lexical", "err", err)
		}

		return nil, true
	}

	neighbors, err := s.embStore.NearestNeighbors(ctx, vec, domain.EmbeddingEntityTypeTender, searchTopK)
	if err != nil {
		slog.Warn("search: nearest neighbors failed; falling back to lexical", "err", err)

		return nil, true
	}

	if len(neighbors) == 0 {
		// Cold-start: no embeddings indexed yet; fall back to lexical.
		return nil, true
	}

	out := make([]uuid.UUID, len(neighbors))
	for i, n := range neighbors {
		out[i] = n.EntityID
	}

	return out, false
}

// nonNilListings returns a non-nil slice, converting nil to an empty slice so
// the JSON response always has "listings": [] instead of "listings": null.
func nonNilListings(ls []*domain.Listing) []*domain.Listing {
	if ls == nil {
		return []*domain.Listing{}
	}

	return ls
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
// When updating a tender and the embeddable text (title or description) changes,
// an embedding_reindex event is enqueued in the same transaction as the update.
// Budget-only or currency-only updates do NOT enqueue a reindex (cost control).
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

	// Snapshot embeddable text before applying changes, to detect drift.
	oldTitle := l.Title
	oldDescription := l.Description

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

	// Determine whether embeddable text changed — only then schedule a reindex.
	textChanged := l.Title != oldTitle || l.Description != oldDescription

	if l.IsTender && textChanged && s.listingOutbox != nil {
		return s.updateTenderWithOutbox(ctx, l)
	}

	if err := s.listings.Update(ctx, l); err != nil {
		return nil, err
	}

	return l, nil
}

// updateTenderWithOutbox updates the tender listing and enqueues an
// embedding_reindex event in a single Postgres transaction.
func (s *ListingService) updateTenderWithOutbox(ctx context.Context, l *domain.Listing) (*domain.Listing, error) {
	var updated *domain.Listing

	err := s.listingOutbox.WithListingOutboxTx(ctx, func(txCtx context.Context, listings store.ListingStore, ob store.OutboxStore) error {
		if updateErr := listings.Update(txCtx, l); updateErr != nil {
			return updateErr
		}

		if enqErr := outbox.EnqueueTenderEmbeddingReindex(txCtx, ob, l.ID); enqErr != nil {
			return fmt.Errorf("enqueue embedding_reindex: %w", enqErr)
		}

		updated = l

		return nil
	})
	if err != nil {
		return nil, err
	}

	return updated, nil
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

	if err := sanitizeMultilineText(description); err != nil {
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

// parseDecimal parses a decimal string and returns a pointer to the result.
// Returns an error if the string is not a valid decimal or is negative.
func parseDecimal(s string) (*decimal.Decimal, error) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid decimal: %w", err)
	}

	if d.IsNegative() {
		return nil, fmt.Errorf("value must be >= 0")
	}

	return &d, nil
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

// sanitizeMultilineText validates multi-line free-text fields (e.g. listing /
// tender description, which come from a textarea). Unlike sanitizeText it PERMITS
// newlines and tabs (CR/LF/HT) so legitimate multi-line content is accepted, but
// still rejects null bytes and all other ASCII control characters (e.g. ESC) to
// keep the terminal-escape / log-injection surface closed (backend-security §5.4).
func sanitizeMultilineText(s string) error {
	for _, r := range s {
		if r == '\x00' {
			return fmt.Errorf("contains null byte")
		}

		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
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
