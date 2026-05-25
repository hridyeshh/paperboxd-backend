ALTER TABLE user_signal_profiles
    ADD COLUMN IF NOT EXISTS fast_finish_embedding vector(1024);

CREATE INDEX IF NOT EXISTS idx_usp_fast_finish_embedding
    ON user_signal_profiles
    USING ivfflat (fast_finish_embedding vector_cosine_ops)
    WITH (lists = 10);

CREATE TABLE IF NOT EXISTS feature_flags (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    description TEXT,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO feature_flags (key, value, description)
VALUES ('ranking_v2', 'false', 'Enable Phase 4 multi-signal ranking formula')
ON CONFLICT (key) DO NOTHING;
