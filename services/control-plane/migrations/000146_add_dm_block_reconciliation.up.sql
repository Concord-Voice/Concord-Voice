LOCK TABLE public.users, public.friendships, public.dm_participants IN SHARE ROW EXCLUSIVE MODE;

-- Durable, bounded evidence that a blocked friendship pair still needs DM
-- membership/key/pending-state reconciliation.
CREATE TABLE dm_block_reconciliations (
    user_a_id      UUID NOT NULL,
    user_b_id      UUID NOT NULL,
    operation_id   UUID NOT NULL,
    remove_a       BOOLEAN NOT NULL DEFAULT FALSE,
    remove_b       BOOLEAN NOT NULL DEFAULT FALSE,
    attempts       INTEGER NOT NULL DEFAULT 0,
    failure_class TEXT,
    reconcile_after TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reconciliation_transaction_id xid8,

    CONSTRAINT dm_block_reconciliations_pkey
        PRIMARY KEY (user_a_id, user_b_id),
    CONSTRAINT dm_block_reconciliations_user_a_fkey
        FOREIGN KEY (user_a_id) REFERENCES users(id) ON DELETE RESTRICT,
    CONSTRAINT dm_block_reconciliations_user_b_fkey
        FOREIGN KEY (user_b_id) REFERENCES users(id) ON DELETE RESTRICT,
    CONSTRAINT dm_block_reconciliations_operation_id_key
        UNIQUE (operation_id),
    CONSTRAINT dm_block_reconciliations_removal_side_check
        CHECK (remove_a OR remove_b),
    CONSTRAINT dm_block_reconciliations_pair_order_check
        CHECK (user_a_id < user_b_id),
    CONSTRAINT dm_block_reconciliations_attempts_check
        CHECK (attempts >= 0),
    CONSTRAINT dm_block_reconciliations_failure_class_check
        CHECK (failure_class IS NULL OR failure_class IN (
            'state_read', 'state_write', 'database', 'dependency', 'delivery'
        )),
    CONSTRAINT dm_block_reconciliations_updated_at_check
        CHECK (updated_at >= created_at)
);

CREATE INDEX idx_dm_block_reconciliations_due
    ON dm_block_reconciliations (reconcile_after, user_a_id, user_b_id);

COMMENT ON TABLE dm_block_reconciliations IS
    'Durable canonical-user-pair evidence for bounded DM block reconciliation. Rows remain until cleanup converges.';
COMMENT ON COLUMN dm_block_reconciliations.user_a_id IS
    'Lexicographically smaller user ID in the blocked friendship pair.';
COMMENT ON COLUMN dm_block_reconciliations.user_b_id IS
    'Lexicographically larger user ID in the blocked friendship pair.';
COMMENT ON COLUMN dm_block_reconciliations.operation_id IS
    'Unique block-operation generation used to fence stale acknowledgements and ambiguous commits.';
COMMENT ON COLUMN dm_block_reconciliations.remove_a IS
    'Whether bounded cleanup must remove the lexicographically smaller user from affected conversations.';
COMMENT ON COLUMN dm_block_reconciliations.remove_b IS
    'Whether bounded cleanup must remove the lexicographically larger user from affected conversations.';
COMMENT ON COLUMN dm_block_reconciliations.attempts IS
    'Number of bounded cleanup attempts; evidence is retained when an attempt fails.';
COMMENT ON COLUMN dm_block_reconciliations.failure_class IS
    'Closed diagnostic vocabulary for the most recent failed cleanup attempt.';
COMMENT ON COLUMN dm_block_reconciliations.reconcile_after IS
    'Earliest time at which the next bounded cleanup attempt may claim this pair.';
COMMENT ON COLUMN dm_block_reconciliations.created_at IS
    'Time the durable block-cleanup obligation was recorded.';
COMMENT ON COLUMN dm_block_reconciliations.updated_at IS
    'Time the obligation or its retry metadata was last changed.';
COMMENT ON COLUMN dm_block_reconciliations.reconciliation_transaction_id IS
    'Top-level transaction that most recently recorded directional block reconciliation evidence.';

