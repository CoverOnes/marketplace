package service_test

import (
	"testing"

	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRRFCombine_HybridTopRanked asserts the RRF property: a document ranked
// top in BOTH lexical and semantic lists outscores a document ranked top in only
// one list (acceptance criterion 3).
func TestRRFCombine_HybridTopRanked(t *testing.T) {
	t.Parallel()

	both := uuid.New()    // appears rank 1 in both lists
	lexOnly := uuid.New() // appears only in lexical list
	semOnly := uuid.New() // appears only in semantic list

	lexical := []uuid.UUID{both, lexOnly}
	semantic := []uuid.UUID{both, semOnly}

	fused := service.RRFCombine(lexical, semantic)

	require.NotEmpty(t, fused)
	assert.Equal(t, both, fused[0], "doc top in both lists must be ranked first after RRF")
}

// TestRRFCombine_LexicalOnlyDoc confirms a doc in only one list still gets scored
// and appears in the output.
func TestRRFCombine_LexicalOnlyDoc(t *testing.T) {
	t.Parallel()

	lexOnly := uuid.New()
	other := uuid.New()

	fused := service.RRFCombine([]uuid.UUID{lexOnly}, []uuid.UUID{other})

	require.Len(t, fused, 2)
	// lexOnly and other are both in the result; exact order depends on equal scores.
	ids := map[uuid.UUID]bool{fused[0]: true, fused[1]: true}
	assert.True(t, ids[lexOnly], "lexOnly must appear in fused output")
	assert.True(t, ids[other], "other must appear in fused output")
}

// TestRRFCombine_EmptyLists returns empty when both lists are empty.
func TestRRFCombine_EmptyLists(t *testing.T) {
	t.Parallel()

	fused := service.RRFCombine(nil, nil)
	assert.Empty(t, fused)
}

// TestRRFCombine_OnlyLexical works correctly when semantic list is empty.
func TestRRFCombine_OnlyLexical(t *testing.T) {
	t.Parallel()

	a := uuid.New()
	b := uuid.New()

	fused := service.RRFCombine([]uuid.UUID{a, b}, nil)

	require.Len(t, fused, 2)
	// Order should be preserved (a ranks higher in lexical).
	assert.Equal(t, a, fused[0])
	assert.Equal(t, b, fused[1])
}

// TestRRFCombine_Deterministic verifies that ties are broken deterministically
// (same input always produces same output).
func TestRRFCombine_Deterministic(t *testing.T) {
	t.Parallel()

	// Two docs that each appear in exactly one list at the same rank → equal scores.
	a := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	b := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	fused1 := service.RRFCombine([]uuid.UUID{a}, []uuid.UUID{b})
	fused2 := service.RRFCombine([]uuid.UUID{a}, []uuid.UUID{b})

	require.Equal(t, fused1, fused2, "RRFCombine must be deterministic for equal-score ties")
}

// TestRRFCombine_RankOrderEffect verifies that rank 1 > rank 2 in score contribution.
func TestRRFCombine_RankOrderEffect(t *testing.T) {
	t.Parallel()

	// a is rank 1 in lexical; b is rank 2 in lexical; neither appears in semantic.
	a := uuid.New()
	b := uuid.New()

	fused := service.RRFCombine([]uuid.UUID{a, b}, nil)

	require.Len(t, fused, 2)
	assert.Equal(t, a, fused[0], "rank-1 doc must score higher than rank-2 doc")
}

// TestRRFCombine_NoDuplicates ensures a doc appearing in both lists is represented
// once in the output, not duplicated.
func TestRRFCombine_NoDuplicates(t *testing.T) {
	t.Parallel()

	shared := uuid.New()
	extra := uuid.New()

	fused := service.RRFCombine([]uuid.UUID{shared, extra}, []uuid.UUID{shared})

	seen := make(map[uuid.UUID]int)
	for _, id := range fused {
		seen[id]++
	}

	assert.Equal(t, 1, seen[shared], "shared doc must appear exactly once in fused output")
}
