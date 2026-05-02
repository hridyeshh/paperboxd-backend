DROP INDEX IF EXISTS idx_account_deletions_deleted_at;
ALTER TABLE account_deletions DROP COLUMN IF EXISTS user_id;
