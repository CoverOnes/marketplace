package domain

import (
	"regexp"
	"strings"
)

// maxRedactedErrLen is the maximum byte length for a redacted error string
// stored or logged at the dead-letter site (backend-security §3.1 / CWE-532).
const maxRedactedErrLen = 500

// credentialPatterns is the single canonical list of credential regex patterns
// (backend-security §3.1). It is the ONLY copy — both the domain-level
// RedactCredentials/RedactErrString and the postgres-level redactBasis delegate
// here so that adding a new pattern automatically covers all redaction paths.
// Package-level: compiled once at init, never mutated.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk_live_[A-Za-z0-9_]+`),
	regexp.MustCompile(`ghp_[A-Za-z0-9_]+`),
	regexp.MustCompile(`xoxb-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`Bearer ey[A-Za-z0-9._-]+`),
	// postgres:// and postgresql:// (RFC-correct scheme) DSNs (backend-security §3.1, Mi-1).
	regexp.MustCompile(`postgres(?:ql)?://[^:]+:[^@]+@\S+`),
	// mongodb:// and mongodb+srv:// (Atlas DNS-seedlist, the common hosted form) DSNs.
	regexp.MustCompile(`mongodb(?:\+srv)?://[^:]+:[^@]+@\S+`),
	// redis:// DSNs with embedded passwords — username may be empty (redis://:pw@host form).
	regexp.MustCompile(`redis://[^:]*:[^@]+@\S+`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// password / api_key key=value pairs. The quoted branches use [^']* / [^"]* (NOT
	// [^\s'"]*) so a value containing the opposite quote style — e.g. api_key: 'secret"x' —
	// is still fully consumed including its closing quote, and the closing quote never
	// leaks out (backend-security §3.1, M-2 mixed-quote evasion + Mi-3 trailing quote).
	regexp.MustCompile(`(?i)password[=:]\s*(?:'[^']*'|"[^"]*"|[^\s'"]+)`),
	regexp.MustCompile(`(?i)api[_-]?key[=:]\s*(?:'[^']*'|"[^"]*"|[^\s'"]+)`),
	// sk- prefixed keys: OpenRouter (sk-or-v1-*), Anthropic (sk-ant-*), generic provider
	// keys. The \b word-boundary anchor prevents matching "sk-" embedded mid-word — without
	// it, benign kebab text like "task-completed-…", "disk-usage-…", "risk-report-…" gets
	// mangled (the "sk-" inside ta·sk / di·sk / ri·sk would match). Require ≥20 chars after
	// "sk-" to avoid false positives on short identifiers. Both \b and {20,} are intentional.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	// HTTP(S) URLs with embedded Basic Auth credentials (user:pass@host).
	// [^:\s]+ captures the username (non-empty, no colon or whitespace).
	// [^@\s]+ captures the password (non-empty, no @ or whitespace).
	regexp.MustCompile(`https?://[^:\s]+:[^@\s]+@\S+`),
}

// RedactCredentials applies the §3.1 credential patterns to s, replacing each
// match with [REDACTED]. No length cap — use for basis fields where truncation
// is handled by the caller (e.g. recommendation_store maxBasisRunes check).
func RedactCredentials(s string) string {
	for _, re := range credentialPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}

	return s
}

// RedactErrString scrubs credential-like substrings from an error message and
// caps the result at maxRedactedErrLen bytes with a "...[truncated]" marker.
// Use before persisting last_error or logging at slog.Error dead-letter sites
// (CWE-532 / backend-security §3.1).
func RedactErrString(s string) string {
	s = RedactCredentials(s)

	if len(s) > maxRedactedErrLen {
		// Truncate at a valid UTF-8 boundary so the stored/logged string is safe.
		truncated := strings.ToValidUTF8(s[:maxRedactedErrLen], "")
		return truncated + "...[truncated]"
	}

	return s
}
