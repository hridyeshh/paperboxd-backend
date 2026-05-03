-- name: CreateOTP :one
INSERT INTO otp_codes (
    email, code_hash, expires_at, max_attempts
) VALUES (
    $1, $2, $3, $4
) RETURNING *;

-- name: GetOTPByEmail :one
SELECT * FROM otp_codes
WHERE email = $1
  AND used_at IS NULL
  AND expires_at > NOW()
ORDER BY created_at DESC
LIMIT 1;

-- name: IncrementOTPAttempts :exec
UPDATE otp_codes
SET attempts = attempts + 1
WHERE id = $1;

-- name: MarkOTPUsed :exec
UPDATE otp_codes
SET used_at = NOW()
WHERE id = $1;

-- name: DeleteOTPByEmail :exec
DELETE FROM otp_codes
WHERE email = $1;

-- name: CreateOTPWithMetadata :one
INSERT INTO otp_codes (
    email, code_hash, expires_at, max_attempts, metadata
) VALUES (
    $1, $2, $3, $4, $5
) RETURNING *;
