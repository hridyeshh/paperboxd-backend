-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (
    user_id, token_hash, expires_at, device_info
) VALUES (
    $1, $2, $3, $4
)
RETURNING *;

-- name: GetRefreshToken :one
SELECT * FROM refresh_tokens
WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > NOW();

-- name: UpdateRefreshTokenLastUsed :exec
UPDATE refresh_tokens
SET last_used_at = NOW()
WHERE id = $1;

-- name: RevokeRefreshToken :exec
UPDATE refresh_tokens
SET revoked_at = NOW()
WHERE token_hash = $1;

-- name: RevokeAllUserTokens :exec
UPDATE refresh_tokens
SET revoked_at = NOW()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: CreatePasswordResetToken :one
INSERT INTO password_reset_tokens (
    user_id, token_hash, expires_at
) VALUES (
    $1, $2, $3
)
RETURNING *;

-- name: GetPasswordResetToken :one
SELECT * FROM password_reset_tokens
WHERE token_hash = $1 AND used_at IS NULL AND expires_at > NOW();

-- name: MarkPasswordResetTokenUsed :exec
UPDATE password_reset_tokens
SET used_at = NOW()
WHERE id = $1;

-- name: UpdateUserPasswordByID :exec
UPDATE users
SET password_hash = $2, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;
