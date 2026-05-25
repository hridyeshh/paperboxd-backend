DROP INDEX IF EXISTS idx_usp_fast_finish_embedding;
ALTER TABLE user_signal_profiles DROP COLUMN IF EXISTS fast_finish_embedding;
DROP TABLE IF EXISTS feature_flags;
