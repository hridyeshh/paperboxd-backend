-- "Diary" became "Thoughts" in every client long ago; the schema kept the old
-- word, so every new feature had to translate between the two. This renames
-- the tables, columns, indexes and the string values stored in rows.
--
-- Constraint names are Postgres-generated, so each rename is guarded: a
-- database whose names differ keeps them rather than failing the boot.

ALTER TABLE diary_entries      RENAME TO thoughts;
ALTER TABLE diary_entry_likes  RENAME TO thought_likes;
ALTER TABLE thought_likes      RENAME COLUMN entry_id TO thought_id;
ALTER TABLE activities         RENAME COLUMN entry_id TO thought_id;
ALTER TABLE users              RENAME COLUMN diary_entries_count TO thoughts_count;
ALTER TABLE leaderboard_stats  RENAME COLUMN diary_entries TO thoughts;
ALTER TABLE leaderboard_stats  RENAME COLUMN diary_rank TO thoughts_rank;
ALTER TABLE user_signal_profiles RENAME COLUMN diary_signal TO thought_signal;
ALTER TABLE user_signal_profiles RENAME COLUMN diary_embedding TO thought_embedding;

ALTER INDEX IF EXISTS idx_diary_entries_user_id        RENAME TO idx_thoughts_user_id;
ALTER INDEX IF EXISTS idx_diary_entries_book_id        RENAME TO idx_thoughts_book_id;
ALTER INDEX IF EXISTS idx_diary_entries_created_at     RENAME TO idx_thoughts_created_at;
ALTER INDEX IF EXISTS idx_diary_entries_user_embedding RENAME TO idx_thoughts_user_embedding;
ALTER INDEX IF EXISTS idx_diary_entry_likes_user_id    RENAME TO idx_thought_likes_user_id;
ALTER INDEX IF EXISTS idx_diary_entry_likes_entry_id   RENAME TO idx_thought_likes_thought_id;
ALTER INDEX IF EXISTS idx_leaderboard_diary_entries    RENAME TO idx_leaderboard_thoughts;

DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT * FROM (VALUES
        ('activities',    'activities_entry_id_fkey',               'activities_thought_id_fkey'),
        ('thought_likes', 'diary_entry_likes_entry_id_fkey',        'thought_likes_thought_id_fkey'),
        ('thought_likes', 'diary_entry_likes_pkey',                 'thought_likes_pkey'),
        ('thought_likes', 'diary_entry_likes_user_id_entry_id_key', 'thought_likes_user_id_thought_id_key'),
        ('thought_likes', 'diary_entry_likes_user_id_fkey',         'thought_likes_user_id_fkey'),
        ('thoughts',      'diary_entries_book_id_fkey',             'thoughts_book_id_fkey'),
        ('thoughts',      'diary_entries_pkey',                     'thoughts_pkey'),
        ('thoughts',      'diary_entries_rating_check',             'thoughts_rating_check'),
        ('thoughts',      'diary_entries_user_id_fkey',             'thoughts_user_id_fkey')
    ) AS t(tbl, old_name, new_name)
    LOOP
        IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = r.old_name AND conrelid = r.tbl::regclass) THEN
            EXECUTE format('ALTER TABLE %I RENAME CONSTRAINT %I TO %I', r.tbl, r.old_name, r.new_name);
        END IF;
    END LOOP;
END $$;

UPDATE activities SET activity_type = 'created_thought' WHERE activity_type = 'created_diary_entry';
UPDATE activities SET activity_type = 'liked_thought'   WHERE activity_type = 'liked_diary_entry';

UPDATE events SET event_type = 'thought_created' WHERE event_type = 'diary_entry_created';
UPDATE events SET event_type = 'thought_liked'   WHERE event_type = 'diary_entry_liked';

UPDATE xp_transactions SET action_type = 'thought'       WHERE action_type = 'diary_entry';
UPDATE xp_transactions SET action_type = 'thought_liked' WHERE action_type = 'diary_liked';

UPDATE reports SET content_type = 'thought' WHERE content_type = 'diary_entry';
