DROP INDEX IF EXISTS idx_users_apple_user_id;
ALTER TABLE users DROP COLUMN IF EXISTS apple_user_id;
