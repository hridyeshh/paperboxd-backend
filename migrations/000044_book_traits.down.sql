DELETE FROM feature_flags WHERE key = 'trait_ranking';
ALTER TABLE user_signal_profiles
    DROP COLUMN IF EXISTS trait_prefs,
    DROP COLUMN IF EXISTS trait_dislikes,
    DROP COLUMN IF EXISTS trait_confidence;
DROP TABLE IF EXISTS book_traits;
