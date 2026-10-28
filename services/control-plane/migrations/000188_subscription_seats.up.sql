-- 000188_subscription_seats.up.sql
-- Wing seats, epic #2033 PR-5 (#2470), spec §5 and §9.
--
-- One row per invite. A Wing payer's live Stripe subscription can carry at
-- most one pending or active seat; a user can hold at most one active seat.
-- The row never grants by itself: entitlements.ResolveTierQ derives premium
-- from an active seat only through a join to a live, premium, source='stripe'
-- plan='wing' parent, so a lapsed, paused or unpaid parent suspends the seat
-- without a write. Terminal states are bookkeeping, and the 30-day cooldown
-- reads them.
--
-- All three foreign keys cascade: erasing either party, or deleting the
-- parent row, deletes the seat (F34). Erasure only reads the rows first so it
-- can push the survivors afterwards.
CREATE TABLE subscription_seats (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id   UUID NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    payer_user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    holder_user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    state             VARCHAR(16) NOT NULL
                          CONSTRAINT subscription_seats_state_check
                          CHECK (state IN ('pending', 'active', 'declined', 'expired', 'revoked', 'left')),
    invited_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    invite_expires_at TIMESTAMPTZ NOT NULL,
    activated_at      TIMESTAMPTZ NULL,
    ended_at          TIMESTAMPTZ NULL,
    end_reason        VARCHAR(24) NULL
                          CONSTRAINT subscription_seats_end_reason_check
                          CHECK (end_reason IN ('payer_withdrew', 'payer_revoked', 'payer_swapped',
                                                'holder_left', 'holder_subscribed', 'plan_changed',
                                                'subscription_ended', 'declined', 'invite_expired')),
    CONSTRAINT subscription_seats_distinct_parties_check CHECK (payer_user_id <> holder_user_id),
    CONSTRAINT subscription_seats_pending_check
        CHECK (state <> 'pending' OR (activated_at IS NULL AND ended_at IS NULL AND end_reason IS NULL)),
    CONSTRAINT subscription_seats_active_check
        CHECK (state <> 'active' OR (activated_at IS NOT NULL AND ended_at IS NULL AND end_reason IS NULL)),
    CONSTRAINT subscription_seats_terminal_check
        CHECK (state IN ('pending', 'active') OR (ended_at IS NOT NULL AND end_reason IS NOT NULL))
);

-- One open seat per subscription, and one active seat per holder.
CREATE UNIQUE INDEX idx_subscription_seats_open_per_subscription
    ON subscription_seats (subscription_id) WHERE state IN ('pending', 'active');
CREATE UNIQUE INDEX idx_subscription_seats_active_holder
    ON subscription_seats (holder_user_id) WHERE state = 'active';
-- A holder's open invites (/subscriptions/me, Checkout's seat_member check).
CREATE INDEX idx_subscription_seats_pending_holder
    ON subscription_seats (holder_user_id) WHERE state = 'pending';
-- The payer's cooldown read.
CREATE INDEX idx_subscription_seats_payer_activated
    ON subscription_seats (payer_user_id, activated_at DESC) WHERE activated_at IS NOT NULL;
-- The drift worker's invite-expiry sweep.
CREATE INDEX idx_subscription_seats_pending_expiry
    ON subscription_seats (invite_expires_at) WHERE state = 'pending';
