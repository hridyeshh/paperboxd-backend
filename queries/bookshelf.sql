-- name: AddToBookshelf :one
INSERT INTO bookshelf (
    user_id, book_id, status, rating, started_at, finished_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT (user_id, book_id) DO UPDATE SET
    status = EXCLUDED.status,
    rating = EXCLUDED.rating,
    started_at = EXCLUDED.started_at,
    finished_at = EXCLUDED.finished_at,
    updated_at = NOW()
RETURNING *;

-- name: RemoveFromBookshelf :exec
DELETE FROM bookshelf WHERE user_id = $1 AND book_id = $2;

-- name: GetUserBookshelf :many
SELECT b.*, bs.status, bs.rating, bs.finished_at, bs.created_at as added_at
FROM bookshelf bs
JOIN books b ON bs.book_id = b.id
WHERE bs.user_id = $1 AND bs.status = $2
-- Most-recently-touched first: a freshly added/updated book (updated_at = NOW())
-- always lands at the top of its tab.
ORDER BY bs.updated_at DESC NULLS LAST, bs.created_at DESC
LIMIT $3 OFFSET $4;

-- name: GetUserReadStats :one
-- Live books-read count and total pages read, computed from the shelf so the
-- profile never shows the stale cached `users.books_read_count` / `total_pages_read`.
SELECT
    COUNT(*)::INT AS books_read,
    COALESCE(SUM(b.page_count) FILTER (WHERE b.page_count IS NOT NULL), 0)::INT AS pages_read
FROM bookshelf bs
JOIN books b ON bs.book_id = b.id
WHERE bs.user_id = $1 AND bs.status = 'read';

-- name: GetBookshelfEntry :one
SELECT * FROM bookshelf WHERE user_id = $1 AND book_id = $2;

-- name: CountUserBooks :one
SELECT COUNT(*) FROM bookshelf WHERE user_id = $1 AND status = $2;

-- name: UpdateTBRNotes :one
UPDATE bookshelf
SET
    tbr_notes = $3,
    tbr_priority = $4,
    tbr_added_at = COALESCE(tbr_added_at, NOW()),
    updated_at = NOW()
WHERE user_id = $1 AND book_id = $2
RETURNING *;

-- name: UpdateReadingProgress :one
UPDATE bookshelf
SET
    current_page = $3,
    reading_velocity = $4,
    estimated_finish_date = $5,
    updated_at = NOW()
WHERE user_id = $1 AND book_id = $2
RETURNING *;

-- name: MarkAsStarted :one
UPDATE bookshelf
SET
    status = 'reading',
    started_at = COALESCE(started_at, NOW()),
    current_page = COALESCE($3, 0),
    updated_at = NOW()
WHERE user_id = $1 AND book_id = $2
RETURNING *;

-- name: MarkAsFinished :one
UPDATE bookshelf
SET
    status = 'read',
    finished_at = NOW(),
    current_page = (SELECT b.page_count FROM books b WHERE b.id = bookshelf.book_id),
    updated_at = NOW()
WHERE user_id = $1 AND book_id = $2
RETURNING *;

-- name: GetCurrentlyReading :many
SELECT
    bs.id,
    bs.user_id,
    bs.book_id,
    bs.status,
    bs.rating,
    bs.started_at,
    bs.finished_at,
    bs.current_page,
    bs.reading_velocity,
    bs.estimated_finish_date,
    bs.created_at,
    bs.updated_at,
    b.title,
    b.slug,
    b.authors,
    b.cover_url,
    b.page_count,
    b.published_date,
    b.isbn_13,
    b.description,
    b.categories,
    b.publisher,
    b.language,
    b.subtitle,
    b.isbndb_id,
    b.google_books_id,
    b.average_rating,
    b.ratings_count
FROM bookshelf bs
JOIN books b ON bs.book_id = b.id
WHERE bs.user_id = $1 AND bs.status = 'reading'
ORDER BY bs.updated_at DESC;

-- name: GetUserTBR :many
SELECT
    bs.id,
    bs.user_id,
    bs.book_id,
    bs.status,
    bs.current_page,
    bs.tbr_notes,
    bs.tbr_priority,
    bs.tbr_added_at,
    bs.created_at,
    bs.updated_at,
    b.title,
    b.slug,
    b.authors,
    b.cover_url,
    b.page_count,
    b.published_date,
    b.isbn_13,
    b.description,
    b.categories,
    b.publisher,
    b.language,
    b.subtitle,
    b.isbndb_id,
    b.google_books_id,
    b.average_rating,
    b.ratings_count
FROM bookshelf bs
JOIN books b ON bs.book_id = b.id
WHERE bs.user_id = $1 AND bs.status = 'to-read'
-- Most-recently-touched first so a book just marked to-read lands on top.
ORDER BY bs.updated_at DESC NULLS LAST, bs.created_at DESC;

-- name: GetUserDNF :many
SELECT
    bs.id,
    bs.user_id,
    bs.book_id,
    bs.status,
    bs.current_page,
    bs.tbr_notes,
    bs.tbr_priority,
    bs.tbr_added_at,
    bs.created_at,
    bs.updated_at,
    b.title,
    b.slug,
    b.authors,
    b.cover_url,
    b.page_count,
    b.published_date,
    b.isbn_13,
    b.description,
    b.categories,
    b.publisher,
    b.language,
    b.subtitle,
    b.isbndb_id,
    b.google_books_id,
    b.average_rating,
    b.ratings_count
FROM bookshelf bs
JOIN books b ON bs.book_id = b.id
WHERE bs.user_id = $1
  AND bs.status = 'to-read'
  AND bs.current_page IS NOT NULL
  AND bs.current_page > 0
ORDER BY bs.updated_at DESC;

-- name: GetUserAuthors :many
SELECT
    author::TEXT AS name,
    COUNT(*)::INT AS book_count,
    COALESCE(MAX(b.cover_url), '')::TEXT AS sample_cover
FROM bookshelf bs
JOIN books b ON bs.book_id = b.id
CROSS JOIN UNNEST(b.authors) AS author
WHERE bs.user_id = $1
  AND bs.status = 'read'
GROUP BY author
ORDER BY book_count DESC, name ASC;

-- name: UpdateBookshelfStatus :one
UPDATE bookshelf
SET
    status = $3,
    updated_at = NOW()
WHERE user_id = $1 AND book_id = $2
RETURNING *;

-- name: UpdateBookshelfRating :one
UPDATE bookshelf
SET
    rating = $3,
    review = $4,
    reviewed_at = CASE
        WHEN $3::int IS NULL AND ($4::text IS NULL OR $4::text = '')
            THEN NULL
        WHEN reviewed_at IS NULL AND ($3::int IS NOT NULL OR ($4::text IS NOT NULL AND $4::text != ''))
            THEN NOW()
        ELSE reviewed_at
    END,
    review_edited = CASE
        WHEN reviewed_at IS NOT NULL AND ($3::int IS NOT NULL OR ($4::text IS NOT NULL AND $4::text != ''))
            THEN TRUE
        WHEN $3::int IS NULL AND ($4::text IS NULL OR $4::text = '')
            THEN FALSE
        ELSE review_edited
    END,
    updated_at = NOW()
WHERE user_id = $1 AND book_id = $2
RETURNING *;

-- name: GetBookReviews :many
SELECT
    bs.user_id,
    bs.rating,
    bs.review,
    bs.reviewed_at,
    bs.review_edited,
    u.username,
    u.avatar_url
FROM bookshelf bs
JOIN users u ON bs.user_id = u.id
WHERE bs.book_id = $1
  AND (bs.rating IS NOT NULL OR (bs.review IS NOT NULL AND bs.review != ''))
ORDER BY bs.reviewed_at DESC NULLS LAST
LIMIT 50;

-- name: GetBookReviewsByFriends :many
-- Reviews on a book authored by users the current viewer follows.
-- $1 = book_id, $2 = viewer's user_id (follower).
SELECT
    bs.user_id,
    bs.rating,
    bs.review,
    bs.reviewed_at,
    bs.review_edited,
    u.username,
    u.name,
    u.avatar_url
FROM bookshelf bs
JOIN users u ON bs.user_id = u.id
JOIN follows f ON f.following_id = bs.user_id
WHERE bs.book_id = $1
  AND f.follower_id = $2
  AND (bs.rating IS NOT NULL OR (bs.review IS NOT NULL AND bs.review != ''))
ORDER BY bs.reviewed_at DESC NULLS LAST
LIMIT 20;

-- name: GetFriendsReadingBook :many
-- Friends (people the viewer follows) who have this book on their shelf
-- with status 'reading' or 'to-read', prioritised by active readers first.
-- $1 = viewer's user_id (follower), $2 = book_id.
SELECT
    u.id AS user_id,
    u.username,
    u.name,
    u.avatar_url,
    bs.current_page,
    bs.started_at,
    bs.status,
    bs.updated_at
FROM follows f
JOIN bookshelf bs ON bs.user_id = f.following_id
JOIN users u ON u.id = f.following_id
WHERE f.follower_id = $1
  AND bs.book_id = $2
  AND bs.status IN ('reading', 'to-read', 'read')
ORDER BY
    CASE bs.status
        WHEN 'reading' THEN 0
        WHEN 'read' THEN 1
        ELSE 2
    END,
    bs.updated_at DESC
LIMIT 20;

-- name: CountFriendsReadingBook :one
-- Counts friends actively reading this book right now.
SELECT COUNT(*) FROM follows f
JOIN bookshelf bs ON bs.user_id = f.following_id
WHERE f.follower_id = $1
  AND bs.book_id = $2
  AND bs.status = 'reading';

-- name: GetReadingProgress :one
-- Reading progress snapshot for a book on a single user's shelf.
SELECT
    bs.current_page,
    bs.reading_velocity,
    bs.estimated_finish_date,
    bs.started_at,
    bs.finished_at,
    bs.status,
    bs.updated_at,
    b.page_count AS total_pages
FROM bookshelf bs
JOIN books b ON b.id = bs.book_id
WHERE bs.user_id = $1 AND bs.book_id = $2;
