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
   OR $1 ILIKE ANY(authors)
ORDER BY view_count DESC
LIMIT $2 OFFSET $3;

-- name: IncrementBookViews :exec
UPDATE books SET view_count = view_count + 1, last_accessed_at = NOW() WHERE id = $1;

-- name: BumpBookAccess :exec
UPDATE books SET last_accessed_at = NOW() WHERE id = $1;

-- name: GetBookByISBN :one
SELECT * FROM books WHERE isbn_13 = $1 OR isbndb_id = $1 LIMIT 1;

-- name: CreateBookFromISBNdb :one
INSERT INTO books (
    title, slug, authors, isbn_13,
    description, published_date, page_count, language, cover_url, categories,
    publisher, isbndb_id, metadata
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
ON CONFLICT (isbn_13) DO UPDATE SET
    title = EXCLUDED.title,
    authors = EXCLUDED.authors,
    description = EXCLUDED.description,
    cover_url = EXCLUDED.cover_url,
    publisher = EXCLUDED.publisher,
    isbndb_id = EXCLUDED.isbndb_id,
    updated_at = NOW()
RETURNING *;

-- name: GetLatestBooks :many
SELECT * FROM books
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: GetPopularBooks :many
SELECT * FROM books
ORDER BY view_count DESC
LIMIT $1 OFFSET $2;

-- name: GetBooksByAuthor :many
SELECT * FROM books
WHERE array_to_string(authors, '|') ILIKE '%' || $1 || '%'
ORDER BY view_count DESC
LIMIT $2 OFFSET $3;

-- name: CleanupStaleBooks :execrows
-- Sliding window: deletes books whose last_accessed_at is older than 15 days,
-- excluding any book referenced by bookshelf or likes.
DELETE FROM books
WHERE last_accessed_at < NOW() - INTERVAL '15 days'
  AND id NOT IN (
      SELECT DISTINCT book_id FROM bookshelf
      UNION
      SELECT DISTINCT book_id FROM likes
  );

-- name: VibeSearchBooks :many
SELECT
  b.id,
  b.title,
  b.slug,
  b.authors,
  b.subtitle,
  b.publisher,
  b.published_date,
  b.description,
  b.page_count,
  b.categories,
  b.language,
  b.cover_url,
  b.average_rating,
  b.ratings_count,
  b.total_reads_count,
  b.total_tbr_count,
  b.google_books_id,
  b.isbndb_id,
  b.embedding,
  (1 - (b.embedding <=> sqlc.arg(query_vec)::vector))::float8 AS similarity_score
FROM books b
WHERE b.embedding IS NOT NULL
ORDER BY b.embedding <=> sqlc.arg(query_vec)::vector
LIMIT sqlc.arg(lim)::int;

-- name: GetBookEmbeddingsByIDs :many
SELECT id::text AS id, embedding
FROM books
WHERE id::text = ANY($1::text[])
  AND embedding IS NOT NULL;
