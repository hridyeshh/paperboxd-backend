-- Two clocks on the taste profile.
--
-- Phase 0 of the roadmap draws the line: long-term taste is what a reader
-- generally likes, recent taste is what they have been gravitating toward
-- lately, and the recommendation is a product of both. Until now the trait
-- profile had one clock. A reader whose last ten books were all bleak would
-- still be served their five-year average, and "you've been reading heavier
-- lately, maybe you need this" was a sentence the dashboard could say but the
-- ranking could not act on.
--
-- trait_recent is the same shape as trait_prefs, computed over the last 90
-- days only. Stored separately rather than blended in, because the blend
-- weight is a ranking decision and belongs in code, not baked into the row.
ALTER TABLE user_signal_profiles
    ADD COLUMN IF NOT EXISTS trait_recent            jsonb,
    ADD COLUMN IF NOT EXISTS trait_recent_confidence jsonb;

INSERT INTO feature_flags (key, value, description)
VALUES ('recent_taste', 'false', 'Blend the last-90-day trait profile into ranking')
ON CONFLICT (key) DO NOTHING;
