ALTER TABLE events ADD COLUMN IF NOT EXISTS session_id uuid;
ALTER TABLE events ADD COLUMN IF NOT EXISTS source     varchar(20);
ALTER TABLE events ADD COLUMN IF NOT EXISTS path       text;

CREATE INDEX IF NOT EXISTS idx_events_created_at
  ON events(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_session
  ON events(session_id) WHERE session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_events_event_type_created
  ON events(event_type, created_at DESC);
