-- Phase 4: add a partial index on tender_collaborators for fast lookup of APPROVED
-- collaborators per listing. Used by the join-while-EXECUTING accept path to avoid
-- full-table scans when polling approved counts during concurrent accepts.
CREATE INDEX tender_collaborators_status_listing_id_idx
    ON tender_collaborators (status, listing_id)
    WHERE status = 'APPROVED';
