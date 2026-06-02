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

// Update persists changes to a listing.
func (s *ListingStore) Update(ctx context.Context, l *domain.Listing) error {
	return updateListing(ctx, s.q, l)
}

// --- helpers shared by pool and tx stores ---

func createListing(ctx context.Context, q querier, l *domain.Listing) error {
	const query = `
INSERT INTO listings
	(id, owner_user_id, title, description, budget_min, budget_max,
	 currency, status, awarded_bid_id, deleted_at, created_at, updated_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
`

	_, err := q.Exec(
		ctx, query,
		l.ID, l.OwnerUserID, l.Title, l.Description,
		decimalToString(l.BudgetMin), decimalToString(l.BudgetMax),
		l.Currency, l.Status, l.AwardedBidID, l.DeletedAt,
		l.CreatedAt, l.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert listing: %w", err)
	}

	return nil
}

func getListingByID(ctx context.Context, q querier, id uuid.UUID) (*domain.Listing, error) {
	const query = `
SELECT id, owner_user_id, title, description, budget_min, budget_max,
       currency, status, awarded_bid_id, deleted_at, created_at, updated_at
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
       currency, status, awarded_bid_id, deleted_at, created_at, updated_at
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
       currency, status, awarded_bid_id, deleted_at, created_at, updated_at
FROM listings
WHERE deleted_at IS NULL`)

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

func updateListing(ctx context.Context, q querier, l *domain.Listing) error {
	const query = `
UPDATE listings
SET title = $2, description = $3, budget_min = $4, budget_max = $5,
    currency = $6, status = $7, awarded_bid_id = $8, updated_at = $9
WHERE id = $1 AND deleted_at IS NULL
`

	tag, err := q.Exec(
		ctx, query,
		l.ID, l.Title, l.Description,
		decimalToString(l.BudgetMin), decimalToString(l.BudgetMax),
		l.Currency, l.Status, l.AwardedBidID, time.Now().UTC(),
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
		l         domain.Listing
		budgetMin *string
		budgetMax *string
	)

	err := row.Scan(
		&l.ID, &l.OwnerUserID, &l.Title, &l.Description,
		&budgetMin, &budgetMax,
		&l.Currency, &l.Status, &l.AwardedBidID, &l.DeletedAt,
		&l.CreatedAt, &l.UpdatedAt,
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

	return &l, nil
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
