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
)

// TenderCollaboratorStore is a pool-backed store for tender collaborators.
type TenderCollaboratorStore struct {
	q querier
}

// NewTenderCollaboratorStore returns a TenderCollaboratorStore backed by pool.
func NewTenderCollaboratorStore(pool *pgxpool.Pool) *TenderCollaboratorStore {
	return &TenderCollaboratorStore{q: pool}
}

// txTenderCollaboratorStore is a transaction-scoped TenderCollaboratorStore.
type txTenderCollaboratorStore struct {
	tx querier
}

func (s *txTenderCollaboratorStore) Create(ctx context.Context, c *domain.TenderCollaborator) error {
	return createTenderCollaborator(ctx, s.tx, c)
}

func (s *txTenderCollaboratorStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.TenderCollaborator, error) {
	return getTenderCollaboratorByID(ctx, s.tx, id)
}

func (s *txTenderCollaboratorStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderCollaborator, error) {
	return getTenderCollaboratorByIDForUpdate(ctx, s.tx, id)
}

func (s *txTenderCollaboratorStore) CountApprovedByRole(ctx context.Context, roleID uuid.UUID) (int, error) {
	return countApprovedCollaboratorsByRole(ctx, s.tx, roleID)
}

func (s *txTenderCollaboratorStore) ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.TenderCollaborator, error) {
	return listTenderCollaboratorsByListing(ctx, s.tx, listingID)
}

func (s *txTenderCollaboratorStore) ListByRole(ctx context.Context, roleID uuid.UUID) ([]*domain.TenderCollaborator, error) {
	return listTenderCollaboratorsByRole(ctx, s.tx, roleID)
}

func (s *txTenderCollaboratorStore) Update(ctx context.Context, c *domain.TenderCollaborator) error {
	return updateTenderCollaborator(ctx, s.tx, c)
}

// --- pool-backed methods ---

func (s *TenderCollaboratorStore) Create(ctx context.Context, c *domain.TenderCollaborator) error {
	return createTenderCollaborator(ctx, s.q, c)
}

func (s *TenderCollaboratorStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.TenderCollaborator, error) {
	return getTenderCollaboratorByID(ctx, s.q, id)
}

// GetByIDForUpdate is not meaningful outside a transaction; delegate to regular read.
func (s *TenderCollaboratorStore) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.TenderCollaborator, error) {
	return getTenderCollaboratorByID(ctx, s.q, id)
}

func (s *TenderCollaboratorStore) CountApprovedByRole(ctx context.Context, roleID uuid.UUID) (int, error) {
	return countApprovedCollaboratorsByRole(ctx, s.q, roleID)
}

func (s *TenderCollaboratorStore) ListByListing(ctx context.Context, listingID uuid.UUID) ([]*domain.TenderCollaborator, error) {
	return listTenderCollaboratorsByListing(ctx, s.q, listingID)
}

func (s *TenderCollaboratorStore) ListByRole(ctx context.Context, roleID uuid.UUID) ([]*domain.TenderCollaborator, error) {
	return listTenderCollaboratorsByRole(ctx, s.q, roleID)
}

func (s *TenderCollaboratorStore) Update(ctx context.Context, c *domain.TenderCollaborator) error {
	return updateTenderCollaborator(ctx, s.q, c)
}

// --- helpers ---

func createTenderCollaborator(ctx context.Context, q querier, c *domain.TenderCollaborator) error {
	const query = `
INSERT INTO tender_collaborators
	(id, tender_role_id, listing_id, vendor_user_id, status, join_message,
	 approved_at, approved_by_user_id, exited_at, exit_reason,
	 profit_share_bps_override, created_at, updated_at)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
`

	_, err := q.Exec(
		ctx, query,
		c.ID, c.TenderRoleID, c.ListingID, c.VendorUserID,
		c.Status, c.JoinMessage,
		c.ApprovedAt, c.ApprovedByUserID, c.ExitedAt, c.ExitReason,
		c.ProfitShareBPSOverride, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.ErrTenderCollaboratorConflict
		}

		return fmt.Errorf("insert tender collaborator: %w", err)
	}

	return nil
}

func getTenderCollaboratorByID(ctx context.Context, q querier, id uuid.UUID) (*domain.TenderCollaborator, error) {
	const query = `
SELECT id, tender_role_id, listing_id, vendor_user_id, status, join_message,
       approved_at, approved_by_user_id, exited_at, exit_reason,
       profit_share_bps_override, created_at, updated_at
FROM tender_collaborators
WHERE id = $1
`

	return scanTenderCollaborator(q.QueryRow(ctx, query, id))
}

