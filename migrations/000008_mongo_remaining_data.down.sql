DROP INDEX IF EXISTS idx_user_preferences_legacy_payload_gin;
DROP TABLE IF EXISTS user_preferences_legacy;

DROP INDEX IF EXISTS idx_user_authors_read_user_id;
DROP TABLE IF EXISTS user_authors_read;

ALTER TABLE likes DROP COLUMN IF EXISTS reason;

ALTER TABLE users DROP COLUMN IF EXISTS reading_goal_current;
ALTER TABLE users DROP COLUMN IF EXISTS reading_goal_target;
ALTER TABLE users DROP COLUMN IF EXISTS reading_goal_year;

ALTER TABLE bookshelf DROP COLUMN IF EXISTS format;
ALTER TABLE bookshelf DROP COLUMN IF EXISTS notes;
