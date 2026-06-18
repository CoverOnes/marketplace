package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeRecommendation builds a minimal domain.AIRecommendation for testing.
// recType is used as-is so callers can vary it (satisfies the unparam linter).
func makeRecommendation(subjectUserID uuid.UUID, recType string) *domain.AIRecommendation {
	basis := "Good skill match [REDACTED:api_key]"

	return &domain.AIRecommendation{
		ID:                 uuid.New(),
		RecommendationType: recType,
		TargetID:           uuid.New(),
		SubjectUserID:      subjectUserID,
		OverallScore:       0.85,
		ScoreBreakdown: domain.ScoreBreakdown{
			Skill:       0.9,
			Reliability: 0.8,
			Collab:      0.85,
			Fit:         0.8,
			Comm:        0.9,
		},
		Basis:        &basis,
		Accepted:     nil,
		ModelVersion: "gpt-4o-2024-05",
		CreatedAt:    time.Now().UTC().Truncate(time.Millisecond),
	}
}

// resetRecommendations clears the ai_recommendation table so each test starts
// from a clean slate. Tests share sharedTestPool + the single table and run
// sequentially to avoid ordering dependencies.
func resetRecommendations(ctx context.Context, t *testing.T) {
	t.Helper()

	_, err := sharedTestPool.Exec(ctx, `DELETE FROM ai_recommendation`)
	require.NoError(t, err)
}

// TestRecommendationStore_Insert_Integration verifies that a recommendation row
// can be inserted and retrieved via ListBySubject.
func TestRecommendationStore_Insert_Integration(t *testing.T) {
	ctx := context.Background()
	resetRecommendations(ctx, t)
	s := pgstore.NewRecommendationStore(sharedTestPool)

	subjectID := uuid.New()
	rec := makeRecommendation(subjectID, "vendor_match")

	require.NoError(t, s.Insert(ctx, rec))

	rows, err := s.ListBySubject(ctx, subjectID)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	got := rows[0]
	assert.Equal(t, rec.ID, got.ID)
	assert.Equal(t, rec.RecommendationType, got.RecommendationType)
	assert.Equal(t, rec.TargetID, got.TargetID)
	assert.Equal(t, rec.SubjectUserID, got.SubjectUserID)
	assert.InDelta(t, rec.OverallScore, got.OverallScore, 0.0001)
	assert.InDelta(t, rec.ScoreBreakdown.Skill, got.ScoreBreakdown.Skill, 0.0001)
	assert.InDelta(t, rec.ScoreBreakdown.Reliability, got.ScoreBreakdown.Reliability, 0.0001)
	assert.InDelta(t, rec.ScoreBreakdown.Collab, got.ScoreBreakdown.Collab, 0.0001)
	assert.InDelta(t, rec.ScoreBreakdown.Fit, got.ScoreBreakdown.Fit, 0.0001)
	assert.InDelta(t, rec.ScoreBreakdown.Comm, got.ScoreBreakdown.Comm, 0.0001)
	require.NotNil(t, got.Basis)
	assert.Equal(t, *rec.Basis, *got.Basis)
	assert.Nil(t, got.Accepted, "accepted must be nil (not-yet-acted-on)")
	assert.Equal(t, rec.ModelVersion, got.ModelVersion)
}

