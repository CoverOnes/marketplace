package domain_test

import (
	"strings"
	"testing"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/stretchr/testify/assert"
)

// TestRedactErrString verifies that domain.RedactErrString scrubs credential
// patterns from error messages and truncates overly long strings.
// (CWE-532 / backend-security §3.1 — used by indexer + poller dead-letter log sites)
func TestRedactErrString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "redis DSN with embedded password",
			input: `connect to redis://:supersecret@localhost:6379/0 failed`,
			// \S+ in the pattern consumes "localhost:6379/0" up to the space
			want: `connect to [REDACTED] failed`,
		},
		{
			name:  "Bearer JWT token in error string",
			input: `auth error: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abc.def rejected`,
			want:  `auth error: [REDACTED] rejected`,
		},
		{
			name:  "benign error string passes through unchanged",
			input: `embedding API returned HTTP 503 after 3 retries`,
			want:  `embedding API returned HTTP 503 after 3 retries`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := domain.RedactErrString(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRedactErrString_Truncation verifies that strings exceeding 500 bytes are
// truncated with the "...[truncated]" marker.
func TestRedactErrString_Truncation(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 600)
	got := domain.RedactErrString(long)

	assert.True(t, strings.HasSuffix(got, "...[truncated]"), "must end with truncation marker")
	assert.LessOrEqual(t, len(got), 514, "truncated string must not exceed maxRedactedErrLen + marker length")
}
