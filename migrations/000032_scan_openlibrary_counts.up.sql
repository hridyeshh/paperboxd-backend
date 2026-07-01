ALTER TABLE scan_community_cache
    RENAME COLUMN reddit_count TO readers_count;

ALTER TABLE scan_community_cache
    RENAME COLUMN web_count TO ratings_count;
