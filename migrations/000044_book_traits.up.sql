-- Book characteristics: the substrate for "you like this *because*".
--
-- Until now a book's only machine-readable shape was `categories` (a coarse,
-- inconsistent genre list from three different metadata providers) and a 1024-d
-- embedding that is accurate but completely opaque -- you cannot read a reason
-- out of it, only a distance. So the reason engine could say "matches your
-- taste for Fiction" and nothing better, and the roadmap's central claim --
-- "you like historical fiction when it is character-driven, but not when it is
-- plot-dense" -- had no vocabulary to be expressed in.
--
-- The nine scalars are real columns because ranking reads them on every
-- candidate and the taste dashboard averages them; the open-ended parts
-- (moods, themes, setting) stay in jsonb because nothing filters on them yet.
--
-- Separate table rather than more columns on `books`: traits are derived, are
-- versioned by extractor, and get recomputed as a batch. `books` is already 30
-- columns of provider metadata and mixing the two makes it unclear which
-- fields are authoritative and which are inferred.
CREATE TABLE IF NOT EXISTS book_traits (
    book_id uuid PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,

    -- All scalars are 0..1 and directional; the comment names what 1.0 means.
    character_driven    real NOT NULL DEFAULT 0.5,  -- 1 = interior/character, 0 = plot engine
    emotional_intensity real NOT NULL DEFAULT 0.5,  -- 1 = devastating
    plot_intensity      real NOT NULL DEFAULT 0.5,  -- 1 = twisty, high-stakes
    pacing              real NOT NULL DEFAULT 0.5,  -- 1 = propulsive, 0 = slow burn
    prose_density       real NOT NULL DEFAULT 0.5,  -- 1 = dense literary prose
    narrative_complexity real NOT NULL DEFAULT 0.5, -- 1 = nonlinear, many POVs, demanding
    darkness            real NOT NULL DEFAULT 0.5,  -- 1 = bleak, 0 = cosy
    romance_centrality  real NOT NULL DEFAULT 0.0,  -- 1 = romance is the plot
    worldbuilding       real NOT NULL DEFAULT 0.0,  -- 1 = dense invented world

    -- moods, themes, tone, pov, setting, time_period, audience, is_series.
    traits jsonb NOT NULL DEFAULT '{}',

    -- 0..1. The extractor lowers this when the description was thin, so
    -- ranking can discount a guess instead of treating it as a measurement.
    confidence real NOT NULL DEFAULT 0.5,

    extractor_version int NOT NULL DEFAULT 1,
    extracted_at timestamptz NOT NULL DEFAULT NOW()
);

-- The backfill worker's queue: "books with a description and no current traits".
CREATE INDEX IF NOT EXISTS idx_book_traits_version
    ON book_traits(extractor_version, extracted_at);

-- Long-term taste on the interpretable axes, alongside the existing opaque
-- centroids. jsonb rather than columns because the axis list will grow and a
-- profile is always read whole.
ALTER TABLE user_signal_profiles
    ADD COLUMN IF NOT EXISTS trait_prefs      jsonb,
    ADD COLUMN IF NOT EXISTS trait_dislikes   jsonb,
    ADD COLUMN IF NOT EXISTS trait_confidence jsonb;

INSERT INTO feature_flags (key, value, description)
VALUES ('trait_ranking', 'false', 'Blend book_traits fit into recommendation ranking')
ON CONFLICT (key) DO NOTHING;
