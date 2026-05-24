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
ORDER BY bs.finished_at DESC NULLS LAST, bs.created_at DESC
LIMIT $3 OFFSET $4;

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
ORDER BY bs.tbr_added_at DESC NULLS LAST, bs.created_at DESC;

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
