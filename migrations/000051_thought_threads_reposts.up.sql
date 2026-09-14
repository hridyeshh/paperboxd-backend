-- Threads: a thought can continue an earlier thought by the same author.
-- Threads are linear and flat — every follow-up points at the first thought,
-- and order is created_at — so reading a thread is one indexed range scan.
ALTER TABLE thoughts
    ADD COLUMN thread_root_id UUID REFERENCES thoughts(id) ON DELETE CASCADE;

CREATE INDEX idx_thoughts_thread_root
    ON thoughts(thread_root_id, created_at)
    WHERE thread_root_id IS NOT NULL;

-- Reposts: a reader puts someone else's public thought on their own profile.
CREATE TABLE thought_reposts (
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    thought_id UUID        NOT NULL REFERENCES thoughts(id) ON DELETE CASCADE,
    -- TIMESTAMP, not TIMESTAMPTZ: the profile feed sorts reposts and thoughts
    -- together, and thoughts.created_at is TIMESTAMP.
    created_at TIMESTAMP   NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, thought_id)
);

CREATE INDEX idx_thought_reposts_user_created ON thought_reposts(user_id, created_at DESC);
CREATE INDEX idx_thought_reposts_thought_id ON thought_reposts(thought_id);
