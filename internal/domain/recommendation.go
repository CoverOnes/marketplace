package domain

import (
	"time"

	"github.com/google/uuid"
)

// ScoreBreakdown holds the per-dimension scores produced by the AI scoring
// service. All values are in [0, 1]. The struct is marshaled to/from the
// score_breakdown jsonb column.
type ScoreBreakdown struct {
	Skill       float64 `json:"skill"`
	Reliability float64 `json:"reliability"`
	Collab      float64 `json:"collab"`
	Fit         float64 `json:"fit"`
	Comm        float64 `json:"comm"`
}

// AIRecommendation is a row in the ai_recommendation table.
// Each row is an immutable audit record of one AI-generated recommendation event.
//
// No FK constraints: subject_user_id and target_id are soft references.
// Referential integrity is enforced in the service layer on insert.
//
// Retention: 30-day TTL enforced by the recommendation retention runner
// (internal/recommendation/retention.go).
type AIRecommendation struct {
	ID                 uuid.UUID
	RecommendationType string
	TargetID           uuid.UUID
	SubjectUserID      uuid.UUID
	OverallScore       float64
	ScoreBreakdown     ScoreBreakdown
	// Basis is the human-readable explainability text.
	// MUST be redacted (backend-security §3.1) before persist: any generated
	// text that matches credential patterns is replaced with [REDACTED:type].
	Basis        *string
	Accepted     *bool
	ModelVersion string
	CreatedAt    time.Time
}
