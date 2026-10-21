-- 000178_subscriptions_stripe_billing.up.sql
-- Stripe recurring subscriptions, epic #2033 PR-1 (Refs #1305). Spec:
-- [internal]specs/2026-10-15-2033-stripe-billing-design.md section 5.
--
-- 1. Widen the status CHECK to every Stripe subscription status. The live set
--    (active, trialing, past_due) is unchanged and the three new values sit
--    outside it, so a row Stripe reports as incomplete_expired, unpaid or
--    paused entitles nobody.
-- 2. Add the plan, billing interval, founders-rate flag and Stripe's
--    flexible-billing cancel_at. Only the Stripe Reconcile path writes them.
-- 3. Pin a Stripe row's shape: a stripe row carries both Stripe ids and a
--    period end; any other row carries at most one Stripe id.
--
-- Additive. founders_rate has a constant DEFAULT, which PostgreSQL 16 applies
-- as a metadata-only change. The two shape CHECKs validate against rows that
-- are all code-sourced today: no production path has ever written a Stripe row.
-- A violating row aborts the file with an error that names the constraint.
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('active', 'trialing', 'past_due', 'canceled', 'incomplete', 'expired',
                      'incomplete_expired', 'unpaid', 'paused'));

ALTER TABLE subscriptions
    ADD COLUMN plan VARCHAR(16) NULL
        CONSTRAINT subscriptions_plan_check CHECK (plan IN ('supersonic', 'wing')),
    ADD COLUMN billing_interval VARCHAR(8) NULL
        CONSTRAINT subscriptions_billing_interval_check CHECK (billing_interval IN ('month', 'year')),
    ADD COLUMN founders_rate BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN cancel_at TIMESTAMPTZ NULL;

ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_stripe_identity_check
    CHECK ((source = 'stripe') = (stripe_subscription_id IS NOT NULL AND stripe_customer_id IS NOT NULL));

ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_stripe_period_check
    CHECK (source <> 'stripe' OR current_period_end IS NOT NULL);
