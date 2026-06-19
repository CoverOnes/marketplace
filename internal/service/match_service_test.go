package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestComputeOverall verifies the locked weight formula for the match score.
// Weights: skill .35 / fit .15 / reliability .25 / collab .20 / comm .05.
func TestComputeOverall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		breakdown ScoreBreakdown
		want      float64
	}{
		{
			// Dispatch-specified case: Skill=1.0, Fit=0.5, others 0.
			// expected = 1.0*0.35 + 0.5*0.15 + 0*0.25 + 0*0.20 + 0*0.05 = 0.35 + 0.075 = 0.425
			name:      "skill=1 fit=0.5 others=0",
			breakdown: ScoreBreakdown{Skill: 1.0, Fit: 0.5, Reliability: 0, Collab: 0, Comm: 0},
			want:      0.425,
		},
		{
			// All zeros: degenerate case for a completely unrelated vendor.
			name:      "all zeros",
			breakdown: ScoreBreakdown{},
			want:      0.0,
		},
		{
			// Perfect score: all dimensions = 1.0 → weights must sum to 1.0.
			name:      "all ones",
			breakdown: ScoreBreakdown{Skill: 1.0, Fit: 1.0, Reliability: 1.0, Collab: 1.0, Comm: 1.0},
			want:      1.0,
		},
		{
			// Only reliability: 0.25 weight.
			name:      "reliability only",
			breakdown: ScoreBreakdown{Reliability: 1.0},
			want:      0.25,
		},
		{
			// Only collab: 0.20 weight.
			name:      "collab only",
			breakdown: ScoreBreakdown{Collab: 1.0},
			want:      0.20,
		},
		{
			// Only comm: 0.05 weight.
			name:      "comm only",
			breakdown: ScoreBreakdown{Comm: 1.0},
			want:      0.05,
		},
		{
			// Only fit: 0.15 weight.
			name:      "fit only",
			breakdown: ScoreBreakdown{Fit: 1.0},
			want:      0.15,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ComputeOverall(tc.breakdown)
			require.InDelta(t, tc.want, got, 1e-9, "ComputeOverall mismatch for breakdown %+v", tc.breakdown)
		})
	}
}

// TestClampSimilarity verifies the cosine-distance → similarity mapping.
func TestClampSimilarity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		dist     float32
		expected float64
	}{
		{
			// Identical vectors: cosine distance = 0 → similarity = 1.
			name:     "identical",
			dist:     0.0,
			expected: 1.0,
		},
		{
			// Orthogonal vectors: cosine distance = 1 → similarity = 0.5.
			name:     "orthogonal",
			dist:     1.0,
			expected: 0.5,
		},
		{
			// Opposite vectors: cosine distance = 2 → similarity = 0.
			name:     "opposite",
			dist:     2.0,
			expected: 0.0,
		},
		{
			// Distance slightly above 2 (floating point artifact): clamp to 0.
			name:     "above 2 clamped to 0",
			dist:     2.1,
			expected: 0.0,
		},
		{
			// Negative distance (should not happen; clamp to 1).
			name:     "negative clamped to 1",
			dist:     -0.1,
			expected: 1.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := clampSimilarity(tc.dist)
			require.InDelta(t, tc.expected, got, 1e-9)
		})
	}
}
