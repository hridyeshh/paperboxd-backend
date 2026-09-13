-- Fuzzy book search (SearchBooksInDB): pg_trgm is already on; fuzzystrmatch
-- adds dmetaphone (sounds alike) and levenshtein (swapped letters).
CREATE EXTENSION IF NOT EXISTS fuzzystrmatch;
