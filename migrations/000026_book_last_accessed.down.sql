DROP INDEX IF EXISTS idx_books_last_accessed;
ALTER TABLE books DROP COLUMN IF EXISTS last_accessed_at;
