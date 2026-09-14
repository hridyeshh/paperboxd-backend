DROP TABLE IF EXISTS thought_reposts;
DROP INDEX IF EXISTS idx_thoughts_thread_root;
ALTER TABLE thoughts DROP COLUMN IF EXISTS thread_root_id;
