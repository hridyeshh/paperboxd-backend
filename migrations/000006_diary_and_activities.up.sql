-- Diary entries table
CREATE TABLE diary_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id UUID REFERENCES books(id) ON DELETE SET NULL,
    title VARCHAR(100),
    content TEXT NOT NULL,
    is_private BOOLEAN NOT NULL DEFAULT false,
    rating INTEGER CHECK (rating BETWEEN 1 AND 5),
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_diary_entries_user_id ON diary_entries(user_id);
CREATE INDEX idx_diary_entries_book_id ON diary_entries(book_id);
CREATE INDEX idx_diary_entries_created_at ON diary_entries(user_id, created_at DESC);

-- Diary entry likes
CREATE TABLE diary_entry_likes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    entry_id UUID NOT NULL REFERENCES diary_entries(id) ON DELETE CASCADE,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE(user_id, entry_id)
);

CREATE INDEX idx_diary_entry_likes_user_id ON diary_entry_likes(user_id);
CREATE INDEX idx_diary_entry_likes_entry_id ON diary_entry_likes(entry_id);

-- Activities table (comprehensive tracking)
CREATE TABLE activities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    activity_type VARCHAR(50) NOT NULL,
    book_id UUID REFERENCES books(id) ON DELETE SET NULL,
    list_id UUID REFERENCES lists(id) ON DELETE CASCADE,
    entry_id UUID REFERENCES diary_entries(id) ON DELETE CASCADE,
    target_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    metadata JSONB,
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_activities_user_id ON activities(user_id);
CREATE INDEX idx_activities_created_at ON activities(created_at DESC);
CREATE INDEX idx_activities_type ON activities(activity_type);

-- Add diary_entries_count to users
ALTER TABLE users ADD COLUMN diary_entries_count INTEGER NOT NULL DEFAULT 0;
