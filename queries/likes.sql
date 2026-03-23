-- name: LikeBook :one
INSERT INTO likes (user_id, book_id)
VALUES ($1, $2)
ON CONFLICT (user_id, book_id) DO NOTHING
RETURNING *;

-- name: UnlikeBook :exec
DELETE FROM likes WHERE user_id = $1 AND book_id = $2;

-- name: GetUserLikes :many
SELECT b.*, l.created_at as liked_at
FROM likes l
JOIN books b ON l.book_id = b.id
WHERE l.user_id = $1
ORDER BY l.created_at DESC
LIMIT $2 OFFSET $3;

-- name: CheckUserLikedBook :one
SELECT EXISTS(SELECT 1 FROM likes WHERE user_id = $1 AND book_id = $2);
