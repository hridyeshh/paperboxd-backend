-- Explicit negative taste.
--
-- Until now the only thing a reader could tell the engine was a rating, which
-- requires having read the book. Everything that happens *before* that -- "not
-- this one", "not right now", "already read it" -- was either invisible or
-- collapsed into recommendation_impressions.seen_count, where a deliberate
-- dismissal looked identical to having scrolled past something three times.
--
-- The reason codes are the valuable half. "Not for me" narrows a candidate
-- pool slightly; "not for me, too slow" is a measurement on one axis, and lets
-- the engine learn the distinction the roadmap asks for: not "dislikes
-- fantasy" but "dislikes high-worldbuilding fantasy".
CREATE TABLE IF NOT EXISTS recommendation_feedback (
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id     uuid NOT NULL REFERENCES books(id) ON DELETE CASCADE,

    -- loved | maybe | not_for_me | already_read | not_now
    verdict     text NOT NULL,

    -- Optional refinement of a negative verdict. Validated in Go against
    -- service.FeedbackReasonCodes so adding one is a code change.
    reason_codes text[] NOT NULL DEFAULT '{}',

    -- Which recommendation reason was on the card when they rejected it. Lets
    -- /analytics/discovery show which *explanations* readers distrust, not just
    -- which books they skip.
    reason_type text,

    created_at  timestamptz NOT NULL DEFAULT NOW(),
    updated_at  timestamptz NOT NULL DEFAULT NOW(),

    -- One standing verdict per reader per book: a later answer replaces an
    -- earlier one rather than accumulating contradictions.
    PRIMARY KEY (user_id, book_id)
);

CREATE INDEX IF NOT EXISTS idx_rec_feedback_user_verdict
    ON recommendation_feedback(user_id, verdict);

-- suppress_until already exists on recommendation_impressions for the
-- "seen too often" case. A dismissal is a different thing with a different
-- lifetime, so it gets its own column rather than overloading that one:
-- "not now" should come back, "not for me" should not.
ALTER TABLE recommendation_impressions
    ADD COLUMN IF NOT EXISTS dismissed_until timestamptz,
    ADD COLUMN IF NOT EXISTS dismissed_forever boolean NOT NULL DEFAULT false;

-- Depth of engagement, derived nightly from bookshelf + reading progress.
-- Stored on the profile so ranking does not recompute it per request.
ALTER TABLE user_signal_profiles
    ADD COLUMN IF NOT EXISTS depth_signal jsonb;

INSERT INTO feature_flags (key, value, description)
VALUES ('negative_taste', 'false', 'Apply explicit and implicit negative signals to ranking')
ON CONFLICT (key) DO NOTHING;
