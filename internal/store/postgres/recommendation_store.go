package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// maxRecommendationTypeLen is the maximum rune count for recommendation_type.
	// MUST match CHECK (char_length(recommendation_type) BETWEEN 1 AND 127) in migration 000013.
	maxRecommendationTypeLen = 127

	// maxModelVersionLenRec is the maximum rune count for model_version in ai_recommendation.
	// MUST match CHECK (char_length(model_version) BETWEEN 1 AND 100) in migration 000013.
	maxModelVersionLenRec = 100

	// maxBasisRunes is the maximum rune count for the basis field (backend-security §5.4).
	maxBasisRunes = 5000

	// defaultListBySubjectLimit is the default and maximum number of rows returned
	// by ListBySubject when limit <= 0.
	defaultListBySubjectLimit = 200
)

// credentialPatterns are the regex patterns applied by redactBasis to scrub
// credential-like strings from AI-generated basis text before persisting
// (backend-security §3.1). Patterns mirror the spec in backend-security-design.md.
var credentialPatterns = []*regexp.Regexp{ // package-level compiled regexes; initialized once at startup, never mutated
	regexp.MustCompile(`sk_live_[A-Za-z0-9_]+`),
	regexp.MustCompile(`ghp_[A-Za-z0-9_]+`),
	regexp.MustCompile(`xoxb-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`Bearer ey[A-Za-z0-9._-]+`),
	// postgres:// and postgresql:// (RFC-correct scheme) DSNs (backend-security §3.1, Mi-1).
	regexp.MustCompile(`postgres(?:ql)?://[^:]+:[^@]+@\S+`),
	// mongodb:// and mongodb+srv:// (Atlas DNS-seedlist, the common hosted form) DSNs
	// (backend-security §3.1, M-1).
	regexp.MustCompile(`mongodb(?:\+srv)?://[^:]+:[^@]+@\S+`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// password / api_key key=value pairs. The quoted branches use [^']* / [^"]* (NOT
	// [^\s'"]*) so a value containing the opposite quote style — e.g. api_key: 'secret"x' —
	// is still fully consumed including its closing quote, and the closing quote never
	// leaks out (backend-security §3.1, M-2 mixed-quote evasion + Mi-3 trailing quote).
	regexp.MustCompile(`(?i)password[=:]\s*(?:'[^']*'|"[^"]*"|[^\s'"]+)`),
	regexp.MustCompile(`(?i)api[_-]?key[=:]\s*(?:'[^']*'|"[^"]*"|[^\s'"]+)`),
}

// redactBasis applies the §3.1 credential patterns to text, replacing each
// match with [REDACTED]. It is called on *r.Basis before any DB Exec.
func redactBasis(text string) string {
	for _, re := range credentialPatterns {
		text = re.ReplaceAllString(text, "[REDACTED]")
	}

	return text
}

// RecommendationStore is a pool-backed recommendation store.
// It satisfies store.RecommendationStore.
type RecommendationStore struct {
	q querier
}

// NewRecommendationStore returns a RecommendationStore backed by pool.
func NewRecommendationStore(pool *pgxpool.Pool) *RecommendationStore {
	return &RecommendationStore{q: pool}
}

// compile-time interface check.
var _ store.RecommendationStore = (*RecommendationStore)(nil)

// Insert writes one AI recommendation audit row.
// Basis MUST already be redacted by the caller (backend-security §3.1).
func (s *RecommendationStore) Insert(ctx context.Context, r *domain.AIRecommendation) error {
	return insertRecommendation(ctx, s.q, r)
}

// ListBySubject returns up to limit recommendation rows for subjectUserID
// ordered by created_at descending. limit is clamped to defaultListBySubjectLimit
// when <= 0.
func (s *RecommendationStore) ListBySubject(ctx context.Context, subjectUserID uuid.UUID, limit int) ([]*domain.AIRecommendation, error) {
	return listRecommendationsBySubject(ctx, s.q, subjectUserID, limit)
}

// DeleteOlderThan removes rows with created_at < cutoff. Returns row count deleted.
func (s *RecommendationStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	return deleteRecommendationsOlderThan(ctx, s.q, cutoff)
}

// --- helpers ---

func insertRecommendation(ctx context.Context, q querier, r *domain.AIRecommendation) error {
	// Go-level validation to surface friendly errors before DB round-trip
	// (backend-security §5.2: don't let raw check_violation surface to callers).
	recTypeLen := utf8.RuneCountInString(r.RecommendationType)
	if recTypeLen < 1 || recTypeLen > maxRecommendationTypeLen {
		return fmt.Errorf(
			"insert recommendation: recommendation_type length must be 1-%d runes (got %d)",
			maxRecommendationTypeLen, recTypeLen,
		)
	}

	modelVersionLen := utf8.RuneCountInString(r.ModelVersion)
	if modelVersionLen < 1 || modelVersionLen > maxModelVersionLenRec {
		return fmt.Errorf(
			"insert recommendation: model_version length must be 1-%d runes (got %d)",
			maxModelVersionLenRec, modelVersionLen,
		)
	}

	if r.OverallScore < 0 || r.OverallScore > 1 {
		return fmt.Errorf("insert recommendation: overall_score must be in [0, 1] (got %v)", r.OverallScore)
	}

	if err := validateScoreBreakdown(r.ScoreBreakdown); err != nil {
		return err
	}

	basisVal, err := prepareBasis(r.Basis)
	if err != nil {
		return err
	}

	breakdownJSON, err := json.Marshal(r.ScoreBreakdown)
	if err != nil {
		return fmt.Errorf("insert recommendation: marshal score_breakdown: %w", err)
	}

	const query = `
INSERT INTO ai_recommendation
    (id, recommendation_type, target_id, subject_user_id,
     overall_score, score_breakdown, basis, accepted, model_version, created_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
`

	_, execErr := q.Exec(
		ctx, query,
		r.ID, r.RecommendationType, r.TargetID, r.SubjectUserID,
		r.OverallScore, breakdownJSON, basisVal, r.Accepted, r.ModelVersion, r.CreatedAt,
	)
	if execErr != nil {
		return fmt.Errorf("insert recommendation: %w", execErr)
	}

	return nil
}

// validateScoreBreakdown rejects any ScoreBreakdown sub-dimension outside [0, 1]
// (security finding Mi-1).
func validateScoreBreakdown(bd domain.ScoreBreakdown) error {
	for dimName, val := range map[string]float64{
		"skill":       bd.Skill,
		"reliability": bd.Reliability,
		"collab":      bd.Collab,
		"fit":         bd.Fit,
		"comm":        bd.Comm,
	} {
		if val < 0 || val > 1 {
			return fmt.Errorf(
				"insert recommendation: score_breakdown.%s must be in [0, 1] (got %v)",
				dimName, val,
			)
		}
	}

	return nil
}

// prepareBasis sanitizes and redacts the optional basis string before persist
// (backend-security §3.1 + §5.4). Returns a pointer to the sanitized+redacted
// copy, or nil when basis is nil. Returns an error when the text fails sanity
// checks (control chars, length cap).
func prepareBasis(basis *string) (*string, error) {
	if basis == nil {
		return nil, nil
	}

	sanitized := *basis

	// Reject control characters (backend-security §5.4).
	for _, ch := range sanitized {
		if ch == '\x00' || ch == '\r' || ch == '\n' || (ch < 0x20 && ch != '\t') {
			return nil, fmt.Errorf("insert recommendation: basis contains invalid control characters")
		}
	}

	// Cap length at maxBasisRunes (backend-security §5.4).
	if utf8.RuneCountInString(sanitized) > maxBasisRunes {
		return nil, fmt.Errorf(
			"insert recommendation: basis exceeds maximum length of %d runes",
			maxBasisRunes,
		)
	}

	// Apply credential redaction (backend-security §3.1).
	redacted := redactBasis(sanitized)

	return &redacted, nil
}

func listRecommendationsBySubject(ctx context.Context, q querier, subjectUserID uuid.UUID, limit int) ([]*domain.AIRecommendation, error) {
	if limit <= 0 {
		limit = defaultListBySubjectLimit
	}

	const query = `
SELECT id, recommendation_type, target_id, subject_user_id,
       overall_score, score_breakdown, basis, accepted, model_version, created_at
FROM ai_recommendation
WHERE subject_user_id = $1
ORDER BY created_at DESC
LIMIT $2
`

	rows, err := q.Query(ctx, query, subjectUserID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recommendations by subject: %w", err)
	}

	defer rows.Close()

	var results []*domain.AIRecommendation

	for rows.Next() {
		rec, scanErr := scanRecommendation(rows)
		if scanErr != nil {
			return nil, scanErr
		}

		results = append(results, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recommendation rows: %w", err)
	}

	return results, nil
}

func deleteRecommendationsOlderThan(ctx context.Context, q querier, cutoff time.Time) (int64, error) {
	const query = `DELETE FROM ai_recommendation WHERE created_at < $1`

	tag, err := q.Exec(ctx, query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete recommendations older than %s: %w", cutoff.Format(time.RFC3339), err)
	}

	return tag.RowsAffected(), nil
}

func scanRecommendation(row rowScanner) (*domain.AIRecommendation, error) {
	var (
		r             domain.AIRecommendation
		breakdownJSON []byte
	)

	err := row.Scan(
		&r.ID, &r.RecommendationType, &r.TargetID, &r.SubjectUserID,
		&r.OverallScore, &breakdownJSON, &r.Basis, &r.Accepted, &r.ModelVersion, &r.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}

		return nil, fmt.Errorf("scan recommendation: %w", err)
	}

	if jsonErr := json.Unmarshal(breakdownJSON, &r.ScoreBreakdown); jsonErr != nil {
		return nil, fmt.Errorf("unmarshal score_breakdown: %w", jsonErr)
	}

	return &r, nil
}
