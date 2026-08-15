-- Monthly Wrapped aggregates.
--
-- Every query is bounded by an explicit [month_start, month_end) window the
-- handler computes in the reader's own timezone, and buckets days/hours in
-- that same zone: a book read at 1am in Delhi belongs to the Delhi night, not
-- to the previous UTC day.
--
-- pages_delta can be negative (a reader correcting a page count downward), so
-- every page total clamps with GREATEST(pages_delta, 0) — a correction is not
-- reading.

-- name: WrappedTotals :one
WITH logs AS (
    SELECT
        (logged_at AT TIME ZONE sqlc.arg(tz)::TEXT)::DATE AS d,
        GREATEST(pages_delta, 0) AS pages
    FROM reading_log
    WHERE user_id = sqlc.arg(user_id)
      AND logged_at >= sqlc.arg(month_start)
      AND logged_at <  sqlc.arg(month_end)
),
days AS (
    SELECT d, SUM(pages)::INT AS pages
    FROM logs
    GROUP BY d
)
SELECT
    COALESCE((SELECT SUM(pages) FROM logs), 0)::INT                  AS pages,
    COALESCE((SELECT COUNT(*)   FROM logs), 0)::INT                  AS sessions,
    COALESCE((SELECT COUNT(*)   FROM days WHERE pages > 0), 0)::INT  AS active_days,
    COALESCE((SELECT MAX(pages) FROM days), 0)::INT                  AS biggest_day_pages,
    (SELECT d FROM days ORDER BY pages DESC, d ASC LIMIT 1)          AS biggest_day;

-- name: WrappedBooksFinished :one
SELECT COUNT(*)::INT AS books
FROM bookshelf
WHERE user_id = sqlc.arg(user_id)
  AND status = 'read'
  AND finished_at >= sqlc.arg(month_start)
  AND finished_at <  sqlc.arg(month_end);

-- name: WrappedTopBooks :many
SELECT
    b.id,
    b.title,
    b.authors,
    b.cover_url,
    b.page_count,
    bs.rating,
    SUM(GREATEST(rl.pages_delta, 0))::INT                                            AS pages,
    COUNT(DISTINCT (rl.logged_at AT TIME ZONE sqlc.arg(tz)::TEXT)::DATE)::INT        AS days
FROM reading_log rl
JOIN books b ON b.id = rl.book_id
LEFT JOIN bookshelf bs ON bs.user_id = rl.user_id AND bs.book_id = rl.book_id
WHERE rl.user_id = sqlc.arg(user_id)
  AND rl.logged_at >= sqlc.arg(month_start)
  AND rl.logged_at <  sqlc.arg(month_end)
GROUP BY b.id, bs.rating
HAVING SUM(GREATEST(rl.pages_delta, 0)) > 0
ORDER BY pages DESC, b.title ASC
LIMIT sqlc.arg(row_limit);

-- name: WrappedTopAuthors :many
-- Pages are attributed to every credited author of a book; a co-authored book
-- counts once for each of them, which is how the reader thinks about it.
WITH per_book AS (
    SELECT
        rl.book_id,
        b.authors,
        SUM(GREATEST(rl.pages_delta, 0))::INT AS pages
    FROM reading_log rl
    JOIN books b ON b.id = rl.book_id
    WHERE rl.user_id = sqlc.arg(user_id)
      AND rl.logged_at >= sqlc.arg(month_start)
      AND rl.logged_at <  sqlc.arg(month_end)
    GROUP BY rl.book_id, b.authors
    HAVING SUM(GREATEST(rl.pages_delta, 0)) > 0
)
SELECT
    a.author::TEXT                     AS name,
    COUNT(DISTINCT pb.book_id)::INT    AS books,
    SUM(pb.pages)::INT                 AS pages
FROM per_book pb, unnest(pb.authors) AS a(author)
WHERE a.author IS NOT NULL AND a.author <> ''
GROUP BY a.author
ORDER BY pages DESC, name ASC
LIMIT sqlc.arg(row_limit);

-- name: WrappedGenres :many
-- Categories weighted by pages read, not by book count: a 900-page epic should
-- outweigh a novella. The handler turns these into percentages.
WITH per_book AS (
    SELECT
        rl.book_id,
        b.categories,
        SUM(GREATEST(rl.pages_delta, 0))::INT AS pages
    FROM reading_log rl
    JOIN books b ON b.id = rl.book_id
    WHERE rl.user_id = sqlc.arg(user_id)
      AND rl.logged_at >= sqlc.arg(month_start)
      AND rl.logged_at <  sqlc.arg(month_end)
    GROUP BY rl.book_id, b.categories
    HAVING SUM(GREATEST(rl.pages_delta, 0)) > 0
)
SELECT
    c.category::TEXT   AS name,
    SUM(pb.pages)::INT AS pages
