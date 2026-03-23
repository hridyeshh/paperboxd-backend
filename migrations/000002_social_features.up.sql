-- (user's book collections)
CREATE TABLE bookshelf (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,

    status VARCHAR(20) NOT NULL DEFAULT 'read',
    rating INTEGER CHECK (rating >= 1 AND rating <= 5),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE(user_id, book_id)
);

CREATE INDEX idx_bookshelf_user ON bookshelf(user_id);
CREATE INDEX idx_bookshelf_status ON bookshelf(user_id, status);
CREATE INDEX idx_bookshelf_finished ON bookshelf(finished_at DESC) WHERE status = 'read';

-- Likes
CREATE TABLE likes (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,

    created_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE(user_id, book_id)
);

CREATE INDEX idx_likes_user ON likes(user_id);
CREATE INDEX idx_likes_book ON likes(book_id);

-- Follows
CREATE TABLE follows (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    follower_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    following_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    created_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE(follower_id, following_id),
    CHECK (follower_id != following_id)
);

CREATE INDEX idx_follows_follower ON follows(follower_id);
CREATE INDEX idx_follows_following ON follows(following_id);

-- Update books table
ALTER TABLE books ADD COLUMN IF NOT EXISTS description TEXT;
ALTER TABLE books ADD COLUMN IF NOT EXISTS published_date DATE;
ALTER TABLE books ADD COLUMN IF NOT EXISTS page_count INTEGER;
ALTER TABLE books ADD COLUMN IF NOT EXISTS language VARCHAR(10);
ALTER TABLE books ADD COLUMN IF NOT EXISTS cover_url TEXT;
ALTER TABLE books ADD COLUMN IF NOT EXISTS categories TEXT[];

-- Triggers
CREATE TRIGGER bookshelf_updated_at BEFORE UPDATE ON bookshelf
    FOR EACH ROW EXECUTE FUNCTION update_updated_at();
