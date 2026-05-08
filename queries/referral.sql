-- name: GetUserReferralCode :one
SELECT id, username, referral_code, referral_count
FROM users
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserByReferralCode :one
SELECT id, username, email
FROM users
WHERE referral_code = $1 AND deleted_at IS NULL;

-- name: SetUserReferredBy :exec
UPDATE users
SET referred_by = $2
WHERE id = $1;

-- name: IncrementReferralCount :exec
UPDATE users
SET referral_count = COALESCE(referral_count, 0) + 1
WHERE id = $1;

-- name: HasClaimedReferralReward :one
SELECT $2::text = ANY(COALESCE(referral_rewards_claimed, '{}')) AS claimed
FROM users
WHERE id = $1;

-- name: MarkReferralRewardClaimed :exec
UPDATE users
SET referral_rewards_claimed = array_append(COALESCE(referral_rewards_claimed, '{}'), $2::text)
WHERE id = $1;

-- name: GetUserReferrals :many
SELECT id, username, email, created_at, total_xp, level, current_streak
FROM users
WHERE referred_by = $1 AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT $2;

-- name: GetReferralStats :one
SELECT
  COUNT(*)                                             AS total_referrals,
  COUNT(*) FILTER (WHERE COALESCE(total_xp, 0) > 0)   AS active_referrals,
  COUNT(*) FILTER (WHERE COALESCE(current_streak, 0) >= 30) AS long_term_referrals
FROM users
WHERE referred_by = $1 AND deleted_at IS NULL;
