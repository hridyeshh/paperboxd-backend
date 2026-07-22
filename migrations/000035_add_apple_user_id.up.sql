-- Stable Sign in with Apple subject (the `sub` claim). Apple only sends the
-- email claim on the FIRST authorization, so email is unreliable as a join key;
-- the subject is stable across every sign-in. Nullable because only Apple users
-- carry one. Partial unique index allows many NULLs but one row per Apple sub.
ALTER TABLE users ADD COLUMN IF NOT EXISTS apple_user_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_apple_user_id
    ON users (apple_user_id)
    WHERE apple_user_id IS NOT NULL;
