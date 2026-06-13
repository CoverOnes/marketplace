// Package postgres_test contains integration tests for the postgres store layer.
// All tests that use the main shared DB must call sharedDSN() to get the DSN
// and sharedPool() to get a ready pool — both are initialized once in TestMain.
// Tests that need schema isolation (TestSchemaIsolation_Integration,
// TestReservedWordSchema_Integration) use freshTestDB(t) to spin their own
// container so they do not pollute the shared schema.
package postgres_test

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

// sharedTestDSN and sharedTestPool are set once in TestMain and read-only
// thereafter. Tests must NOT call pool.Close() on sharedTestPool.
var (
	sharedTestDSN  string
	sharedTestPool *pgxpool.Pool
)

// TestMain starts ONE Postgres container for the entire postgres_test package,
// applies migrations once, and tears down the container after all tests finish.
// This replaces the old per-test tcpostgres.Run approach, which created a new
// container for every test function and exhausted Docker resources under -count=1.
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
		fmt.Fprintf(os.Stderr, "start shared test container: %v\n", err)
		os.Exit(1)
	}

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get shared container DSN: %v\n", err)
		_ = ctr.Terminate(ctx)
		os.Exit(1)
	}

	sharedTestDSN = dsn

	pool, err := pgstore.NewPool(ctx, dsn, "", pgstore.PoolOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create shared pool: %v\n", err)
		_ = ctr.Terminate(ctx)
		os.Exit(1)
	}

	sharedTestPool = pool

	// Run migrations once for the shared container.
	if err := applyMigrations(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "apply shared migrations: %v\n", err)
		pool.Close()
		_ = ctr.Terminate(ctx)
		os.Exit(1)
	}

	code := m.Run()

	pool.Close()
	_ = ctr.Terminate(ctx)
	os.Exit(code)
}

// applyMigrations runs all embedded *.up.sql files in order against pool.
// Called once from TestMain; also exposed for the schema-isolation helpers.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
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

// freshTestDB spins up a dedicated Postgres container for tests that need their
// own isolated schema (e.g. TestSchemaIsolation_Integration). Unlike the shared
// container, the returned DSN has NO migrations applied yet.
func freshTestDB(t *testing.T) string {
	t.Helper()

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
		t.Fatalf("start fresh test container: %v", err)
	}

	t.Cleanup(func() {
		if termErr := ctr.Terminate(ctx); termErr != nil {
			t.Logf("terminate fresh container: %v", termErr)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get fresh container DSN: %v", err)
	}

	return dsn
}
