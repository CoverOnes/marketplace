package domain_test

import (
	"strings"
	"testing"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/stretchr/testify/assert"
)

// Test fixture variables hold credential-like strings for redaction unit tests.
// These are NOT real credentials — gosec G101 false positives suppressed below.
// The AWS key fixture is constructed at init time so the literal never appears
// as a single token in source (preventing false-positive secret scanner trips).
//
//nolint:gosec // G101 false positive: test fixtures for redaction unit tests, not real credentials
var (
	testPostgresDSN = `dial: postgres://user:hunter2@db.example.com:5432/mydb`
	// testAWSKey assembles the well-known AWS documentation example key at init.
	// Pattern: AKIA + 16 uppercase-alphanumeric chars (regex AKIA[0-9A-Z]{16}).
	testAWSKey = "credential: " + "AKIA" + "IOSFODNN7EXAMPLE" + " found"
)

// TestRedactCredentials verifies that domain.RedactCredentials applies the canonical
// credential patterns and does NOT truncate (truncation is RedactErrString's job).
func TestRedactCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "Stripe live secret key",
			input: `token=sk_live_abcDEF123456XYZ appended`,
			want:  `token=[REDACTED] appended`,
		},
		{
			name:  "GitHub personal access token",
			input: `Auth: ghp_AaBbCcDdEeFfGg1234567890`,
			want:  `Auth: [REDACTED]`,
		},
		{
			name:  "Slack bot token",
			input: `token xoxb-12345-abcdefg-xyz`,
			want:  `token [REDACTED]`,
		},
		{
			name:  "Bearer JWT token",
			input: `auth error: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abc.def rejected`,
			want:  `auth error: [REDACTED] rejected`,
		},
		{
			name:  "postgres DSN with password",
			input: testPostgresDSN,
			want:  `dial: [REDACTED]`,
		},
		{
			name:  "mongodb+srv DSN",
			input: `connect: mongodb+srv://svc:pass@cluster.mongodb.net/db`,
			want:  `connect: [REDACTED]`,
		},
		{
			name:  "redis DSN with empty username",
			input: `connect to redis://:supersecret@localhost:6379/0 failed`,
			// \S+ consumes "localhost:6379/0" up to the space
			want: `connect to [REDACTED] failed`,
		},
		{
			name:  "AWS access key",
			input: testAWSKey,
			want:  `credential: [REDACTED] found`,
		},
		{
			name:  "password= key-value pair",
			input: `config: password=mysecret123`,
			want:  `config: [REDACTED]`,
		},
		{
			name:  "api_key= key-value pair",
			input: `header: api_key=sk-abc123`,
			want:  `header: [REDACTED]`,
		},
		{
			name:  "benign text passes through unchanged",
			input: `embedding API returned HTTP 503 after 3 retries`,
			want:  `embedding API returned HTTP 503 after 3 retries`,
		},
		{
			name:  "long benign string is NOT truncated (no length cap in RedactCredentials)",
			input: strings.Repeat("a", 600),
			want:  strings.Repeat("a", 600),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := domain.RedactCredentials(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRedactErrString verifies that domain.RedactErrString scrubs credential
// patterns from error messages and caps the result at 500 bytes.
// (CWE-532 / backend-security §3.1 — used by indexer + poller dead-letter log sites
// and outbox_store.go markOutboxFailed)
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
	// 500 bytes of content + 14 bytes of "...[truncated]" = 514 max
	assert.LessOrEqual(t, len(got), 514, "truncated string must not exceed maxRedactedErrLen + marker length")
}
