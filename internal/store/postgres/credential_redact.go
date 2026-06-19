package postgres

import (
	"regexp"
	"strings"
)

// credentialPatterns are the regex patterns used to scrub credential-like strings
// before persisting or logging any user/LLM-supplied text (backend-security §3.1).
// Patterns mirror the spec in backend-security-design.md.
// Package-level: compiled once at startup, never mutated.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk_live_[A-Za-z0-9_]+`),
	regexp.MustCompile(`ghp_[A-Za-z0-9_]+`),
	regexp.MustCompile(`xoxb-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`Bearer ey[A-Za-z0-9._-]+`),
	// postgres:// and postgresql:// (RFC-correct scheme) DSNs (backend-security §3.1, Mi-1).
	regexp.MustCompile(`postgres(?:ql)?://[^:]+:[^@]+@\S+`),
	// mongodb:// and mongodb+srv:// (Atlas DNS-seedlist, the common hosted form) DSNs
	// (backend-security §3.1, M-1).
	regexp.MustCompile(`mongodb(?:\+srv)?://[^:]+:[^@]+@\S+`),
	// redis:// DSNs with embedded passwords — username may be empty (redis://:pw@host form).
	// (backend-security §3.1).
	regexp.MustCompile(`redis://[^:]*:[^@]+@\S+`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// password / api_key key=value pairs. The quoted branches use [^']* / [^"]* (NOT
	// [^\s'"]*) so a value containing the opposite quote style — e.g. api_key: 'secret"x' —
	// is still fully consumed including its closing quote, and the closing quote never
	// leaks out (backend-security §3.1, M-2 mixed-quote evasion + Mi-3 trailing quote).
	regexp.MustCompile(`(?i)password[=:]\s*(?:'[^']*'|"[^"]*"|[^\s'"]+)`),
	regexp.MustCompile(`(?i)api[_-]?key[=:]\s*(?:'[^']*'|"[^"]*"|[^\s'"]+)`),
}

// maxRedactedLen is the maximum byte length stored/logged after redaction.
// Strings longer than this are truncated with a "...[truncated]" marker.
const maxRedactedLen = 500

// redactBasis applies the §3.1 credential patterns to text, replacing each
// match with [REDACTED]. It is called on *r.Basis before any DB Exec.
func redactBasis(text string) string {
	for _, re := range credentialPatterns {
		text = re.ReplaceAllString(text, "[REDACTED]")
	}

	return text
}

// redactErrString applies the §3.1 credential patterns to an error string and
// caps the output at maxRedactedLen bytes. Used before persisting last_error in
// the outbox and before logging it at the dead-letter site (CWE-532 / §3.1).
func redactErrString(s string) string {
	for _, re := range credentialPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}

	if len(s) > maxRedactedLen {
		// Truncate at a valid UTF-8 boundary.
		truncated := strings.ToValidUTF8(s[:maxRedactedLen], "")
		return truncated + "...[truncated]"
	}

	return s
}
