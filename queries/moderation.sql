-- name: BlockUser :exec
INSERT INTO blocks (blocker_id, blocked_id)
VALUES ($1, $2)
ON CONFLICT (blocker_id, blocked_id) DO NOTHING;

-- name: UnblockUser :exec
DELETE FROM blocks WHERE blocker_id = $1 AND blocked_id = $2;

-- name: CheckBlockedEither :one
SELECT EXISTS(
    SELECT 1 FROM blocks
    WHERE (blocker_id = $1 AND blocked_id = $2)
       OR (blocker_id = $2 AND blocked_id = $1)
);

-- name: CheckBlocked :one
SELECT EXISTS(
    SELECT 1 FROM blocks WHERE blocker_id = $1 AND blocked_id = $2
);

-- name: CreateReport :one
INSERT INTO reports (reporter_id, content_type, content_id, reason, details)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;
