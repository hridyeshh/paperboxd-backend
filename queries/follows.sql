-- name: FollowUser :one
INSERT INTO follows (follower_id, following_id)
VALUES ($1, $2)
ON CONFLICT (follower_id, following_id) DO NOTHING
RETURNING *;

-- name: UnfollowUser :exec
DELETE FROM follows WHERE follower_id = $1 AND following_id = $2;

-- name: GetFollowers :many
SELECT u.* FROM follows f
JOIN users u ON f.follower_id = u.id
WHERE f.following_id = $1 AND u.deleted_at IS NULL
ORDER BY f.created_at DESC
LIMIT $2 OFFSET $3;

-- name: GetFollowing :many
SELECT u.* FROM follows f
JOIN users u ON f.following_id = u.id
WHERE f.follower_id = $1 AND u.deleted_at IS NULL
ORDER BY f.created_at DESC
LIMIT $2 OFFSET $3;

-- name: CheckFollowing :one
SELECT EXISTS(SELECT 1 FROM follows WHERE follower_id = $1 AND following_id = $2);

-- name: CountFollowers :one
SELECT COUNT(*) FROM follows f
JOIN users u ON f.follower_id = u.id
WHERE f.following_id = $1 AND u.deleted_at IS NULL;

-- name: CountFollowing :one
SELECT COUNT(*) FROM follows f
JOIN users u ON f.following_id = u.id
WHERE f.follower_id = $1 AND u.deleted_at IS NULL;

-- ── Follow requests (private profiles) ───────────────────────────────────────

-- name: CreateFollowRequest :one
INSERT INTO follow_requests (requester_id, target_id)
VALUES ($1, $2)
ON CONFLICT (requester_id, target_id)
DO UPDATE SET created_at = follow_requests.created_at
RETURNING *;

-- name: CheckFollowRequest :one
SELECT EXISTS(
    SELECT 1 FROM follow_requests WHERE requester_id = $1 AND target_id = $2
);

-- name: DeleteFollowRequest :exec
DELETE FROM follow_requests WHERE requester_id = $1 AND target_id = $2;

-- name: ListIncomingFollowRequests :many
SELECT fr.id AS request_id, fr.created_at AS requested_at, sqlc.embed(u)
FROM follow_requests fr
JOIN users u ON u.id = fr.requester_id
WHERE fr.target_id = $1 AND u.deleted_at IS NULL
ORDER BY fr.created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountIncomingFollowRequests :one
SELECT COUNT(*) FROM follow_requests fr
JOIN users u ON u.id = fr.requester_id
WHERE fr.target_id = $1 AND u.deleted_at IS NULL;

-- Going public accepts everyone who was waiting, so no request is orphaned
-- behind a switch the requester cannot see.
-- name: AcceptAllFollowRequests :exec
WITH moved AS (
    DELETE FROM follow_requests WHERE target_id = $1
    RETURNING requester_id, target_id
)
INSERT INTO follows (follower_id, following_id)
SELECT requester_id, target_id FROM moved
ON CONFLICT (follower_id, following_id) DO NOTHING;
