UPDATE reports SET content_type = 'diary_entry' WHERE content_type = 'thought';

UPDATE xp_transactions SET action_type = 'diary_liked' WHERE action_type = 'thought_liked';
UPDATE xp_transactions SET action_type = 'diary_entry' WHERE action_type = 'thought';

UPDATE events SET event_type = 'diary_entry_liked'   WHERE event_type = 'thought_liked';
UPDATE events SET event_type = 'diary_entry_created' WHERE event_type = 'thought_created';

UPDATE activities SET activity_type = 'liked_diary_entry'   WHERE activity_type = 'liked_thought';
UPDATE activities SET activity_type = 'created_diary_entry' WHERE activity_type = 'created_thought';

DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT * FROM (VALUES
        ('activities',    'activities_thought_id_fkey',           'activities_entry_id_fkey'),
        ('thought_likes', 'thought_likes_thought_id_fkey',        'diary_entry_likes_entry_id_fkey'),
        ('thought_likes', 'thought_likes_pkey',                   'diary_entry_likes_pkey'),
        ('thought_likes', 'thought_likes_user_id_thought_id_key', 'diary_entry_likes_user_id_entry_id_key'),
        ('thought_likes', 'thought_likes_user_id_fkey',           'diary_entry_likes_user_id_fkey'),
        ('thoughts',      'thoughts_book_id_fkey',                'diary_entries_book_id_fkey'),
        ('thoughts',      'thoughts_pkey',                        'diary_entries_pkey'),
        ('thoughts',      'thoughts_rating_check',                'diary_entries_rating_check'),
        ('thoughts',      'thoughts_user_id_fkey',                'diary_entries_user_id_fkey')
    ) AS t(tbl, old_name, new_name)
    LOOP
        IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = r.old_name AND conrelid = r.tbl::regclass) THEN
            EXECUTE format('ALTER TABLE %I RENAME CONSTRAINT %I TO %I', r.tbl, r.old_name, r.new_name);
        END IF;
    END LOOP;
END $$;

ALTER INDEX IF EXISTS idx_leaderboard_thoughts      RENAME TO idx_leaderboard_diary_entries;
ALTER INDEX IF EXISTS idx_thought_likes_thought_id  RENAME TO idx_diary_entry_likes_entry_id;
ALTER INDEX IF EXISTS idx_thought_likes_user_id     RENAME TO idx_diary_entry_likes_user_id;
ALTER INDEX IF EXISTS idx_thoughts_user_embedding   RENAME TO idx_diary_entries_user_embedding;
ALTER INDEX IF EXISTS idx_thoughts_created_at       RENAME TO idx_diary_entries_created_at;
ALTER INDEX IF EXISTS idx_thoughts_book_id          RENAME TO idx_diary_entries_book_id;
ALTER INDEX IF EXISTS idx_thoughts_user_id          RENAME TO idx_diary_entries_user_id;

ALTER TABLE user_signal_profiles RENAME COLUMN thought_embedding TO diary_embedding;
ALTER TABLE user_signal_profiles RENAME COLUMN thought_signal TO diary_signal;
ALTER TABLE leaderboard_stats  RENAME COLUMN thoughts_rank TO diary_rank;
ALTER TABLE leaderboard_stats  RENAME COLUMN thoughts TO diary_entries;
ALTER TABLE users              RENAME COLUMN thoughts_count TO diary_entries_count;
ALTER TABLE activities         RENAME COLUMN thought_id TO entry_id;
ALTER TABLE thought_likes      RENAME COLUMN thought_id TO entry_id;
ALTER TABLE thought_likes      RENAME TO diary_entry_likes;
ALTER TABLE thoughts           RENAME TO diary_entries;
