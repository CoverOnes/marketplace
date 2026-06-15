-- Migration 000011: listing_attachments table.
-- Soft-references file_objects from the file service (NO FK — CONVENTIONS §11 / CLAUDE.md #9).
-- Metadata (filename, content_type, size_bytes) is copied at attach time so the
-- attachment record is self-contained even if the file service entry is later deleted.
-- Referential integrity (file exists + STORED status + owner-match + MIME allowlist)
-- is enforced in the service layer on attach; the DB only stores what was validated.
--
-- Cap of 10 attachments per listing is enforced in the service layer, NOT via DB constraint.
-- pgvector extension is already installed by migration 000010.
--
-- Retention: listing_attachments rows outlive their parent listing for audit purposes.
-- No TTL — they are immutable once created (only soft-detach via detached_at).

CREATE TABLE listing_attachments (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id     uuid        NOT NULL,
    file_id        uuid        NOT NULL,
    uploader_id    uuid        NOT NULL,
    filename       text        NOT NULL CHECK (char_length(filename) BETWEEN 1 AND 255),
    content_type   text        NOT NULL CHECK (char_length(content_type) BETWEEN 1 AND 127),
    size_bytes     bigint      NOT NULL CHECK (size_bytes >= 0),
    detached_at    timestamptz,
    detached_by    uuid,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Index for fetching all active attachments for a listing (hot read path).
CREATE INDEX listing_attachments_listing_id_idx
    ON listing_attachments (listing_id)
    WHERE detached_at IS NULL;

-- Index for looking up a specific file attachment (dedup + owner-match on attach).
CREATE INDEX listing_attachments_file_id_idx
    ON listing_attachments (file_id);

-- Index for the uploader (admin/audit queries: "what did user X attach?").
CREATE INDEX listing_attachments_uploader_id_idx
    ON listing_attachments (uploader_id);
