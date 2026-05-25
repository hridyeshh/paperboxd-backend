DROP INDEX IF EXISTS idx_diary_entries_user_embedding;

ALTER TABLE diary_entries
    DROP COLUMN IF EXISTS embedding,
    DROP COLUMN IF EXISTS embedding_text;

ALTER TABLE user_signal_profiles
    DROP COLUMN IF EXISTS velocity_signal,
    DROP COLUMN IF EXISTS diary_signal,
    DROP COLUMN IF EXISTS diary_embedding,
    DROP COLUMN IF EXISTS social_signal,
    DROP COLUMN IF EXISTS signal_version;
