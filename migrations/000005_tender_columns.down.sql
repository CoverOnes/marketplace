-- Migration 000005 down: remove tender discriminator columns from listings
DROP INDEX IF EXISTS listings_tender_status_created_at_idx;

ALTER TABLE listings
    DROP COLUMN IF EXISTS kyc_tier_required,
    DROP COLUMN IF EXISTS tender_status,
    DROP COLUMN IF EXISTS recruiter_mode,
    DROP COLUMN IF EXISTS is_tender;
