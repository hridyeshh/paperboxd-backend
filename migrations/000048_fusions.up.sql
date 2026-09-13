-- Fusion: two readers' shelves side by side, made from a one-time link.
--
-- A reader creates an invite; the first other reader to accept it becomes the
-- other half of the Fusion. The link is the consent: nothing is computed and
-- nobody's ratings are shown to anyone until the invitee taps Fuse.

CREATE TABLE IF NOT EXISTS fusion_invites (
    token        text PRIMARY KEY,
    inviter_id   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT NOW(),
    expires_at   timestamptz NOT NULL,
    cancelled_at timestamptz,
    -- consumed_at is the "used" marker. consumed_by can go NULL when that
    -- reader deletes their account; the link must stay spent regardless.
    consumed_at  timestamptz,
    consumed_by  uuid REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_fusion_invites_inviter ON fusion_invites(inviter_id, created_at DESC);

-- One Fusion per pair, stored one direction (user_a < user_b) like
-- taste_overlap. snapshot holds the rendered story from each side
-- ({"a": ..., "b": ...}) because the sentences are perspective-dependent.
CREATE TABLE IF NOT EXISTS fusions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_a      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_b      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    inviter_id  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT NOW(),
    snapshot    jsonb,
    computed_at timestamptz,

    UNIQUE (user_a, user_b),
    CHECK (user_a < user_b)
);

CREATE INDEX IF NOT EXISTS idx_fusions_b ON fusions(user_b);
