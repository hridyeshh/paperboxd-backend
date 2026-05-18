-- Event tracking for future click/impression signals (Phase 3+)
CREATE TABLE IF NOT EXISTS events (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id    uuid REFERENCES books(id) ON DELETE SET NULL,
    event_type varchar(50) NOT NULL,
    metadata   jsonb,
    created_at timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_events_user_created ON events(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);

-- Computed signal profiles per user (genre/author weights with temporal decay)
CREATE TABLE IF NOT EXISTS user_signal_profiles (
    user_id        uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    genre_weights  jsonb NOT NULL DEFAULT '{}',
    author_weights jsonb NOT NULL DEFAULT '{}',
    computed_at    timestamptz NOT NULL DEFAULT NOW(),
    bookshelf_hash varchar(64)
);

CREATE INDEX IF NOT EXISTS idx_user_signal_profiles_computed ON user_signal_profiles(computed_at);
