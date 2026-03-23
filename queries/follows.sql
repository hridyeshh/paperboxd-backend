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
WHERE f.following_id = $1
ORDER BY f.created_at DESC
LIMIT $2 OFFSET $3;

-- name: GetFollowing :many
SELECT u.* FROM follows f
JOIN users u ON f.following_id = u.id
WHERE f.follower_id = $1
ORDER BY f.created_at DESC
LIMIT $2 OFFSET $3;

-- name: CheckFollowing :one
SELECT EXISTS(SELECT 1 FROM follows WHERE follower_id = $1 AND following_id = $2);

-- name: CountFollowers :one
SELECT COUNT(*) FROM follows WHERE following_id = $1;

-- name: CountFollowing :one
SELECT COUNT(*) FROM follows WHERE follower_id = $1;
