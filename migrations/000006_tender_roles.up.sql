-- Migration 000006: create tender_roles table
-- NO FOREIGN KEY constraints (CONVENTIONS §11 / CLAUDE.md #9).
-- listing_id is a soft reference to listings.id validated in the service layer.

CREATE TABLE tender_roles (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id          uuid        NOT NULL,
    title               text        NOT NULL,
    description         text        NOT NULL DEFAULT '',
    max_collaborators   int         CHECK (max_collaborators > 0),  -- NULL = no cap
    profit_share_bps    int         CHECK (profit_share_bps IS NULL OR (profit_share_bps >= 0 AND profit_share_bps <= 10000)),
    profit_share_note   text        NOT NULL DEFAULT '',
    status              text        NOT NULL DEFAULT 'OPEN'
                                        CHECK (status IN ('OPEN', 'FILLED', 'CLOSED')),
    sort_order          int         NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Advisory indexes (no FK required — soft refs tracked in code).
CREATE INDEX tender_roles_listing_id_idx
    ON tender_roles (listing_id);

CREATE INDEX tender_roles_status_idx
    ON tender_roles (status);
