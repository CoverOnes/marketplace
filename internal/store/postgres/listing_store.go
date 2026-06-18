package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// querier is satisfied by both pgxpool.Pool and pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ListingStore is a pool-backed listing store.
type ListingStore struct {
	q querier
}

// NewListingStore returns a ListingStore backed by pool.
func NewListingStore(pool *pgxpool.Pool) *ListingStore {
	return &ListingStore{q: pool}
}

// txListingStore is a transaction-scoped ListingStore.
type txListingStore struct {
	tx querier
}

func (s *txListingStore) Create(ctx context.Context, l *domain.Listing) error {
	return createListing(ctx, s.tx, l)
}

func (s *txListingStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Listing, error) {
	return getListingByID(ctx, s.tx, id)
}

func (s *txListingStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Listing, error) {
	return getListingByIDForUpdate(ctx, s.tx, id)
}

func (s *txListingStore) List(ctx context.Context, filter store.ListingFilter) ([]*domain.Listing, error) {
	return listListings(ctx, s.tx, filter)
}

func (s *txListingStore) Search(ctx context.Context, filter store.SearchFilter) ([]*domain.Listing, error) {
	return searchListings(ctx, s.tx, filter)
}

func (s *txListingStore) GetByIDs(ctx context.Context, ids []uuid.UUID, viewerID uuid.UUID) ([]*domain.Listing, error) {
	return getListingsByIDs(ctx, s.tx, ids, viewerID)
}

func (s *txListingStore) Update(ctx context.Context, l *domain.Listing) error {
	return updateListing(ctx, s.tx, l)
}

// Create inserts a new listing.
func (s *ListingStore) Create(ctx context.Context, l *domain.Listing) error {
	return createListing(ctx, s.q, l)
}

// GetByID fetches a listing by primary key (excludes soft-deleted rows).
func (s *ListingStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Listing, error) {
	return getListingByID(ctx, s.q, id)
}

// GetByIDForUpdate is not meaningful outside a transaction; delegate to regular read.
// The pool-backed store implements the interface for completeness.
func (s *ListingStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Listing, error) {
	return getListingByID(ctx, s.q, id)
}

// List returns listings matching the filter.
func (s *ListingStore) List(ctx context.Context, filter store.ListingFilter) ([]*domain.Listing, error) {
	return listListings(ctx, s.q, filter)
}

// Search runs a full-text + structured filter query with keyset pagination.
func (s *ListingStore) Search(ctx context.Context, filter store.SearchFilter) ([]*domain.Listing, error) {
	return searchListings(ctx, s.q, filter)
}

// GetByIDs hydrates a ranked candidate ID set while enforcing visibility in SQL.
// IDs the viewer cannot see are dropped by the WHERE clause, never post-filtered in Go.
func (s *ListingStore) GetByIDs(ctx context.Context, ids []uuid.UUID, viewerID uuid.UUID) ([]*domain.Listing, error) {
	return getListingsByIDs(ctx, s.q, ids, viewerID)
}

// Update persists changes to a listing.
func (s *ListingStore) Update(ctx context.Context, l *domain.Listing) error {
	return updateListing(ctx, s.q, l)
}

// --- helpers shared by pool and tx stores ---

func createListing(ctx context.Context, q querier, l *domain.Listing) error {
	const query = `
INSERT INTO listings
	(id, owner_user_id, title, description, budget_min, budget_max,
	 currency, status, awarded_bid_id,
	 is_tender, recruiter_mode, tender_status, kyc_tier_required,
	 deleted_at, created_at, updated_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
`

	recruiterMode := l.RecruiterMode
	if recruiterMode == "" {
		recruiterMode = domain.RecruiterModeClosed
	}

	_, err := q.Exec(
		ctx, query,
		l.ID, l.OwnerUserID, l.Title, l.Description,
		decimalToString(l.BudgetMin), decimalToString(l.BudgetMax),
		l.Currency, l.Status, l.AwardedBidID,
		l.IsTender, string(recruiterMode), tenderStatusToString(l.TenderStatus), l.KYCTierRequired,
		l.DeletedAt, l.CreatedAt, l.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert listing: %w", err)
	}

	return nil
}

