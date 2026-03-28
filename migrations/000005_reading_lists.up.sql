-- Reading lists table
CREATE TABLE lists (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title VARCHAR(50) NOT NULL,
    description VARCHAR(200),
    is_private BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_lists_user_id ON lists(user_id);
CREATE INDEX idx_lists_created_at ON lists(user_id, created_at DESC);

-- Books in lists (many-to-many)
CREATE TABLE list_books (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    list_id UUID NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
    book_id UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    added_at TIMESTAMP NOT NULL DEFAULT NOW(),
    display_order INTEGER NOT NULL DEFAULT 0,
    UNIQUE(list_id, book_id)
);

CREATE INDEX idx_list_books_list_id ON list_books(list_id);
CREATE INDEX idx_list_books_book_id ON list_books(book_id);
CREATE INDEX idx_list_books_order ON list_books(list_id, display_order);

-- Access control for private lists
CREATE TABLE list_access (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    list_id UUID NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    granted_by UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    granted_at TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE(list_id, user_id)
);

CREATE INDEX idx_list_access_list_id ON list_access(list_id);
CREATE INDEX idx_list_access_user_id ON list_access(user_id);

-- Saved lists (users saving others' lists to their profile)
CREATE TABLE saved_lists (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    list_id UUID NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
    saved_at TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE(user_id, list_id)
);

CREATE INDEX idx_saved_lists_user_id ON saved_lists(user_id);
CREATE INDEX idx_saved_lists_list_id ON saved_lists(list_id);

-- Add lists_count to users
ALTER TABLE users ADD COLUMN lists_count INTEGER NOT NULL DEFAULT 0;
