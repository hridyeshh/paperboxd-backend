-- Stop retaining deleted users' email addresses in cleartext, indefinitely.
--
-- account_deletions is deliberately not FK-linked to users, so it survives the
-- 30-day hard purge in internal/cron/nightly.go. That made the privacy policy's
-- "permanently purged within 30 days" untrue: the address outlived the account
-- forever. The exit reasons are what retention analysis actually needs; the
-- address was never part of that.
--
-- Existing rows are hashed in place (SHA-256, same construction as the Go
-- side), so historical rows stay countable and de-duplicable without being
-- readable or reversible back to a person.

ALTER TABLE account_deletions ADD COLUMN IF NOT EXISTS email_hash TEXT;

UPDATE account_deletions
SET email_hash = encode(sha256(convert_to(lower(trim(email)), 'UTF8')), 'hex')
WHERE email_hash IS NULL AND email IS NOT NULL;

ALTER TABLE account_deletions DROP COLUMN IF EXISTS email;
ALTER TABLE account_deletions DROP COLUMN IF EXISTS username;
