// Package migrate provides a boot-time migration runner for the marketplace
// service. It applies embedded SQL migrations via golang-migrate over the pgx/v5
// stdlib adapter so that a fresh deploy always starts with an up-to-date schema.
package migrate

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers "pgx5" driver
	"github.com/golang-migrate/migrate/v4/source/iofs"

	migrations "github.com/CoverOnes/marketplace/migrations"
)

// Run applies all pending *.up.sql migrations from the embedded FS against the
// Postgres database identified by dsn. It is idempotent: calling it on an
// already-migrated DB is a no-op (ErrNoChange is silently swallowed).
//
// dsn must use the pgx5:// scheme (or a DSN that golang-migrate can rewrite to
// pgx5://). In practice we accept a plain postgres:// DSN and rewrite the scheme
// here, keeping main.go unaware of the driver detail.
//
// Errors from the migration runner (other than ErrNoChange) are returned
// unwrapped so callers can decide whether to fatal or warn.
func Run(dsn string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("create iofs migration source: %w", err)
	}

	// golang-migrate pgx/v5 driver registers under the "pgx5" scheme.
	// Accept both postgres:// and pgx5:// by rewriting the scheme.
	driverDSN := rewriteScheme(dsn)

	m, err := migrate.NewWithSourceInstance("iofs", src, driverDSN)
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}

	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			slog.Warn("migrate close source error", "err", srcErr)
		}

		if dbErr != nil {
			slog.Warn("migrate close db error", "err", dbErr)
		}
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("run migrations: %w", err)
	}

	slog.Info("migrations applied (or already up-to-date)")

	return nil
}

// rewriteScheme converts a postgres:// or postgresql:// DSN to the pgx5://
// scheme that golang-migrate's pgx/v5 driver expects. Any other scheme (e.g.
// already "pgx5://") is returned unchanged.
func rewriteScheme(dsn string) string {
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if len(dsn) >= len(prefix) && dsn[:len(prefix)] == prefix {
			return "pgx5://" + dsn[len(prefix):]
		}
	}

	return dsn
}