// getTenderCollaboratorByIDForUpdate fetches a collaborator with SELECT ... FOR UPDATE
// to prevent TOCTOU races.
// Must be called within an active transaction.
func getTenderCollaboratorByIDForUpdate(ctx context.Context, q querier, id uuid.UUID) (*domain.TenderCollaborator, error) {
	const query = `
SELECT id, tender_role_id, listing_id, vendor_user_id, status, join_message,
       approved_at, approved_by_user_id, exited_at, exit_reason,
       profit_share_bps_override, created_at, updated_at
FROM tender_collaborators
WHERE id = $1
FOR UPDATE
`

	return scanTenderCollaborator(q.QueryRow(ctx, query, id))
}

// countApprovedCollaboratorsByRole returns the number of APPROVED collaborators
// for the given role. Must be called inside a transaction after locking the role
// row to avoid TOCTOU races on max_collaborators enforcement.
func countApprovedCollaboratorsByRole(ctx context.Context, q querier, roleID uuid.UUID) (int, error) {
	const query = `
SELECT COUNT(*) FROM tender_collaborators
WHERE tender_role_id = $1 AND status = 'APPROVED'
`

	var count int

	if err := q.QueryRow(ctx, query, roleID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count approved collaborators: %w", err)
	}

	return count, nil
}

func listTenderCollaboratorsByListing(ctx context.Context, q querier, listingID uuid.UUID) ([]*domain.TenderCollaborator, error) {
	const query = `
SELECT id, tender_role_id, listing_id, vendor_user_id, status, join_message,
       approved_at, approved_by_user_id, exited_at, exit_reason,
       profit_share_bps_override, created_at, updated_at
FROM tender_collaborators
WHERE listing_id = $1
ORDER BY created_at DESC
`

	return queryTenderCollaborators(ctx, q, query, listingID)
}

func listTenderCollaboratorsByRole(ctx context.Context, q querier, roleID uuid.UUID) ([]*domain.TenderCollaborator, error) {
	const query = `
SELECT id, tender_role_id, listing_id, vendor_user_id, status, join_message,
       approved_at, approved_by_user_id, exited_at, exit_reason,
       profit_share_bps_override, created_at, updated_at
FROM tender_collaborators
WHERE tender_role_id = $1
ORDER BY created_at DESC
`

	return queryTenderCollaborators(ctx, q, query, roleID)
}

func queryTenderCollaborators(ctx context.Context, q querier, query string, arg any) ([]*domain.TenderCollaborator, error) {
	rows, err := q.Query(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("list tender collaborators: %w", err)
	}

	defer rows.Close()

	var collabs []*domain.TenderCollaborator

	for rows.Next() {
		c, scanErr := scanTenderCollaborator(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		collabs = append(collabs, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tender collaborators: %w", err)
	}

	return collabs, nil
}

func updateTenderCollaborator(ctx context.Context, q querier, c *domain.TenderCollaborator) error {
	const query = `
UPDATE tender_collaborators
SET status = $2, join_message = $3,
    approved_at = $4, approved_by_user_id = $5,
    exited_at = $6, exit_reason = $7,
    profit_share_bps_override = $8, updated_at = $9
WHERE id = $1
`

	tag, err := q.Exec(
		ctx, query,
		c.ID, c.Status, c.JoinMessage,
		c.ApprovedAt, c.ApprovedByUserID,
		c.ExitedAt, c.ExitReason,
		c.ProfitShareBPSOverride, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("update tender collaborator: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrTenderCollaboratorNotFound
	}

	return nil
}

func scanTenderCollaborator(row rowScanner) (*domain.TenderCollaborator, error) {
	var c domain.TenderCollaborator

	err := row.Scan(
		&c.ID, &c.TenderRoleID, &c.ListingID, &c.VendorUserID,
		&c.Status, &c.JoinMessage,
		&c.ApprovedAt, &c.ApprovedByUserID, &c.ExitedAt, &c.ExitReason,
		&c.ProfitShareBPSOverride, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTenderCollaboratorNotFound
		}

		return nil, fmt.Errorf("scan tender collaborator: %w", err)
	}

	return &c, nil
}
