-- Reconcile books.embedding with the model that actually writes it.
--
-- 000017 declared `books.embedding vector(384)` with the comment "384
-- dimensions for Cohere embed-english-v3.0". That model returns 1024. The 384
-- figure belongs to embed-english-light-v3.0, and in the Go code the constant
-- `cohereDims = 384` is only ever used by the Noop embedder.
--
-- The later signal columns were declared correctly: diary_entries.embedding,
-- user_signal_profiles.diary_embedding and .fast_finish_embedding are all
-- vector(1024) (000024, 000025). So on any database still honouring 000017 the
-- two halves disagree, and util.CosineSimilarity returns 0 whenever the lengths
-- differ -- which silently zeroes VelocityBoost and DiaryBoost inside scoreV2
-- and makes reason rules 3 and 4 unreachable.
--
-- Production evidently had books.embedding widened by hand (the backfill would
-- have failed on every row otherwise), so this is written to be a no-op there
-- and a repair anywhere else. That divergence is the real bug: a fresh database
-- built from migrations alone did not match production.
--
-- pgvector stores the declared dimension directly in atttypmod.
DO $$
DECLARE
    dims int;
BEGIN
    SELECT atttypmod INTO dims
    FROM pg_attribute
    WHERE attrelid = 'books'::regclass
      AND attname  = 'embedding'
      AND NOT attisdropped;

    IF dims IS NULL THEN
        RAISE NOTICE 'books.embedding does not exist; nothing to reconcile';
    ELSIF dims = 1024 THEN
        RAISE NOTICE 'books.embedding is already vector(1024); no change';
    ELSE
        RAISE NOTICE 'books.embedding is vector(%); rewriting to vector(1024)', dims;
        -- Vectors from a different model are not convertible, only discardable.
        -- Nulling embedding_text alongside them puts the rows back in the queue
        -- that GetBooksWithoutEmbeddings and GetBooksForEnrichment read from, so
        -- `cmd/backfill-embeddings` refills them at the right width.
        UPDATE books
        SET embedding = NULL, embedding_text = NULL
        WHERE embedding IS NOT NULL OR embedding_text IS NOT NULL;

        ALTER TABLE books ALTER COLUMN embedding TYPE vector(1024);
    END IF;
END $$;

-- Deliberately no ivfflat index on books.embedding (000017 left a TODO for one).
-- At the current corpus size an exact scan beats an approximate index on both
-- latency and recall, and an under-sized `lists` value loses candidates without
-- reporting anything. Add it when the table passes ~100k rows:
--   CREATE INDEX CONCURRENTLY books_embedding_idx
--     ON books USING ivfflat (embedding vector_cosine_ops) WITH (lists = 1000);
