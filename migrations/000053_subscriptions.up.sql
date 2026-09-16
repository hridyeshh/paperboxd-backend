-- Plus: the store subscription that currently backs a reader's entitlement.
--
-- One row per user. store_id is the store's own stable id for the
-- subscription (Apple originalTransactionId, Google purchaseToken) — unique
-- per store so one receipt can never unlock two accounts. Renewals and
-- cancellations arrive by webhook keyed on store_id, not user. Entitlement
-- is simply expires_at > now(); nothing else is consulted.

CREATE TABLE IF NOT EXISTS subscriptions (
    user_id     uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    store       text NOT NULL CHECK (store IN ('apple', 'google')),
    store_id    text NOT NULL,
    product_id  text NOT NULL,
    expires_at  timestamptz NOT NULL,
    auto_renew  boolean NOT NULL DEFAULT TRUE,
    environment text NOT NULL DEFAULT 'production',
    created_at  timestamptz NOT NULL DEFAULT NOW(),
    updated_at  timestamptz NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_store_id
    ON subscriptions (store, store_id);
