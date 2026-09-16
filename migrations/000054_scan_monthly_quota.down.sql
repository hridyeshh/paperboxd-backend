ALTER TABLE users
    ALTER COLUMN scan_uses_remaining SET DEFAULT 7,
    DROP COLUMN IF EXISTS scan_period;
