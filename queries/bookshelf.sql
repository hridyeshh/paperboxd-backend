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
