package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// NewEmbeddingPool creates a pgxpool.Pool with pgvector types registered on
// every connection. It is identical to NewPool but adds an AfterConnect hook
// that calls pgxvec.RegisterTypes so the vector codec is available for Scan
// and Query operations.
//
// schema and opts behave identically to NewPool. The pgvector extension MUST
// already be enabled in the database (migration 000010 creates it).
func NewEmbeddingPool(ctx context.Context, dsn, schema string, opts PoolOptions) (*pgxpool.Pool, error) {
	// Reuse the validated pool config from NewPool — but we need to intercept
	// AfterConnect to chain pgvector registration. Build config directly.
	if schema != "" && !poolSchemaNameRe.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q: must match ^[a-zA-Z_][a-zA-Z0-9_]*$", schema)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pgx config: %w", err)
	}

	maxConns := opts.MaxConns
	if maxConns <= 0 {
		maxConns = 10
	}

	minConns := opts.MinConns
	if minConns <= 0 {
		minConns = 2
	}

	cfg.MaxConns = maxConns
	cfg.MinConns = minConns

	var quotedSchema string
	if schema != "" {
		quotedSchema = pgx.Identifier{schema}.Sanitize()
	}

	cfg.AfterConnect = func(connectCtx context.Context, conn *pgx.Conn) error {
		if schema != "" {
			if _, execErr := conn.Exec(connectCtx, "SET search_path = "+quotedSchema+", public"); execErr != nil {
				return fmt.Errorf("set search_path=%s,public: %w", schema, execErr)
			}
		}

		if regErr := pgxvec.RegisterTypes(connectCtx, conn); regErr != nil {
			return fmt.Errorf("register pgvector types: %w", regErr)
		}

		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create embedding pgxpool: %w", err)
	}

	if schema != "" {
		if _, execErr := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quotedSchema); execErr != nil {
			pool.Close()
			return nil, fmt.Errorf("create schema %q: %w", schema, execErr)
		}
	}

	if pingErr := pool.Ping(ctx); pingErr != nil {
		pool.Close()
		return nil, fmt.Errorf("ping embedding postgres: %w", pingErr)
	}

	return pool, nil
}
