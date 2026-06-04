-- Migration 000008: create tender_milestones table
-- NO FOREIGN KEY constraints (CONVENTIONS §11 / CLAUDE.md #9).
-- listing_id is a soft reference to listings.id validated in the service layer.
-- Payout computation against milestones is Phase 3; this migration ships the
-- table + CRUD infrastructure only.

CREATE TABLE tender_milestones (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id  uuid        NOT NULL,
    title       text        NOT NULL,
    due_date    date,
    amount      numeric(14,2) CHECK (amount IS NULL OR amount >= 0),
    currency    char(3),
    status      text        NOT NULL DEFAULT 'PENDING'
                                CHECK (status IN ('PENDING', 'REACHED', 'SKIPPED')),
    reached_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Advisory index for listing-scoped milestone queries.
CREATE INDEX tender_milestones_listing_id_idx
    ON tender_milestones (listing_id);
