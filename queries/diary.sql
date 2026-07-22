-- name: CreateDiaryEntry :one
INSERT INTO diary_entries (user_id, book_id, title, content, is_private, rating)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetDiaryEntryByID :one
SELECT * FROM diary_entries
WHERE id = $1;

-- name: GetUserDiaryEntries :many
SELECT
    de.id,
    de.user_id,
    de.book_id,
    de.title,
    de.content,
    de.is_private,
    de.rating,
    de.created_at,
    de.updated_at,
    b.title as book_title,
    b.slug as book_slug,
    b.authors as book_authors,
    b.cover_url as book_cover_url
FROM diary_entries de
LEFT JOIN books b ON de.book_id = b.id
WHERE de.user_id = $1
  AND (de.is_private = false OR de.user_id = $2)
ORDER BY de.created_at DESC
LIMIT $3 OFFSET $4;

-- name: GetBookDiaryEntries :many
SELECT
    de.id,
    de.user_id,
    de.book_id,
    de.title,
    de.content,
    de.is_private,
    de.rating,
    de.created_at,
    de.updated_at,
    u.username,
    u.name,
    u.avatar_url
FROM diary_entries de
JOIN users u ON de.user_id = u.id
WHERE de.book_id = sqlc.arg(book_id) AND de.is_private = false
  AND NOT EXISTS (
      SELECT 1 FROM blocks bl
      WHERE (bl.blocker_id = sqlc.arg(viewer_id) AND bl.blocked_id = de.user_id)
         OR (bl.blocker_id = de.user_id AND bl.blocked_id = sqlc.arg(viewer_id))
  )
ORDER BY de.created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: UpdateDiaryEntry :one
UPDATE diary_entries
SET
    title = $2,
    content = $3,
    is_private = $4,
    rating = $5,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteDiaryEntry :exec
DELETE FROM diary_entries
WHERE id = $1;

-- name: CountUserDiaryEntries :one
SELECT COUNT(*) FROM diary_entries
WHERE user_id = $1;

-- name: IncrementUserDiaryCount :exec
UPDATE users
SET diary_entries_count = diary_entries_count + 1
WHERE id = $1;

-- name: DecrementUserDiaryCount :exec
UPDATE users
SET diary_entries_count = GREATEST(diary_entries_count - 1, 0)
WHERE id = $1;

-- Likes

-- name: LikeDiaryEntry :one
INSERT INTO diary_entry_likes (user_id, entry_id)
VALUES ($1, $2)
RETURNING *;

-- name: UnlikeDiaryEntry :exec
DELETE FROM diary_entry_likes
WHERE user_id = $1 AND entry_id = $2;

-- name: GetEntryLikes :many
SELECT
    del.id,
    del.user_id,
    del.entry_id,
    del.created_at,
    u.username,
    u.name,
    u.avatar_url
FROM diary_entry_likes del
JOIN users u ON del.user_id = u.id
WHERE del.entry_id = $1
ORDER BY del.created_at DESC;

-- name: CheckEntryLiked :one
SELECT EXISTS(
    SELECT 1 FROM diary_entry_likes
    WHERE user_id = $1 AND entry_id = $2
);

-- name: CountEntryLikes :one
SELECT COUNT(*) FROM diary_entry_likes
WHERE entry_id = $1;

-- name: CheckEntryOwnership :one
SELECT user_id FROM diary_entries
WHERE id = $1;

-- name: UpdateDiaryEntryEmbedding :exec
UPDATE diary_entries
SET embedding = $2::vector, embedding_text = $3
WHERE id = $1;

-- name: GetDiaryEmbeddingsForUser :many
SELECT id, embedding_text, embedding
FROM diary_entries
WHERE user_id = $1
  AND embedding IS NOT NULL
ORDER BY created_at DESC
LIMIT 50;

-- name: GetDiaryEntriesWithoutEmbedding :many
SELECT
    de.id,
    de.user_id,
    de.content,
    de.embedding_text,
    b.title   AS book_title,
    b.authors AS book_authors
FROM diary_entries de
LEFT JOIN books b ON de.book_id = b.id
WHERE de.embedding IS NULL
ORDER BY de.created_at DESC;
