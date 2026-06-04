package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// BidStore is a pool-backed bid store.
type BidStore struct {
	q querier
}

// NewBidStore returns a BidStore backed by pool.
func NewBidStore(pool *pgxpool.Pool) *BidStore {
	return &BidStore{q: pool}
}

// txBidStore is a transaction-scoped BidStore.
type txBidStore struct {
	tx querier
}

func (s *txBidStore) Create(ctx context.Context, b *domain.Bid) error {
	return createBid(ctx, s.tx, b)
}

func (s *txBidStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Bid, error) {
	return getBidByID(ctx, s.tx, id)
}

func (s *txBidStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Bid, error) {
	return getBidByIDForUpdate(ctx, s.tx, id)
}

func (s *txBidStore) ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.Bid, error) {
	return listBidsByListing(ctx, s.tx, listingID)
}

func (s *txBidStore) ListByBidder(ctx context.Context, bidderUserID uuid.UUID) ([]*domain.Bid, error) {
	return listBidsByBidder(ctx, s.tx, bidderUserID)
}

func (s *txBidStore) Update(ctx context.Context, b *domain.Bid) error {
	return updateBid(ctx, s.tx, b)
}

func (s *txBidStore) RejectSiblingBids(ctx context.Context, listingID, acceptedBidID uuid.UUID) error {
	return rejectSiblingBids(ctx, s.tx, listingID, acceptedBidID)
}

// --- pool-backed methods ---

func (s *BidStore) Create(ctx context.Context, b *domain.Bid) error {
	return createBid(ctx, s.q, b)
}

func (s *BidStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Bid, error) {
	return getBidByID(ctx, s.q, id)
}

// GetByIDForUpdate is not meaningful outside a transaction; delegate to regular read.
// The pool-backed store implements the interface for completeness.
func (s *BidStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Bid, error) {
	return getBidByID(ctx, s.q, id)
}

func (s *BidStore) ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.Bid, error) {
	return listBidsByListing(ctx, s.q, listingID)
}

func (s *BidStore) ListByBidder(ctx context.Context, bidderUserID uuid.UUID) ([]*domain.Bid, error) {
	return listBidsByBidder(ctx, s.q, bidderUserID)
}

func (s *BidStore) Update(ctx context.Context, b *domain.Bid) error {
	return updateBid(ctx, s.q, b)
}

func (s *BidStore) RejectSiblingBids(ctx context.Context, listingID, acceptedBidID uuid.UUID) error {
	return rejectSiblingBids(ctx, s.q, listingID, acceptedBidID)
}

// --- helpers ---

func createBid(ctx context.Context, q querier, b *domain.Bid) error {
	const query = `
INSERT INTO bids
	(id, listing_id, bidder_user_id, amount, currency, message,
	 status, role_id, decided_at, created_at, updated_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
`

	_, err := q.Exec(
		ctx, query,
		b.ID, b.ListingID, b.BidderUserID,
		b.Amount.String(), b.Currency, b.Message,
		b.Status, b.RoleID, b.DecidedAt, b.CreatedAt, b.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.ErrBidAlreadyExists
		}

		return fmt.Errorf("insert bid: %w", err)
	}

	return nil
}

func getBidByID(ctx context.Context, q querier, id uuid.UUID) (*domain.Bid, error) {
	const query = `
SELECT id, listing_id, bidder_user_id, amount, currency, message,
       status, role_id, decided_at, created_at, updated_at
FROM bids
WHERE id = $1
`

	return scanBid(q.QueryRow(ctx, query, id))
}

// getBidByIDForUpdate fetches a bid with SELECT ... FOR UPDATE to prevent TOCTOU races.
// Must be called within an active transaction.
func getBidByIDForUpdate(ctx context.Context, q querier, id uuid.UUID) (*domain.Bid, error) {
	const query = `
SELECT id, listing_id, bidder_user_id, amount, currency, message,
       status, role_id, decided_at, created_at, updated_at
FROM bids
WHERE id = $1
FOR UPDATE
`

	return scanBid(q.QueryRow(ctx, query, id))
}

func listBidsByListing(ctx context.Context, q querier, listingID uuid.UUID) ([]*domain.Bid, error) {
	const query = `
SELECT id, listing_id, bidder_user_id, amount, currency, message,
       status, role_id, decided_at, created_at, updated_at
FROM bids
WHERE listing_id = $1
ORDER BY created_at DESC
`

	return queryBids(ctx, q, query, listingID)
}

func listBidsByBidder(ctx context.Context, q querier, bidderUserID uuid.UUID) ([]*domain.Bid, error) {
	const query = `
SELECT id, listing_id, bidder_user_id, amount, currency, message,
       status, role_id, decided_at, created_at, updated_at
FROM bids
WHERE bidder_user_id = $1
ORDER BY created_at DESC
`

	return queryBids(ctx, q, query, bidderUserID)
}

func queryBids(ctx context.Context, q querier, query string, arg any) ([]*domain.Bid, error) {
	rows, err := q.Query(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("list bids: %w", err)
	}

	defer rows.Close()

	var bids []*domain.Bid

	for rows.Next() {
		b, scanErr := scanBid(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		bids = append(bids, b)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bids: %w", err)
	}

	return bids, nil
}

func updateBid(ctx context.Context, q querier, b *domain.Bid) error {
	const query = `
UPDATE bids
SET status = $2, decided_at = $3, updated_at = $4
WHERE id = $1
`

	tag, err := q.Exec(
		ctx, query,
		b.ID, b.Status, b.DecidedAt, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("update bid: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrBidNotFound
	}

	return nil
}

func rejectSiblingBids(ctx context.Context, q querier, listingID, acceptedBidID uuid.UUID) error {
	const query = `
UPDATE bids
SET status = 'REJECTED', decided_at = now(), updated_at = now()
WHERE listing_id = $1 AND id != $2 AND status = 'PENDING'
`

	_, err := q.Exec(ctx, query, listingID, acceptedBidID)
	if err != nil {
		return fmt.Errorf("reject sibling bids: %w", err)
	}

	return nil
}

func scanBid(row rowScanner) (*domain.Bid, error) {
	var (
		b      domain.Bid
		amount string
	)

	err := row.Scan(
		&b.ID, &b.ListingID, &b.BidderUserID,
		&amount, &b.Currency, &b.Message,
		&b.Status, &b.RoleID, &b.DecidedAt, &b.CreatedAt, &b.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrBidNotFound
		}

		return nil, fmt.Errorf("scan bid: %w", err)
	}

	d, err := decimal.NewFromString(amount)
	if err != nil {
		return nil, fmt.Errorf("parse bid amount: %w", err)
	}

	b.Amount = d

	return &b, nil
}
