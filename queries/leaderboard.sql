-- ============================================================================
-- USER XP AND LEVEL MANAGEMENT
-- ============================================================================

-- name: AddXP :exec
UPDATE users
SET
  total_xp = total_xp + $2,
  level = CASE
    -- Level 31+: 12000+ XP (500 XP per level)
    WHEN (total_xp + $2) >= 12000 THEN 31 + ((total_xp + $2 - 12000) / 500)
    -- Level 26-30: 8000-12000 XP (800 XP per level)
    WHEN (total_xp + $2) >= 8000 THEN 26 + ((total_xp + $2 - 8000) / 800)
    -- Level 21-25: 5000-8000 XP (600 XP per level)
    WHEN (total_xp + $2) >= 5000 THEN 21 + ((total_xp + $2 - 5000) / 600)
    -- Level 16-20: 3000-5000 XP (400 XP per level)
    WHEN (total_xp + $2) >= 3000 THEN 16 + ((total_xp + $2 - 3000) / 400)
    -- Level 11-15: 1500-3000 XP (300 XP per level)
    WHEN (total_xp + $2) >= 1500 THEN 11 + ((total_xp + $2 - 1500) / 300)
    -- Level 6-10: 500-1500 XP (200 XP per level)
    WHEN (total_xp + $2) >= 500 THEN 6 + ((total_xp + $2 - 500) / 200)
    -- Level 1-5: 0-500 XP (100 XP per level)
    ELSE 1 + ((total_xp + $2) / 100)
  END
-- last_activity_date is UpdateUserStreak's to set: AwardXP calls it right after
-- this. Setting it here first made every streak update read "already active
-- today", so the streak never advanced.
WHERE id = $1;

-- name: GetUserXP :one
SELECT id, username, total_xp, level, current_streak, longest_streak, last_activity_date, show_on_leaderboard
FROM users
WHERE id = $1;

-- ============================================================================
-- STREAK MANAGEMENT
-- ============================================================================

-- name: UpdateUserStreak :one
-- Counts today as an activity day. Only the first call of a UTC day matches
-- (pgx.ErrNoRows after that), so the caller knows when the streak moved; the
-- row lock makes that true for exactly one of two concurrent calls.
UPDATE users
SET
  current_streak = CASE
    -- Continued streak (activity yesterday)
    WHEN last_activity_date = CURRENT_DATE - 1 THEN COALESCE(current_streak, 0) + 1
    -- Broken streak (more than 1 day gap) or first ever activity
    ELSE 1
  END,
  longest_streak = GREATEST(
    COALESCE(longest_streak, 0),
    CASE
      WHEN last_activity_date = CURRENT_DATE - 1 THEN COALESCE(current_streak, 0) + 1
      ELSE 1
    END
  ),
  last_activity_date = CURRENT_DATE
WHERE id = $1
  AND last_activity_date IS DISTINCT FROM CURRENT_DATE
RETURNING current_streak;

-- ============================================================================
-- XP TRANSACTION LOGGING
-- ============================================================================

-- name: LogXPTransaction :exec
INSERT INTO xp_transactions (user_id, action_type, xp_amount, reference_id, metadata)
VALUES ($1, $2, $3, $4, $5);

-- name: GetUserXPToday :one
SELECT COALESCE(SUM(xp_amount), 0)::INTEGER as total_xp
FROM xp_transactions
WHERE user_id = $1
  AND DATE(created_at) = CURRENT_DATE;

-- name: GetUserXPHistory :many
SELECT id, action_type, xp_amount, reference_id, metadata, created_at
FROM xp_transactions
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- ============================================================================
-- LEADERBOARD STATS QUERIES
-- ============================================================================

-- name: GetUserLeaderboardStats :one
SELECT * FROM leaderboard_stats WHERE user_id = $1;

-- name: RebuildUserLeaderboardStats :one
INSERT INTO leaderboard_stats (
  user_id,
  username,
  books_read,
  pages_read,
  thoughts,
  genres_explored,
  total_xp,
  level,
  current_streak
)
SELECT
  u.id,
  u.username,
  COALESCE((SELECT COUNT(*) FROM bookshelf WHERE user_id = u.id AND status = 'read'), 0)::INTEGER as books_read,
  COALESCE((SELECT SUM(b.page_count)
    FROM bookshelf bs
    JOIN books b ON bs.book_id = b.id
    WHERE bs.user_id = u.id AND bs.status = 'read' AND b.page_count IS NOT NULL), 0)::INTEGER as pages_read,
  COALESCE((SELECT COUNT(*) FROM thoughts WHERE user_id = u.id AND thread_root_id IS NULL), 0)::INTEGER as thoughts,
  COALESCE(array_length(u.favorite_genres, 1), 0) as genres_explored,
  u.total_xp,
  u.level,
  -- users.current_streak is never decayed; a run whose last day is older than
  -- yesterday is already broken (same rule as activeStreak in reading_log.go).
  CASE WHEN u.last_activity_date >= CURRENT_DATE - 1 THEN u.current_streak ELSE 0 END
