ALTER TABLE users DROP COLUMN IF EXISTS favorites_count;
DROP TABLE IF EXISTS favorites;
ALTER TABLE bookshelf DROP COLUMN IF EXISTS estimated_finish_date;
ALTER TABLE bookshelf DROP COLUMN IF EXISTS reading_velocity;
ALTER TABLE bookshelf DROP COLUMN IF EXISTS current_page;
ALTER TABLE bookshelf DROP COLUMN IF EXISTS tbr_added_at;
ALTER TABLE bookshelf DROP COLUMN IF EXISTS tbr_priority;
ALTER TABLE bookshelf DROP COLUMN IF EXISTS tbr_notes;
