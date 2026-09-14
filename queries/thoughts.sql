-- name: CreateThought :one
INSERT INTO thoughts (user_id, book_id, title, content, is_private, rating, thread_root_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetThoughtByID :one
SELECT * FROM thoughts
WHERE id = $1;

-- name: GetUserThoughtsFeed :many
-- The profile's Thoughts tab: the owner's thoughts that start a thread (or
-- stand alone) interleaved with thoughts they reposted, newest activity first.
-- A repost is re-checked against the original author on every read, so a
-- thought that went private, an author who went private, or a block made after
-- the repost all hide it without touching thought_reposts.
WITH items AS (
    SELECT t.id AS thought_id, false AS is_repost, t.created_at AS sort_at
    FROM thoughts t
    WHERE t.user_id = sqlc.arg(profile_id)
      AND t.thread_root_id IS NULL
      AND (t.is_private = false OR t.user_id = sqlc.arg(viewer_id))
    UNION ALL
    SELECT r.thought_id, true, r.created_at
    FROM thought_reposts r
    WHERE r.user_id = sqlc.arg(profile_id)
)
SELECT
    t.id,
    t.user_id,
    t.book_id,
    t.title,
    t.content,
    t.is_private,
    t.rating,
    t.thread_root_id,
    t.created_at,
    t.updated_at,
    i.is_repost::bool AS is_repost,
    u.username,
    u.name,
    u.avatar_url,
    b.title     AS book_title,
    b.slug      AS book_slug,
    b.authors   AS book_authors,
    b.cover_url AS book_cover_url,
    (SELECT COUNT(*) FROM thought_likes l   WHERE l.thought_id = t.id)       AS likes_count,
    (SELECT COUNT(*) FROM thought_reposts rp WHERE rp.thought_id = t.id)     AS reposts_count,
    (SELECT COUNT(*) FROM thoughts c        WHERE c.thread_root_id = t.id)   AS thread_count,
    EXISTS(SELECT 1 FROM thought_likes l    WHERE l.thought_id = t.id  AND l.user_id  = sqlc.arg(viewer_id)) AS is_liked,
    EXISTS(SELECT 1 FROM thought_reposts rp WHERE rp.thought_id = t.id AND rp.user_id = sqlc.arg(viewer_id)) AS is_reposted
FROM items i
JOIN thoughts t ON t.id = i.thought_id
JOIN users u ON u.id = t.user_id
LEFT JOIN books b ON b.id = t.book_id
WHERE i.is_repost = false
   OR (
        t.is_private = false
        AND u.deleted_at IS NULL
        AND (
            u.is_public
            OR u.id = sqlc.arg(viewer_id)
            OR EXISTS (SELECT 1 FROM follows f WHERE f.follower_id = sqlc.arg(viewer_id) AND f.following_id = u.id)
        )
        AND NOT EXISTS (
            SELECT 1 FROM blocks bl
            WHERE (bl.blocker_id = sqlc.arg(viewer_id) AND bl.blocked_id = u.id)
               OR (bl.blocker_id = u.id AND bl.blocked_id = sqlc.arg(viewer_id))
        )
   )
ORDER BY i.sort_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountUserThoughtsFeed :one
-- ponytail: counts reposts before the read-time visibility filter, so a page
-- count can run one short page long; filter here too if that ever shows.
SELECT
    (SELECT COUNT(*) FROM thoughts t
      WHERE t.user_id = sqlc.arg(profile_id)
        AND t.thread_root_id IS NULL
        AND (t.is_private = false OR t.user_id = sqlc.arg(viewer_id)))
  + (SELECT COUNT(*) FROM thought_reposts r WHERE r.user_id = sqlc.arg(profile_id));

-- name: GetThoughtThread :many
-- The first thought and every follow-up, in writing order.
SELECT
    t.id,
    t.user_id,
    t.book_id,
    t.title,
    t.content,
    t.is_private,
    t.rating,
    t.thread_root_id,
    t.created_at,
    t.updated_at,
    u.username,
    u.name,
    u.avatar_url,
    b.title     AS book_title,
    b.slug      AS book_slug,
    b.authors   AS book_authors,
    b.cover_url AS book_cover_url,
    (SELECT COUNT(*) FROM thought_likes l   WHERE l.thought_id = t.id)   AS likes_count,
    (SELECT COUNT(*) FROM thought_reposts rp WHERE rp.thought_id = t.id) AS reposts_count,
    EXISTS(SELECT 1 FROM thought_likes l    WHERE l.thought_id = t.id  AND l.user_id  = sqlc.arg(viewer_id)) AS is_liked,
    EXISTS(SELECT 1 FROM thought_reposts rp WHERE rp.thought_id = t.id AND rp.user_id = sqlc.arg(viewer_id)) AS is_reposted
FROM thoughts t
JOIN users u ON u.id = t.user_id
LEFT JOIN books b ON b.id = t.book_id
WHERE t.id = sqlc.arg(root_id) OR t.thread_root_id = sqlc.arg(root_id)
ORDER BY t.thread_root_id IS NOT NULL, t.created_at;

-- name: GetBookThoughts :many
-- Follow-ups are left out: they belong to their thread, not to the book page.
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
FROM thoughts de
JOIN users u ON de.user_id = u.id
WHERE de.book_id = sqlc.arg(book_id) AND de.is_private = false
  AND de.thread_root_id IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM blocks bl
      WHERE (bl.blocker_id = sqlc.arg(viewer_id) AND bl.blocked_id = de.user_id)
         OR (bl.blocker_id = de.user_id AND bl.blocked_id = sqlc.arg(viewer_id))
  )
