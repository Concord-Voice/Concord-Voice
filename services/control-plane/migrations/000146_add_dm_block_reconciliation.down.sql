LOCK TABLE public.users, public.friendships, public.dm_participants IN SHARE ROW EXCLUSIVE MODE;
DO $$
BEGIN
    IF to_regclass('public.dm_block_reconciliations') IS NULL THEN
        RETURN;
    END IF;

    LOCK TABLE public.dm_block_reconciliations IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM public.dm_block_reconciliations) THEN
        RAISE EXCEPTION
            'refusing to roll back dm_block_reconciliations while evidence remains';
    END IF;

    DROP TRIGGER IF EXISTS stamp_dm_block_reconciliation_transaction ON public.dm_block_reconciliations;
    ALTER TABLE public.dm_block_reconciliations
        DROP COLUMN IF EXISTS reconciliation_transaction_id;
END
$$;

DROP TRIGGER IF EXISTS require_blocked_friendship_reconciliation ON public.friendships;
DROP FUNCTION IF EXISTS public.require_blocked_friendship_reconciliation();
DROP FUNCTION IF EXISTS public.stamp_dm_block_reconciliation_transaction();

DROP TRIGGER IF EXISTS reject_blocked_dm_participant ON public.dm_participants;
DROP TRIGGER IF EXISTS serialize_dm_blocked_friendship ON public.friendships;
DROP FUNCTION IF EXISTS public.serialize_dm_blocked_friendship();
DROP FUNCTION IF EXISTS public.reject_blocked_dm_participant();
DROP INDEX IF EXISTS idx_dm_block_reconciliations_due;
DROP TABLE IF EXISTS public.dm_block_reconciliations;
