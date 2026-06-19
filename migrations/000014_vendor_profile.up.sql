-- Migration 000014: vendor_profile table.
-- NO FOREIGN KEY constraints platform-wide (CONVENTIONS §11 / CLAUDE.md #9).
-- owner_user_id is a soft reference to the user service; referential integrity
-- is enforced in the service layer on upsert.
--
-- Design:
--   * One profile per user (UNIQUE on owner_user_id) — enforced by unique index.
--   * Upsert-semantics: PUT /v1/vendor-profile is idempotent; the Postgres
--     ON CONFLICT (owner_user_id) DO UPDATE path preserves created_at and bumps
--     updated_at. There is never a separate "create" vs "update" operation.
--   * display_name is required (1..200 rune CHECK).
--   * headline and bio are optional free-text.
--   * skills is a text[] column — no FK to a taxonomy table.
--   * V2 will wrap the Upsert in an outbox tx to trigger embedding reindex;
--     the schema is designed so that V2 only adds an outbox enqueue, not a
--     schema change.
--
-- Go-level validation mirrors the DB CHECKs (backend-security §5.2):
--   display_name 1..200 runes, headline ≤200, bio ≤5000, each skill ≤100 runes,
--   skills slice ≤50.

CREATE TABLE vendor_profile (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id   uuid        NOT NULL,
    display_name    text        NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 200),
    headline        text        CHECK (headline IS NULL OR char_length(headline) <= 200),
    bio             text        CHECK (bio IS NULL OR char_length(bio) <= 5000),
    skills          text[]      NOT NULL DEFAULT '{}',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Unique index on owner_user_id enforces one profile per user.
-- Also used by the ON CONFLICT (owner_user_id) DO UPDATE upsert path.
CREATE UNIQUE INDEX vendor_profile_owner_user_id_idx
    ON vendor_profile (owner_user_id);
