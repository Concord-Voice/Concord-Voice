-- 000193_seat_end_reason_pair_ineligible.down.sql
-- Guarded down for 000193 (the 000117 shape, schema-aware).
--
-- A 'pair_ineligible' row records a seat that a block or an unfriend ended,
-- and the 30-day cooldown reads it. Relabelling the row would rewrite history,
-- and deleting it would lift a cooldown, so the down refuses while one exists.
-- The ACCESS EXCLUSIVE lock is taken BEFORE the check, so a row that commits
-- while this file waits for the lock is seen.
--
-- Recovery after a refused down: golang-migrate records the TARGET version
-- (192) as dirty, but the schema is still at 193. Confirm that the constraint
-- admits 'pair_ineligible', then `force 193` (never 192) and boot.
DO $$
DECLARE
    migration_schema TEXT := current_schema();
    has_row BOOLEAN;
BEGIN
    IF to_regclass(format('%I.subscription_seats', migration_schema)) IS NULL THEN
        RETURN;
    END IF;

    EXECUTE format(
        'LOCK TABLE %I.subscription_seats IN ACCESS EXCLUSIVE MODE',
        migration_schema);

    EXECUTE format(
        'SELECT EXISTS (SELECT 1 FROM %I.subscription_seats WHERE end_reason = ''pair_ineligible'')',
        migration_schema)
        INTO has_row;
    IF has_row THEN
        RAISE EXCEPTION
            'cannot remove end_reason pair_ineligible while a seat carries it'
            USING ERRCODE = 'restrict_violation';
    END IF;

    EXECUTE format(
        'ALTER TABLE %I.subscription_seats DROP CONSTRAINT subscription_seats_end_reason_check',
        migration_schema);
    EXECUTE format(
        'ALTER TABLE %I.subscription_seats ADD CONSTRAINT subscription_seats_end_reason_check '
        'CHECK (end_reason IN (''payer_withdrew'', ''payer_revoked'', ''payer_swapped'', '
        '''holder_left'', ''holder_subscribed'', ''plan_changed'', '
        '''subscription_ended'', ''declined'', ''invite_expired''))',
        migration_schema);
END
$$;
