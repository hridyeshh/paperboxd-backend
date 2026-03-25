-- User table additions
ALTER TABLE users ALTER COLUMN pronouns TYPE TEXT[] USING CASE WHEN pronouns IS NULL THEN NULL ELSE ARRAY[pronouns] END;
ALTER TABLE users ADD COLUMN IF NOT EXISTS birthday DATE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS gender VARCHAR(50);
ALTER TABLE users ADD COLUMN IF NOT EXISTS links TEXT[] DEFAULT '{}';
ALTER TABLE users ADD COLUMN IF NOT EXISTS total_pages_read INTEGER DEFAULT 0;

-- Book table additions
ALTER TABLE books ADD COLUMN IF NOT EXISTS subtitle TEXT;
ALTER TABLE books ADD COLUMN IF NOT EXISTS publisher TEXT;
ALTER TABLE books ADD COLUMN IF NOT EXISTS isbndb_id VARCHAR(50);
ALTER TABLE books ADD COLUMN IF NOT EXISTS open_library_id VARCHAR(50);
ALTER TABLE books ADD COLUMN IF NOT EXISTS average_rating FLOAT8;
ALTER TABLE books ADD COLUMN IF NOT EXISTS ratings_count INTEGER DEFAULT 0;
ALTER TABLE books ADD COLUMN IF NOT EXISTS preview_link TEXT;
ALTER TABLE books ADD COLUMN IF NOT EXISTS total_reads_count INTEGER DEFAULT 0;
ALTER TABLE books ADD COLUMN IF NOT EXISTS total_tbr_count INTEGER DEFAULT 0;

-- Add indexes
CREATE INDEX IF NOT EXISTS idx_books_isbndb ON books(isbndb_id) WHERE isbndb_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_books_open_library ON books(open_library_id) WHERE open_library_id IS NOT NULL;

-- Update existing books to have default values
UPDATE books SET
    ratings_count = 0,
    total_reads_count = 0,
    total_tbr_count = 0
WHERE ratings_count IS NULL;
