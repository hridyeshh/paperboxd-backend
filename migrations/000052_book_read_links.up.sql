-- Read Now: where a reader can get the text of a book.
--
-- One row per book, filled on first detail view (store links) and by the
-- nightly Gutenberg backfill (public-domain text). Each source has its own
-- *_checked_at: NULL means not yet answered, and a definitive "not carried"
-- (link NULL, checked_at set) is cached independently of the other sources,
-- so one failing lookup never discards another's answer. Amazon and WorldCat
-- links are built from the ISBN at response time and are not stored. Store
-- lookups are India-only (country=IN) for now.

CREATE TABLE IF NOT EXISTS book_read_links (
    book_id                uuid PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,
    google_play_buy_link   text,
    google_play_checked_at timestamptz,
    apple_books_url        text,
    apple_books_checked_at timestamptz,
    is_public_domain       boolean NOT NULL DEFAULT FALSE,
    gutenberg_id           integer,
    gutenberg_html_url     text,
    gutenberg_epub_url     text,
    gutenberg_checked_at   timestamptz,
    updated_at             timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_book_read_links_public_domain
    ON book_read_links (is_public_domain) WHERE is_public_domain;