-- This is the database backstop for every writer, including old binaries and
-- direct SQL. Lock every subject user in database order before checking the
-- freshly visible friendship graph, so a concurrent block cannot commit after
-- the check and before a participant row is inserted.
CREATE FUNCTION public.reject_blocked_dm_participant()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM 1
    FROM public.users u
    WHERE u.id IN (
        SELECT dp.user_id
        FROM public.dm_participants dp
        WHERE dp.conversation_id = NEW.conversation_id
        UNION
        SELECT NEW.user_id
    )
    ORDER BY u.id
    FOR SHARE;

    PERFORM 1 FROM public.dm_conversations
    WHERE id = NEW.conversation_id
    FOR NO KEY UPDATE;

    IF EXISTS (
        WITH subjects AS (
            SELECT dp.user_id
            FROM public.dm_participants dp
            WHERE dp.conversation_id = NEW.conversation_id
            UNION
            SELECT NEW.user_id
        )
        SELECT 1
        FROM subjects a
        JOIN subjects b ON a.user_id < b.user_id
        JOIN public.friendships f
          ON (f.requester_id = a.user_id AND f.addressee_id = b.user_id)
          OR (f.requester_id = b.user_id AND f.addressee_id = a.user_id)
        WHERE f.status = 'blocked'
    ) THEN
        RAISE EXCEPTION 'blocked friendship pair cannot share a DM conversation'
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

CREATE FUNCTION public.serialize_dm_blocked_friendship()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    old_requester UUID;
    old_addressee UUID;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        old_requester := OLD.requester_id;
        old_addressee := OLD.addressee_id;
    END IF;
    IF NEW.status = 'blocked' THEN
        PERFORM 1
        FROM public.users u
        WHERE u.id IN (NEW.requester_id, NEW.addressee_id, old_requester, old_addressee)
        ORDER BY u.id
        FOR NO KEY UPDATE;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER serialize_dm_blocked_friendship
    BEFORE INSERT OR UPDATE OF status, requester_id, addressee_id
    ON public.friendships
    FOR EACH ROW
    EXECUTE FUNCTION public.serialize_dm_blocked_friendship();

CREATE TRIGGER reject_blocked_dm_participant
    BEFORE INSERT OR UPDATE OF conversation_id, user_id
    ON public.dm_participants
    FOR EACH ROW
    EXECUTE FUNCTION public.reject_blocked_dm_participant();

CREATE FUNCTION public.stamp_dm_block_reconciliation_transaction()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.reconciliation_transaction_id := pg_current_xact_id();
    RETURN NEW;
END;
$$;

CREATE TRIGGER stamp_dm_block_reconciliation_transaction
    BEFORE INSERT OR UPDATE OF operation_id, remove_a, remove_b
    ON public.dm_block_reconciliations
    FOR EACH ROW
    EXECUTE FUNCTION public.stamp_dm_block_reconciliation_transaction();

CREATE FUNCTION public.require_blocked_friendship_reconciliation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    pair_a UUID;
    pair_b UUID;
    final_requester UUID;
    final_addressee UUID;
    final_status VARCHAR(20);
BEGIN
    SELECT requester_id, addressee_id, status
    INTO final_requester, final_addressee, final_status
    FROM public.friendships
    WHERE id = NEW.id;

    IF NOT FOUND OR final_status IS DISTINCT FROM 'blocked' THEN
        RETURN NULL;
    END IF;

    pair_a := LEAST(final_requester, final_addressee);
    pair_b := GREATEST(final_requester, final_addressee);

    IF NOT EXISTS (
        SELECT 1
        FROM public.dm_block_reconciliations
        WHERE user_a_id = pair_a
          AND user_b_id = pair_b
          AND reconciliation_transaction_id = pg_current_xact_id()
    ) THEN
        RAISE EXCEPTION 'blocked friendship requires current reconciliation evidence'
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER require_blocked_friendship_reconciliation
    AFTER INSERT OR UPDATE OF id, status, requester_id, addressee_id
    ON public.friendships
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION public.require_blocked_friendship_reconciliation();
