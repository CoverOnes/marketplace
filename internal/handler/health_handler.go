// Package handler wires up Gin routes for the marketplace service.
package handler

import (
	"github.com/CoverOnes/marketplace/internal/platform/health"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewHealthHandler returns a health handler backed by pool.
func NewHealthHandler(pool *pgxpool.Pool) *health.Handler {
	return health.NewHandler(pool)
}
