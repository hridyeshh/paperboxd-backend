-- Remove indexes
DROP INDEX IF EXISTS idx_xp_transactions_user_date;
DROP INDEX IF EXISTS idx_xp_transactions_reference_id;
DROP INDEX IF EXISTS idx_xp_transactions_action_type;
DROP INDEX IF EXISTS idx_xp_transactions_created_at;
DROP INDEX IF EXISTS idx_xp_transactions_user_id;

-- Remove table
DROP TABLE IF EXISTS xp_transactions;
