-- Keep the evidence check and column teardown atomic with respect to writers.
-- A hide or clear committed after an unlocked check must never be discarded.
LOCK TABLE dm_participants, dm_message_hidden_ranges IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'dm_participants'
          AND column_name = 'hidden_at'
    ) AND EXISTS (
        SELECT 1 FROM dm_participants WHERE hidden_at IS NOT NULL
    ) THEN
        RAISE EXCEPTION
            'cannot remove dm_participants.hidden_at while hide state exists';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'dm_message_hidden_ranges'
          AND column_name = 'includes_own'
    ) AND EXISTS (
        SELECT 1
        FROM dm_message_hidden_ranges
        WHERE includes_own IS TRUE
    ) THEN
        RAISE EXCEPTION
            'cannot remove dm_message_hidden_ranges.includes_own while clear state exists';
    END IF;
END
$$;

ALTER TABLE dm_message_hidden_ranges
    DROP COLUMN IF EXISTS includes_own;

ALTER TABLE dm_participants
    DROP COLUMN IF EXISTS hidden_at;
