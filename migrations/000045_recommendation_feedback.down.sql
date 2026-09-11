DELETE FROM feature_flags WHERE key = 'negative_taste';
ALTER TABLE user_signal_profiles DROP COLUMN IF EXISTS depth_signal;
ALTER TABLE recommendation_impressions
    DROP COLUMN IF EXISTS dismissed_until,
    DROP COLUMN IF EXISTS dismissed_forever;
DROP TABLE IF EXISTS recommendation_feedback;
