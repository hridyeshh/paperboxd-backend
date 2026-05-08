DROP INDEX IF EXISTS idx_users_referred_by;
DROP INDEX IF EXISTS idx_users_referral_code;

ALTER TABLE users DROP COLUMN IF EXISTS referral_rewards_claimed;
ALTER TABLE users DROP COLUMN IF EXISTS referral_count;
ALTER TABLE users DROP COLUMN IF EXISTS referred_by;
ALTER TABLE users DROP COLUMN IF EXISTS referral_code;
