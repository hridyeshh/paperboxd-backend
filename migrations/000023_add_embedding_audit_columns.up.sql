ALTER TABLE books
    ADD COLUMN IF NOT EXISTS embedding_text     TEXT,
    ADD COLUMN IF NOT EXISTS description_source TEXT;
