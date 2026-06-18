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

	rows, err := s.ListBySubject(ctx, subjectID, 0)
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

	rows, err := s.ListBySubject(ctx, subjectA, 0)
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

	rows, err := s.ListBySubject(ctx, uuid.New(), 0)
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
	rows, err := s.ListBySubject(ctx, subjectID, 0)
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

	rows, err := s.ListBySubject(ctx, subjectID, 0)
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

			rows, err := s.ListBySubject(ctx, subjectID, 0)
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

	// Verify created_at index exists (db-inspector item 7).
	var createdAtIdxExists bool

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE indexname = 'ai_recommendation_created_at_idx'
		)`,
	).Scan(&createdAtIdxExists)

	require.NoError(t, err)
	assert.True(t, createdAtIdxExists, "ai_recommendation_created_at_idx must exist after migration 000013")
}

// TestRecommendationStore_ScoreBreakdownValidation_Integration verifies that
// Insert rejects sub-dimension scores outside [0, 1].
func TestRecommendationStore_ScoreBreakdownValidation_Integration(t *testing.T) {
	ctx := context.Background()
	s := pgstore.NewRecommendationStore(sharedTestPool)
	subjectID := uuid.New()

	tests := []struct {
		name    string
		mutate  func(r *domain.AIRecommendation)
		wantErr string
	}{
		{
			name: "skill below 0",
			mutate: func(r *domain.AIRecommendation) {
				r.ScoreBreakdown.Skill = -0.1
			},
			wantErr: "score_breakdown.skill must be in [0, 1]",
		},
		{
			name: "reliability above 1",
			mutate: func(r *domain.AIRecommendation) {
				r.ScoreBreakdown.Reliability = 1.01
			},
			wantErr: "score_breakdown.reliability must be in [0, 1]",
		},
		{
			name: "collab below 0",
			mutate: func(r *domain.AIRecommendation) {
				r.ScoreBreakdown.Collab = -1.0
			},
			wantErr: "score_breakdown.collab must be in [0, 1]",
		},
		{
			name: "fit above 1",
			mutate: func(r *domain.AIRecommendation) {
				r.ScoreBreakdown.Fit = 2.0
			},
			wantErr: "score_breakdown.fit must be in [0, 1]",
		},
		{
			name: "comm below 0",
			mutate: func(r *domain.AIRecommendation) {
				r.ScoreBreakdown.Comm = -0.5
			},
			wantErr: "score_breakdown.comm must be in [0, 1]",
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

// TestRecommendationStore_BasisSanitization_Integration verifies that Insert
// rejects basis strings with control characters or over the rune cap, and that
// credential patterns are redacted before persistence.
func TestRecommendationStore_BasisSanitization_Integration(t *testing.T) {
	ctx := context.Background()
	resetRecommendations(ctx, t)
	s := pgstore.NewRecommendationStore(sharedTestPool)
	subjectID := uuid.New()

	// Basis with null byte must be rejected.
	t.Run("null byte rejected", func(t *testing.T) {
		rec := makeRecommendation(subjectID, "vendor_match")
		nullBasis := "good match\x00injected"
		rec.Basis = &nullBasis
		require.ErrorContains(t, s.Insert(ctx, rec), "invalid control characters")
	})

	// Basis with newline must be rejected.
	t.Run("newline rejected", func(t *testing.T) {
		rec := makeRecommendation(subjectID, "vendor_match")
		newlineBasis := "line1\nline2"
		rec.Basis = &newlineBasis
		require.ErrorContains(t, s.Insert(ctx, rec), "invalid control characters")
	})

	// Basis exceeding maxBasisRunes must be rejected.
	t.Run("too long basis rejected", func(t *testing.T) {
		rec := makeRecommendation(subjectID, "vendor_match")
		// Fill with 'a' runes so the control-char check passes and we reach the length check.
		runeSlice := make([]rune, 5001)
		for i := range runeSlice {
			runeSlice[i] = 'a'
		}
		longBasis := string(runeSlice)
		rec.Basis = &longBasis
		require.ErrorContains(t, s.Insert(ctx, rec), "exceeds maximum length")
	})

	// Basis with a credential pattern must be redacted before storing.
	t.Run("credential redacted before storing", func(t *testing.T) {
		rec := makeRecommendation(subjectID, "vendor_match")
		credBasis := "Good match. Key: ghp_ABCDEF1234567890 was present." //nolint:gosec // G101 false positive: test fixture string, not a real credential
		rec.Basis = &credBasis
		require.NoError(t, s.Insert(ctx, rec))

		rows, err := s.ListBySubject(ctx, subjectID, 1)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.NotNil(t, rows[0].Basis)
		assert.Contains(t, *rows[0].Basis, "[REDACTED]")
		assert.NotContains(t, *rows[0].Basis, "ghp_ABCDEF1234567890")
	})
}

// TestRecommendationStore_ListBySubject_Limit_Integration verifies that
// ListBySubject honors the explicit limit parameter.
func TestRecommendationStore_ListBySubject_Limit_Integration(t *testing.T) {
	ctx := context.Background()
	resetRecommendations(ctx, t)
	s := pgstore.NewRecommendationStore(sharedTestPool)

	subjectID := uuid.New()

	// Insert 3 rows with distinct timestamps.
	for i := range 3 {
		rec := makeRecommendation(subjectID, "vendor_match")
		rec.CreatedAt = time.Now().UTC().Add(time.Duration(-i) * time.Hour).Truncate(time.Millisecond)
		rec.ID = uuid.New()
		require.NoError(t, s.Insert(ctx, rec))
	}

	// Request only 2 rows; must not return all 3.
	rows, err := s.ListBySubject(ctx, subjectID, 2)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "limit=2 must return exactly 2 rows")

	// limit=0 uses default (200); all 3 rows returned.
	all, err := s.ListBySubject(ctx, subjectID, 0)
	require.NoError(t, err)
	assert.Len(t, all, 3, "limit=0 must use default and return all 3 rows")
}
