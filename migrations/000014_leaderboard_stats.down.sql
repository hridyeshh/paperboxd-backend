-- Remove indexes
DROP INDEX IF EXISTS idx_leaderboard_user_xp;
DROP INDEX IF EXISTS idx_leaderboard_updated_at;
DROP INDEX IF EXISTS idx_leaderboard_username;
DROP INDEX IF EXISTS idx_leaderboard_current_streak;
DROP INDEX IF EXISTS idx_leaderboard_total_xp;
DROP INDEX IF EXISTS idx_leaderboard_genres_explored;
DROP INDEX IF EXISTS idx_leaderboard_diary_entries;
DROP INDEX IF EXISTS idx_leaderboard_pages_read;
DROP INDEX IF EXISTS idx_leaderboard_books_read;

-- Remove table
DROP TABLE IF EXISTS leaderboard_stats;