FROM per_book pb, unnest(pb.categories) AS c(category)
WHERE c.category IS NOT NULL AND c.category <> ''
GROUP BY c.category
ORDER BY pages DESC, name ASC
LIMIT sqlc.arg(row_limit);

-- name: WrappedHourHistogram :many
SELECT
    EXTRACT(HOUR FROM (logged_at AT TIME ZONE sqlc.arg(tz)::TEXT))::INT AS hour,
    SUM(GREATEST(pages_delta, 0))::INT                                  AS pages
FROM reading_log
WHERE user_id = sqlc.arg(user_id)
  AND logged_at >= sqlc.arg(month_start)
  AND logged_at <  sqlc.arg(month_end)
GROUP BY 1
ORDER BY 1;

-- name: WrappedDailyPages :many
-- Only days with entries come back; the handler fills the rest of the month
-- with zeros so the calendar grid is always month-length.
SELECT
    (logged_at AT TIME ZONE sqlc.arg(tz)::TEXT)::DATE AS log_date,
    SUM(GREATEST(pages_delta, 0))::INT                AS pages
FROM reading_log
WHERE user_id = sqlc.arg(user_id)
  AND logged_at >= sqlc.arg(month_start)
  AND logged_at <  sqlc.arg(month_end)
GROUP BY 1
ORDER BY 1;

-- name: WrappedTopRated :one
-- The month's best book: highest rating finished inside the window, most
-- recent finish breaking ties. Its review is the chapter's pull quote.
SELECT
    b.title,
    b.authors,
    b.cover_url,
    bs.rating,
    bs.review,
    bs.finished_at
FROM bookshelf bs
JOIN books b ON b.id = bs.book_id
WHERE bs.user_id = sqlc.arg(user_id)
  AND bs.status = 'read'
  AND bs.rating IS NOT NULL
  AND bs.finished_at >= sqlc.arg(month_start)
  AND bs.finished_at <  sqlc.arg(month_end)
ORDER BY bs.rating DESC, bs.finished_at DESC
LIMIT 1;

-- name: WrappedAbandoned :one
-- Still marked "reading", started before the month ended, and untouched since
-- stall_before. Ordered by how little of it was read — the most abandoned one
-- makes the best chapter.
SELECT
    b.title,
    b.authors,
    b.page_count,
    bs.current_page,
    bs.started_at,
    MAX(rl.logged_at)::TIMESTAMPTZ AS last_logged
FROM bookshelf bs
JOIN books b ON b.id = bs.book_id
LEFT JOIN reading_log rl
       ON rl.user_id = bs.user_id
      AND rl.book_id = bs.book_id
      AND rl.logged_at < sqlc.arg(month_end)
WHERE bs.user_id = sqlc.arg(user_id)
  AND bs.status = 'reading'
  AND COALESCE(bs.current_page, 0) > 0
  AND bs.started_at IS NOT NULL
  AND bs.started_at < sqlc.arg(month_end)
GROUP BY b.id, bs.current_page, bs.started_at
HAVING COALESCE(MAX(rl.logged_at), bs.started_at) < sqlc.arg(stall_before)
ORDER BY (COALESCE(bs.current_page, 0)::FLOAT / NULLIF(b.page_count, 0)) ASC NULLS LAST
LIMIT 1;

-- name: WrappedRank :one
-- Where the reader landed against everyone who read anything that month.
WITH totals AS (
    SELECT user_id, SUM(GREATEST(pages_delta, 0))::INT AS pages
    FROM reading_log
    WHERE logged_at >= sqlc.arg(month_start)
      AND logged_at <  sqlc.arg(month_end)
    GROUP BY user_id
    HAVING SUM(GREATEST(pages_delta, 0)) > 0
)
SELECT
    COALESCE((SELECT COUNT(*) FROM totals), 0)::INT AS readers,
    COALESCE((
        SELECT COUNT(*) FROM totals
        WHERE pages < (SELECT pages FROM totals WHERE user_id = sqlc.arg(user_id))
    ), 0)::INT AS beaten;
