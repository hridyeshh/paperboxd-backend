-- Event taxonomy: one naming convention, anonymous events, a usable funnel.
--
-- Three things were wrong with `events` and each one blocks the discovery work:
--
--   1. `user_id` NOT NULL made every pre-signup event unrecordable, so the
--      acquisition half of the funnel (landing -> signup -> activation) could
--      not be measured at all.
--   2. Three naming conventions coexisted: the server emitted dotted names
--      ("book.viewed"), recommendation feedback emitted bare verbs
--      ("impression", "click", "dismiss") and iOS emitted "book_impression".
--      Grouping by event_type therefore split one behaviour across three rows.
--   3. `session_id` was declared in 000028 and never written.
--
-- This migration normalises the names in place and opens the table to
-- anonymous rows. Validation of new writes lives in Go (service.EventType) --
-- a CHECK constraint would force a migration every time an event is added.

-- 1. Anonymous events. anon_id is a client-generated UUID kept in localStorage
--    (web) / UserDefaults (mobile) so a pre-signup session can be stitched to
--    the account it eventually creates.
ALTER TABLE events ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE events ADD COLUMN IF NOT EXISTS anon_id uuid;

-- A row with neither identifier is unattributable noise.
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_actor_present;
ALTER TABLE events ADD CONSTRAINT events_actor_present
    CHECK (user_id IS NOT NULL OR anon_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_events_anon_created
    ON events(anon_id, created_at DESC) WHERE anon_id IS NOT NULL;

-- 2. Normalise historical names to snake_case. Dotted -> underscored, and the
--    three bare recommendation verbs get the rec_ prefix that says which
--    surface they came from.
UPDATE events SET event_type = replace(event_type, '.', '_')
WHERE event_type LIKE '%.%';

UPDATE events SET event_type = 'rec_impression' WHERE event_type IN ('impression', 'book_impression');
UPDATE events SET event_type = 'rec_click'      WHERE event_type = 'click';
UPDATE events SET event_type = 'rec_dismiss'    WHERE event_type = 'dismiss';

-- 3. The discovery funnel is always "these event types for this user ordered by
--    time". Without book_id in the index every funnel query re-reads the heap.
CREATE INDEX IF NOT EXISTS idx_events_user_book_type
    ON events(user_id, book_id, event_type) WHERE book_id IS NOT NULL;
