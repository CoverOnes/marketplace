package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// TenderMilestoneStore is a pool-backed store for tender milestones.
type TenderMilestoneStore struct {
	q querier
}

// NewTenderMilestoneStore returns a TenderMilestoneStore backed by pool.
func NewTenderMilestoneStore(pool *pgxpool.Pool) *TenderMilestoneStore {
	return &TenderMilestoneStore{q: pool}
}

// txTenderMilestoneStore is a transaction-scoped TenderMilestoneStore.
type txTenderMilestoneStore struct {
	tx querier
}

func (s *txTenderMilestoneStore) Create(ctx context.Context, m *domain.TenderMilestone) error {
	return createTenderMilestone(ctx, s.tx, m)
}

func (s *txTenderMilestoneStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.TenderMilestone, error) {
	return getTenderMilestoneByID(ctx, s.tx, id)
}

func (s *txTenderMilestoneStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderMilestone, error) {
	return getTenderMilestoneByIDForUpdate(ctx, s.tx, id)
}

func (s *txTenderMilestoneStore) ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.TenderMilestone, error) {
	return listTenderMilestonesByListing(ctx, s.tx, listingID)
}

func (s *txTenderMilestoneStore) Update(ctx context.Context, m *domain.TenderMilestone) error {
	return updateTenderMilestone(ctx, s.tx, m)
}

// --- pool-backed methods ---

// Create inserts a new tender milestone.
func (s *TenderMilestoneStore) Create(ctx context.Context, m *domain.TenderMilestone) error {
	return createTenderMilestone(ctx, s.q, m)
}

// GetByID fetches a tender milestone by primary key.
func (s *TenderMilestoneStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.TenderMilestone, error) {
	return getTenderMilestoneByID(ctx, s.q, id)
}

// GetByIDForUpdate is not meaningful outside a transaction; delegates to regular read.
func (s *TenderMilestoneStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderMilestone, error) {
	return getTenderMilestoneByID(ctx, s.q, id)
}

// ListByListing returns all milestones for a listing ordered by creation time.
func (s *TenderMilestoneStore) ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.TenderMilestone, error) {
	return listTenderMilestonesByListing(ctx, s.q, listingID)
}

// Update persists milestone changes.
func (s *TenderMilestoneStore) Update(ctx context.Context, m *domain.TenderMilestone) error {
	return updateTenderMilestone(ctx, s.q, m)
}

// --- helpers ---

func createTenderMilestone(ctx context.Context, q querier, m *domain.TenderMilestone) error {
	const query = `
INSERT INTO tender_milestones
	(id, listing_id, title, due_date, amount, currency, status, reached_at, created_at, updated_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
`

	_, err := q.Exec(
		ctx, query,
		m.ID, m.ListingID, m.Title, m.DueDate,
		decimalToString(m.Amount), m.Currency,
		m.Status, m.ReachedAt, m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert tender milestone: %w", err)
	}

	return nil
}

func getTenderMilestoneByID(ctx context.Context, q querier, id uuid.UUID) (*domain.TenderMilestone, error) {
	const query = `
SELECT id, listing_id, title, due_date, amount, currency, status, reached_at, created_at, updated_at
FROM tender_milestones
WHERE id = $1
`

	return scanTenderMilestone(q.QueryRow(ctx, query, id))
}

// getTenderMilestoneByIDForUpdate fetches a milestone with SELECT ... FOR UPDATE
// to prevent TOCTOU races on concurrent status transitions.
// Must be called within an active transaction.
func getTenderMilestoneByIDForUpdate(ctx context.Context, q querier, id uuid.UUID) (*domain.TenderMilestone, error) {
	const query = `
SELECT id, listing_id, title, due_date, amount, currency, status, reached_at, created_at, updated_at
FROM tender_milestones
WHERE id = $1
FOR UPDATE
`

	return scanTenderMilestone(q.QueryRow(ctx, query, id))
}

func listTenderMilestonesByListing(ctx context.Context, q querier, listingID uuid.UUID) ([]*domain.TenderMilestone, error) {
	const query = `
SELECT id, listing_id, title, due_date, amount, currency, status, reached_at, created_at, updated_at
FROM tender_milestones
WHERE listing_id = $1
ORDER BY created_at ASC
`

	rows, err := q.Query(ctx, query, listingID)
	if err != nil {
		return nil, fmt.Errorf("list tender milestones: %w", err)
	}

	defer rows.Close()

	var milestones []*domain.TenderMilestone

	for rows.Next() {
		m, scanErr := scanTenderMilestone(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		milestones = append(milestones, m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tender milestones: %w", err)
	}

	return milestones, nil
}

func updateTenderMilestone(ctx context.Context, q querier, m *domain.TenderMilestone) error {
	const query = `
UPDATE tender_milestones
SET title = $2, due_date = $3, amount = $4, currency = $5,
    status = $6, reached_at = $7, updated_at = $8
WHERE id = $1
`

	tag, err := q.Exec(
		ctx, query,
		m.ID, m.Title, m.DueDate,
		decimalToString(m.Amount), m.Currency,
		m.Status, m.ReachedAt, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("update tender milestone: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrTenderMilestoneNotFound
	}

	return nil
}

func scanTenderMilestone(row rowScanner) (*domain.TenderMilestone, error) {
	var (
		m      domain.TenderMilestone
		amount *string
	)

	err := row.Scan(
		&m.ID, &m.ListingID, &m.Title, &m.DueDate,
		&amount, &m.Currency,
		&m.Status, &m.ReachedAt, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTenderMilestoneNotFound
		}

		return nil, fmt.Errorf("scan tender milestone: %w", err)
	}

	if amount != nil {
		d, parseErr := decimal.NewFromString(*amount)
		if parseErr != nil {
			return nil, fmt.Errorf("parse milestone amount: %w", parseErr)
		}

		m.Amount = &d
	}

	return &m, nil
}
