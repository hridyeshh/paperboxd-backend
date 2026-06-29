DROP INDEX IF EXISTS idx_scan_cache_cached_at;
DROP TABLE IF EXISTS scan_community_cache;
ALTER TABLE users DROP COLUMN IF EXISTS scan_uses_remaining;
