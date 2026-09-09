-- Irreversible by design. The up migration deletes leaked cleartext and the
-- vectors derived from it; there is nothing to restore, and restoring it would
-- reinstate the leak. Recomputing embeddings for still-private entries is
-- exactly what the accompanying handler guards now prevent.
SELECT 1;
