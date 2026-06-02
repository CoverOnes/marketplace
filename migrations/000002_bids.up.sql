-- Migration 000002: create bids table
-- NO FOREIGN KEY constraints (CONVENTIONS §11 / CLAUDE.md #9).
-- listing_id and bidder_user_id are soft references validated in the service layer.

CREATE TABLE bids (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id     uuid        NOT NULL,
    bidder_user_id uuid        NOT NULL,
    amount         numeric(14,2) NOT NULL CHECK (amount >= 0),
    currency       text        NOT NULL DEFAULT 'TWD' CHECK (char_length(currency) = 3),
    message        text        NOT NULL DEFAULT '',
    status         text        NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','ACCEPTED','REJECTED','WITHDRAWN')),
    decided_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- List bids for a listing (owner view).
CREATE INDEX bids_listing_id_idx
    ON bids (listing_id);

-- My bids (bidder view) + withdraw owner check.
CREATE INDEX bids_bidder_user_id_idx
    ON bids (bidder_user_id);

-- Filter by status.
CREATE INDEX bids_status_idx
    ON bids (status);

-- Newest-first ordering.
CREATE INDEX bids_created_at_idx
    ON bids (created_at DESC);

-- Prevent duplicate live (PENDING) bids per (listing, bidder) pair.
-- WITHDRAWN frees the slot so the bidder may re-bid on the same listing.
CREATE UNIQUE INDEX bids_listing_pending_one
    ON bids (listing_id, bidder_user_id)
    WHERE status = 'PENDING';
