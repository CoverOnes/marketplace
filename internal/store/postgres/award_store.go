package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// AwardStore is a pool-backed award store.
type AwardStore struct {
	q querier
}

// NewAwardStore returns an AwardStore backed by pool.
func NewAwardStore(pool *pgxpool.Pool) *AwardStore {
	return &AwardStore{q: pool}
}

// txAwardStore is a transaction-scoped AwardStore.
type txAwardStore struct {
	tx querier
}

func (s *txAwardStore) Create(ctx context.Context, a *domain.Award) error {
	return createAward(ctx, s.tx, a)
}

func (s *txAwardStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Award, error) {
	return getAwardByID(ctx, s.tx, id)
}

func (s *txAwardStore) MarkEventPublished(ctx context.Context, awardID uuid.UUID) error {
	return markAwardEventPublished(ctx, s.tx, awardID)
}

// --- pool-backed methods ---

func (s *AwardStore) Create(ctx context.Context, a *domain.Award) error {
	return createAward(ctx, s.q, a)
}

func (s *AwardStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Award, error) {
	return getAwardByID(ctx, s.q, id)
}

func (s *AwardStore) MarkEventPublished(ctx context.Context, awardID uuid.UUID) error {
	return markAwardEventPublished(ctx, s.q, awardID)
}

// --- helpers ---

func createAward(ctx context.Context, q querier, a *domain.Award) error {
	const query = `
INSERT INTO awards
	(id, listing_id, bid_id, owner_user_id, bidder_user_id,
	 amount, currency, event_published_at, created_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9)
`

	_, err := q.Exec(
		ctx, query,
		a.ID, a.ListingID, a.BidID, a.OwnerUserID, a.BidderUserID,
		a.Amount.String(), a.Currency, a.EventPublishedAt, a.CreatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// awards_listing_id_unique violated — concurrent accept attempt
			return domain.ErrConflict
		}

		return fmt.Errorf("insert award: %w", err)
	}

	return nil
}

func getAwardByID(ctx context.Context, q querier, id uuid.UUID) (*domain.Award, error) {
	const query = `
SELECT id, listing_id, bid_id, owner_user_id, bidder_user_id,
       amount, currency, event_published_at, created_at
FROM awards
WHERE id = $1
`

	return scanAward(q.QueryRow(ctx, query, id))
}

func markAwardEventPublished(ctx context.Context, q querier, awardID uuid.UUID) error {
	const query = `
UPDATE awards
SET event_published_at = now()
WHERE id = $1
`

	tag, err := q.Exec(ctx, query, awardID)
	if err != nil {
		return fmt.Errorf("mark award event published: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrAwardNotFound
	}

	return nil
}

func scanAward(row rowScanner) (*domain.Award, error) {
	var (
		a      domain.Award
		amount string
	)

	err := row.Scan(
		&a.ID, &a.ListingID, &a.BidID, &a.OwnerUserID, &a.BidderUserID,
		&amount, &a.Currency, &a.EventPublishedAt, &a.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrAwardNotFound
		}

		return nil, fmt.Errorf("scan award: %w", err)
	}

	d, err := decimal.NewFromString(amount)
	if err != nil {
		return nil, fmt.Errorf("parse award amount: %w", err)
	}

	a.Amount = d

	return &a, nil
}
