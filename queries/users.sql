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
