-- Read state for the in-app notifications sheet (Android NotificationsSheet,
-- iOS NotificationsView). Those already render `activities` rows addressed to
-- the viewer via target_user_id, so push notifications reuse that table rather
-- than duplicating a parallel `notifications` store — one write, one read path.
--
-- TIMESTAMPTZ while activities.created_at is plain TIMESTAMP: deliberate. New
-- columns follow the TIMESTAMPTZ convention used since 000034; back-filling the
-- existing column is a separate change with its own risk.
ALTER TABLE activities ADD COLUMN IF NOT EXISTS read_at TIMESTAMPTZ;

-- The notifications sheet reads "activities aimed at me, newest first", which
-- the existing idx_activities_user_id (actor, not recipient) cannot serve.
CREATE INDEX IF NOT EXISTS idx_activities_target_created
    ON activities (target_user_id, created_at DESC)
    WHERE target_user_id IS NOT NULL;