func getListingByID(ctx context.Context, q querier, id uuid.UUID) (*domain.Listing, error) {
	const query = `
SELECT id, owner_user_id, title, description, budget_min, budget_max,
       currency, status, awarded_bid_id,
       is_tender, recruiter_mode, tender_status, kyc_tier_required,
       deleted_at, created_at, updated_at
FROM listings
WHERE id = $1 AND deleted_at IS NULL
`

	return scanListing(q.QueryRow(ctx, query, id))
}

// getListingByIDForUpdate fetches a listing with SELECT ... FOR UPDATE to prevent TOCTOU races.
// Must be called within an active transaction.
func getListingByIDForUpdate(ctx context.Context, q querier, id uuid.UUID) (*domain.Listing, error) {
	const query = `
SELECT id, owner_user_id, title, description, budget_min, budget_max,
       currency, status, awarded_bid_id,
       is_tender, recruiter_mode, tender_status, kyc_tier_required,
       deleted_at, created_at, updated_at
FROM listings
WHERE id = $1 AND deleted_at IS NULL
FOR UPDATE
`

	return scanListing(q.QueryRow(ctx, query, id))
}

func listListings(ctx context.Context, q querier, filter store.ListingFilter) ([]*domain.Listing, error) {
	var (
		sb   strings.Builder
		args []any
		n    = 1
	)

	sb.WriteString(`
SELECT id, owner_user_id, title, description, budget_min, budget_max,
       currency, status, awarded_bid_id,
       is_tender, recruiter_mode, tender_status, kyc_tier_required,
       deleted_at, created_at, updated_at
FROM listings
WHERE deleted_at IS NULL`)

	// Visibility (P0 IDOR fix): OPEN listings are public; non-OPEN listings are
	// only returned to their owner. Enforced in SQL so ?status=AWARDED|CLOSED
	// cannot enumerate other users' rows. Mirrors buildSearchQuery.
	fmt.Fprintf(&sb, " AND (status = 'OPEN' OR owner_user_id = $%d)", n)
	args = append(args, filter.VisibleToUserID)
	n++

	if filter.Status != nil {
		fmt.Fprintf(&sb, " AND status = $%d", n)
		args = append(args, string(*filter.Status))
		n++
	}

	if filter.OwnerUserID != nil {
		fmt.Fprintf(&sb, " AND owner_user_id = $%d", n)
		args = append(args, *filter.OwnerUserID)
		n++
	}

	sb.WriteString(" ORDER BY created_at DESC")

	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	fmt.Fprintf(&sb, " LIMIT $%d", n)
	args = append(args, limit)
	n++

	if filter.Offset > 0 {
		fmt.Fprintf(&sb, " OFFSET $%d", n)
		args = append(args, filter.Offset)
	}

	rows, err := q.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list listings: %w", err)
	}

	defer rows.Close()

	var listings []*domain.Listing

	for rows.Next() {
		l, scanErr := scanListing(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		listings = append(listings, l)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate listings: %w", err)
	}

	return listings, nil
}

// ftsExpr is the full-text vector expression. It MUST stay byte-for-byte
// identical to the GIN index expression in migration 000004 so the planner can
// use the index; otherwise Postgres falls back to a sequential scan.
const ftsExpr = `to_tsvector('simple', coalesce(title, '') || ' ' || coalesce(description, ''))`

