-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserByUsername :one
SELECT * FROM users
WHERE username = $1 AND deleted_at IS NULL;

-- name: GetUserByEmail :one
SELECT * FROM users
WHERE email = $1 AND deleted_at IS NULL;

-- name: CreateUser :one
INSERT INTO users (
    username, email, password_hash, name, favorite_genres
) VALUES (
    $1, $2, $3, $4, $5
)
RETURNING *;

-- name: GetUserByAppleUserID :one
SELECT * FROM users
WHERE apple_user_id = $1 AND deleted_at IS NULL;

-- name: LinkAppleUserID :exec
UPDATE users
SET apple_user_id = $2, updated_at = NOW()
WHERE id = $1;

-- name: UpdateUserLastActive :exec
UPDATE users
SET last_active = NOW()
WHERE id = $1;

-- name: UpdateUser :one
UPDATE users SET
    name = COALESCE($2, name),
    bio = COALESCE($3, bio),
    pronouns = COALESCE($4, pronouns),
    avatar_url = COALESCE($5, avatar_url),
    birthday = COALESCE($6, birthday),
    gender = COALESCE($7, gender),
    links = COALESCE($8, links),
    banner_url = COALESCE($9, banner_url),
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateUserGenres :one
UPDATE users SET favorite_genres = $2, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpsertAuthorRead :one
INSERT INTO user_authors_read (user_id, author_name)
VALUES ($1, $2)
ON CONFLICT (user_id, author_name) DO UPDATE SET author_name = EXCLUDED.author_name
RETURNING *;

-- name: UpdateUsername :one
UPDATE users SET username = $2, onboarding_completed = true, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: SearchUsers :many
SELECT * FROM users
WHERE (username ILIKE '%' || $1 || '%'
   OR name ILIKE '%' || $1 || '%')
   AND deleted_at IS NULL
ORDER BY followers_count DESC
LIMIT $2 OFFSET $3;

-- name: SoftDeleteUser :exec
-- Soft-delete the user and free their email/username so they (or anyone) can
-- re-register with the same identifiers. The original values are preserved in
-- the account_deletions audit table by RecordAccountDeletion (called first).
-- The UUID-based placeholders are deterministic and lowercase, satisfying the
-- column-level UNIQUE constraints and the username_lowercase CHECK.
UPDATE users
SET deleted_at = NOW(),
    updated_at = NOW(),
    email      = 'd_' || REPLACE(id::text, '-', '') || '@deleted.local',
    username   = 'd_' || REPLACE(id::text, '-', '')
WHERE id = $1 AND deleted_at IS NULL;

-- name: RecordAccountDeletion :exec
-- email_hash, not the address: this row outlives the 30-day hard purge, so a
-- cleartext address here would outlive the account forever. See migration 41.
INSERT INTO account_deletions (
    user_id, email_hash, reasons
) VALUES (
    $1, $2, $3
);

-- name: SetUserVisibility :one
UPDATE users SET is_public = $2, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: SuggestedUsers :many
-- Candidate readers to follow: public, not self, not already followed, not
-- blocked either way, and with something on their shelf so the feed they
-- produce is non-empty. Ordered by favourite-genre overlap, then live read
-- count (the cached users.books_read_count drifts), then followers.
--
-- The handler over-fetches here and re-ranks with SharedReadCounts and
-- MutualFollowCounts, which carry the stronger "you both read X" and "followed
-- by people you follow" signals. Those cannot live in this query: sqlc's
-- analyser cannot resolve the same table aliased twice inside a subquery.
SELECT
    u.id,
    u.username,
    u.name,
    u.avatar_url,
    u.bio,
    u.followers_count,
    (SELECT COUNT(*) FROM bookshelf bs WHERE bs.user_id = u.id AND bs.status = 'read')::int AS books_read_count,
    ARRAY(SELECT g FROM unnest(u.favorite_genres) AS g WHERE g = ANY(sqlc.arg(viewer_genres)::text[]))::text[] AS shared_genres
FROM users u
WHERE u.deleted_at IS NULL
  AND u.is_public = true
  AND u.id <> sqlc.arg(viewer_id)
  AND NOT EXISTS (
      SELECT 1 FROM follows f
      WHERE f.follower_id = sqlc.arg(viewer_id) AND f.following_id = u.id
  )
  AND NOT EXISTS (
      SELECT 1 FROM blocks b
      WHERE (b.blocker_id = sqlc.arg(viewer_id) AND b.blocked_id = u.id)
         OR (b.blocker_id = u.id AND b.blocked_id = sqlc.arg(viewer_id))
  )
  AND EXISTS (SELECT 1 FROM bookshelf bs WHERE bs.user_id = u.id)
ORDER BY
    cardinality(ARRAY(SELECT g FROM unnest(u.favorite_genres) AS g WHERE g = ANY(sqlc.arg(viewer_genres)::text[]))) DESC,
    books_read_count DESC,
    u.followers_count DESC
LIMIT sqlc.arg(row_limit);

-- name: UserReadBookIDs :many
-- The viewer's finished books, for the "you both read X" signal. Capped: a
-- heavy reader's whole shelf is not needed to find overlap worth naming.
SELECT book_id FROM bookshelf
WHERE user_id = $1 AND status = 'read'
LIMIT 500;

-- name: SharedReadCounts :many
-- How many of those books each candidate has also finished, plus one title to
-- name. MIN(title) keeps the sample stable between calls.
SELECT bs.user_id, COUNT(*)::int AS shared, MIN(bk.title)::text AS sample_title
FROM bookshelf bs
JOIN books bk ON bk.id = bs.book_id
WHERE bs.user_id = ANY(sqlc.arg(candidate_ids)::uuid[])
  AND bs.status = 'read'
  AND bs.book_id = ANY(sqlc.arg(book_ids)::uuid[])
GROUP BY bs.user_id;

-- name: FollowingIDs :many
SELECT following_id FROM follows WHERE follower_id = $1 LIMIT 1000;

-- name: MutualFollowCounts :many
-- Candidates followed by people the viewer already follows.
SELECT f.following_id AS user_id, COUNT(*)::int AS mutuals
FROM follows f
WHERE f.following_id = ANY(sqlc.arg(candidate_ids)::uuid[])
  AND f.follower_id = ANY(sqlc.arg(follower_ids)::uuid[])
GROUP BY f.following_id;

-- name: GetPopularReaders :many
-- Public readers for the logged-out "who is here" strip. Followers first, then
-- live read count; must have something on the shelf so the Top 4 / count are
-- not both empty.
SELECT
    u.id,
    u.username,
    u.name,
    u.avatar_url,
    u.bio,
    u.followers_count,
    (SELECT COUNT(*) FROM bookshelf bs WHERE bs.user_id = u.id AND bs.status = 'read')::int AS books_read_count,
    ARRAY(
        SELECT COALESCE(b.cover_url, '')
        FROM favorites f JOIN books b ON b.id = f.book_id
        WHERE f.user_id = u.id
        ORDER BY f.display_order
        LIMIT 4
    )::text[] AS favorite_covers
FROM users u
WHERE u.deleted_at IS NULL
  AND u.is_public = true
  AND EXISTS (SELECT 1 FROM bookshelf bs WHERE bs.user_id = u.id)
ORDER BY u.followers_count DESC, books_read_count DESC, u.created_at ASC
LIMIT $1;
