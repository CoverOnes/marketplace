package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRedactBasis_CredentialPatterns verifies that redactBasis replaces each
// §3.1 credential pattern with [REDACTED] and leaves clean text unchanged.
// Tests at least 6 distinct evasion / credential strings as required by §3.1.
func TestRedactBasis_CredentialPatterns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "stripe live secret key",
			input: "The vendor scored well; token=sk_live_abcDEF123456XYZ appended",
			want:  "The vendor scored well; token=[REDACTED] appended",
		},
		{
			name:  "GitHub personal access token",
			input: "Auth: ghp_AaBbCcDdEeFfGg1234567890 is the token",
			want:  "Auth: [REDACTED] is the token",
		},
		{
			name:  "Slack bot token",
			input: "Published via xoxb-1234567890-abcdef-XYZ-token",
			want:  "Published via [REDACTED]",
		},
		{
			name:  "Bearer JWT",
			input: "Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.payload.sig",
			want:  "Authorization: [REDACTED]",
		},
		{ //nolint:gosec // G101 false positive: test fixture string used to verify the redactBasis function, not a real credential
			name:  "Postgres DSN with credentials",
			input: "postgres://admin:s3cr3t@db.example.com/marketplace was used",
			want:  "[REDACTED] was used",
		},
		{
			name:  "AWS access key",
			input: "Access key AKIAIOSFODNN7EXAMPLE was rotated",
			want:  "Access key [REDACTED] was rotated",
		},
		{
			name:  "password in key=value form",
			input: "config: password=hunter2 is set",
			want:  "config: [REDACTED] is set",
		},
		{
			name:  "api_key in key=value form",
			input: "api_key: 'abc123secret' attached",
			// The regex `(?i)api[_-]?key[=:]\s*['"]?[^\s'"]+` matches up to the
			// closing quote but not including it. The trailing "' attached" remains.
			want: "[REDACTED]' attached",
		},
		{
			name:  "api-key variant",
			input: "api-key=deadbeef is passed",
			want:  "[REDACTED] is passed",
		},
		{
			name:  "no credential pattern — clean text unchanged",
			input: "Good skill match and communication score.",
			want:  "Good skill match and communication score.",
		},
		{
			name:  "multiple patterns in single string",
			input: "key=ghp_ABCDEF and password=secret123 both present",
			want:  "key=[REDACTED] and [REDACTED] both present",
		},
		{
			name:  "Postgres DSN with special chars in password",
			input: "dsn=postgres://user:p@ssw0rd!@host/db",
			want:  "dsn=[REDACTED]",
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
