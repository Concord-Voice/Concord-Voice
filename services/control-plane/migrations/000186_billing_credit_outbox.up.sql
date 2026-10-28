-- 000186_billing_credit_outbox.up.sql
-- Stripe recurring subscriptions, epic #2033 D1 (code credit). Spec §18.
--
-- A premium code redeemed on a renewing Stripe subscription becomes Stripe
-- customer-balance credit. The redemption commits the code claim, its ledger
-- row and one row here in one transaction; the Stripe balance transaction is
-- applied afterwards from this row (Idempotency-Key from redemption_id) and the
-- row is deleted once Stripe accepts it. No Stripe call runs inside the
-- redemption transaction, so a rollback can never leave credit behind.
--
-- The foreign key cascades: erasing the account deletes its ledger rows and
-- with them any credit not yet applied (the erasure outbox then deletes the
-- Customer). The worker backs off through attempts and next_attempt_at and
-- records an error class (VARCHAR(32)), never an error message.
CREATE TABLE billing_credit_outbox (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    redemption_id      UUID NOT NULL UNIQUE REFERENCES code_redemptions(id) ON DELETE CASCADE,
    stripe_customer_id TEXT NOT NULL,
    amount             BIGINT NOT NULL
                           CONSTRAINT billing_credit_outbox_amount_check CHECK (amount > 0),
    currency           VARCHAR(3) NOT NULL,
    description        TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts           INTEGER NOT NULL DEFAULT 0
                           CONSTRAINT billing_credit_outbox_attempts_check CHECK (attempts >= 0),
    next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error_class   VARCHAR(32) NULL
);
