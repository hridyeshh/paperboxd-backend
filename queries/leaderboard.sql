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
  END,
  last_activity_date = CURRENT_DATE
WHERE id = $1;

-- name: GetUserXP :one
SELECT id, username, total_xp, level, current_streak, longest_streak, last_activity_date, show_on_leaderboard
FROM users
WHERE id = $1;

-- ============================================================================
-- STREAK MANAGEMENT
-- ============================================================================

-- name: UpdateUserStreak :exec
UPDATE users
SET
  current_streak = CASE
    -- Continued streak (activity yesterday)
    WHEN last_activity_date = CURRENT_DATE - INTERVAL '1 day' THEN current_streak + 1
    -- Same day activity (no change)
    WHEN last_activity_date = CURRENT_DATE THEN current_streak
    -- Broken streak (more than 1 day gap)
    ELSE 1
  END,
  longest_streak = GREATEST(
    longest_streak,
    CASE
      WHEN last_activity_date = CURRENT_DATE - INTERVAL '1 day' THEN current_streak + 1
      WHEN last_activity_date = CURRENT_DATE THEN current_streak
      ELSE 1
    END
  ),
  last_activity_date = CURRENT_DATE
WHERE id = $1;

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
  diary_entries,
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
  COALESCE((SELECT COUNT(*) FROM diary_entries WHERE user_id = u.id), 0)::INTEGER as diary_entries,
  COALESCE(array_length(u.favorite_genres, 1), 0) as genres_explored,
  u.total_xp,
  u.level,
  u.current_streak
FROM users u
WHERE u.id = $1
ON CONFLICT (user_id)
DO UPDATE SET
  username = EXCLUDED.username,
  books_read = EXCLUDED.books_read,
  pages_read = EXCLUDED.pages_read,
  diary_entries = EXCLUDED.diary_entries,
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
  diary_entries,
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
  COALESCE((SELECT COUNT(*) FROM diary_entries WHERE user_id = u.id), 0)::INTEGER,
  COALESCE(array_length(u.favorite_genres, 1), 0),
  u.total_xp,
  u.level,
  u.current_streak
FROM users u
ON CONFLICT (user_id)
DO UPDATE SET
  username = EXCLUDED.username,
  books_read = EXCLUDED.books_read,
  pages_read = EXCLUDED.pages_read,
  diary_entries = EXCLUDED.diary_entries,
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
    ROW_NUMBER() OVER (ORDER BY diary_entries DESC, total_xp DESC) as diary_rank,
    ROW_NUMBER() OVER (ORDER BY genres_explored DESC, total_xp DESC) as genres_rank,
    ROW_NUMBER() OVER (ORDER BY total_xp DESC, books_read DESC) as xp_rank,
    ROW_NUMBER() OVER (ORDER BY current_streak DESC, total_xp DESC) as streak_rank
  FROM leaderboard_stats
)
UPDATE leaderboard_stats ls
SET
  books_rank = r.books_rank::INTEGER,
  pages_rank = r.pages_rank::INTEGER,
  diary_rank = r.diary_rank::INTEGER,
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
WHERE (
    ls.user_id = $1
    OR ls.user_id IN (SELECT f.following_id FROM follows f WHERE f.follower_id = $1)
  )
  AND (ls.total_xp > 0 OR ls.books_read > 0 OR ls.diary_entries > 0)
ORDER BY ls.total_xp DESC, ls.books_read DESC
LIMIT $2;

-- name: GetGlobalLeaderboard :many
SELECT ls.*
FROM leaderboard_stats ls
INNER JOIN users u ON ls.user_id = u.id
WHERE u.show_on_leaderboard = true
  AND ls.total_xp > 0
ORDER BY ls.total_xp DESC, ls.books_read DESC
LIMIT $1;

-- name: GetLeaderboardByDimension :many
SELECT ls.*
FROM leaderboard_stats ls
INNER JOIN users u ON ls.user_id = u.id
WHERE u.show_on_leaderboard = true
ORDER BY
  CASE $1::text
    WHEN 'books' THEN ls.books_read
    WHEN 'pages' THEN ls.pages_read
    WHEN 'diary' THEN ls.diary_entries
    WHEN 'genres' THEN ls.genres_explored
    WHEN 'streak' THEN ls.current_streak
    ELSE ls.total_xp
  END DESC,
  ls.total_xp DESC
LIMIT $2;
