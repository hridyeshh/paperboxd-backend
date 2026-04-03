-- Tables required by the MongoDB → PostgreSQL migration script.

-- Newsletter subscribers
CREATE TABLE IF NOT EXISTS newsletters (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email       VARCHAR(255) NOT NULL UNIQUE,
    is_active   BOOLEAN NOT NULL DEFAULT true,
    source      VARCHAR(50) NOT NULL DEFAULT 'footer',
    subscribed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    unsubscribed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_newsletters_email ON newsletters(email);
CREATE INDEX IF NOT EXISTS idx_newsletters_active ON newsletters(is_active) WHERE is_active = true;

-- Account deletion audit log
CREATE TABLE IF NOT EXISTS account_deletions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email       VARCHAR(255) NOT NULL,
    username    VARCHAR(50),
    reasons     TEXT[] NOT NULL DEFAULT '{}',
    deleted_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_account_deletions_email ON account_deletions(email);
