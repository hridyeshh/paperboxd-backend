-- Messages: 1:1 DMs where Paperboxd objects (books, lists, thoughts, profiles,
-- Fusions) ride along as attachments. See docs/MESSAGES.md.
--
-- One conversation per pair, stored one direction (user_a < user_b) like
-- fusions and taste_overlap. Per-reader state (inbox vs request, read marker,
-- mute, "delete chat") lives on conversation_members.

CREATE TABLE IF NOT EXISTS conversations (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_a          uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_b          uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Who sent the first message; the new-conversations-per-day cap counts it.
    created_by      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT NOW(),
    last_message_id bigint,
    last_message_at timestamptz,

    UNIQUE (user_a, user_b),
    CHECK (user_a < user_b)
);

CREATE INDEX IF NOT EXISTS idx_conversations_b ON conversations(user_b);
CREATE INDEX IF NOT EXISTS idx_conversations_created_by ON conversations(created_by, created_at DESC);

CREATE TABLE IF NOT EXISTS conversation_members (
    conversation_id      uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    user_id              uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- 'request' until the reader accepts or replies. A reader who follows the
    -- sender starts 'accepted'.
    state                text NOT NULL DEFAULT 'accepted' CHECK (state IN ('accepted', 'request')),
    last_read_message_id bigint NOT NULL DEFAULT 0,
    muted                boolean NOT NULL DEFAULT false,
    -- "Delete chat" hides everything up to here for this reader only.
    cleared_at           timestamptz,

    PRIMARY KEY (conversation_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_conversation_members_user ON conversation_members(user_id, state);

CREATE TABLE IF NOT EXISTS messages (
    id              bigserial PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    sender_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    body            text NOT NULL DEFAULT '' CHECK (char_length(body) <= 2000),
    -- Client-generated per send so a retried POST returns the first message.
    client_id       uuid,
    created_at      timestamptz NOT NULL DEFAULT NOW(),
    -- Unsend: body is wiped and attachments deleted; the row stays as a marker.
    deleted_at      timestamptz
);

CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_client_id
    ON messages(conversation_id, client_id) WHERE client_id IS NOT NULL;

-- snapshot is the card as it looked when sent, built server side, so a card
-- still renders after the object is edited, deleted or made private. Opening
-- it re-checks access against the live object.
CREATE TABLE IF NOT EXISTS message_attachments (
    id              bigserial PRIMARY KEY,
    message_id      bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    kind            text NOT NULL CHECK (kind IN ('book', 'list', 'thought', 'profile', 'fusion')),
    ref_id          text NOT NULL,
    snapshot        jsonb NOT NULL DEFAULT '{}',
    position        smallint NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_message_attachments_message ON message_attachments(message_id);
-- The Shared tab: everything sent in a conversation, by kind, newest first.
CREATE INDEX IF NOT EXISTS idx_message_attachments_shared ON message_attachments(conversation_id, kind, id DESC);
