-- 000182_billing_erasure_outbox.up.sql
-- Stripe recurring subscriptions, epic #2033 PR-1 (Refs #1305). Spec sections 5
-- and 10.
--
-- Durable obligations to erase a Stripe Customer once its local account is
-- gone. Account erasure (users.deleteAccountTx, from PR-1) and
-- Checkout's 23503 path enqueue one row per Customer with
-- ON CONFLICT (stripe_customer_id) DO NOTHING. PR-2's worker deletes the
-- Customer at Stripe, backing off through attempts and next_attempt_at, and
-- records an error class (VARCHAR(32)), never an error message.
--
-- No user_id and no foreign key, on purpose: an obligation must outlive the
-- users row it came from and must not keep a user identifier after erasure.
CREATE TABLE billing_erasure_outbox (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    stripe_customer_id TEXT NOT NULL UNIQUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts           INTEGER NOT NULL DEFAULT 0
                           CONSTRAINT billing_erasure_outbox_attempts_check CHECK (attempts >= 0),
    next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error_class   VARCHAR(32) NULL
);
