DELETE FROM feature_flags WHERE key = 'recent_taste';
ALTER TABLE user_signal_profiles
    DROP COLUMN IF EXISTS trait_recent,
    DROP COLUMN IF EXISTS trait_recent_confidence;
