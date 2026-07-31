-- Fail closed with a diagnosable message. Without this block a surviving row
-- aborts the ADD CONSTRAINT below with Postgres's opaque "check constraint
-- ... is violated by some row", which names neither the column nor the value.
--
-- No such row should exist anywhere: the Kickstarter campaign was all-or-nothing
-- and closed unfunded, so no funds were collected and no entitlement was ever
-- issued. This migration deliberately does NOT rewrite entitlement rows — if the
-- premise is wrong that is an operator decision, not something to coerce silently.
DO $$
DECLARE
    legacy_rows BIGINT;
BEGIN
    SELECT COUNT(*) INTO legacy_rows
      FROM subscriptions
     WHERE source = 'kickstarter';

    IF legacy_rows > 0 THEN
        RAISE EXCEPTION
            'subscriptions.source still holds % kickstarter-sourced row(s); the campaign closed unfunded so none should exist. Investigate before migrating — this migration does not rewrite entitlement rows.',
            legacy_rows;
    END IF;
END $$;

ALTER TABLE subscriptions
    DROP CONSTRAINT subscriptions_source_check;

ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_source_check
    CHECK (source IN ('code', 'stripe'));
