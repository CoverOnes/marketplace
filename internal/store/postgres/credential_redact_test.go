package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRedactBasis_DelegatesCredentials verifies that redactBasis (which now
// delegates to domain.RedactCredentials) correctly scrubs credential patterns.
// This confirms the delegation wiring is intact after the single-source refactor.
func TestRedactBasis_DelegatesCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "Bearer JWT token is redacted",
			input: `basis: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig`,
			want:  `basis: [REDACTED]`,
		},
		{
			name:  "benign text passes through unchanged",
			input: `vendor has strong Go skills and completed 12 projects`,
			want:  `vendor has strong Go skills and completed 12 projects`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := redactBasis(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}
