-- name: LogReadingProgress :exec
INSERT INTO reading_log (user_id, book_id, pages_delta)
VALUES ($1, $2, $3);

-- name: GetTodayReadingStats :one
SELECT
    COALESCE(SUM(pages_delta), 0)::INT AS total_pages,
    COUNT(DISTINCT book_id)::INT       AS total_books
FROM reading_log
WHERE user_id  = $1
  AND (logged_at AT TIME ZONE 'UTC')::DATE = (NOW() AT TIME ZONE 'UTC')::DATE;

-- name: GetLastLoggedBookToday :one
SELECT
    rl.book_id,
    b.title,
    b.slug,
    b.authors,
    b.cover_url,
    b.page_count,
    bs.current_page,
    rl.logged_at
FROM reading_log rl
JOIN books     b  ON b.id  = rl.book_id
JOIN bookshelf bs ON bs.user_id = rl.user_id AND bs.book_id = rl.book_id
WHERE rl.user_id  = $1
  AND (rl.logged_at AT TIME ZONE 'UTC')::DATE = (NOW() AT TIME ZONE 'UTC')::DATE
ORDER BY rl.logged_at DESC
LIMIT 1;

-- name: GetLastLoggedBook :one
SELECT
    rl.book_id,
    b.title,
    b.slug,
    b.authors,
    b.cover_url,
    b.page_count,
    bs.current_page,
    rl.logged_at
FROM reading_log rl
JOIN books     b  ON b.id  = rl.book_id
JOIN bookshelf bs ON bs.user_id = rl.user_id AND bs.book_id = rl.book_id
WHERE rl.user_id  = $1
ORDER BY rl.logged_at DESC
LIMIT 1;

-- name: GetCurrentStreak :one
-- A streak day is a UTC day whose NET pages read is positive. Correcting a page
-- count downward (an error fix, net <= 0 for the day) is not reading progress, so
-- it neither starts nor extends a streak.
WITH days AS (
    SELECT (logged_at AT TIME ZONE 'UTC')::DATE AS d
    FROM reading_log
    WHERE user_id = $1
    GROUP BY 1
    HAVING SUM(pages_delta) > 0
),
grouped AS (
    SELECT d, d - (ROW_NUMBER() OVER (ORDER BY d))::INT AS island
    FROM days
),
islands AS (
    SELECT island, MAX(d) AS last_day, COUNT(*) AS len
    FROM grouped
    GROUP BY island
)
SELECT COALESCE((
    SELECT len
    FROM islands
    WHERE last_day >= (NOW() AT TIME ZONE 'UTC')::DATE - INTERVAL '1 day'
    ORDER BY last_day DESC
    LIMIT 1
), 0)::INT AS streak;

-- name: GetWeeklyReadingStats :many
SELECT
    (logged_at AT TIME ZONE 'UTC')::DATE AS log_date,
    COALESCE(SUM(pages_delta), 0)::INT   AS pages,
    COUNT(DISTINCT book_id)::INT         AS books
FROM reading_log
WHERE user_id  = $1
  AND (logged_at AT TIME ZONE 'UTC')::DATE >= (NOW() AT TIME ZONE 'UTC')::DATE - INTERVAL '6 days'
GROUP BY (logged_at AT TIME ZONE 'UTC')::DATE
ORDER BY log_date;

-- name: GetReadingActivityRange :many
-- Per-day page totals across an inclusive date range, for the GitHub-style
-- reading heatmap. Only days with at least one logged entry are returned; the
-- handler fills the gaps with zeros.
SELECT
    (logged_at AT TIME ZONE 'UTC')::DATE AS log_date,
    COALESCE(SUM(pages_delta), 0)::INT   AS pages,
    COUNT(DISTINCT book_id)::INT         AS books
FROM reading_log
WHERE user_id  = $1
  AND (logged_at AT TIME ZONE 'UTC')::DATE >= sqlc.arg(start_date)::DATE
  AND (logged_at AT TIME ZONE 'UTC')::DATE <= sqlc.arg(end_date)::DATE
GROUP BY (logged_at AT TIME ZONE 'UTC')::DATE
ORDER BY log_date;
