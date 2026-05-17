DROP TABLE IF EXISTS recommendation_impressions;
ALTER TABLE books ADD COLUMN IF NOT EXISTS embedding vector(1024);
DROP EXTENSION IF EXISTS vector;
