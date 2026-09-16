-- name: UpsertSubscription :one
-- Client path: a signed-in reader hands us a receipt. Conflict on user_id so a
-- plan change or a second store simply replaces what backs the entitlement.
-- The (store, store_id) unique index is deliberately NOT a conflict target:
-- a receipt already linked to another account must fail, not move.
INSERT INTO subscriptions (user_id, store, store_id, product_id, expires_at, auto_renew, environment)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id) DO UPDATE
SET store       = EXCLUDED.store,
    store_id    = EXCLUDED.store_id,
    product_id  = EXCLUDED.product_id,
    expires_at  = EXCLUDED.expires_at,
    auto_renew  = EXCLUDED.auto_renew,
    environment = EXCLUDED.environment,
    updated_at  = NOW()
RETURNING *;

-- name: UpdateSubscriptionByStoreID :one
-- Webhook path: the store tells us about a receipt, not a user. Returns the
-- owner so the analytics event can be attributed; ErrNoRows when nobody has
-- linked this receipt yet.
UPDATE subscriptions
SET product_id = $3,
    expires_at = $4,
    auto_renew = $5,
    updated_at = NOW()
WHERE store = $1 AND store_id = $2
RETURNING user_id;

-- name: GetSubscription :one
SELECT * FROM subscriptions
WHERE user_id = $1;
