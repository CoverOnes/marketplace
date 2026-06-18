// Package embedding provides helpers for composing and indexing text embeddings
// for marketplace entities.
package embedding

import (
	"unicode/utf8"

	"github.com/CoverOnes/marketplace/internal/domain"
)

// maxTextRunes is the character cap applied to the composed text before sending to
// the embedding API. text-embedding-3-small supports up to ~32 KiB; we cap at 20 000
// runes to stay well within the limit and bound outbound payload size.
// Must match client.maxEmbeddingTextRunes (the client also caps, so this is a
// belt-and-suspenders guard at the composition layer).
const maxTextRunes = 20_000

// ComposeTenderText returns the embeddable plain-text representation of a tender
// listing. Only title and description are embedded; budget and currency are
// structured filters and MUST NOT be included (they inflate the embedding distance
// space without semantic benefit and would skew cosine similarity).
//
// The output is deterministic: title + newline + description, capped at 20 000
// runes. The function is pure (no I/O); tests can golden-compare the output.
func ComposeTenderText(l *domain.Listing) string {
	if l == nil {
		return ""
	}

	composed := l.Title + "\n" + l.Description

	if utf8.RuneCountInString(composed) > maxTextRunes {
		runes := []rune(composed)
		composed = string(runes[:maxTextRunes])
	}

	return composed
}
