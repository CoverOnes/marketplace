package postgres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

//nolint:gosec // G101 false positive: test fixture DSN for redactErrString unit test, not a real credential
var postgresTestDSN = `dial: postgres://user:mysecretpw@db.example.com:5432/mydb?sslmode=require`

// TestRedactErrString verifies that redactErrString scrubs credential patterns from
// error strings (CWE-532 / backend-security §3.1) and truncates long strings.
func TestRedactErrString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "redis DSN with embedded password",
			input: `dial tcp: connect to redis://:supersecret@localhost:6379/0: connection refused`,
			// \S+ in the pattern consumes "localhost:6379/0:" so the trailing colon is part of the match
			want: `dial tcp: connect to [REDACTED] connection refused`,
		},
		{
			name:  "Bearer JWT token",
			input: `authentication failed: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig`,
			want:  `authentication failed: [REDACTED]`,
		},
		{
			name:  "benign error string unchanged",
			input: `timeout waiting for embedding API response after 45s`,
			want:  `timeout waiting for embedding API response after 45s`,
		},
		{
			name:  "postgres DSN with password",
			input: postgresTestDSN, // see var below — G101 false positive, test fixture only
			want:  `dial: [REDACTED]`,
		},
		{
			name:  "long string is truncated",
			input: strings.Repeat("x", maxRedactedLen+100),
			want:  strings.Repeat("x", maxRedactedLen) + "...[truncated]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := redactErrString(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}
