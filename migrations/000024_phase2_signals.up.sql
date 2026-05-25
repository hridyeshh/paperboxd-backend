ALTER TABLE diary_entries
    ADD COLUMN IF NOT EXISTS embedding      vector(1024),
    ADD COLUMN IF NOT EXISTS embedding_text TEXT;

CREATE INDEX IF NOT EXISTS idx_diary_entries_user_embedding
    ON diary_entries(user_id) WHERE embedding IS NOT NULL;

ALTER TABLE user_signal_profiles
    ADD COLUMN IF NOT EXISTS velocity_signal  jsonb,
    ADD COLUMN IF NOT EXISTS diary_signal     jsonb,
    ADD COLUMN IF NOT EXISTS diary_embedding  vector(1024),
    ADD COLUMN IF NOT EXISTS social_signal    jsonb,
    ADD COLUMN IF NOT EXISTS signal_version   int NOT NULL DEFAULT 0;