ORDER BY de.created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: UpdateThought :one
UPDATE thoughts
SET
    title = $2,
    content = $3,
    is_private = $4,
    rating = $5,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: SetThreadPrivacy :exec
-- A thread is private or public as a whole. Going private also retracts the
-- follow-ups' embeddings, for the same reason the single-thought path does.
UPDATE thoughts
SET is_private     = sqlc.arg(is_private)::bool,
    embedding      = CASE WHEN sqlc.arg(is_private)::bool THEN NULL ELSE embedding END,
    embedding_text = CASE WHEN sqlc.arg(is_private)::bool THEN NULL ELSE embedding_text END,
    updated_at     = NOW()
WHERE thread_root_id = sqlc.arg(root_id);

-- name: DeleteThought :exec
DELETE FROM thoughts
WHERE id = $1;

-- name: CountUserThoughts :one
-- Thoughts that start a thread or stand alone; follow-ups are part of those.
SELECT COUNT(*) FROM thoughts
WHERE user_id = $1 AND thread_root_id IS NULL;

-- name: IncrementUserThoughtCount :exec
UPDATE users
SET thoughts_count = thoughts_count + 1
WHERE id = $1;

-- name: DecrementUserThoughtCount :exec
UPDATE users
SET thoughts_count = GREATEST(thoughts_count - 1, 0)
WHERE id = $1;

-- Likes

-- name: LikeThought :one
INSERT INTO thought_likes (user_id, thought_id)
VALUES ($1, $2)
RETURNING *;

-- name: UnlikeThought :exec
DELETE FROM thought_likes
WHERE user_id = $1 AND thought_id = $2;

-- name: GetThoughtLikes :many
SELECT
    tl.id,
    tl.user_id,
    tl.thought_id,
    tl.created_at,
    u.username,
    u.name,
    u.avatar_url
FROM thought_likes tl
JOIN users u ON tl.user_id = u.id
WHERE tl.thought_id = $1
ORDER BY tl.created_at DESC;

-- name: CheckThoughtLiked :one
SELECT EXISTS(
    SELECT 1 FROM thought_likes
    WHERE user_id = $1 AND thought_id = $2
);

-- name: CountThoughtLikes :one
SELECT COUNT(*) FROM thought_likes
WHERE thought_id = $1;

-- name: CheckThoughtOwnership :one
SELECT user_id FROM thoughts
WHERE id = $1;

-- Reposts

-- name: RepostThought :execrows
INSERT INTO thought_reposts (user_id, thought_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: UnrepostThought :execrows
DELETE FROM thought_reposts
WHERE user_id = $1 AND thought_id = $2;

-- name: CheckThoughtReposted :one
SELECT EXISTS(
    SELECT 1 FROM thought_reposts
    WHERE user_id = $1 AND thought_id = $2
);

-- name: CountThoughtReposts :one
SELECT COUNT(*) FROM thought_reposts
WHERE thought_id = $1;

-- name: CountThreadFollowUps :one
SELECT COUNT(*) FROM thoughts
WHERE thread_root_id = $1;

-- name: DeleteRepostActivity :exec
-- Undoing a repost takes back the "reposted your thought" notification too.
DELETE FROM activities
WHERE user_id = $1 AND thought_id = $2 AND activity_type = 'reposted_thought';

-- Embeddings

-- name: UpdateThoughtEmbedding :exec
UPDATE thoughts
SET embedding = $2::vector, embedding_text = $3
WHERE id = $1;

-- name: GetThoughtEmbeddingsForUser :many
SELECT id, embedding_text, embedding
FROM thoughts
WHERE user_id = $1
  AND embedding IS NOT NULL
ORDER BY created_at DESC
LIMIT 50;

-- name: GetThoughtsWithoutEmbedding :many
SELECT
    de.id,
    de.user_id,
    de.content,
    de.embedding_text,
    b.title   AS book_title,
    b.authors AS book_authors
FROM thoughts de
LEFT JOIN books b ON de.book_id = b.id
WHERE de.embedding IS NULL
ORDER BY de.created_at DESC;
