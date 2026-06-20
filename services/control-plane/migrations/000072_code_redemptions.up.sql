-- 000072_code_redemptions.up.sql
-- Redemption ledger (epic #1294, child #1295). UNIQUE(code_id,user_id) = a user redeems
-- any given code at most once (promo per-user dedup). FKs reference redemption_codes
-- (000071) and subscriptions (000070), both created earlier. Net-new — DROP is safe.
CREATE TABLE code_redemptions (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code_id                   UUID NOT NULL REFERENCES redemption_codes(id) ON DELETE CASCADE,
    user_id                   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    resulting_subscription_id UUID REFERENCES subscriptions(id) ON DELETE SET NULL,
    redeemed_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (code_id, user_id)
);
