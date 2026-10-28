ALTER TABLE subscriptions
    ADD COLUMN stripe_unit_amount BIGINT CHECK (stripe_unit_amount >= 0),
    ADD COLUMN stripe_schedule_id TEXT,
    ADD COLUMN pending_plan VARCHAR(16) CHECK (pending_plan IN ('supersonic', 'wing')),
    ADD COLUMN pending_billing_interval VARCHAR(8) CHECK (pending_billing_interval IN ('month', 'year')),
    ADD COLUMN pending_change_at TIMESTAMPTZ;

COMMENT ON COLUMN subscriptions.stripe_unit_amount IS
    'Subscriber actual USD price in cents after the founders discount, refreshed from Stripe by Reconcile.';
COMMENT ON COLUMN subscriptions.stripe_schedule_id IS
    'Stripe schedule currently managing this subscription, including its booked next-period change.';
