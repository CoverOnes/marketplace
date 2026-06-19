package domain

import (
	"regexp"
	"strings"
)

// maxRedactedErrLen is the maximum byte length for a redacted error string
// stored or logged at the dead-letter site (backend-security §3.1 / CWE-532).
const maxRedactedErrLen = 500

// errCredentialPatterns are the credential regex patterns applied by RedactErrString.
// These mirror the patterns in internal/store/postgres/credential_redact.go; both sets
// must be kept in sync when new credential patterns are added.
// Package-level: compiled once at init, never mutated.
var errCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk_live_[A-Za-z0-9_]+`),
	regexp.MustCompile(`ghp_[A-Za-z0-9_]+`),
	regexp.MustCompile(`xoxb-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`Bearer ey[A-Za-z0-9._-]+`),
	regexp.MustCompile(`postgres(?:ql)?://[^:]+:[^@]+@\S+`),
	regexp.MustCompile(`mongodb(?:\+srv)?://[^:]+:[^@]+@\S+`),
	// redis:// DSNs with embedded passwords — username may be empty (redis://:pw@host form).
	regexp.MustCompile(`redis://[^:]*:[^@]+@\S+`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)password[=:]\s*(?:'[^']*'|"[^"]*"|[^\s'"]+)`),
	regexp.MustCompile(`(?i)api[_-]?key[=:]\s*(?:'[^']*'|"[^"]*"|[^\s'"]+)`),
}

// RedactErrString scrubs credential-like substrings from an error message and
// caps the result at maxRedactedErrLen bytes with a "...[truncated]" marker.
// Use before logging at slog.Error dead-letter sites (CWE-532 / backend-security §3.1).
func RedactErrString(s string) string {
	for _, re := range errCredentialPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}

	if len(s) > maxRedactedErrLen {
		truncated := strings.ToValidUTF8(s[:maxRedactedErrLen], "")
		return truncated + "...[truncated]"
	}

	return s
}