// searchListings runs the full-text + structured-filter search query with
// keyset pagination. Rows are ordered newest-first (created_at DESC, id DESC),
// a stable total order that the (created_at, id) cursor pages through. When a
// query string is supplied it is additionally filtered by plainto_tsquery; the
// ordering stays keyset-stable on (created_at, id) so the cursor remains sound.
func searchListings(ctx context.Context, q querier, filter store.SearchFilter) ([]*domain.Listing, error) {
	sb, args := buildSearchQuery(filter)

	rows, err := q.Query(ctx, sb, args...)
	if err != nil {
		return nil, fmt.Errorf("search listings: %w", err)
	}

	defer rows.Close()

	var listings []*domain.Listing

	for rows.Next() {
		l, scanErr := scanListing(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		listings = append(listings, l)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search listings: %w", err)
	}

	return listings, nil
}

// buildSearchQuery assembles the parameterised SQL string and argument slice for
// searchListings. Split out from searchListings to keep both functions under the
// gocyclo limit (.golangci.yml min-complexity: 15).
func buildSearchQuery(filter store.SearchFilter) (query string, args []any) {
	var (
		sb strings.Builder
		n  = 1
	)

	sb.WriteString(`
SELECT id, owner_user_id, title, description, budget_min, budget_max,
       currency, status, awarded_bid_id,
       is_tender, recruiter_mode, tender_status, kyc_tier_required,
       deleted_at, created_at, updated_at
FROM listings
WHERE deleted_at IS NULL`)

	// Visibility: OPEN listings are public; non-OPEN only to their owner.
	// Enforced in SQL so keyset pagination stays correct (see SearchFilter docs).
	fmt.Fprintf(&sb, " AND (status = 'OPEN' OR owner_user_id = $%d)", n)
	args = append(args, filter.VisibleToUserID)
	n++

	if filter.Query != "" {
		fmt.Fprintf(&sb, " AND "+ftsExpr+" @@ plainto_tsquery('simple', $%d)", n)
		args = append(args, filter.Query)
		n++
	}

	if filter.Status != nil {
		fmt.Fprintf(&sb, " AND status = $%d", n)
		args = append(args, string(*filter.Status))
		n++
	}

	if filter.BudgetMin != nil {
		// Listings whose advertised budget_max is at least the requested floor
		// (or have no budget_max set) remain candidates.
		fmt.Fprintf(&sb, " AND (budget_max IS NULL OR budget_max >= $%d)", n)
		args = append(args, decimalToString(filter.BudgetMin))
		n++
	}

	if filter.BudgetMax != nil {
		fmt.Fprintf(&sb, " AND (budget_min IS NULL OR budget_min <= $%d)", n)
		args = append(args, decimalToString(filter.BudgetMax))
		n++
	}

	if filter.After != nil {
		// Keyset: strictly older than the cursor position, ties broken by id.
		fmt.Fprintf(&sb, " AND (created_at, id) < ($%d, $%d)", n, n+1)
		args = append(args, filter.After.CreatedAt, filter.After.ID)
		n += 2
	}

	sb.WriteString(" ORDER BY created_at DESC, id DESC")

	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}

	fmt.Fprintf(&sb, " LIMIT $%d", n)
	args = append(args, limit)

	return sb.String(), args
}

