-- name: CreateBook :one
INSERT INTO books (
    title, slug, authors, isbn_13, google_books_id,
    description, published_date, page_count, language, cover_url, categories,
    subtitle, publisher, isbndb_id, open_library_id, average_rating, preview_link,
    metadata
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18
)
ON CONFLICT (google_books_id) DO UPDATE SET
    title = EXCLUDED.title,
    authors = EXCLUDED.authors,
    description = EXCLUDED.description,
    cover_url = EXCLUDED.cover_url,
    subtitle = EXCLUDED.subtitle,
    publisher = EXCLUDED.publisher,
    isbndb_id = EXCLUDED.isbndb_id,
    average_rating = EXCLUDED.average_rating,
    updated_at = NOW()
RETURNING *;

-- name: GetBookByID :one
SELECT * FROM books WHERE id = $1;

-- name: GetBookBySlug :one
SELECT * FROM books WHERE slug = $1;

-- name: GetBookByGoogleID :one
SELECT * FROM books WHERE google_books_id = $1;

-- name: SearchBooksInDB :many
SELECT * FROM books
WHERE title ILIKE '%' || $1 || '%'
   OR $1 = ANY(authors)
ORDER BY view_count DESC
LIMIT $2 OFFSET $3;

-- name: IncrementBookViews :exec
UPDATE books SET view_count = view_count + 1 WHERE id = $1;
