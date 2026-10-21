-- 000180_stripe_customers.up.sql
-- Stripe recurring subscriptions, epic #2033 PR-1 (Refs #1305). Spec section 5.
--
-- Maps a user to the one Stripe Customer created for them. user_id is the
-- primary key (one Customer per user) and stripe_customer_id is UNIQUE (one
-- user per Customer), so a webhook resolves a Customer to exactly one user.
-- Checkout writes the row and stamps last_checkout_at; the drift worker's
-- discovery pass reads last_checkout_at to find subscriptions whose creation
-- webhooks never arrived (spec F12).
--
-- ON DELETE CASCADE: account erasure removes the mapping in its own
-- transaction. The Stripe-side Customer is erased through
-- billing_erasure_outbox (000182), which keeps no user id.
CREATE TABLE stripe_customers (
    user_id            UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    stripe_customer_id TEXT NOT NULL UNIQUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_checkout_at   TIMESTAMPTZ NULL
);
