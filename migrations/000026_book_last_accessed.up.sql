ALTER TABLE books
    ADD COLUMN IF NOT EXISTS last_accessed_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

UPDATE books SET last_accessed_at = created_at
WHERE last_accessed_at IS NULL OR last_accessed_at = created_at;

CREATE INDEX IF NOT EXISTS idx_books_last_accessed
    ON books(last_accessed_at);
