-- The cleartext address and username are gone and cannot be recovered from the
-- hash. Restoring the columns gives back the shape, never the data.
ALTER TABLE account_deletions ADD COLUMN IF NOT EXISTS email TEXT;
ALTER TABLE account_deletions ADD COLUMN IF NOT EXISTS username TEXT;
ALTER TABLE account_deletions DROP COLUMN IF EXISTS email_hash;
