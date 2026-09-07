-- Ratings and reviews now require having read at least 20 pages, or having
-- finished the book (see canRateEntry in internal/handler/bookshelf.go).
-- Before the gate existed the mobile clients silently shelved a book as
-- 'reading' with zero pages the moment someone tapped a star, so the table
-- holds ratings nobody earned. Clear those.
--
-- DESTRUCTIVE AND IRREVERSIBLE — the down migration cannot restore the values.
-- Take a backup of (user_id, book_id, rating, review, reviewed_at) first.

UPDATE bookshelf
SET rating        = NULL,
    review        = NULL,
    reviewed_at   = NULL,
    review_edited = false,
    updated_at    = NOW()
WHERE (rating IS NOT NULL OR review IS NOT NULL)
  AND status <> 'read'
  AND (current_page IS NULL OR current_page < 20);
