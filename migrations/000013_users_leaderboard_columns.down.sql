-- Remove indexes
DROP INDEX IF EXISTS idx_users_show_on_leaderboard;
DROP INDEX IF EXISTS idx_users_current_streak;
DROP INDEX IF EXISTS idx_users_level;
DROP INDEX IF EXISTS idx_users_total_xp;

-- Remove columns
ALTER TABLE users DROP COLUMN IF EXISTS show_on_leaderboard;
ALTER TABLE users DROP COLUMN IF EXISTS last_activity_date;
ALTER TABLE users DROP COLUMN IF EXISTS longest_streak;
ALTER TABLE users DROP COLUMN IF EXISTS current_streak;
ALTER TABLE users DROP COLUMN IF EXISTS level;
ALTER TABLE users DROP COLUMN IF EXISTS total_xp;
