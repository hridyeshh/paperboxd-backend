-- Retract diary text that was embedded despite being marked private.
--
-- Until the guards added alongside this migration, CreateDiaryEntry embedded
-- every entry with a book_id regardless of is_private, so private entries were
-- sent to Cohere and their text persisted here in cleartext embedding_text.
--
-- Order matters. Clearing the per-entry vectors first means the centroid
-- recompute below (ComputeAndSaveDiaryCentroid, which selects
-- `WHERE embedding IS NOT NULL`) naturally excludes them, so the aggregate is
-- rebuilt from the reader's remaining public entries rather than discarded.

UPDATE diary_entries
SET embedding = NULL,
    embedding_text = NULL
WHERE is_private = true
  AND (embedding IS NOT NULL OR embedding_text IS NOT NULL);

-- Drop the aggregate for every reader who had a private entry.
--
-- diary_embedding is the mean of up to 50 entries, so it cannot be surgically
-- edited to remove one contributor — and it cannot be left alone either, since
-- it still encodes the private text this migration is retracting.
--
-- Nulling is the only correct option here, and it is safe: the vector is
-- derived data, rebuilt from the reader's surviving public entries the next
-- time their diary centroid is computed. Until that runs they lose diary-based
-- personalization and fall back to the bookshelf signals, which is the right
-- trade against keeping private-derived vectors in the table.
--
-- Note: before the accompanying change to internal/cron/nightly.go, nothing in
-- production called ComputeAndSaveDiaryCentroid at all — the nightly job only
-- refreshed bookshelf signals, so a "backdate computed_at and let it rebuild"
-- strategy would have rebuilt nothing.

UPDATE user_signal_profiles
SET diary_embedding = NULL,
    computed_at = '1970-01-01'::timestamptz
WHERE user_id IN (
    SELECT DISTINCT user_id
    FROM diary_entries
    WHERE is_private = true
);
