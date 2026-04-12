-- Remaining Mongo → PG fields (see cmd/migrate-mongo-to-pg).

ALTER TABLE bookshelf
    ADD COLUMN IF NOT EXISTS notes TEXT,
    ADD COLUMN IF NOT EXISTS format VARCHAR(20);

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS reading_goal_year INTEGER,
    ADD COLUMN IF NOT EXISTS reading_goal_target INTEGER,
    ADD COLUMN IF NOT EXISTS reading_goal_current INTEGER DEFAULT 0;

ALTER TABLE likes
    ADD COLUMN IF NOT EXISTS reason TEXT;

-- Embedded authorsRead[] on Mongo users
CREATE TABLE IF NOT EXISTS user_authors_read (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    author_name TEXT NOT NULL,
    books_read INTEGER NOT NULL DEFAULT 0,
    books_tbr INTEGER NOT NULL DEFAULT 0,
    favorite_book_id UUID REFERENCES books(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(user_id, author_name)
);

CREATE INDEX IF NOT EXISTS idx_user_authors_read_user_id ON user_authors_read (user_id);

-- Full userpreferences documents (recommendation / onboarding ML data)
CREATE TABLE IF NOT EXISTS user_preferences_legacy (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    payload JSONB NOT NULL,
    mongo_updated_at TIMESTAMPTZ,
    migrated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_preferences_legacy_payload_gin
    ON user_preferences_legacy USING gin (payload);
