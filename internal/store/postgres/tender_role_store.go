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
)

// TenderRoleStore is a pool-backed store for tender roles.
type TenderRoleStore struct {
	q querier
}

// NewTenderRoleStore returns a TenderRoleStore backed by pool.
func NewTenderRoleStore(pool *pgxpool.Pool) *TenderRoleStore {
	return &TenderRoleStore{q: pool}
}

// txTenderRoleStore is a transaction-scoped TenderRoleStore.
type txTenderRoleStore struct {
	tx querier
}

func (s *txTenderRoleStore) Create(ctx context.Context, r *domain.TenderRole) error {
	return createTenderRole(ctx, s.tx, r)
}

func (s *txTenderRoleStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.TenderRole, error) {
	return getTenderRoleByID(ctx, s.tx, id)
}

func (s *txTenderRoleStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderRole, error) {
	return getTenderRoleByIDForUpdate(ctx, s.tx, id)
}

func (s *txTenderRoleStore) ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.TenderRole, error) {
	return listTenderRolesByListing(ctx, s.tx, listingID)
}

func (s *txTenderRoleStore) Update(ctx context.Context, r *domain.TenderRole) error {
	return updateTenderRole(ctx, s.tx, r)
}

// --- pool-backed methods ---

func (s *TenderRoleStore) Create(ctx context.Context, r *domain.TenderRole) error {
	return createTenderRole(ctx, s.q, r)
}

func (s *TenderRoleStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.TenderRole, error) {
	return getTenderRoleByID(ctx, s.q, id)
}

// GetByIDForUpdate is not meaningful outside a transaction; delegate to regular read.
func (s *TenderRoleStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderRole, error) {
	return getTenderRoleByID(ctx, s.q, id)
}

func (s *TenderRoleStore) ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.TenderRole, error) {
	return listTenderRolesByListing(ctx, s.q, listingID)
}

func (s *TenderRoleStore) Update(ctx context.Context, r *domain.TenderRole) error {
	return updateTenderRole(ctx, s.q, r)
}

// --- helpers ---

func createTenderRole(ctx context.Context, q querier, r *domain.TenderRole) error {
	const query = `
INSERT INTO tender_roles
	(id, listing_id, title, description, max_collaborators, profit_share_bps,
	 profit_share_note, status, sort_order, created_at, updated_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
`

	_, err := q.Exec(
		ctx, query,
		r.ID, r.ListingID, r.Title, r.Description,
		r.MaxCollaborators, r.ProfitShareBPS, r.ProfitShareNote,
		r.Status, r.SortOrder, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert tender role: %w", err)
	}

	return nil
}

func getTenderRoleByID(ctx context.Context, q querier, id uuid.UUID) (*domain.TenderRole, error) {
	const query = `
SELECT id, listing_id, title, description, max_collaborators, profit_share_bps,
       profit_share_note, status, sort_order, created_at, updated_at
FROM tender_roles
WHERE id = $1
`

	return scanTenderRole(q.QueryRow(ctx, query, id))
}

// getTenderRoleByIDForUpdate fetches a tender role with SELECT ... FOR UPDATE
// to prevent TOCTOU races on max_collaborators checks.
// Must be called within an active transaction.
func getTenderRoleByIDForUpdate(ctx context.Context, q querier, id uuid.UUID) (*domain.TenderRole, error) {
	const query = `
SELECT id, listing_id, title, description, max_collaborators, profit_share_bps,
       profit_share_note, status, sort_order, created_at, updated_at
FROM tender_roles
WHERE id = $1
FOR UPDATE
`

	return scanTenderRole(q.QueryRow(ctx, query, id))
}

func listTenderRolesByListing(ctx context.Context, q querier, listingID uuid.UUID) ([]*domain.TenderRole, error) {
	const query = `
SELECT id, listing_id, title, description, max_collaborators, profit_share_bps,
       profit_share_note, status, sort_order, created_at, updated_at
FROM tender_roles
WHERE listing_id = $1
ORDER BY sort_order ASC, created_at ASC
`

	rows, err := q.Query(ctx, query, listingID)
	if err != nil {
		return nil, fmt.Errorf("list tender roles: %w", err)
	}

	defer rows.Close()

	var roles []*domain.TenderRole

	for rows.Next() {
		r, scanErr := scanTenderRole(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		roles = append(roles, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tender roles: %w", err)
	}

	return roles, nil
}

func updateTenderRole(ctx context.Context, q querier, r *domain.TenderRole) error {
	const query = `
UPDATE tender_roles
SET title = $2, description = $3, max_collaborators = $4, profit_share_bps = $5,
    profit_share_note = $6, status = $7, sort_order = $8, updated_at = $9
WHERE id = $1
`

	tag, err := q.Exec(
		ctx, query,
		r.ID, r.Title, r.Description,
		r.MaxCollaborators, r.ProfitShareBPS, r.ProfitShareNote,
		r.Status, r.SortOrder, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("update tender role: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrTenderRoleNotFound
	}

	return nil
}

func scanTenderRole(row rowScanner) (*domain.TenderRole, error) {
	var r domain.TenderRole

	err := row.Scan(
		&r.ID, &r.ListingID, &r.Title, &r.Description,
		&r.MaxCollaborators, &r.ProfitShareBPS, &r.ProfitShareNote,
		&r.Status, &r.SortOrder, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTenderRoleNotFound
		}

		return nil, fmt.Errorf("scan tender role: %w", err)
	}

	return &r, nil
}
