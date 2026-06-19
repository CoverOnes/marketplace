-- down: plain SQL only, NO psql metacommands (backend-security §6.1).
DROP INDEX IF EXISTS event_outbox_dead_letter_idx;
ALTER TABLE event_outbox DROP COLUMN IF EXISTS dead_lettered_at;