// getListingsByIDs fetches listings for the given ID set, enforcing visibility
// IN SQL (OPEN public, non-OPEN owner-only). Rows not visible to viewerID are
// silently omitted — no post-filtering in Go, which would leak hidden-doc counts.
// The returned slice is ordered to match the input ID ordering using a CASE
// expression, so the caller (RRF) does not need a secondary sort.
func getListingsByIDs(ctx context.Context, q querier, ids []uuid.UUID, viewerID uuid.UUID) ([]*domain.Listing, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var (
		sb   strings.Builder
		args []any
		n    = 1
	)

	sb.WriteString(`
SELECT id, owner_user_id, title, description, budget_min, budget_max,
       currency, status, awarded_bid_id,
       is_tender, recruiter_mode, tender_status, kyc_tier_required,
       deleted_at, created_at, updated_at
FROM listings
WHERE deleted_at IS NULL`)

	// Visibility: same predicate as buildSearchQuery — OPEN public, non-OPEN owner-only.
	fmt.Fprintf(&sb, " AND (status = 'OPEN' OR owner_user_id = $%d)", n)
	args = append(args, viewerID)
	n++

	// Build the $N,$N+1,... IN clause for the id set.
	sb.WriteString(" AND id IN (")

	for i, id := range ids {
		if i > 0 {
			sb.WriteString(",")
		}

		fmt.Fprintf(&sb, "$%d", n)
		args = append(args, id)
		n++
	}

	sb.WriteString(")")

	// Preserve the input ordering via CASE so RRF rank is stable.
	sb.WriteString(" ORDER BY CASE id")

	for i, id := range ids {
		fmt.Fprintf(&sb, " WHEN $%d THEN %d", n, i)
		args = append(args, id)
		n++
	}

	sb.WriteString(" END")

	rows, err := q.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("get listings by ids: %w", err)
	}

	defer rows.Close()

	var listings []*domain.Listing

	for rows.Next() {
		l, scanErr := scanListing(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		listings = append(listings, l)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate listings by ids: %w", err)
	}

	return listings, nil
}

func updateListing(ctx context.Context, q querier, l *domain.Listing) error {
	const query = `
UPDATE listings
SET title = $2, description = $3, budget_min = $4, budget_max = $5,
    currency = $6, status = $7, awarded_bid_id = $8,
    is_tender = $9, recruiter_mode = $10, tender_status = $11, kyc_tier_required = $12,
    updated_at = $13
WHERE id = $1 AND deleted_at IS NULL
`

	recruiterMode := l.RecruiterMode
	if recruiterMode == "" {
		recruiterMode = domain.RecruiterModeClosed
	}

	tag, err := q.Exec(
		ctx, query,
		l.ID, l.Title, l.Description,
		decimalToString(l.BudgetMin), decimalToString(l.BudgetMax),
		l.Currency, l.Status, l.AwardedBidID,
		l.IsTender, string(recruiterMode), tenderStatusToString(l.TenderStatus), l.KYCTierRequired,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("update listing: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrListingNotFound
	}

	return nil
}

// rowScanner is satisfied by pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanListing(row rowScanner) (*domain.Listing, error) {
	var (
		l             domain.Listing
		budgetMin     *string
		budgetMax     *string
		recruiterMode string
		tenderStatus  *string
	)

	err := row.Scan(
		&l.ID, &l.OwnerUserID, &l.Title, &l.Description,
		&budgetMin, &budgetMax,
		&l.Currency, &l.Status, &l.AwardedBidID,
		&l.IsTender, &recruiterMode, &tenderStatus, &l.KYCTierRequired,
		&l.DeletedAt, &l.CreatedAt, &l.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrListingNotFound
		}

		return nil, fmt.Errorf("scan listing: %w", err)
	}

	if budgetMin != nil {
		d, parseErr := decimal.NewFromString(*budgetMin)
		if parseErr != nil {
			return nil, fmt.Errorf("parse budget_min: %w", parseErr)
		}

		l.BudgetMin = &d
	}

	if budgetMax != nil {
		d, parseErr := decimal.NewFromString(*budgetMax)
		if parseErr != nil {
			return nil, fmt.Errorf("parse budget_max: %w", parseErr)
		}

		l.BudgetMax = &d
	}

	l.RecruiterMode = domain.RecruiterMode(recruiterMode)

	if tenderStatus != nil {
		ts := domain.TenderStatus(*tenderStatus)
		l.TenderStatus = &ts
	}

	return &l, nil
}

// tenderStatusToString converts *TenderStatus to *string for pgx NULL handling.
func tenderStatusToString(ts *domain.TenderStatus) *string {
	if ts == nil {
		return nil
	}

	s := string(*ts)

	return &s
}

// decimalToString converts a *decimal.Decimal to *string for pgx numeric scanning.
// pgx can store/load numeric as string to avoid float precision loss.
func decimalToString(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}

	s := d.String()

	return &s
}
