-- Migration 000007 down: drop tender_collaborators table + remove bids.role_id
ALTER TABLE bids
    DROP COLUMN IF EXISTS role_id;

DROP TABLE IF EXISTS tender_collaborators;
