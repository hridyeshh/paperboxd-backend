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
INSERT INTO account_deletions (
    user_id, email, username, reasons
) VALUES (
    $1, $2, $3, $4
);