FROM users u
WHERE u.id = $1
ON CONFLICT (user_id)
DO UPDATE SET
  username = EXCLUDED.username,
  books_read = EXCLUDED.books_read,
  pages_read = EXCLUDED.pages_read,
  thoughts = EXCLUDED.thoughts,
  genres_explored = EXCLUDED.genres_explored,
  total_xp = EXCLUDED.total_xp,
  level = EXCLUDED.level,
  current_streak = EXCLUDED.current_streak,
  updated_at = NOW()
RETURNING *;

-- name: RebuildAllLeaderboardStats :exec
INSERT INTO leaderboard_stats (
  user_id,
  username,
  books_read,
  pages_read,
  thoughts,
  genres_explored,
  total_xp,
  level,
  current_streak
)
SELECT
  u.id,
  u.username,
  COALESCE((SELECT COUNT(*) FROM bookshelf WHERE user_id = u.id AND status = 'read'), 0)::INTEGER,
  COALESCE((SELECT SUM(b.page_count)
    FROM bookshelf bs
    JOIN books b ON bs.book_id = b.id
    WHERE bs.user_id = u.id AND bs.status = 'read' AND b.page_count IS NOT NULL), 0)::INTEGER,
  COALESCE((SELECT COUNT(*) FROM thoughts WHERE user_id = u.id AND thread_root_id IS NULL), 0)::INTEGER,
  COALESCE(array_length(u.favorite_genres, 1), 0),
  u.total_xp,
  u.level,
  -- users.current_streak is never decayed; a run whose last day is older than
  -- yesterday is already broken (same rule as activeStreak in reading_log.go).
  CASE WHEN u.last_activity_date >= CURRENT_DATE - 1 THEN u.current_streak ELSE 0 END
FROM users u
ON CONFLICT (user_id)
DO UPDATE SET
  username = EXCLUDED.username,
  books_read = EXCLUDED.books_read,
  pages_read = EXCLUDED.pages_read,
  thoughts = EXCLUDED.thoughts,
  genres_explored = EXCLUDED.genres_explored,
  total_xp = EXCLUDED.total_xp,
  level = EXCLUDED.level,
  current_streak = EXCLUDED.current_streak,
  updated_at = NOW();

-- name: UpdateLeaderboardRankings :exec
WITH ranked AS (
  SELECT
    user_id,
    ROW_NUMBER() OVER (ORDER BY books_read DESC, total_xp DESC) as books_rank,
    ROW_NUMBER() OVER (ORDER BY pages_read DESC, total_xp DESC) as pages_rank,
    ROW_NUMBER() OVER (ORDER BY thoughts DESC, total_xp DESC) as thoughts_rank,
    ROW_NUMBER() OVER (ORDER BY genres_explored DESC, total_xp DESC) as genres_rank,
    ROW_NUMBER() OVER (ORDER BY total_xp DESC, books_read DESC) as xp_rank,
    ROW_NUMBER() OVER (ORDER BY current_streak DESC, total_xp DESC) as streak_rank
  FROM leaderboard_stats
)
UPDATE leaderboard_stats ls
SET
  books_rank = r.books_rank::INTEGER,
  pages_rank = r.pages_rank::INTEGER,
  thoughts_rank = r.thoughts_rank::INTEGER,
  genres_rank = r.genres_rank::INTEGER,
  xp_rank = r.xp_rank::INTEGER,
  streak_rank = r.streak_rank::INTEGER
FROM ranked r
WHERE ls.user_id = r.user_id;

-- ============================================================================
-- LEADERBOARD QUERIES
-- ============================================================================

-- name: GetFriendsLeaderboard :many
-- Includes the caller and everyone they follow, ranked together.
SELECT ls.*
FROM leaderboard_stats ls
INNER JOIN users u ON ls.user_id = u.id
WHERE u.deleted_at IS NULL
  AND (
    ls.user_id = $1
    OR ls.user_id IN (SELECT f.following_id FROM follows f WHERE f.follower_id = $1)
  )
  AND (ls.total_xp > 0 OR ls.books_read > 0 OR ls.thoughts > 0)
ORDER BY ls.total_xp DESC, ls.books_read DESC
LIMIT $2;

-- name: GetGlobalLeaderboard :many
SELECT ls.*
FROM leaderboard_stats ls
INNER JOIN users u ON ls.user_id = u.id
WHERE u.show_on_leaderboard = true
  AND u.deleted_at IS NULL
  AND ls.total_xp > 0
ORDER BY ls.total_xp DESC, ls.books_read DESC
LIMIT $1;

-- name: GetLeaderboardByDimension :many
SELECT ls.*
FROM leaderboard_stats ls
INNER JOIN users u ON ls.user_id = u.id
WHERE u.show_on_leaderboard = true
  AND u.deleted_at IS NULL
ORDER BY
  CASE $1::text
    WHEN 'books' THEN ls.books_read
    WHEN 'pages' THEN ls.pages_read
    WHEN 'thoughts' THEN ls.thoughts
    WHEN 'genres' THEN ls.genres_explored
    WHEN 'streak' THEN ls.current_streak
    ELSE ls.total_xp
  END DESC,
  ls.total_xp DESC
LIMIT $2;
