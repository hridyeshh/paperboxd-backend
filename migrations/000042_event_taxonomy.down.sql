DROP INDEX IF EXISTS idx_events_user_book_type;
DROP INDEX IF EXISTS idx_events_anon_created;
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_actor_present;
ALTER TABLE events DROP COLUMN IF EXISTS anon_id;
-- user_id is deliberately left nullable: rows written while this migration was
-- applied may have no user, and restoring NOT NULL would fail on them.
