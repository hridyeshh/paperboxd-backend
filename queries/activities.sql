-- name: CreateActivity :one
INSERT INTO activities (user_id, activity_type, book_id, list_id, entry_id, target_user_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetUserActivities :many
SELECT
    a.id,
    a.user_id,
    a.activity_type,
    a.book_id,
    a.list_id,
    a.entry_id,
    a.target_user_id,
    a.metadata,
    a.created_at,
    u.username,
    u.name,
    u.avatar_url,
    b.title as book_title,
    b.slug as book_slug,
    l.title as list_title,
    de.title as entry_title,
    tu.username as target_username
FROM activities a
JOIN users u ON a.user_id = u.id
LEFT JOIN books b ON a.book_id = b.id
LEFT JOIN lists l ON a.list_id = l.id
LEFT JOIN diary_entries de ON a.entry_id = de.id
LEFT JOIN users tu ON a.target_user_id = tu.id
WHERE a.user_id = $1
  -- fusion_joined is addressed to one reader; never on a profile.
  AND a.activity_type <> 'fusion_joined'
ORDER BY a.created_at DESC
LIMIT $2 OFFSET $3;

-- name: GetFollowingActivities :many
SELECT
    a.id,
    a.user_id,
    a.activity_type,
    a.book_id,
    a.list_id,
    a.entry_id,
    a.target_user_id,
    a.metadata,
    a.created_at,
    u.username,
    u.name,
    u.avatar_url,
    b.title as book_title,
    b.slug as book_slug,
    l.title as list_title,
    de.title as entry_title,
    tu.username as target_username
FROM activities a
JOIN users u ON a.user_id = u.id
LEFT JOIN books b ON a.book_id = b.id
LEFT JOIN lists l ON a.list_id = l.id
LEFT JOIN diary_entries de ON a.entry_id = de.id
LEFT JOIN users tu ON a.target_user_id = tu.id
WHERE u.deleted_at IS NULL
  AND (
    -- fusion_joined is addressed to the inviter only, never broadcast to the
    -- invitee's followers.
    (a.user_id IN (
        SELECT following_id FROM follows WHERE follower_id = $1
    ) AND a.activity_type <> 'fusion_joined')
    OR (a.activity_type IN ('shared_list', 'shared_book', 'granted_access', 'liked_diary_entry', 'fusion_joined')
        AND a.target_user_id = $1)
  )
ORDER BY a.created_at DESC
LIMIT $2 OFFSET $3;

-- name: CheckNewActivities :one
SELECT EXISTS(
    SELECT 1 FROM activities a
    JOIN users u ON a.user_id = u.id
    WHERE u.deleted_at IS NULL
    AND (
        (a.user_id IN (SELECT following_id FROM follows WHERE follower_id = $1)
         AND a.activity_type <> 'fusion_joined')
        OR (a.activity_type IN ('shared_list', 'shared_book', 'granted_access', 'liked_diary_entry', 'fusion_joined')
            AND a.target_user_id = $1)
    )
    AND a.created_at > $2
);

-- name: GetActivityByID :one
SELECT * FROM activities
WHERE id = $1;

-- name: DeleteUserActivities :exec
DELETE FROM activities
WHERE user_id = $1 AND activity_type = $2;

-- name: CountUnreadActivities :one
-- Unread badge for the notifications sheet. target_user_id is the "addressed to
-- you" marker — every activity type that carries one (liked_diary_entry,
-- shared_list, shared_book, granted_access, fusion_joined) is notification-worthy, so no
-- activity_type filter is needed here.
SELECT COUNT(*) FROM activities
WHERE target_user_id = $1 AND read_at IS NULL;

-- name: MarkActivitiesRead :exec
-- Called when the sheet opens. Idempotent; leaves already-read rows alone so a
-- reopen does not churn timestamps.
UPDATE activities
SET read_at = NOW()
WHERE target_user_id = $1 AND read_at IS NULL;

-- name: ActivityExistsRecent :one
-- Dedupe guard for shelf activities: the same (user, book, type) within a day
-- is a re-save, not news.
SELECT EXISTS(
    SELECT 1 FROM activities
    WHERE user_id = $1 AND book_id = $2 AND activity_type = $3
      AND created_at > NOW() - INTERVAL '1 day'
);

-- name: GetPublicActivities :many
-- Community feed for logged-out visitors and thin follow graphs. Only public,
-- live accounts; only broadcast rows (anything addressed to a target user is a
-- notification, not news); private lists and diary entries stay hidden.
-- Over-fetch and collapse per user in Go so one import does not own the feed.
SELECT
    a.id,
    a.user_id,
    a.activity_type,
    a.book_id,
    a.list_id,
    a.entry_id,
    a.target_user_id,
    a.metadata,
    a.created_at,
    u.username,
    u.name,
    u.avatar_url,
    b.title as book_title,
    b.slug as book_slug,
    b.cover_url as book_cover,
    l.title as list_title,
    de.title as entry_title,
    tu.username as target_username
FROM activities a
JOIN users u ON a.user_id = u.id
LEFT JOIN books b ON a.book_id = b.id
LEFT JOIN lists l ON a.list_id = l.id
LEFT JOIN diary_entries de ON a.entry_id = de.id
LEFT JOIN users tu ON a.target_user_id = tu.id
WHERE u.deleted_at IS NULL
  AND u.is_public = true
  AND a.target_user_id IS NULL
  AND (a.list_id IS NULL OR l.is_private = false)
  AND (a.entry_id IS NULL OR de.is_private = false)
ORDER BY a.created_at DESC
LIMIT $1;
