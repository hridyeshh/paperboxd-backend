ALTER TABLE scan_community_cache
    DROP COLUMN IF EXISTS reddit_count,
    DROP COLUMN IF EXISTS web_count;
