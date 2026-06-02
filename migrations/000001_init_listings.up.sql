-- Migration 000001: create listings table
-- NO FOREIGN KEY constraints platform-wide (CONVENTIONS §11 / CLAUDE.md #9).
-- Referential integrity enforced in the service layer (validate-on-write + nullable JOIN).

CREATE EXTENSION IF NOT EXISTS pgcrypto; -- provides gen_random_uuid()

CREATE TABLE listings (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id uuid        NOT NULL,
    title         text        NOT NULL,
    description   text        NOT NULL DEFAULT '',
    budget_min    numeric(14,2),
    budget_max    numeric(14,2),
    currency      text        NOT NULL DEFAULT 'TWD' CHECK (char_length(currency) = 3),
    status        text        NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','AWARDED','CLOSED')),
    awarded_bid_id uuid,
    deleted_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Advisory indexes (no FK required — soft refs tracked in code).
-- owner browse + existence check.
CREATE INDEX listings_owner_user_id_idx
    ON listings (owner_user_id)
    WHERE deleted_at IS NULL;

-- Public browse: OPEN listings ordered newest-first.
CREATE INDEX listings_status_created_at_idx
    ON listings (status, created_at DESC)
    WHERE deleted_at IS NULL;

-- General created_at for admin/audit queries.
CREATE INDEX listings_created_at_idx
    ON listings (created_at DESC)
    WHERE deleted_at IS NULL;
