package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runTx begins a transaction on pool, executes fn, and commits.
// If fn returns an error the transaction is rolled back.
// This helper eliminates structural duplication between the two TxManager types
// (dupl threshold 150 — both WithTx and WithTenderTx share the same begin/rollback/commit skeleton).
func runTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			_ = rbErr
		}
	}()

	if fnErr := fn(tx); fnErr != nil {
		return fnErr
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		return fmt.Errorf("commit transaction: %w", commitErr)
	}

	return nil
}

// TxManager implements store.TxManager using pgxpool.Pool.
type TxManager struct {
	pool *pgxpool.Pool
}

// NewTxManager returns a TxManager backed by the given pool.
func NewTxManager(pool *pgxpool.Pool) *TxManager {
	return &TxManager{pool: pool}
}

// WithTx runs fn inside a single Postgres transaction.
// If fn returns an error the transaction is rolled back; otherwise it is committed.
func (m *TxManager) WithTx(
	ctx context.Context,
	fn func(ctx context.Context, listings store.ListingStore, bids store.BidStore, awards store.AwardStore) error,
) error {
	return runTx(ctx, m.pool, func(tx pgx.Tx) error {
		txListings := &txListingStore{tx: tx}
		txBids := &txBidStore{tx: tx}
		txAwards := &txAwardStore{tx: tx}

		return fn(ctx, txListings, txBids, txAwards)
	})
}

// TenderTxManager implements store.TenderTxManager using pgxpool.Pool.
type TenderTxManager struct {
	pool *pgxpool.Pool
}

// NewTenderTxManager returns a TenderTxManager backed by the given pool.
func NewTenderTxManager(pool *pgxpool.Pool) *TenderTxManager {
	return &TenderTxManager{pool: pool}
}

// WithTenderTx runs fn inside a single Postgres transaction with tender-specific stores.
// If fn returns an error the transaction is rolled back; otherwise it is committed.
func (m *TenderTxManager) WithTenderTx(
	ctx context.Context,
	fn func(ctx context.Context, listings store.ListingStore, roles store.TenderRoleStore, collaborators store.TenderCollaboratorStore) error,
) error {
	return runTx(ctx, m.pool, func(tx pgx.Tx) error {
		txListings := &txListingStore{tx: tx}
		txRoles := &txTenderRoleStore{tx: tx}
		txCollaborators := &txTenderCollaboratorStore{tx: tx}

		return fn(ctx, txListings, txRoles, txCollaborators)
	})
}

// OutboxTxManager implements store.OutboxTxManager using pgxpool.Pool.
// It runs the business operation and outbox Enqueue in a single transaction so
// events are never lost on server restart between commit and publish.
type OutboxTxManager struct {
	pool *pgxpool.Pool
}

// NewOutboxTxManager returns an OutboxTxManager backed by the given pool.
func NewOutboxTxManager(pool *pgxpool.Pool) *OutboxTxManager {
	return &OutboxTxManager{pool: pool}
}

// WithOutboxTx runs fn inside a single Postgres transaction with tender stores and
// a transaction-scoped OutboxStore. If fn returns an error the transaction is rolled back.
func (m *OutboxTxManager) WithOutboxTx(
	ctx context.Context,
	fn func(
		ctx context.Context,
		listings store.ListingStore,
		roles store.TenderRoleStore,
		collaborators store.TenderCollaboratorStore,
		outbox store.OutboxStore,
	) error,
) error {
	return runTx(ctx, m.pool, func(tx pgx.Tx) error {
		txListings := &txListingStore{tx: tx}
		txRoles := &txTenderRoleStore{tx: tx}
		txCollaborators := &txTenderCollaboratorStore{tx: tx}
		txOutbox := &txOutboxStore{tx: tx}

		return fn(ctx, txListings, txRoles, txCollaborators, txOutbox)
	})
}

// MilestoneTxManager implements store.MilestoneTxManager using pgxpool.Pool.
type MilestoneTxManager struct {
	pool *pgxpool.Pool
}

// NewMilestoneTxManager returns a MilestoneTxManager backed by the given pool.
func NewMilestoneTxManager(pool *pgxpool.Pool) *MilestoneTxManager {
	return &MilestoneTxManager{pool: pool}
}

// WithMilestoneTx runs fn inside a single Postgres transaction with listing and
// milestone stores. If fn returns an error the transaction is rolled back.
func (m *MilestoneTxManager) WithMilestoneTx(
	ctx context.Context,
	fn func(ctx context.Context, listings store.ListingStore, milestones store.TenderMilestoneStore) error,
) error {
	return runTx(ctx, m.pool, func(tx pgx.Tx) error {
		txListings := &txListingStore{tx: tx}
		txMilestones := &txTenderMilestoneStore{tx: tx}

		return fn(ctx, txListings, txMilestones)
	})
}

// BidOutboxTxManager implements store.BidOutboxTxManager using pgxpool.Pool.
// It runs the bid accept operation and outbox Enqueue in a single transaction
// to standardize the ad-hoc bid_service MarkEventPublished flag onto the outbox.
type BidOutboxTxManager struct {
	pool *pgxpool.Pool
}

// NewBidOutboxTxManager returns a BidOutboxTxManager backed by the given pool.
func NewBidOutboxTxManager(pool *pgxpool.Pool) *BidOutboxTxManager {
	return &BidOutboxTxManager{pool: pool}
}

// WithBidOutboxTx runs fn inside a single Postgres transaction with bid stores
// and a transaction-scoped OutboxStore. If fn returns an error the transaction is rolled back.
func (m *BidOutboxTxManager) WithBidOutboxTx(
	ctx context.Context,
	fn func(
		ctx context.Context,
		listings store.ListingStore,
		bids store.BidStore,
		awards store.AwardStore,
		outbox store.OutboxStore,
	) error,
) error {
	return runTx(ctx, m.pool, func(tx pgx.Tx) error {
		txListings := &txListingStore{tx: tx}
		txBids := &txBidStore{tx: tx}
		txAwards := &txAwardStore{tx: tx}
		txOutbox := &txOutboxStore{tx: tx}

		return fn(ctx, txListings, txBids, txAwards, txOutbox)
	})
}
