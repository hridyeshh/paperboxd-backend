ALTER TABLE users ADD COLUMN IF NOT EXISTS onboarding_completed boolean NOT NULL DEFAULT false;

-- Mark existing users as onboarded so they don't see the mobile onboarding flow.
-- Criteria: has logged any books, has genre preferences, or has a display name set.
UPDATE users
SET onboarding_completed = true
WHERE deleted_at IS NULL
  AND (
    books_read_count > 0
    OR array_length(favorite_genres, 1) > 0
    OR (name IS NOT NULL AND name <> '')
  );
