-- Add leaderboard and XP tracking columns to users table
ALTER TABLE users ADD COLUMN IF NOT EXISTS total_xp INTEGER DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS level INTEGER DEFAULT 1;
ALTER TABLE users ADD COLUMN IF NOT EXISTS current_streak INTEGER DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS longest_streak INTEGER DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_activity_date DATE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS show_on_leaderboard BOOLEAN DEFAULT true;

-- Create indexes for leaderboard queries
CREATE INDEX IF NOT EXISTS idx_users_total_xp ON users(total_xp DESC);
CREATE INDEX IF NOT EXISTS idx_users_level ON users(level DESC);
CREATE INDEX IF NOT EXISTS idx_users_current_streak ON users(current_streak DESC);
CREATE INDEX IF NOT EXISTS idx_users_show_on_leaderboard ON users(show_on_leaderboard);

-- Update existing users to have default values
UPDATE users SET total_xp = 0 WHERE total_xp IS NULL;
UPDATE users SET level = 1 WHERE level IS NULL;
UPDATE users SET current_streak = 0 WHERE current_streak IS NULL;
UPDATE users SET longest_streak = 0 WHERE longest_streak IS NULL;
UPDATE users SET show_on_leaderboard = true WHERE show_on_leaderboard IS NULL;
