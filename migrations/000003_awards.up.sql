-- Migration 000003: create awards table
-- Awards are the authoritative deal-closed record.
-- Written in the SAME transaction that accepts the bid + flips listing to AWARDED.
-- NO FOREIGN KEY constraints (CONVENTIONS §11 / CLAUDE.md #9).

CREATE TABLE awards (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id          uuid        NOT NULL,
    bid_id              uuid        NOT NULL,
    owner_user_id       uuid        NOT NULL,
    bidder_user_id      uuid        NOT NULL,
    amount              numeric(14,2) NOT NULL,
    currency            text        NOT NULL DEFAULT 'TWD',
    event_published_at  timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- Enforce exactly one award per listing (idempotent deal-closed).
-- Concurrent accept attempts for the same listing collapse to one winner (23505 -> 409).
CREATE UNIQUE INDEX awards_listing_id_unique
    ON awards (listing_id);

-- Contractor's award history.
CREATE INDEX awards_bidder_user_id_idx
    ON awards (bidder_user_id);

-- Future outbox-relay sweep: unpublished awards eligible for retry.
CREATE INDEX awards_event_published_at_idx
    ON awards (event_published_at)
    WHERE event_published_at IS NULL;
