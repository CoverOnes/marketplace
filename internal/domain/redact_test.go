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
		// FIX B: sk- prefixed provider keys (OpenRouter, Anthropic, generic).
		{
			name:  "OpenRouter sk-or-v1- key is redacted",
			input: `authorization failed: sk-or-v1-abcdefghijklmnopqrstu123456`,
			want:  `authorization failed: [REDACTED]`,
		},
		{
			name:  "short sk- value (under 20 chars) is NOT redacted",
			input: `config key: sk-short123`,
			// "sk-" + "short123" = 8 chars, well under the {20,} minimum — must pass through
			want: `config key: sk-short123`,
		},
		{
			name:  "embedded sk- inside a benign word is NOT redacted (word-boundary guard)",
			input: `processing task-completed-successfully-now-foobar done`,
			// "sk-" appears mid-word inside "ta·sk-completed-…"; the \b anchor must prevent
			// matching it, else legitimate error/log text gets mangled to "ta[REDACTED]".
			want: `processing task-completed-successfully-now-foobar done`,
		},
		// FIX B: HTTP(S) URLs with Basic Auth credentials embedded.
		{
			name:  "HTTPS URL with basic auth credentials is redacted",
			input: `request to https://apiuser:s3cr3tP@ss@api.example.com/v1/embed failed`,
			want:  `request to [REDACTED] failed`,
		},
		{
			name:  "benign HTTPS URL without credentials is NOT redacted",
			input: `request to https://api.example.com/v1/embed failed`,
			// No user:pass@ component — must not be redacted
			want: `request to https://api.example.com/v1/embed failed`,
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

// TestRedactErrString_Truncation verifies truncation behavior at the 500-byte boundary.
func TestRedactErrString_Truncation(t *testing.T) {
	t.Parallel()

	t.Run("600-byte string is truncated with marker", func(t *testing.T) {
		t.Parallel()

		long := strings.Repeat("a", 600)
		got := domain.RedactErrString(long)

		assert.True(t, strings.HasSuffix(got, "...[truncated]"), "must end with truncation marker")
		// 500 bytes of content + 14 bytes of "...[truncated]" = 514 max
		assert.LessOrEqual(t, len(got), 514, "truncated string must not exceed maxRedactedErrLen + marker length")
	})

	t.Run("exactly-500-byte string is NOT truncated", func(t *testing.T) {
		t.Parallel()

		exact := strings.Repeat("b", 500)
		got := domain.RedactErrString(exact)

		assert.Equal(t, exact, got, "exactly-500-byte benign string must pass through unchanged")
		assert.False(t, strings.HasSuffix(got, "...[truncated]"), "must NOT have truncation marker at exactly 500 bytes")
	})
}
