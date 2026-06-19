package postgres

import "github.com/CoverOnes/marketplace/internal/domain"

// redactBasis scrubs credential-like strings from AI-generated basis text before
// persisting (backend-security §3.1). It delegates to domain.RedactCredentials
// so the canonical pattern list lives in exactly one place.
// Called on *r.Basis before any DB Exec in recommendation_store.go.
func redactBasis(text string) string {
	return domain.RedactCredentials(text)
}
