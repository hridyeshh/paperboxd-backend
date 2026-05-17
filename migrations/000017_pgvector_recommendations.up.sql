-- Enable pgvector extension
CREATE EXTENSION IF NOT EXISTS vector;

-- Add embedding column to books (384 dimensions for Cohere embed-english-v3.0)
ALTER TABLE books ADD COLUMN IF NOT EXISTS embedding vector(384);

-- TODO: Create this index only AFTER the backfill job completes.
-- Run: CREATE INDEX books_embedding_idx ON books USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);

-- Track impressions per user/book to suppress over-shown recommendations (Phase 5)
CREATE TABLE IF NOT EXISTS recommendation_impressions (
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id        UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    seen_count     INTEGER DEFAULT 1,
    last_seen      TIMESTAMPTZ DEFAULT NOW(),
    suppress_until TIMESTAMPTZ,
    PRIMARY KEY (user_id, book_id)
);
