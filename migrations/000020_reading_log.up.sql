CREATE TABLE IF NOT EXISTS reading_log (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id     UUID        NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    pages_delta INT         NOT NULL DEFAULT 0,
    logged_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_reading_log_user_logged_at
    ON reading_log (user_id, logged_at DESC);
