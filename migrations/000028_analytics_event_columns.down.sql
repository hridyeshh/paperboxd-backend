DROP INDEX IF EXISTS idx_events_event_type_created;
DROP INDEX IF EXISTS idx_events_session;
DROP INDEX IF EXISTS idx_events_created_at;
ALTER TABLE events DROP COLUMN IF EXISTS path;
ALTER TABLE events DROP COLUMN IF EXISTS source;
ALTER TABLE events DROP COLUMN IF EXISTS session_id;
