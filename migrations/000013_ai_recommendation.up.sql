-- Migration 000013: AI recommendation audit table.
-- NO FOREIGN KEY constraints platform-wide (CONVENTIONS §11 / CLAUDE.md #9).
-- subject_user_id and target_id are soft references; referential integrity is
-- enforced in the service layer on insert.
--
-- Design:
--   * Immutable audit rows: one INSERT per recommendation event; never updated.
--   * score_breakdown (jsonb): structured per-dimension scores written by the
--     AI scoring service (T5). Keys: skill, reliability, collab, fit, comm.
--   * basis (text): human-readable explainability text for the recommendation.
--     MUST be redacted by the caller (backend-security §3.1) before INSERT --
--     any generated text containing credentials is replaced with [REDACTED:type].
--   * accepted (bool, nullable): NULL = not yet acted on; TRUE/FALSE = outcome.
--   * Retention: 30-day TTL. Rows older than 30 days are deleted by the
--     recommendation retention runner (internal/recommendation/retention.go)
--     which runs on a 24-hour interval -- mirroring the outbox poller pattern.
--     Reason: AI recommendation audit data is operationally useful for 30 days;
--     beyond that it constitutes excess PII per GDPR Art. 5(1)(c) (data minimisation).

CREATE TABLE ai_recommendation (
    id                  uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    recommendation_type text         NOT NULL CHECK (char_length(recommendation_type) BETWEEN 1 AND 127),
    target_id           uuid         NOT NULL,
    subject_user_id     uuid         NOT NULL,
    overall_score       numeric(5,4) NOT NULL CHECK (overall_score BETWEEN 0 AND 1),
    score_breakdown     jsonb        NOT NULL DEFAULT '{}',
    basis               text,
    accepted            boolean,
    model_version       text         NOT NULL CHECK (char_length(model_version) BETWEEN 1 AND 100),
    created_at          timestamptz  NOT NULL DEFAULT now()
);

-- Primary access pattern: list recommendations for a subject user, ordered
-- by recency. Partial index not applicable -- subject_user_id cardinality is high.
CREATE INDEX ai_recommendation_subject_created_idx
    ON ai_recommendation (subject_user_id, created_at DESC);
