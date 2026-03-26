-- name: GetUserFavorites :many
SELECT
    f.id,
    f.user_id,
    f.book_id,
    f.display_order,
    f.favorite_note,
    f.created_at,
    f.updated_at,
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
    b.ratings_count,
    b.preview_link
FROM favorites f
JOIN books b ON f.book_id = b.id
WHERE f.user_id = $1
ORDER BY f.display_order ASC;

-- name: AddToFavorites :one
INSERT INTO favorites (user_id, book_id, display_order, favorite_note)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: RemoveFromFavorites :exec
DELETE FROM favorites
WHERE user_id = $1 AND book_id = $2;

-- name: UpdateFavoriteNote :one
UPDATE favorites
SET favorite_note = $2, updated_at = NOW()
WHERE user_id = $1 AND book_id = $3
RETURNING *;

-- name: ReorderFavorites :exec
UPDATE favorites
SET display_order = $2, updated_at = NOW()
WHERE id = $1;

-- name: GetFavoriteByUserAndBook :one
SELECT * FROM favorites
WHERE user_id = $1 AND book_id = $2;

-- name: CountUserFavorites :one
SELECT COUNT(*) FROM favorites
WHERE user_id = $1;

-- name: CheckFavoriteExists :one
SELECT EXISTS(
    SELECT 1 FROM favorites
    WHERE user_id = $1 AND book_id = $2
);

-- name: IncrementUserFavoritesCount :exec
UPDATE users SET favorites_count = favorites_count + 1 WHERE id = $1;

-- name: DecrementUserFavoritesCount :exec
UPDATE users SET favorites_count = GREATEST(favorites_count - 1, 0) WHERE id = $1;
