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
-- Substring hits first, then fuzzy: trigram word similarity (dropped/extra
-- letters), double metaphone (sounds alike) and, for single-word queries,
-- a per-word edit distance of 2 (swapped letters). Exact hits rank above
-- fuzzy, fuzzy by closeness, ties by popularity.
-- ponytail: seq scan, ~100ms per 30k books; past ~100k rows move the
-- fuzzy branches to a trigram-indexed UNION and drop the unnest one.
SELECT * FROM books
WHERE title ILIKE '%' || @q::text || '%'
   OR @q::text ILIKE ANY(authors)
   OR isbn_13 = replace(@q::text, '-', '')
   OR word_similarity(@q::text, title) >= 0.4
   OR word_similarity(@q::text, array_to_string(authors, ' ')) >= 0.4
   OR (dmetaphone(@q::text) <> '' AND dmetaphone(title) = dmetaphone(@q::text))
   OR (length(@q::text) >= 5 AND position(' ' IN @q::text) = 0 AND EXISTS (
        SELECT 1 FROM unnest(string_to_array(lower(title), ' ')) AS w
        WHERE levenshtein_less_equal(lower(@q::text), w, 2) <= 2
           OR dmetaphone(w) = dmetaphone(@q::text)))
ORDER BY
    (title ILIKE '%' || @q::text || '%') DESC,
    GREATEST(word_similarity(@q::text, title), word_similarity(@q::text, array_to_string(authors, ' '))) DESC,
    view_count DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

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

-- name: GetRandomBooks :many
SELECT * FROM books
ORDER BY RANDOM()
LIMIT $1;

-- name: GetBooksByAuthor :many
SELECT * FROM books
WHERE array_to_string(authors, '|') ILIKE '%' || $1 || '%'
ORDER BY view_count DESC
LIMIT $2 OFFSET $3;

-- name: CleanupStaleBooks :execrows
-- Sliding window: deletes books whose last_accessed_at is older than 15 days
-- and that no user-owned row points at. Every table below either CASCADEs
-- (bookshelf, likes, favorites, list_books, reading_log) or SET NULLs
-- (thoughts, activities) on book delete — so any book referenced here is
-- user data and must survive. NOT EXISTS rather than NOT IN so a NULL book_id
-- in the nullable tables can never make the predicate unknown.
DELETE FROM books b
WHERE b.last_accessed_at < NOW() - INTERVAL '15 days'
  AND NOT EXISTS (SELECT 1 FROM bookshelf     x WHERE x.book_id = b.id)
  AND NOT EXISTS (SELECT 1 FROM likes         x WHERE x.book_id = b.id)
  AND NOT EXISTS (SELECT 1 FROM favorites     x WHERE x.book_id = b.id)
  AND NOT EXISTS (SELECT 1 FROM list_books    x WHERE x.book_id = b.id)
  AND NOT EXISTS (SELECT 1 FROM reading_log   x WHERE x.book_id = b.id)
  AND NOT EXISTS (SELECT 1 FROM thoughts x WHERE x.book_id = b.id)
  AND NOT EXISTS (SELECT 1 FROM activities    x WHERE x.book_id = b.id);

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
  -- Page bounds are applied here, not after retrieval: "under 250 pages"
  -- over the 120 nearest neighbours of a long-book query leaves a handful.
  -- A book with no page count cannot satisfy a length request.
  AND (sqlc.narg(max_pages)::int IS NULL OR (b.page_count > 0 AND b.page_count <= sqlc.narg(max_pages)::int))
  AND (sqlc.narg(min_pages)::int IS NULL OR (b.page_count > 0 AND b.page_count >= sqlc.narg(min_pages)::int))
ORDER BY b.embedding <=> sqlc.arg(query_vec)::vector
LIMIT sqlc.arg(lim)::int;

-- name: GetBookEmbeddingsByIDs :many
SELECT id::text AS id, embedding
FROM books
WHERE id::text = ANY($1::text[])
  AND embedding IS NOT NULL;

-- name: GetTrendingBooks :many
-- Books most shelved in the last 7 days by live accounts. bookshelf.created_at
-- is untouched by the upsert, so re-saves do not count twice.
SELECT sqlc.embed(b), COUNT(bs.id)::int AS adds_7d
FROM books b
JOIN bookshelf bs ON bs.book_id = b.id AND bs.created_at > NOW() - INTERVAL '7 days'
JOIN users u ON u.id = bs.user_id AND u.deleted_at IS NULL
GROUP BY b.id
ORDER BY adds_7d DESC, b.view_count DESC
LIMIT $1;

-- name: GetRisingBooks :many
-- Books being shelved faster this week than last. Momentum, not volume: a
-- steady bestseller does not qualify, a book three people just discovered does.
SELECT
    sqlc.embed(b),
    COUNT(*) FILTER (WHERE bs.created_at > NOW() - INTERVAL '7 days')::int AS recent,
    COUNT(*) FILTER (WHERE bs.created_at <= NOW() - INTERVAL '7 days')::int AS prior
FROM books b
JOIN bookshelf bs ON bs.book_id = b.id AND bs.created_at > NOW() - INTERVAL '14 days'
JOIN users u ON u.id = bs.user_id AND u.deleted_at IS NULL
GROUP BY b.id
HAVING COUNT(*) FILTER (WHERE bs.created_at > NOW() - INTERVAL '7 days')
     > COUNT(*) FILTER (WHERE bs.created_at <= NOW() - INTERVAL '7 days')
ORDER BY
    (COUNT(*) FILTER (WHERE bs.created_at > NOW() - INTERVAL '7 days')
   - COUNT(*) FILTER (WHERE bs.created_at <= NOW() - INTERVAL '7 days')) DESC
LIMIT $1;

-- name: GetMostTBRBooks :many
-- Most added to a TBR in the last week — intent to read, distinct from reads.
SELECT sqlc.embed(b), COUNT(*)::int AS tbr_7d
FROM books b
JOIN bookshelf bs ON bs.book_id = b.id
    AND bs.status = 'to-read'
    AND bs.created_at > NOW() - INTERVAL '7 days'
JOIN users u ON u.id = bs.user_id AND u.deleted_at IS NULL
GROUP BY b.id
ORDER BY tbr_7d DESC, b.view_count DESC
LIMIT $1;

-- name: GetHiddenGems :many
-- Loved by the few who found it: a real Paperboxd average of 4+ from at least
-- three raters, but not yet widely read. The upper bound is what makes it a
-- gem rather than a hit.
SELECT
    sqlc.embed(b),
    AVG(bs.rating)::float8 AS rating,
    COUNT(bs.rating)::int AS ratings_count
FROM books b
JOIN bookshelf bs ON bs.book_id = b.id AND bs.rating IS NOT NULL
JOIN users u ON u.id = bs.user_id AND u.deleted_at IS NULL
GROUP BY b.id
HAVING COUNT(bs.rating) >= 3 AND COUNT(bs.rating) <= 25 AND AVG(bs.rating) >= 4.0
ORDER BY AVG(bs.rating) DESC, COUNT(bs.rating) DESC
LIMIT $1;
