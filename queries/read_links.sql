-- name: GetReadLinksByBookID :one
SELECT * FROM book_read_links WHERE book_id = $1;

-- name: UpsertStoreLinks :one
-- Writes only the stores whose lookup answered (google_checked /
-- apple_checked); the others keep their link and checked_at untouched.
INSERT INTO book_read_links (
    book_id, google_play_buy_link, google_play_checked_at, apple_books_url, apple_books_checked_at
) VALUES (
    @book_id,
    sqlc.narg(google_play_buy_link), CASE WHEN @google_checked::boolean THEN NOW() END,
    sqlc.narg(apple_books_url),      CASE WHEN @apple_checked::boolean THEN NOW() END
)
ON CONFLICT (book_id) DO UPDATE SET
    google_play_buy_link   = CASE WHEN @google_checked::boolean THEN EXCLUDED.google_play_buy_link ELSE book_read_links.google_play_buy_link END,
    google_play_checked_at = CASE WHEN @google_checked::boolean THEN NOW() ELSE book_read_links.google_play_checked_at END,
    apple_books_url        = CASE WHEN @apple_checked::boolean THEN EXCLUDED.apple_books_url ELSE book_read_links.apple_books_url END,
    apple_books_checked_at = CASE WHEN @apple_checked::boolean THEN NOW() ELSE book_read_links.apple_books_checked_at END,
    updated_at             = NOW()
RETURNING *;

-- name: UpsertPublicDomain :one
-- Gutenberg half only; store links are left alone.
INSERT INTO book_read_links (
    book_id, is_public_domain, gutenberg_id, gutenberg_html_url, gutenberg_epub_url, gutenberg_checked_at
) VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (book_id) DO UPDATE SET
    is_public_domain     = EXCLUDED.is_public_domain,
    gutenberg_id         = EXCLUDED.gutenberg_id,
    gutenberg_html_url   = EXCLUDED.gutenberg_html_url,
    gutenberg_epub_url   = EXCLUDED.gutenberg_epub_url,
    gutenberg_checked_at = NOW(),
    updated_at           = NOW()
RETURNING *;

-- name: ListBooksNeedingGutenbergCheck :many
-- Most recently opened first, so a capped nightly run covers the books
-- people actually look at.
SELECT b.id, b.title, b.authors FROM books b
LEFT JOIN book_read_links rl ON rl.book_id = b.id
WHERE rl.gutenberg_checked_at IS NULL
   OR rl.gutenberg_checked_at < NOW() - INTERVAL '30 days'
ORDER BY b.last_accessed_at DESC NULLS LAST, b.view_count DESC NULLS LAST
LIMIT $1;
