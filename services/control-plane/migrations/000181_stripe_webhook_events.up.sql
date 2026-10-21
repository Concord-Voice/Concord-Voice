-- 000181_stripe_webhook_events.up.sql
-- Stripe recurring subscriptions, epic #2033 PR-1 (Refs #1305). Spec section 5.
--
-- Webhook dedupe: one row per Stripe event id the webhook path has fully
-- processed. Reconcile is idempotent, so the table only saves repeat work; a
-- lost row costs a repeated Stripe fetch, never a wrong entitlement. No
-- payload column: a row holds the event id and type, never customer data.
-- Retention is 30 days; PR-2's prune deletes by processed_at, hence the index.
--
-- The event_id CHECK escapes the underscore ('evt\_%'). In LIKE, a bare '_'
-- matches any one character, so 'evtX...' would pass without the escape.
CREATE TABLE stripe_webhook_events (
    event_id     TEXT PRIMARY KEY
                     CONSTRAINT stripe_webhook_events_event_id_check CHECK (event_id LIKE 'evt\_%'),
    event_type   TEXT NOT NULL
                     CONSTRAINT stripe_webhook_events_event_type_check CHECK (char_length(event_type) <= 128),
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_stripe_webhook_events_processed_at
    ON stripe_webhook_events (processed_at);
