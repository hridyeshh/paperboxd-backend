DROP INDEX IF EXISTS idx_activities_target_created;
ALTER TABLE activities DROP COLUMN IF EXISTS read_at;
