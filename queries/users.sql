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
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateUsername :one
UPDATE users SET username = $2, updated_at = NOW()
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
UPDATE users
SET deleted_at = NOW(),
    updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: RecordAccountDeletion :exec
INSERT INTO account_deletions (
    user_id, email, username, reasons
) VALUES (
    $1, $2, $3, $4
);
