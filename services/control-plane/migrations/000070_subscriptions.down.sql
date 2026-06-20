-- 000070_subscriptions.down.sql
-- Reverses 000070. subscriptions is net-new in the paired up, so DROP TABLE is safe
-- per [internal]rules/migrations.md (no data migration required).
DROP INDEX IF EXISTS idx_subscriptions_stripe_customer;
DROP INDEX IF EXISTS idx_subscriptions_user_active;
DROP TABLE IF EXISTS subscriptions;
