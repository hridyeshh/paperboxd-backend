-- Private profiles: a follow of a private account becomes a pending request that
-- the target approves. Approved requests move into `follows`, so every existing
-- follow query stays correct without a status filter.

-- is_public was nullable with a default; a NULL reads as false in Go
-- (pgtype.Bool.Bool), which would silently make legacy rows private. Backfill
-- and lock it down before anything starts enforcing it.
UPDATE users SET is_public = true WHERE is_public IS NULL;
ALTER TABLE users ALTER COLUMN is_public SET DEFAULT true;
ALTER TABLE users ALTER COLUMN is_public SET NOT NULL;

CREATE TABLE follow_requests (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    requester_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT follow_requests_not_self UNIQUE (requester_id, target_id),
    CONSTRAINT follow_requests_distinct CHECK (requester_id <> target_id)
);

CREATE INDEX idx_follow_requests_target ON follow_requests(target_id, created_at DESC);
CREATE INDEX idx_follow_requests_requester ON follow_requests(requester_id);
