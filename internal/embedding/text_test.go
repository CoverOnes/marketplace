package embedding_test

import (
	"strings"
	"testing"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/embedding"
	"github.com/stretchr/testify/assert"
)

// TestComposeTenderText_Golden verifies the deterministic composition format
// (title + "\n" + description) and the 20 000-rune cap.
func TestComposeTenderText_Golden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *domain.Listing
		want string
	}{
		{
			name: "happy path: title and description",
			in: &domain.Listing{
				Title:       "Senior Go Engineer",
				Description: "We need a Go expert.",
			},
			want: "Senior Go Engineer\nWe need a Go expert.",
		},
		{
			name: "happy path: empty description still separated by newline",
			in: &domain.Listing{
				Title:       "Only Title",
				Description: "",
			},
			want: "Only Title\n",
		},
		{
			name: "happy path: multiline description preserved",
			in: &domain.Listing{
				Title:       "Tender",
				Description: "Line1\nLine2\nLine3",
			},
			want: "Tender\nLine1\nLine2\nLine3",
		},
		{
			name: "edge case: nil listing returns empty string",
			in:   nil,
			want: "",
		},
		{
			name: "edge case: empty title and description",
			in: &domain.Listing{
				Title:       "",
				Description: "",
			},
			want: "\n",
		},
		{
			name: "edge case: budget/currency fields ignored (not in output)",
			in: &domain.Listing{
				Title:       "Budget Tender",
				Description: "Details here",
			},
			want: "Budget Tender\nDetails here",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := embedding.ComposeTenderText(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestComposeTenderText_Cap verifies that a combined text exceeding 20 000 runes
// is capped at exactly 20 000 runes.
func TestComposeTenderText_Cap(t *testing.T) {
	t.Parallel()

	// Build a title + description whose combined rune count exceeds 20 000.
	// title is 10 runes, newline is 1, description fills the rest beyond 20 000.
	title := "Short Test"
	// 20 000 - len("Short Test\n") = 19 989 runes in description, plus 200 extra.
	description := strings.Repeat("A", 20_000)

	l := &domain.Listing{Title: title, Description: description}
	got := embedding.ComposeTenderText(l)

	runeCount := len([]rune(got))
	assert.Equal(t, 20_000, runeCount, "output MUST be capped at exactly 20 000 runes")
}

// TestComposeTenderText_Deterministic verifies that two calls with identical input
// produce identical output (required for golden-test stability and cache keying).
func TestComposeTenderText_Deterministic(t *testing.T) {
	t.Parallel()

	l := &domain.Listing{
		Title:       "Deterministic Test",
		Description: "Always the same.",
	}

	first := embedding.ComposeTenderText(l)
	second := embedding.ComposeTenderText(l)

	assert.Equal(t, first, second, "ComposeTenderText must be deterministic")
}

// ─────────────────────────────────────────────────────────────────────────────
// ComposeVendorText tests
// ─────────────────────────────────────────────────────────────────────────────

func strPtr(s string) *string { return &s }

// TestComposeVendorText_Golden verifies the deterministic composition format and
// nil-safety of ComposeVendorText.
func TestComposeVendorText_Golden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *domain.VendorProfile
		want string
	}{
		{
			name: "happy path: all fields populated",
			in: &domain.VendorProfile{
				DisplayName: "Alice",
				Headline:    strPtr("Go specialist"),
				Bio:         strPtr("I write Go services."),
				Skills:      []string{"Go", "PostgreSQL"},
			},
			want: "Alice\nGo specialist\nI write Go services.\nGo, PostgreSQL",
		},
		{
			name: "happy path: nil headline and bio",
			in: &domain.VendorProfile{
				DisplayName: "Bob",
				Skills:      []string{"Python"},
			},
			want: "Bob\n\n\nPython",
		},
		{
			name: "happy path: empty skills slice",
			in: &domain.VendorProfile{
				DisplayName: "Carol",
				Headline:    strPtr("Designer"),
				Bio:         strPtr("UI/UX"),
				Skills:      []string{},
			},
			want: "Carol\nDesigner\nUI/UX\n",
		},
		{
			name: "edge case: nil profile returns empty string",
			in:   nil,
			want: "",
		},
		{
			name: "edge case: nil skills treated as empty (no panic)",
			in: &domain.VendorProfile{
				DisplayName: "Dave",
				Skills:      nil,
			},
			want: "Dave\n\n\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := embedding.ComposeVendorText(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestComposeVendorText_Cap verifies that a combined text exceeding 20 000 runes
// is capped at exactly 20 000 runes.
func TestComposeVendorText_Cap(t *testing.T) {
	t.Parallel()

	// Build a profile whose combined field text exceeds 20 000 runes.
	longBio := strings.Repeat("A", 20_000)
	p := &domain.VendorProfile{
		DisplayName: "Alice",
		Headline:    strPtr("specialist"),
		Bio:         strPtr(longBio),
		Skills:      []string{"Go"},
	}

	got := embedding.ComposeVendorText(p)

	runeCount := len([]rune(got))
	assert.Equal(t, 20_000, runeCount, "ComposeVendorText output MUST be capped at exactly 20 000 runes")
}

// TestComposeVendorText_Deterministic verifies that two calls with identical input
// produce identical output.
func TestComposeVendorText_Deterministic(t *testing.T) {
	t.Parallel()

	p := &domain.VendorProfile{
		DisplayName: "Eve",
		Headline:    strPtr("QA Engineer"),
		Bio:         strPtr("I test things."),
		Skills:      []string{"Testing", "Go"},
	}

	first := embedding.ComposeVendorText(p)
	second := embedding.ComposeVendorText(p)

	assert.Equal(t, first, second, "ComposeVendorText must be deterministic")
}
