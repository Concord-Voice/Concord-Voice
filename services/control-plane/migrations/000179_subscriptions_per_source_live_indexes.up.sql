-- 000179_subscriptions_per_source_live_indexes.up.sql
-- Stripe recurring subscriptions, epic #2033 PR-1 (Refs #1305). Spec section 5.
--
-- 000070's idx_subscriptions_user_active allowed one live row per user across
-- every source. A user who holds a live code grant and then buys must hold
-- both rows, so uniqueness becomes per source:
--   * one live CODE row per user (the redemption grant extends it in place);
--   * stripe_subscription_id unique wherever set: the Reconcile upsert key.
--     An ON CONFLICT on it must repeat the WHERE predicate to infer the index;
--   * live Stripe rows by user (not unique: two live Stripe subscriptions are
--     legal, and both entitle);
--   * live Stripe rows by period end, for the drift worker's lapsed bucket;
--   * every row by user. Each index above is partial on a live status, so
--     none serves a lookup of all of a user's rows: the users ON DELETE
--     CASCADE (account erasure) and any history read. Stripe rows are never
--     reused, so the table grows, and without this index those lookups scan it.
-- The live set is ('active', 'trialing', 'past_due'), as in 000070 and the
-- entitlement readers.
--
-- Plain CREATE INDEX, not CONCURRENTLY: golang-migrate sends this file as one
-- implicit transaction, where CONCURRENTLY cannot run, and subscriptions holds
-- only code grants before GA, so the brief ACCESS EXCLUSIVE build is cheap.
DROP INDEX IF EXISTS idx_subscriptions_user_active;

CREATE UNIQUE INDEX idx_subscriptions_user_live_code
    ON subscriptions (user_id)
    WHERE source = 'code' AND status IN ('active', 'trialing', 'past_due');

CREATE UNIQUE INDEX idx_subscriptions_stripe_subscription_id
    ON subscriptions (stripe_subscription_id)
    WHERE stripe_subscription_id IS NOT NULL;

CREATE INDEX idx_subscriptions_user_live_stripe
    ON subscriptions (user_id)
    WHERE source = 'stripe' AND status IN ('active', 'trialing', 'past_due');

CREATE INDEX idx_subscriptions_stripe_live_period_end
    ON subscriptions (current_period_end)
    WHERE source = 'stripe' AND status IN ('active', 'trialing', 'past_due');

CREATE INDEX idx_subscriptions_user_id
    ON subscriptions (user_id);
