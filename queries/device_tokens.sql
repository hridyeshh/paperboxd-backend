-- name: UpsertDeviceToken :one
-- Conflict target is `token` alone: a push token belongs to a device, so when a
-- different account signs in on that device the row must change hands rather
-- than accumulate a second owner. See migrations/000036 for the full rationale.
INSERT INTO device_tokens (user_id, platform, token)
VALUES ($1, $2, $3)
ON CONFLICT (token) DO UPDATE
SET user_id    = EXCLUDED.user_id,
    platform   = EXCLUDED.platform,
    updated_at = NOW()
RETURNING *;

-- name: ListDeviceTokensByUser :many
SELECT * FROM device_tokens
WHERE user_id = $1;

-- name: DeleteDeviceToken :exec
-- Logout path. Scoped to the caller so one user cannot deregister another's device.
DELETE FROM device_tokens
WHERE user_id = $1 AND token = $2;

-- name: DeleteDeviceTokenByToken :exec
-- Provider-rejection path (FCM UNREGISTERED, APNs 410 Unregistered). The token
-- is dead regardless of who owns it, so this is intentionally unscoped.
DELETE FROM device_tokens
WHERE token = $1;

-- name: DeleteStaleDeviceTokens :exec
DELETE FROM device_tokens
WHERE updated_at < $1;
