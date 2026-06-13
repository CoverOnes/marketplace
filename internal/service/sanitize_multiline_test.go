package service

import "testing"

// TestSanitizeMultilineText verifies that multi-line free-text fields (listing /
// tender description) accept newlines/tabs but still reject null bytes and other
// ASCII control characters. Regression for the "發布失敗 — description contains
// illegal control characters (null, CR, LF)" bug where a multi-line textarea
// description was rejected by the single-line sanitizeText.
func TestSanitizeMultilineText(t *testing.T) {
	allowed := []string{
		"line1\nline2",      // LF — the reported bug
		"a\tb",              // tab
		"crlf\r\nline",      // CRLF (textarea)
		"uiux 禮物盒\n設計",      // CJK + newline
		"",                  // empty
		"plain single line", // no control chars
	}
	for _, s := range allowed {
		if err := sanitizeMultilineText(s); err != nil {
			t.Errorf("sanitizeMultilineText(%q) should be allowed, got error: %v", s, err)
		}
	}

	rejected := []string{
		"a\x00b",     // null byte
		"esc\x1bseq", // ESC — terminal escape injection surface
		"bell\x07",   // BEL
	}
	for _, s := range rejected {
		if err := sanitizeMultilineText(s); err == nil {
			t.Errorf("sanitizeMultilineText(%q) should be rejected (control char)", s)
		}
	}

	// Single-line sanitizeText (used for title) must STILL reject newlines.
	if err := sanitizeText("title\nwith newline"); err == nil {
		t.Error("sanitizeText must still reject newlines for single-line fields (title)")
	}
}
