-- Revert migration 000004: drop the full-text search GIN index.
DROP INDEX IF EXISTS listings_fts_idx;
