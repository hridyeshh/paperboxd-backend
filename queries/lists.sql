-- name: CreateList :one
INSERT INTO lists (user_id, title, description, is_private)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetListByID :one
SELECT * FROM lists
WHERE id = $1;

-- name: GetUserLists :many
SELECT
    l.id,
    l.user_id,
    l.title,
    l.description,
    l.is_private,
    l.created_at,
    l.updated_at,
    COUNT(DISTINCT lb.book_id) as book_count
FROM lists l
LEFT JOIN list_books lb ON l.id = lb.list_id
WHERE l.user_id = $1
GROUP BY l.id
ORDER BY l.updated_at DESC
LIMIT $2 OFFSET $3;

-- name: GetUserSavedLists :many
SELECT
    l.*,
    u.username as owner_username,
    sl.saved_at,
    COUNT(DISTINCT lb.book_id) as book_count
FROM saved_lists sl
JOIN lists l ON sl.list_id = l.id
JOIN users u ON l.user_id = u.id
LEFT JOIN list_books lb ON l.id = lb.list_id
WHERE sl.user_id = $1
GROUP BY l.id, u.username, sl.saved_at
ORDER BY sl.saved_at DESC
LIMIT $2 OFFSET $3;

-- name: UpdateList :one
UPDATE lists
SET
    title = $2,
    description = $3,
    is_private = $4,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteList :exec
DELETE FROM lists
WHERE id = $1;

-- name: CountUserLists :one
SELECT COUNT(*) FROM lists
WHERE user_id = $1;

-- name: IncrementUserListsCount :exec
UPDATE users
SET lists_count = lists_count + 1
WHERE id = $1;

-- name: DecrementUserListsCount :exec
UPDATE users
SET lists_count = GREATEST(lists_count - 1, 0)
WHERE id = $1;

-- List Books Operations

-- name: AddBookToList :one
INSERT INTO list_books (list_id, book_id, display_order)
VALUES ($1, $2, $3)
RETURNING *;

-- name: RemoveBookFromList :exec
DELETE FROM list_books
WHERE list_id = $1 AND book_id = $2;

-- name: GetListBooks :many
SELECT
    lb.id,
    lb.list_id,
    lb.book_id,
    lb.added_at,
    lb.display_order,
    b.title,
    b.slug,
    b.authors,
    b.cover_url,
    b.published_date,
    b.page_count,
    b.isbn_13,
    b.description,
    b.categories,
    b.publisher,
    b.language,
    b.subtitle,
    b.isbndb_id,
    b.google_books_id,
    b.average_rating,
    b.ratings_count
FROM list_books lb
JOIN books b ON lb.book_id = b.id
WHERE lb.list_id = $1
ORDER BY lb.display_order ASC, lb.added_at DESC;

-- name: CheckBookInList :one
SELECT EXISTS(
    SELECT 1 FROM list_books
    WHERE list_id = $1 AND book_id = $2
);

-- name: CountListBooks :one
SELECT COUNT(*) FROM list_books
WHERE list_id = $1;

-- Access Control

-- name: GrantListAccess :one
INSERT INTO list_access (list_id, user_id, granted_by)
VALUES ($1, $2, $3)
RETURNING *;

-- name: RevokeListAccess :exec
DELETE FROM list_access
WHERE list_id = $1 AND user_id = $2;

-- name: CheckListAccess :one
SELECT EXISTS(
    SELECT 1 FROM list_access
    WHERE list_id = $1 AND user_id = $2
);

-- name: GetListAccessUsers :many
SELECT
    la.id,
    la.list_id,
    la.user_id,
    la.granted_by,
    la.granted_at,
    u.username,
    u.name,
    u.avatar_url
FROM list_access la
JOIN users u ON la.user_id = u.id
WHERE la.list_id = $1
ORDER BY la.granted_at DESC;

-- Saved Lists

-- name: SaveList :one
INSERT INTO saved_lists (user_id, list_id)
VALUES ($1, $2)
RETURNING *;

-- name: UnsaveList :exec
DELETE FROM saved_lists
WHERE user_id = $1 AND list_id = $2;

-- name: CheckListSaved :one
SELECT EXISTS(
    SELECT 1 FROM saved_lists
    WHERE user_id = $1 AND list_id = $2
);

-- name: CountListSaves :one
SELECT COUNT(*) FROM saved_lists
WHERE list_id = $1;

-- Authorization Helpers

-- name: CheckListOwnership :one
SELECT user_id FROM lists
WHERE id = $1;

-- name: CheckCanAccessList :one
SELECT EXISTS(
    SELECT 1 FROM lists l
    WHERE l.id = $1
    AND (
        l.is_private = false
        OR l.user_id = $2
        OR EXISTS(SELECT 1 FROM list_access WHERE list_id = $1 AND user_id = $2)
    )
);
