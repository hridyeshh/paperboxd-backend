ALTER TABLE books
    DROP COLUMN IF EXISTS embedding_text,
    DROP COLUMN IF EXISTS description_source;
