-- 000083_subscription_expired_status.up.sql
-- Subscription expiry sweeper (epic #1294 follow-up, #2158). Adds 'expired' to the
-- subscriptions.status enum so the expiry sweeper can flip a lapsed code grant to a
-- terminal, honest status. The flip is also the sweeper's idempotency marker: once a
-- row is 'expired' it leaves the ('active','trialing','past_due') set the sweep
-- selects on, so it is never re-notified. 'expired' is intentionally distinct from
-- 'canceled' (user-initiated) — natural period lapse vs. an explicit cancellation —
-- so future Stripe billing analytics (#1306) can tell them apart.
--
-- Additive enum change: no data migration, no rewrite of existing rows. The partial
-- unique index idx_subscriptions_user_active keys on ('active','trialing','past_due'),
-- so 'expired' rows fall outside it — a user whose grant expired can redeem a fresh
-- code (a NEW active row) with no unique-index conflict.
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('active', 'trialing', 'past_due', 'canceled', 'incomplete', 'expired'));
