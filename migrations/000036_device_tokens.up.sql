-- Push notification device tokens (FCM registration tokens for Android, APNs
-- device tokens for iOS). One row per physical device.
--
-- UNIQUE is on `token` alone, NOT (user_id, platform, token). A push token
-- identifies a device install, not a user: when someone signs out and a second
-- account signs in on the same phone, the provider hands us the SAME token for
-- the new user. Keying by (user_id, token) would leave the previous user's row
-- in place and that device would keep receiving their notifications. Keying by
-- token alone makes the upsert reassign ownership, so the handover is correct
-- even when the logout DELETE never lands (offline sign-out, app uninstall).
CREATE TABLE IF NOT EXISTS device_tokens (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform   TEXT        NOT NULL CHECK (platform IN ('android', 'ios')),
    token      TEXT        NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Send path is "load every token for this user, then dispatch by platform".
CREATE INDEX IF NOT EXISTS idx_device_tokens_user ON device_tokens (user_id);

-- Stale-token sweep (Phase 7) scans by last-seen.
CREATE INDEX IF NOT EXISTS idx_device_tokens_updated_at ON device_tokens (updated_at);
