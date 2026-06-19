-- up: add dead_lettered_at to event_outbox for poison-event capping.
-- NO FOREIGN KEY (CLAUDE.md #9).
-- Rows with dead_lettered_at IS NOT NULL are permanently excluded from
-- PollReady so they do not pollute the batch limits (poller=100, indexer=20).
-- Retained for manual inspection; requeue by setting dead_lettered_at = NULL.
-- Retention: dead-lettered rows have published_at IS NULL so they survive the
-- existing published-row housekeeping.  Operators should purge them with:
--   DELETE FROM event_outbox WHERE dead_lettered_at < now() - interval '30 days'.
ALTER TABLE event_outbox ADD COLUMN dead_lettered_at timestamptz;

CREATE INDEX event_outbox_dead_letter_idx
    ON event_outbox (dead_lettered_at)
    WHERE published_at IS NULL AND dead_lettered_at IS NOT NULL;
