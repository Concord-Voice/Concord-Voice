-- 000193_seat_end_reason_pair_ineligible.up.sql
-- Wing seats the friendship no longer supports (follow-up to #4133).
--
-- Admits end_reason 'pair_ineligible': an unfriend or a block ended a pending
-- invite, or a block by either side ended an active seat. One value for every
-- case, so the row does not say whether the pair unfriended or blocked.
--
-- subscription_seats is small (at most one open seat per Wing subscription
-- plus its history), so the widened CHECK is added validated in one step
-- rather than the NOT VALID + VALIDATE split 000155/000156 use for
-- message_purges.
ALTER TABLE subscription_seats DROP CONSTRAINT subscription_seats_end_reason_check;

ALTER TABLE subscription_seats ADD CONSTRAINT subscription_seats_end_reason_check
    CHECK (end_reason IN ('payer_withdrew', 'payer_revoked', 'payer_swapped',
                          'holder_left', 'holder_subscribed', 'plan_changed',
                          'subscription_ended', 'declined', 'invite_expired',
                          'pair_ineligible'));
