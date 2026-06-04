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
