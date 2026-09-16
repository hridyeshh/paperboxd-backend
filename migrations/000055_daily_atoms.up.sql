-- Paperboxd Daily: the editorial half of the home page.
--
-- An atom is one small, finite thing to read — a question, an idea, a rabbit
-- hole through several books, or a public-domain passage. Atoms are written
-- by people and seeded from content/daily/atoms.json (cmd/seed-daily); none
-- is generated at request time. Every atom names where it came from
-- (source_kind) and the books it is about, so the card can say "From: ..."
-- and mean it.
--
-- Reader thoughts and the personal pick are not stored here: they come live
-- from thoughts and the recommendation pool when the feed is assembled.
CREATE TABLE IF NOT EXISTS daily_atoms (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug         text NOT NULL UNIQUE,
    kind         text NOT NULL CHECK (kind IN ('question', 'idea', 'rabbit_hole', 'passage')),
    -- The hook line on the card.
    title        text NOT NULL,
    -- One sentence under the title.
    dek          text,
    -- Markdown. The two-to-five minute read behind the card.
    body         text NOT NULL,
    read_seconds integer NOT NULL CHECK (read_seconds > 0),
    source_kind  text NOT NULL CHECK (source_kind IN ('editorial', 'public_domain', 'author_interview', 'readers')),
    source_note  text,
    source_url   text,
    -- Ordered [{"id": "<book uuid>", "note": "why this book is here"}]. For a
    -- rabbit hole the order is the path.
    books        jsonb NOT NULL DEFAULT '[]',
    -- Lowercase subject words matched loosely against a reader's genres.
    topics       text[] NOT NULL DEFAULT '{}',
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published')),
    created_at   timestamptz NOT NULL DEFAULT NOW(),
    updated_at   timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_daily_atoms_published
    ON daily_atoms (kind, created_at) WHERE status = 'published';

INSERT INTO feature_flags (key, value, description)
VALUES ('daily', 'false', 'Paperboxd Daily atoms on the home feed')
ON CONFLICT (key) DO NOTHING;
