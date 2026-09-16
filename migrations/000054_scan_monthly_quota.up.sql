-- Scan & Know quota becomes monthly: 3 free scans per calendar month, Plus
-- unlimited. scan_period is the month the current scan_uses_remaining belongs
-- to; the scan handler tops the count back up when it sees a new month.
-- Existing readers keep whatever lifetime count they have until their first
-- scan next month.
ALTER TABLE users
    ALTER COLUMN scan_uses_remaining SET DEFAULT 3,
    ADD COLUMN IF NOT EXISTS scan_period date NOT NULL DEFAULT date_trunc('month', now())::date;
