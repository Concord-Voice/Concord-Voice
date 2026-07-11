-- 000083_subscription_expired_status.down.sql
-- Reverse the 'expired' status addition. Any rows the sweeper already flipped to
-- 'expired' would violate the restored CHECK, so map them to 'canceled' (the closest
-- pre-existing terminal status) FIRST — a lapsed subscription reverting to a terminal
-- state is the honest reversal, and ResolveTier excludes both 'expired' and 'canceled'
-- (neither is in the live-status set) so no user's resolved tier changes.
UPDATE subscriptions SET status = 'canceled', updated_at = NOW() WHERE status = 'expired';
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('active', 'trialing', 'past_due', 'canceled', 'incomplete'));
