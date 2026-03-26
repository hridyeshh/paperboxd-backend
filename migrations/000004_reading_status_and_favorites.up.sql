-- Extend bookshelf for TBR features
ALTER TABLE bookshelf
ADD COLUMN tbr_notes TEXT,
ADD COLUMN tbr_priority VARCHAR(10) CHECK (tbr_priority IN ('high', 'medium', 'low')),
ADD COLUMN tbr_added_at TIMESTAMP;

-- Extend bookshelf for Currently Reading progress tracking
ALTER TABLE bookshelf
ADD COLUMN current_page INTEGER CHECK (current_page >= 0),
ADD COLUMN reading_velocity FLOAT8 CHECK (reading_velocity >= 0),
ADD COLUMN estimated_finish_date DATE;

-- Create favorites table (max 4 per user, Letterboxd-style)
CREATE TABLE favorites (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    display_order INTEGER NOT NULL CHECK (display_order BETWEEN 1 AND 4),
    favorite_note TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE(user_id, book_id),
    UNIQUE(user_id, display_order)
);

CREATE INDEX idx_favorites_user_id ON favorites(user_id);
CREATE INDEX idx_favorites_book_id ON favorites(book_id);
CREATE INDEX idx_favorites_display_order ON favorites(user_id, display_order);

-- Add favorites_count to users
ALTER TABLE users
ADD COLUMN favorites_count INTEGER NOT NULL DEFAULT 0;
