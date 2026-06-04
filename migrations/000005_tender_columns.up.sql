-- Migration 000005: add tender discriminator columns to listings
-- NO FOREIGN KEY constraints (CONVENTIONS §11 / CLAUDE.md #9).
-- The 1:1 CLASSIC flow (listings.status OPEN→AWARDED→CLOSED) is 100% intact.
-- Tender lifecycle is tracked in the NEW tender_status column (separate from status).

ALTER TABLE listings
    ADD COLUMN is_tender          bool NOT NULL DEFAULT false,
    ADD COLUMN recruiter_mode     text NOT NULL DEFAULT 'CLOSED'
                                      CHECK (recruiter_mode IN ('CLOSED', 'OPEN')),
    ADD COLUMN tender_status      text
                                      CHECK (tender_status IS NULL OR tender_status IN (
                                          'OPEN', 'PARTIALLY_STAFFED', 'EXECUTING',
                                          'SETTLING', 'COMPLETED', 'CANCELLED'
                                      )),
    ADD COLUMN kyc_tier_required  int  NOT NULL DEFAULT 2
                                      CHECK (kyc_tier_required BETWEEN 0 AND 5);

-- Partial index for tender-specific browsing (is_tender = true rows only).
-- Covers the common query: list open tenders ordered newest-first.
CREATE INDEX listings_tender_status_created_at_idx
    ON listings (is_tender, tender_status, created_at DESC)
    WHERE deleted_at IS NULL;
