-- 000070_subscriptions.up.sql
-- Subscription entitlements foundation (epic #1294, child #1295). Source of truth for
-- a user's tier; fed by Kickstarter redemption codes (Beta) and Stripe subs (v1.0).
-- Net-new table — DROP is safe in the paired down (no data migration needed).
CREATE TABLE subscriptions (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                 UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- tier is intentionally CHECK-free: spec §10 keeps it a bare VARCHAR so new tiers
    -- ('pro', 'team', ...) can be added without a migration. The For() resolver
    -- fail-closes any unknown tier to the free set, so an out-of-enum tier degrades
    -- to least privilege rather than escalating.
    tier                    VARCHAR(32) NOT NULL,            -- 'free' | 'premium' (extensible)
    -- status/source ARE bounded enums (Stripe-defined + our issuer set). The status
    -- CHECK is load-bearing: the partial unique index below keys on
    -- status IN ('active','trialing','past_due'), so an out-of-enum (typo'd) status
    -- would otherwise dodge the "one active subscription per user" guard.
    status                  VARCHAR(32) NOT NULL
                                CHECK (status IN ('active', 'trialing', 'past_due', 'canceled', 'incomplete')),
    source                  VARCHAR(32) NOT NULL
                                CHECK (source IN ('kickstarter', 'stripe', 'code')),
    stripe_customer_id      TEXT,
    stripe_subscription_id  TEXT,
    current_period_end      TIMESTAMPTZ,
    cancel_at_period_end    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One active subscription per user.
CREATE UNIQUE INDEX idx_subscriptions_user_active
    ON subscriptions(user_id)
    WHERE status IN ('active', 'trialing', 'past_due');

-- Partial: stripe_customer_id is NULL for every kickstarter/code-sourced (Beta) row,
-- and the only lookup (Stripe webhook customer -> subscription) queries non-NULL values.
CREATE INDEX idx_subscriptions_stripe_customer
    ON subscriptions(stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;
