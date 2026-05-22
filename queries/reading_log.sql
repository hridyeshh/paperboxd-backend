-- name: LogReadingProgress :exec
INSERT INTO reading_log (user_id, book_id, pages_delta)
VALUES ($1, $2, $3);

-- name: GetTodayReadingStats :one
SELECT
    COALESCE(SUM(pages_delta), 0)::INT AS total_pages,
    COUNT(DISTINCT book_id)::INT       AS total_books
FROM reading_log
WHERE user_id  = $1
  AND logged_at >= CURRENT_DATE;

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
  AND rl.logged_at >= CURRENT_DATE
ORDER BY rl.logged_at DESC
LIMIT 1;

-- name: GetWeeklyReadingStats :many
SELECT
    DATE(logged_at)                    AS log_date,
    COALESCE(SUM(pages_delta), 0)::INT AS pages,
    COUNT(DISTINCT book_id)::INT       AS books
FROM reading_log
WHERE user_id  = $1
  AND logged_at >= CURRENT_DATE - INTERVAL '6 days'
GROUP BY DATE(logged_at)
ORDER BY log_date;