// TestRecommendationStore_Insert_ValidationErrors_Integration verifies that
// insert rejects invalid inputs before touching the DB.
func TestRecommendationStore_Insert_ValidationErrors_Integration(t *testing.T) {
	ctx := context.Background()
	s := pgstore.NewRecommendationStore(sharedTestPool)
	subjectID := uuid.New()

	tooLongType := string(make([]byte, 128))
	tooLongVersion := string(make([]byte, 101))

	tests := []struct {
		name    string
		mutate  func(r *domain.AIRecommendation)
		wantErr string
	}{
		{
			name:    "empty recommendation_type",
			mutate:  func(r *domain.AIRecommendation) { r.RecommendationType = "" },
			wantErr: "recommendation_type length must be 1",
		},
		{
			name:    "recommendation_type too long",
			mutate:  func(r *domain.AIRecommendation) { r.RecommendationType = tooLongType },
			wantErr: "recommendation_type length must be 1",
		},
		{
			name:    "empty model_version",
			mutate:  func(r *domain.AIRecommendation) { r.ModelVersion = "" },
			wantErr: "model_version length must be 1",
		},
		{
			name:    "model_version too long",
			mutate:  func(r *domain.AIRecommendation) { r.ModelVersion = tooLongVersion },
			wantErr: "model_version length must be 1",
		},
		{
			name:    "overall_score below 0",
			mutate:  func(r *domain.AIRecommendation) { r.OverallScore = -0.1 },
			wantErr: "overall_score must be in [0, 1]",
		},
		{
			name:    "overall_score above 1",
			mutate:  func(r *domain.AIRecommendation) { r.OverallScore = 1.1 },
			wantErr: "overall_score must be in [0, 1]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := makeRecommendation(subjectID, "vendor_match")
			tc.mutate(rec)
			err := s.Insert(ctx, rec)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestRecommendationStore_ListBySubject_MultipleRows_Integration verifies that
// ListBySubject returns rows ordered by created_at descending and only for the
// queried subject.
func TestRecommendationStore_ListBySubject_MultipleRows_Integration(t *testing.T) {
	ctx := context.Background()
	resetRecommendations(ctx, t)
	s := pgstore.NewRecommendationStore(sharedTestPool)

	subjectA := uuid.New()
	subjectB := uuid.New()

	// Insert two rows for subjectA at different times.
	recOlder := makeRecommendation(subjectA, "vendor_match")
	recOlder.CreatedAt = time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)
	require.NoError(t, s.Insert(ctx, recOlder))

	recNewer := makeRecommendation(subjectA, "vendor_match")
	recNewer.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, s.Insert(ctx, recNewer))

	// Insert one row for subjectB with a different type to verify isolation
	// and to vary recType across callers (satisfies unparam linter).
	recB := makeRecommendation(subjectB, "tender_match")
	require.NoError(t, s.Insert(ctx, recB))

	rows, err := s.ListBySubject(ctx, subjectA)
	require.NoError(t, err)
	require.Len(t, rows, 2, "must return exactly 2 rows for subjectA")

	// Verify descending order (newer first).
	assert.Equal(t, recNewer.ID, rows[0].ID, "newest row must come first")
	assert.Equal(t, recOlder.ID, rows[1].ID, "oldest row must come second")

	// subjectB must have no rows in subjectA's result.
	for _, r := range rows {
		assert.Equal(t, subjectA, r.SubjectUserID)
	}
}

// TestRecommendationStore_ListBySubject_Empty_Integration verifies that
// ListBySubject returns an empty (nil) slice when no rows exist for the subject.
func TestRecommendationStore_ListBySubject_Empty_Integration(t *testing.T) {
	ctx := context.Background()
	s := pgstore.NewRecommendationStore(sharedTestPool)

	rows, err := s.ListBySubject(ctx, uuid.New())
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestRecommendationStore_Retention_Integration verifies that DeleteOlderThan
// removes only rows older than the cutoff, leaving newer rows intact.
func TestRecommendationStore_Retention_Integration(t *testing.T) {
	ctx := context.Background()
	resetRecommendations(ctx, t)
	s := pgstore.NewRecommendationStore(sharedTestPool)

	subjectID := uuid.New()

	// Insert an "old" row (2 hours ago).
	old := makeRecommendation(subjectID, "vendor_match")
	old.CreatedAt = time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)
	require.NoError(t, s.Insert(ctx, old))

	// Insert a "new" row (now).
	newRec := makeRecommendation(subjectID, "vendor_match")
	newRec.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, s.Insert(ctx, newRec))

	// Cutoff = 1 hour ago: only the "old" row (2 hours) should be deleted.
	cutoff := time.Now().UTC().Add(-1 * time.Hour)

	n, err := s.DeleteOlderThan(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "exactly one old row must be deleted")

	// The new row must survive.
	rows, err := s.ListBySubject(ctx, subjectID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "new row must survive retention delete")
	assert.Equal(t, newRec.ID, rows[0].ID)
}

