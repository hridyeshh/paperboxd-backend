ALTER TABLE users
    ADD COLUMN IF NOT EXISTS scan_uses_remaining INTEGER NOT NULL DEFAULT 7;

CREATE TABLE IF NOT EXISTS scan_community_cache (
    isbn              TEXT PRIMARY KEY,
    community_summary TEXT        NOT NULL,
    cached_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_scan_cache_cached_at
    ON scan_community_cache (cached_at);
