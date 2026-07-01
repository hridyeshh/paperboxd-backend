ALTER TABLE scan_community_cache
    RENAME COLUMN readers_count TO reddit_count;

ALTER TABLE scan_community_cache
    RENAME COLUMN ratings_count TO web_count;
