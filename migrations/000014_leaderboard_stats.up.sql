-- Precomputed leaderboard statistics table
CREATE TABLE IF NOT EXISTS leaderboard_stats (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    username VARCHAR(255) NOT NULL,

    -- Core statistics
    books_read INTEGER DEFAULT 0,
    pages_read INTEGER DEFAULT 0,
    diary_entries INTEGER DEFAULT 0,
    genres_explored INTEGER DEFAULT 0,

    -- XP and level (denormalized from users for performance)
    total_xp INTEGER DEFAULT 0,
    level INTEGER DEFAULT 1,
    current_streak INTEGER DEFAULT 0,

    -- Rankings (1-based, lower number = better rank, NULL if unranked)
    books_rank INTEGER,
    pages_rank INTEGER,
    diary_rank INTEGER,
    genres_rank INTEGER,
    xp_rank INTEGER,
    streak_rank INTEGER,

    -- Metadata
    updated_at TIMESTAMP DEFAULT NOW(),

    CONSTRAINT unique_user_leaderboard UNIQUE(user_id)
);

-- Indexes for fast leaderboard queries
CREATE INDEX IF NOT EXISTS idx_leaderboard_books_read ON leaderboard_stats(books_read DESC);
CREATE INDEX IF NOT EXISTS idx_leaderboard_pages_read ON leaderboard_stats(pages_read DESC);
CREATE INDEX IF NOT EXISTS idx_leaderboard_diary_entries ON leaderboard_stats(diary_entries DESC);
CREATE INDEX IF NOT EXISTS idx_leaderboard_genres_explored ON leaderboard_stats(genres_explored DESC);
CREATE INDEX IF NOT EXISTS idx_leaderboard_total_xp ON leaderboard_stats(total_xp DESC);
CREATE INDEX IF NOT EXISTS idx_leaderboard_current_streak ON leaderboard_stats(current_streak DESC);
CREATE INDEX IF NOT EXISTS idx_leaderboard_username ON leaderboard_stats(username);
CREATE INDEX IF NOT EXISTS idx_leaderboard_updated_at ON leaderboard_stats(updated_at);

-- Composite index for friends leaderboard queries
CREATE INDEX IF NOT EXISTS idx_leaderboard_user_xp ON leaderboard_stats(user_id, total_xp DESC);
