-- Recompute every reader's streak from their XP history.
--
-- AddXP used to set users.last_activity_date = CURRENT_DATE before AwardXP
-- called UpdateUserStreak, so UpdateUserStreak always saw "already active
-- today" and current_streak never advanced. xp_transactions logs every XP
-- action (daily opens included), so its UTC days are the activity days the
-- streak should have counted. created_at is UTC: the pool pins the session
-- TimeZone (cmd/api/main.go).
--
-- Only the reader's own actions count: XP received for what someone else did,
-- or as a bonus, is excluded (keep in sync with notActivity in xp_service.go).
WITH days AS (
  SELECT DISTINCT user_id, created_at::date AS d
  FROM xp_transactions
  WHERE action_type NOT IN (
    'thought_liked', 'follow_gained',
    'referral_signup', 'referral_book', 'referral_30day',
    'streak_7', 'streak_30', 'streak_100', 'streak_365'
  )
),
runs AS (
  -- Consecutive days share d - row_number (gaps-and-islands).
  SELECT user_id, d, d - ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY d)::int AS grp
  FROM days
),
islands AS (
  SELECT user_id, MAX(d) AS last_day, COUNT(*)::int AS len
  FROM runs
  GROUP BY user_id, grp
),
per_user AS (
  SELECT DISTINCT ON (user_id)
    user_id, last_day, len AS latest_len,
    MAX(len) OVER (PARTITION BY user_id) AS longest
  FROM islands
  ORDER BY user_id, last_day DESC
)
UPDATE users u
SET
  current_streak     = p.latest_len,
  longest_streak     = GREATEST(COALESCE(u.longest_streak, 0), p.longest),
  last_activity_date = p.last_day
FROM per_user p
WHERE u.id = p.user_id;

-- Readers with no activity of their own have no streak.
UPDATE users u
SET current_streak = 0
WHERE NOT EXISTS (
  SELECT 1 FROM xp_transactions x
  WHERE x.user_id = u.id
    AND x.action_type NOT IN (
      'thought_liked', 'follow_gained',
      'referral_signup', 'referral_book', 'referral_30day',
      'streak_7', 'streak_30', 'streak_100', 'streak_365'
    )
);

-- leaderboard_stats copies current_streak; the nightly rebuild refreshes it.
