// Package recommendation provides the AI recommendation retention runner for the
// marketplace service. The runner deletes ai_recommendation rows older than
// recommendationRetentionPeriod (30 days) on each scheduled cycle.
//
// # Retention rationale
//
// Migration 000013 documents the 30-day retention period: AI recommendation
// audit data is operationally useful for 30 days; beyond that it constitutes
// excess PII storage under GDPR Art. 5(1)(c) data minimisation.
//
// # Pattern
//
// Mirrors internal/outbox/poller.go: a long-lived goroutine driven by a time.Ticker,
// using context.Background()-derived per-operation timeouts so the goroutine is not
// canceled when any specific request context closes (backend-security §5 goroutine rule).
package recommendation

import (
	"context"
	"log/slog"
	"time"

	"github.com/CoverOnes/marketplace/internal/store"
)

const (
	// recommendationRetentionPeriod is how long ai_recommendation rows are kept.
	// Documented in migration 000013 comment.
	recommendationRetentionPeriod = 30 * 24 * time.Hour

	// defaultRetentionInterval is the default interval between retention passes.
	defaultRetentionInterval = 24 * time.Hour
)

// RetentionRunner is an in-process retention runner for ai_recommendation rows.
// Start it with Run; stop it by canceling the context passed to Run.
type RetentionRunner struct {
	store    store.RecommendationStore
	interval time.Duration
}

// NewRetentionRunner returns a RetentionRunner backed by s.
// interval is the retention check frequency (default 24h when <= 0).
func NewRetentionRunner(s store.RecommendationStore, interval time.Duration) *RetentionRunner {
	if interval <= 0 {
		interval = defaultRetentionInterval
	}

	return &RetentionRunner{
		store:    s,
		interval: interval,
	}
}

// Run starts the retention loop. It blocks until ctx is canceled.
// Safe to call from a goroutine; each retention pass uses an independent
// context.Background()-derived timeout so it is not canceled on caller shutdown
// mid-pass (backend-security §5: goroutine must not inherit request context).
func (r *RetentionRunner) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	slog.Info("recommendation retention runner started",
		"interval", r.interval,
		"retention_period", recommendationRetentionPeriod,
	)

	// Run one pass immediately on startup so a freshly deployed service cleans
	// up without waiting up to 24 h for the first tick.
	r.runPass() //nolint:contextcheck // intentionally uses context.Background() -- backend-security §5: goroutine must not inherit request context

	for {
		select {
		case <-ctx.Done():
			slog.Info("recommendation retention runner stopping")

			return
		case <-ticker.C:
			r.runPass() //nolint:contextcheck // intentionally uses context.Background() -- backend-security §5: goroutine must not inherit request context
		}
	}
}

// runPass deletes ai_recommendation rows older than recommendationRetentionPeriod.
func (r *RetentionRunner) runPass() {
	retCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cutoff := time.Now().UTC().Add(-recommendationRetentionPeriod)

	n, err := r.store.DeleteOlderThan(retCtx, cutoff)
	if err != nil {
		slog.Warn("recommendation retention delete failed", "err", err)

		return
	}

	if n > 0 {
		slog.Info("recommendation retention: deleted old rows", "count", n, "cutoff", cutoff)
	}
}
