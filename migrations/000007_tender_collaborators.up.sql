-- Migration 000007: create tender_collaborators table + alter bids to add role_id
-- NO FOREIGN KEY constraints (CONVENTIONS §11 / CLAUDE.md #9).
-- All uuid columns are soft references validated in the service layer.

CREATE TABLE tender_collaborators (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tender_role_id          uuid        NOT NULL,
    listing_id              uuid        NOT NULL,  -- denormalized for efficient listing-scoped queries
    vendor_user_id          uuid        NOT NULL,
    status                  text        NOT NULL DEFAULT 'PENDING'
                                            CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'WITHDRAWN', 'EXITED')),
    join_message            text        NOT NULL DEFAULT '',
    approved_at             timestamptz,
    approved_by_user_id     uuid,
    exited_at               timestamptz,
    exit_reason             text        NOT NULL DEFAULT '',
    profit_share_bps_override int       CHECK (profit_share_bps_override IS NULL OR
                                               (profit_share_bps_override >= 0 AND profit_share_bps_override <= 10000)),
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);

-- UNIQUE partial index: a vendor may have at most ONE live (PENDING or APPROVED)
-- application per role. REJECTED/WITHDRAWN/EXITED free the slot so a vendor may
-- re-apply. This prevents duplicate live collaborators without a FK constraint.
CREATE UNIQUE INDEX tender_collaborators_live_unique_idx
    ON tender_collaborators (tender_role_id, vendor_user_id)
    WHERE status IN ('PENDING', 'APPROVED');

-- Advisory indexes for common query paths.
CREATE INDEX tender_collaborators_listing_id_idx
    ON tender_collaborators (listing_id);

CREATE INDEX tender_collaborators_vendor_user_id_idx
    ON tender_collaborators (vendor_user_id);

CREATE INDEX tender_collaborators_tender_role_id_idx
    ON tender_collaborators (tender_role_id);

CREATE INDEX tender_collaborators_status_idx
    ON tender_collaborators (status);

-- Alter bids: add nullable role_id (NULL = classic bid; non-NULL = tender bid).
ALTER TABLE bids
    ADD COLUMN role_id uuid;  -- soft reference to tender_roles.id; NULL for classic bids
