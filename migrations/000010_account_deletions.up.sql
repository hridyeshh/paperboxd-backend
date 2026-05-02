-- Extend account_deletions for live deletion tracking via DELETE /api/v1/users/me.
-- The table was created in 000007 for the MongoDB→Postgres migration script but
-- was never wired to the live API. This migration adds user_id (nullable because
-- rows inserted by the old migration script don't have one) and an index for the
-- "most recent deletions" access pattern used by retention analysis.
ALTER TABLE account_deletions ADD COLUMN IF NOT EXISTS user_id UUID;
CREATE INDEX IF NOT EXISTS idx_account_deletions_deleted_at ON account_deletions(deleted_at DESC);
