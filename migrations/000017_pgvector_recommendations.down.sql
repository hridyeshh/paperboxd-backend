DROP TABLE IF EXISTS recommendation_impressions;
ALTER TABLE books DROP COLUMN IF EXISTS embedding;
DROP EXTENSION IF EXISTS vector;
