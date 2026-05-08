-- Add referral columns to users table
ALTER TABLE users ADD COLUMN IF NOT EXISTS referral_code VARCHAR(10) UNIQUE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS referred_by UUID REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE users ADD COLUMN IF NOT EXISTS referral_count INTEGER DEFAULT 0;
-- TEXT[] is simpler than JSONB for sqlc compatibility with array operators
ALTER TABLE users ADD COLUMN IF NOT EXISTS referral_rewards_claimed TEXT[] DEFAULT '{}';

-- Indexes for referral code lookups
CREATE INDEX IF NOT EXISTS idx_users_referral_code ON users(referral_code);
CREATE INDEX IF NOT EXISTS idx_users_referred_by ON users(referred_by);

-- Generate referral codes for existing users (8-char uppercase alphanumeric)
UPDATE users
SET referral_code = UPPER(SUBSTR(MD5(RANDOM()::TEXT || id::TEXT), 1, 8))
WHERE referral_code IS NULL AND deleted_at IS NULL;
