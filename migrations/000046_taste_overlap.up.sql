-- Reader-to-reader taste similarity.
--
-- "20,000 people liked this" is a fact about the world; "4 readers with your
-- taste loved this" is a fact about you. The second needs a number between
-- every pair of readers, and computing it per request is O(users × shelf) --
-- fine at forty readers, unacceptable at four thousand. So it is materialised
-- nightly, top-N per reader, and read as a lookup.
--
-- Stored one direction only (user_a < user_b) to halve the rows; readers are
-- looked up on either column.
CREATE TABLE IF NOT EXISTS taste_overlap (
    user_a uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_b uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- 0..1. Blend of shelf Jaccard, rating agreement on shared books, and
    -- trait-profile distance. See service.ComputeOverlap.
    overlap real NOT NULL,

    -- Evidence behind the number, so the client can render "you both loved"
    -- without a second query.
    shared_count  int NOT NULL DEFAULT 0,
    shared_loved  uuid[] NOT NULL DEFAULT '{}',   -- both rated 4+
    disagreed     uuid[] NOT NULL DEFAULT '{}',   -- rated ≥2 stars apart
    -- Books b loved that a has not shelved: the "steal their TBR" list.
    a_could_read  uuid[] NOT NULL DEFAULT '{}',
    b_could_read  uuid[] NOT NULL DEFAULT '{}',

    computed_at timestamptz NOT NULL DEFAULT NOW(),

    PRIMARY KEY (user_a, user_b),
    CHECK (user_a < user_b)
);

CREATE INDEX IF NOT EXISTS idx_taste_overlap_a ON taste_overlap(user_a, overlap DESC);
CREATE INDEX IF NOT EXISTS idx_taste_overlap_b ON taste_overlap(user_b, overlap DESC);

INSERT INTO feature_flags (key, value, description)
VALUES ('taste_twins', 'false', 'Expose taste overlap, twins and people-like-you')
ON CONFLICT (key) DO NOTHING;
