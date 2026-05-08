-- XP transaction audit log
CREATE TABLE IF NOT EXISTS xp_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Action details
    action_type VARCHAR(50) NOT NULL,  -- 'book_read', 'diary_entry', 'list_created', etc.
    xp_amount INTEGER NOT NULL,

    -- Optional reference to related entity
    reference_id UUID,  -- book_id, diary_id, list_id, etc.

    -- Additional metadata (JSON for flexibility)
    metadata JSONB,

    -- Timestamp
    created_at TIMESTAMP DEFAULT NOW()
);

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_xp_transactions_user_id ON xp_transactions(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_xp_transactions_created_at ON xp_transactions(created_at);
CREATE INDEX IF NOT EXISTS idx_xp_transactions_action_type ON xp_transactions(action_type);
CREATE INDEX IF NOT EXISTS idx_xp_transactions_reference_id ON xp_transactions(reference_id);

-- Index for daily XP cap queries (user + date)
CREATE INDEX IF NOT EXISTS idx_xp_transactions_user_date ON xp_transactions(user_id, DATE(created_at));