// TestRecommendationStore_Retention_DeleteAll_Integration verifies that
// DeleteOlderThan with a future cutoff deletes all rows.
func TestRecommendationStore_Retention_DeleteAll_Integration(t *testing.T) {
	ctx := context.Background()
	resetRecommendations(ctx, t)
	s := pgstore.NewRecommendationStore(sharedTestPool)

	subjectID := uuid.New()
	// Use a third type variation to further satisfy the unparam linter.
	rec := makeRecommendation(subjectID, "listing_match")
	require.NoError(t, s.Insert(ctx, rec))

	// Cutoff in the future -- all rows should be deleted.
	n, err := s.DeleteOlderThan(ctx, time.Now().UTC().Add(1*time.Minute))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1), "at least one row must be deleted")

	rows, err := s.ListBySubject(ctx, subjectID)
	require.NoError(t, err)
	assert.Empty(t, rows, "no rows must remain after full retention delete")
}

// TestRecommendationStore_AcceptedField_Integration verifies that the accepted
// nullable bool persists correctly for all three states (nil, true, false).
func TestRecommendationStore_AcceptedField_Integration(t *testing.T) {
	ctx := context.Background()
	resetRecommendations(ctx, t)
	s := pgstore.NewRecommendationStore(sharedTestPool)

	boolTrue := true
	boolFalse := false

	tests := []struct {
		name     string
		accepted *bool
	}{
		{name: "nil (pending)", accepted: nil},
		{name: "true (accepted)", accepted: &boolTrue},
		{name: "false (rejected)", accepted: &boolFalse},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subjectID := uuid.New()
			rec := makeRecommendation(subjectID, "vendor_match")
			rec.Accepted = tc.accepted
			require.NoError(t, s.Insert(ctx, rec))

			rows, err := s.ListBySubject(ctx, subjectID)
			require.NoError(t, err)
			require.Len(t, rows, 1)

			if tc.accepted == nil {
				assert.Nil(t, rows[0].Accepted)
			} else {
				require.NotNil(t, rows[0].Accepted)
				assert.Equal(t, *tc.accepted, *rows[0].Accepted)
			}
		})
	}
}

// TestRecommendationStore_MigrationDDL_Integration confirms the migration applied
// correctly: the ai_recommendation table exists with expected columns and index,
// and has ZERO foreign key constraints (platform red-line: CLAUDE.md #9).
func TestRecommendationStore_MigrationDDL_Integration(t *testing.T) {
	ctx := context.Background()

	var tableExists bool

	err := sharedTestPool.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_name = 'ai_recommendation'
		)`,
	).Scan(&tableExists)

	require.NoError(t, err)
	assert.True(t, tableExists, "ai_recommendation table must exist after migration 000013")

	// Verify the subject index exists.
	var indexExists bool

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE indexname = 'ai_recommendation_subject_created_idx'
		)`,
	).Scan(&indexExists)

	require.NoError(t, err)
	assert.True(t, indexExists, "ai_recommendation_subject_created_idx must exist after migration 000013")

	// Verify score_breakdown column is jsonb.
	var colType string

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT data_type FROM information_schema.columns
		 WHERE table_name = 'ai_recommendation' AND column_name = 'score_breakdown'`,
	).Scan(&colType)

	require.NoError(t, err)
	assert.Equal(t, "jsonb", colType, "score_breakdown must be jsonb")

	// Verify NO FK constraints exist on the table (platform red-line: CLAUDE.md #9).
	var fkCount int

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM information_schema.table_constraints
		 WHERE table_name = 'ai_recommendation' AND constraint_type = 'FOREIGN KEY'`,
	).Scan(&fkCount)

	require.NoError(t, err)
	assert.Equal(t, 0, fkCount, "ai_recommendation must have ZERO foreign key constraints (CLAUDE.md #9)")
}
