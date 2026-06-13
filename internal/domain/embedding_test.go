package domain_test

import (
	"testing"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/stretchr/testify/assert"
)

func TestEmbeddingEntityType_IsValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input domain.EmbeddingEntityType
		want  bool
	}{
		{name: "tender is valid", input: domain.EmbeddingEntityTypeTender, want: true},
		{name: "vendor is valid", input: domain.EmbeddingEntityTypeVendor, want: true},
		{name: "empty string is invalid", input: "", want: false},
		{name: "arbitrary string is invalid", input: "contract", want: false},
		{name: "uppercase TENDER is invalid", input: "TENDER", want: false},
		{name: "mixed case Tender is invalid", input: "Tender", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.input.IsValid())
		})
	}
}
