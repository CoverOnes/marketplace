// Package service_test integration-test helpers.
// TestMain starts ONE Postgres container for the entire service_test package,
// applies migrations once, and tears down the container after all tests finish.
// Individual tests use sharedServiceDSN — no per-test container spinup needed.
package service_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"

	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	migrations "github.com/CoverOnes/marketplace/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// sharedServiceDSN is set once in TestMain; read-only for all integration tests.
var sharedServiceDSN string

// TestMain starts ONE Postgres container for the entire service_test package.
// Integration tests use sharedServiceDSN to create their own short-lived pools.
func TestMain(m *testing.M) {
	ctx := context.Background()

	ctr, err := tcpostgres.Run(
		ctx,
		"pgvector/pgvector:pg17",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start shared service test container: %v\n", err)
		os.Exit(1)
	}

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get shared service container DSN: %v\n", err)
		_ = ctr.Terminate(ctx)
		os.Exit(1)
	}

	sharedServiceDSN = dsn

	pool, err := pgstore.NewPool(ctx, dsn, "", pgstore.PoolOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create shared service pool: %v\n", err)
		_ = ctr.Terminate(ctx)
		os.Exit(1)
	}

	if err := serviceApplyMigrations(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "apply service migrations: %v\n", err)
		pool.Close()
		_ = ctr.Terminate(ctx)
		os.Exit(1)
	}

	pool.Close()

	code := m.Run()

	_ = ctr.Terminate(ctx)
	os.Exit(code)
}

// serviceApplyMigrations runs all embedded *.up.sql files in order against pool.
// Replaces the per-test runServiceMigrations helper.
func serviceApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	var upFiles []string

	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !d.IsDir() && strings.HasSuffix(path, ".up.sql") {
			upFiles = append(upFiles, path)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("walk migrations FS: %w", err)
	}

	if len(upFiles) == 0 {
		return fmt.Errorf("no *.up.sql files found in embedded FS")
	}

	sort.Strings(upFiles)

	for _, file := range upFiles {
		data, readErr := migrations.FS.ReadFile(file)
		if readErr != nil {
			return fmt.Errorf("read migration %s: %w", file, readErr)
		}

		if _, execErr := pool.Exec(ctx, string(data)); execErr != nil {
			return fmt.Errorf("apply migration %s: %w", file, execErr)
		}
	}

	return nil
}
