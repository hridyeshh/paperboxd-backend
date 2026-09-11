-- Not reversible. Narrowing back to vector(384) would discard every embedding
-- the application can actually produce, which is strictly worse than leaving
-- the column at the width the embedding model writes.
SELECT 1;
