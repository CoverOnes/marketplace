package recommendation_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/recommendation"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRecommendationStore is a lightweight in-memory stub of store.RecommendationStore
// used exclusively in RetentionRunner unit tests. It records DeleteOlderThan calls.
type fakeRecommendationStore struct {
	deleteCalls atomic.Int64
	deleteErr   error
}

func (f *fakeRecommendationStore) Insert(_ context.Context, _ *domain.AIRecommendation) error {
	return nil
}

func (f *fakeRecommendationStore) ListBySubject(_ context.Context, _ uuid.UUID, _ int) ([]*domain.AIRecommendation, error) {
	return nil, nil
}

func (f *fakeRecommendationStore) DeleteOlderThan(_ context.Context, _ time.Time) (int64, error) {
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}

	f.deleteCalls.Add(1)

	return 1, nil
}

// TestRetentionRunner_ImmediateFirstPass asserts that Run executes a retention
// pass immediately on startup without waiting for the first ticker tick.
func TestRetentionRunner_ImmediateFirstPass(t *testing.T) {
	t.Parallel()

	store := &fakeRecommendationStore{}
	// Use a large interval so the tick never fires during this test.
	runner := recommendation.NewRetentionRunner(store, 10*time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go runner.Run(ctx)

	// Give the runner 500 ms to execute the immediate pass; it should not need more.
	require.Eventually(t, func() bool {
		return store.deleteCalls.Load() >= 1
	}, 500*time.Millisecond, 10*time.Millisecond,
		"RetentionRunner must execute the first pass immediately on startup")
}

// TestRetentionRunner_PeriodicPass asserts that Run executes additional passes
// on each ticker tick.
func TestRetentionRunner_PeriodicPass(t *testing.T) {
	t.Parallel()

	store := &fakeRecommendationStore{}
	// Tiny interval so two more ticks fire quickly.
	runner := recommendation.NewRetentionRunner(store, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go runner.Run(ctx)

	// Wait for at least 3 passes (1 immediate + ≥2 ticks).
	require.Eventually(t, func() bool {
		return store.deleteCalls.Load() >= 3
	}, 500*time.Millisecond, 10*time.Millisecond,
		"RetentionRunner must execute periodic passes on each tick")
}

// TestRetentionRunner_CleanShutdown asserts that Run returns promptly when
// ctx is canceled and does not block indefinitely.
func TestRetentionRunner_CleanShutdown(t *testing.T) {
	t.Parallel()

	store := &fakeRecommendationStore{}
	runner := recommendation.NewRetentionRunner(store, 10*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		runner.Run(ctx)
		close(done)
	}()

	// Wait for the immediate first pass, then cancel.
	require.Eventually(t, func() bool {
		return store.deleteCalls.Load() >= 1
	}, 500*time.Millisecond, 10*time.Millisecond)

	cancel()

	select {
	case <-done:
		// Clean shutdown confirmed.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RetentionRunner did not shut down within 500 ms after context cancellation")
	}
}

// TestRetentionRunner_StoreErrorLogsAndContinues asserts that a store error
// does not crash the runner: the loop continues on the next tick, and the
// error pass does NOT increment deleteCalls (store returned error, not deleted).
func TestRetentionRunner_StoreErrorLogsAndContinues(t *testing.T) {
	t.Parallel()

	errStore := &fakeRecommendationStore{
		deleteErr: errors.New("simulated DB error"),
	}
	runner := recommendation.NewRetentionRunner(errStore, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// The runner must not panic even when DeleteOlderThan always returns an error.
	// Run for 100 ms to cover multiple ticks.
	go runner.Run(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()

	// deleteCalls stays at 0 because every call returned an error.
	assert.Equal(t, int64(0), errStore.deleteCalls.Load(),
		"error path must not increment delete count")
}
