-- #2272 Stage B schema (migration 000194) CANDIDATE: inert until reviewed authority adapters ship.
-- No lineage backfill, existing-table rewrite, credentials or permission grants.
-- Principal UUIDs deliberately have NO users FK: erasure must retain obligations.
-- New tables are empty, so ordinary transactional indexes are intentional.
-- This file is one atomic implicit transaction under the migration runner.

CREATE TABLE login_family_rollout (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    activated_at TIMESTAMPTZ,
    receipt JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((activated_at IS NULL) = (receipt IS NULL)),
    CHECK (receipt IS NULL OR jsonb_typeof(receipt) = 'object')
);
INSERT INTO login_family_rollout(id) VALUES (TRUE);

CREATE TABLE login_family_revocations (
    operation_id UUID PRIMARY KEY CHECK (operation_id <> '00000000-0000-0000-0000-000000000000'),
    user_id UUID NOT NULL CHECK (user_id <> '00000000-0000-0000-0000-000000000000'),
    scope TEXT NOT NULL CHECK (scope IN ('session', 'user')),
    reason TEXT NOT NULL CHECK (reason IN
        ('logout', 'explicit_revoke', 'displacement', 'theft', 'disablement', 'erasure')),
    creation_xid XID8 NOT NULL DEFAULT pg_current_xact_id(),
    sealed_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    failure_class TEXT CHECK (failure_class IS NULL OR failure_class = 'delivery'),
    reconcile_after TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (operation_id, user_id),
    CHECK (completed_at IS NULL OR sealed_at IS NOT NULL),
    CHECK (updated_at >= created_at AND reconcile_after >= created_at)
);

CREATE TABLE login_families (
    family_id UUID PRIMARY KEY CHECK (family_id <> '00000000-0000-0000-0000-000000000000'),
    user_id UUID NOT NULL CHECK (user_id <> '00000000-0000-0000-0000-000000000000'),
    credential_epoch TEXT NOT NULL CHECK
        (credential_epoch = '' OR credential_epoch ~ '^[0-9a-f]{32}$'),
    terminal_at TIMESTAMPTZ,
    auth_operation_id UUID CHECK (auth_operation_id <> '00000000-0000-0000-0000-000000000000'),
    epoch_generation UUID CHECK (epoch_generation <> '00000000-0000-0000-0000-000000000000'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (family_id, user_id),
    UNIQUE (family_id, user_id, credential_epoch),
    FOREIGN KEY (auth_operation_id, user_id)
        REFERENCES login_family_revocations(operation_id, user_id) ON DELETE RESTRICT,
    CHECK ((terminal_at IS NULL AND auth_operation_id IS NULL AND epoch_generation IS NULL)
        OR (terminal_at IS NOT NULL
            AND num_nonnulls(auth_operation_id, epoch_generation) = 1))
    -- epoch_generation is an immutable locator, NOT an FK to the ephemeral parent.
);

CREATE TABLE login_family_sids (
    sid UUID PRIMARY KEY CHECK (sid <> '00000000-0000-0000-0000-000000000000'),
    family_id UUID NOT NULL CHECK (family_id <> '00000000-0000-0000-0000-000000000000'),
    user_id UUID NOT NULL CHECK (user_id <> '00000000-0000-0000-0000-000000000000'),
    access_expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (family_id, user_id)
        REFERENCES login_families(family_id, user_id) ON DELETE RESTRICT
);

CREATE TABLE login_family_revocation_targets (
    operation_id UUID NOT NULL CHECK (operation_id <> '00000000-0000-0000-0000-000000000000'),
    family_id UUID NOT NULL UNIQUE CHECK (family_id <> '00000000-0000-0000-0000-000000000000'),
    user_id UUID NOT NULL CHECK (user_id <> '00000000-0000-0000-0000-000000000000'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (operation_id, family_id),
    FOREIGN KEY (operation_id, user_id)
        REFERENCES login_family_revocations(operation_id, user_id) ON DELETE RESTRICT,
    FOREIGN KEY (family_id, user_id)
        REFERENCES login_families(family_id, user_id) ON DELETE RESTRICT
);

CREATE TABLE voice_auth_boot_fences (
    node_boot_id UUID PRIMARY KEY CHECK (node_boot_id <> '00000000-0000-0000-0000-000000000000'),
    retired_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE voice_auth_connections (
    connection_generation UUID PRIMARY KEY CHECK (connection_generation <> '00000000-0000-0000-0000-000000000000'),
    family_id UUID NOT NULL CHECK (family_id <> '00000000-0000-0000-0000-000000000000'),
    user_id UUID NOT NULL CHECK (user_id <> '00000000-0000-0000-0000-000000000000'),
    credential_epoch TEXT NOT NULL CHECK
        (credential_epoch = '' OR credential_epoch ~ '^[0-9a-f]{32}$'),
    node_boot_id UUID NOT NULL REFERENCES voice_auth_boot_fences(node_boot_id) ON DELETE RESTRICT
        CHECK (node_boot_id <> '00000000-0000-0000-0000-000000000000'),
    socket_id TEXT NOT NULL CHECK (octet_length(socket_id) BETWEEN 1 AND 255),
    registration_token_sha256 TEXT NOT NULL CHECK (registration_token_sha256 ~ '^[0-9a-f]{64}$'),
    registration_body_sha256 TEXT NOT NULL CHECK (registration_body_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (node_boot_id, socket_id),
    FOREIGN KEY (family_id, user_id, credential_epoch)
        REFERENCES login_families(family_id, user_id, credential_epoch) ON DELETE RESTRICT
);

CREATE TABLE voice_auth_room_bindings (
    session_generation UUID PRIMARY KEY REFERENCES voice_enforcement_sessions(session_generation)
        ON DELETE RESTRICT CHECK (session_generation <> '00000000-0000-0000-0000-000000000000'),
    connection_generation UUID NOT NULL REFERENCES voice_auth_connections(connection_generation)
        ON DELETE RESTRICT CHECK (connection_generation <> '00000000-0000-0000-0000-000000000000'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE voice_auth_connection_closures (
    connection_generation UUID PRIMARY KEY CHECK (connection_generation <> '00000000-0000-0000-0000-000000000000'),
    node_boot_id UUID NOT NULL CHECK (node_boot_id <> '00000000-0000-0000-0000-000000000000'),
    socket_id TEXT NOT NULL CHECK (octet_length(socket_id) BETWEEN 1 AND 255),
    credential_epoch TEXT NOT NULL CHECK
        (credential_epoch = '' OR credential_epoch ~ '^[0-9a-f]{32}$'),
    variant TEXT NOT NULL CHECK (variant IN ('bound', 'unregistered')),
    user_id UUID CHECK (user_id <> '00000000-0000-0000-0000-000000000000'),
    family_id UUID CHECK (family_id <> '00000000-0000-0000-0000-000000000000'),
    registration_token_sha256 TEXT NOT NULL CHECK (registration_token_sha256 ~ '^[0-9a-f]{64}$'),
    registration_body_sha256 TEXT NOT NULL CHECK (registration_body_sha256 ~ '^[0-9a-f]{64}$'),
    closed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((variant = 'bound' AND user_id IS NOT NULL AND family_id IS NOT NULL)
        OR (variant = 'unregistered' AND user_id IS NULL AND family_id IS NULL))
    -- No FK: provisional close can precede bootstrap, registration or principal erasure.
);

CREATE TABLE voice_auth_room_closures (
    session_generation UUID PRIMARY KEY CHECK (session_generation <> '00000000-0000-0000-0000-000000000000'),
    connection_generation UUID NOT NULL CHECK (connection_generation <> '00000000-0000-0000-0000-000000000000'),
    node_boot_id UUID NOT NULL CHECK (node_boot_id <> '00000000-0000-0000-0000-000000000000'),
    socket_id TEXT NOT NULL CHECK (octet_length(socket_id) BETWEEN 1 AND 255),
    credential_epoch TEXT NOT NULL CHECK
        (credential_epoch = '' OR credential_epoch ~ '^[0-9a-f]{32}$'),
    user_id UUID NOT NULL CHECK (user_id <> '00000000-0000-0000-0000-000000000000'),
    family_id UUID NOT NULL CHECK (family_id <> '00000000-0000-0000-0000-000000000000'),
    room_id UUID NOT NULL CHECK (room_id <> '00000000-0000-0000-0000-000000000000'),
    room_kind TEXT NOT NULL CHECK (room_kind IN ('dm', 'channel')),
    closed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
    -- Copied Binding survives live connection, room, family and principal deletion.
);

CREATE INDEX idx_login_families_user_family ON login_families(user_id, family_id);
CREATE INDEX idx_login_families_epoch_owner ON login_families(epoch_generation, family_id)
    WHERE epoch_generation IS NOT NULL;
CREATE INDEX idx_login_families_auth_owner ON login_families(auth_operation_id, family_id)
    WHERE auth_operation_id IS NOT NULL;
CREATE INDEX idx_login_family_sids_family ON login_family_sids(family_id, sid);
CREATE INDEX idx_login_family_revocations_due
    ON login_family_revocations(reconcile_after, created_at, operation_id)
    WHERE completed_at IS NULL;
CREATE INDEX idx_voice_auth_connections_family_epoch
    ON voice_auth_connections(family_id, credential_epoch, connection_generation);
CREATE INDEX idx_voice_auth_connections_user_epoch
    ON voice_auth_connections(user_id, credential_epoch, connection_generation);
CREATE INDEX idx_voice_auth_room_bindings_connection
    ON voice_auth_room_bindings(connection_generation, session_generation);
CREATE INDEX idx_voice_auth_connection_closures_boot
    ON voice_auth_connection_closures(node_boot_id, connection_generation);
CREATE INDEX idx_voice_auth_room_closures_boot
    ON voice_auth_room_closures(node_boot_id, session_generation);
CREATE INDEX idx_voice_auth_room_closures_connection
    ON voice_auth_room_closures(connection_generation, session_generation);

-- Domain separation is part of the SQL contract. Hash collisions only serialize;
-- full UUID/tuple equality, never the advisory key, decides identity.
CREATE FUNCTION login_family_generation_lock(kind TEXT, generation UUID)
RETURNS VOID LANGUAGE plpgsql AS $$
BEGIN
    IF kind IS NULL OR kind NOT IN ('connection', 'room', 'boot')
        OR generation IS NULL OR generation = '00000000-0000-0000-0000-000000000000'::UUID THEN
        RAISE EXCEPTION 'invalid generation lock domain' USING ERRCODE = 'check_violation';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended(
        'concord.login-family.v1/' || kind || '/' || generation::TEXT, 0));
END
$$;

CREATE FUNCTION login_family_immutable()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'login-family evidence is immutable' USING ERRCODE = 'restrict_violation';
END
$$;

CREATE FUNCTION login_family_state_guard()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'login_family_rollout' THEN
        IF TG_OP = 'DELETE' THEN
            RAISE EXCEPTION 'rollout singleton cannot be deleted' USING ERRCODE = 'restrict_violation';
        END IF;
        IF (to_jsonb(NEW) - ARRAY['activated_at','receipt','updated_at'])
            IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['activated_at','receipt','updated_at'])
            OR (OLD.activated_at IS NOT NULL AND
                (NEW.activated_at, NEW.receipt) IS DISTINCT FROM (OLD.activated_at, OLD.receipt)) THEN
            RAISE EXCEPTION 'activation floor is immutable' USING ERRCODE = 'restrict_violation';
        END IF;
    ELSIF TG_TABLE_NAME = 'login_families' THEN
        IF TG_OP = 'INSERT' THEN
            IF NEW.terminal_at IS NOT NULL THEN
                RAISE EXCEPTION 'family must start live' USING ERRCODE = 'restrict_violation';
            END IF;
            RETURN NEW;
        END IF;
        IF (to_jsonb(NEW) - ARRAY['terminal_at','auth_operation_id','epoch_generation','updated_at'])
            IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['terminal_at','auth_operation_id','epoch_generation','updated_at'])
            OR (OLD.terminal_at IS NOT NULL AND
                (NEW.terminal_at, NEW.auth_operation_id, NEW.epoch_generation)
                IS DISTINCT FROM (OLD.terminal_at, OLD.auth_operation_id, OLD.epoch_generation)) THEN
            RAISE EXCEPTION 'family identity or terminal owner changed' USING ERRCODE = 'restrict_violation';
        END IF;
    ELSIF TG_TABLE_NAME = 'login_family_revocations' THEN
        IF TG_OP = 'INSERT' THEN
            IF NEW.sealed_at IS NOT NULL OR NEW.completed_at IS NOT NULL THEN
                RAISE EXCEPTION 'operation must start unsealed' USING ERRCODE = 'restrict_violation';
            END IF;
            NEW.creation_xid := pg_current_xact_id();
        ELSE
            IF (to_jsonb(NEW) - ARRAY['sealed_at','completed_at','attempts','failure_class','reconcile_after','updated_at'])
                IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['sealed_at','completed_at','attempts','failure_class','reconcile_after','updated_at'])
                OR (OLD.sealed_at IS NOT NULL AND NEW.sealed_at IS DISTINCT FROM OLD.sealed_at)
                OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at)
                OR (OLD.sealed_at IS NULL AND NEW.creation_xid <> pg_current_xact_id()) THEN
                RAISE EXCEPTION 'operation identity, seal or completion changed' USING ERRCODE = 'restrict_violation';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER login_family_rollout_state BEFORE UPDATE OR DELETE ON login_family_rollout
    FOR EACH ROW EXECUTE FUNCTION login_family_state_guard();
CREATE TRIGGER login_families_state BEFORE INSERT OR UPDATE ON login_families
    FOR EACH ROW EXECUTE FUNCTION login_family_state_guard();
CREATE TRIGGER login_family_revocations_state BEFORE INSERT OR UPDATE ON login_family_revocations
    FOR EACH ROW EXECUTE FUNCTION login_family_state_guard();

-- Every queued event checks CURRENT rows at deferred execution, not NEW's stale
-- unsealed snapshot. This also covers INSERT -> seal in the creating transaction.
CREATE FUNCTION login_family_frontier_check()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    op UUID;
BEGIN
    IF TG_TABLE_NAME = 'login_family_revocations' THEN
        op := CASE WHEN TG_OP = 'DELETE' THEN OLD.operation_id ELSE NEW.operation_id END;
    ELSIF TG_TABLE_NAME = 'login_family_revocation_targets' THEN
        op := CASE WHEN TG_OP = 'DELETE' THEN OLD.operation_id ELSE NEW.operation_id END;
        IF TG_OP = 'DELETE' AND
            (EXISTS (SELECT 1 FROM login_family_revocations WHERE operation_id = op)
             OR EXISTS (SELECT 1 FROM login_families WHERE family_id = OLD.family_id)) THEN
            RAISE EXCEPTION 'sealed cohort must be collected whole' USING ERRCODE = 'restrict_violation';
        END IF;
    ELSE
        op := CASE WHEN TG_OP = 'DELETE' THEN OLD.auth_operation_id ELSE NEW.auth_operation_id END;
    END IF;
    IF op IS NOT NULL AND EXISTS (SELECT 1 FROM login_family_revocations WHERE operation_id = op) THEN
        IF EXISTS (SELECT 1 FROM login_family_revocations WHERE operation_id = op AND sealed_at IS NULL)
            OR NOT EXISTS (SELECT 1 FROM login_family_revocation_targets WHERE operation_id = op)
            OR EXISTS (
                SELECT 1 FROM login_family_revocation_targets t
                LEFT JOIN login_families f ON f.family_id = t.family_id
                WHERE t.operation_id = op AND
                    (f.family_id IS NULL OR f.user_id <> t.user_id OR f.auth_operation_id IS DISTINCT FROM op))
            OR EXISTS (
                SELECT 1 FROM login_families f
                WHERE f.auth_operation_id = op AND NOT EXISTS (
                    SELECT 1 FROM login_family_revocation_targets t
                    WHERE t.operation_id = op AND t.family_id = f.family_id AND t.user_id = f.user_id)) THEN
            RAISE EXCEPTION 'unsealed or inconsistent auth frontier' USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER login_family_revocations_frontier
    AFTER INSERT OR UPDATE OR DELETE ON login_family_revocations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION login_family_frontier_check();
CREATE CONSTRAINT TRIGGER login_families_frontier
    AFTER INSERT OR UPDATE OR DELETE ON login_families
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION login_family_frontier_check();
CREATE CONSTRAINT TRIGGER login_family_targets_frontier
    AFTER INSERT OR DELETE ON login_family_revocation_targets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION login_family_frontier_check();

CREATE FUNCTION login_family_target_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM login_family_revocations WHERE operation_id = NEW.operation_id
        AND user_id = NEW.user_id AND sealed_at IS NULL
        AND creation_xid = pg_current_xact_id() FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'target requires creating unsealed operation' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER login_family_target_insert BEFORE INSERT ON login_family_revocation_targets
    FOR EACH ROW EXECUTE FUNCTION login_family_target_insert();

-- No generic GC bypass flag. These checks are conservative relational minima;
-- the deletion-only adapter must additionally prove every external obligation.
CREATE FUNCTION login_family_gc_guard()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'login_family_sids' THEN
        IF OLD.access_expires_at > clock_timestamp()
            OR EXISTS (SELECT 1 FROM refresh_tokens WHERE id = OLD.sid OR predecessor_id = OLD.sid)
            OR EXISTS (SELECT 1 FROM voice_auth_connections WHERE family_id = OLD.family_id)
            OR EXISTS (SELECT 1 FROM login_families f
                LEFT JOIN login_family_revocations o ON o.operation_id = f.auth_operation_id
                WHERE f.family_id = OLD.family_id AND
                    ((f.auth_operation_id IS NOT NULL AND o.completed_at IS NULL)
                    OR EXISTS (SELECT 1 FROM credential_epoch_voice_ejections e
                        WHERE e.generation = f.epoch_generation))) THEN
            RAISE EXCEPTION 'SID evidence still required' USING ERRCODE = 'restrict_violation';
        END IF;
    ELSIF TG_TABLE_NAME = 'login_families' THEN
        IF EXISTS (SELECT 1 FROM login_family_revocations
                WHERE operation_id = OLD.auth_operation_id AND completed_at IS NULL)
            OR EXISTS (SELECT 1 FROM credential_epoch_voice_ejections WHERE generation = OLD.epoch_generation) THEN
            RAISE EXCEPTION 'family owner still pending' USING ERRCODE = 'restrict_violation';
        END IF;
    ELSE
        IF OLD.completed_at IS NULL THEN
            RAISE EXCEPTION 'auth operation still pending' USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    RETURN OLD;
END
$$;
CREATE TRIGGER login_family_sids_gc BEFORE DELETE ON login_family_sids
    FOR EACH ROW EXECUTE FUNCTION login_family_gc_guard();
CREATE TRIGGER login_families_gc BEFORE DELETE ON login_families
    FOR EACH ROW EXECUTE FUNCTION login_family_gc_guard();
CREATE TRIGGER login_family_revocations_gc BEFORE DELETE ON login_family_revocations
    FOR EACH ROW EXECUTE FUNCTION login_family_gc_guard();

-- Boot key serialization covers absent bootstrap versus directly-retired INSERT.
-- Unique-key conflicts must be reread by adapters; never DO UPDATE retired state.
CREATE FUNCTION login_family_boot_guard()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE boot UUID;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'boot fences are permanent' USING ERRCODE = 'restrict_violation';
    END IF;
    boot := NEW.node_boot_id;
    PERFORM login_family_generation_lock('boot', boot);
    IF TG_OP = 'UPDATE' AND
        ((to_jsonb(NEW) - ARRAY['retired_at','updated_at'])
            IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['retired_at','updated_at'])
        OR (OLD.retired_at IS NOT NULL AND NEW.retired_at IS DISTINCT FROM OLD.retired_at)) THEN
        RAISE EXCEPTION 'boot identity or retirement changed' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.retired_at IS NOT NULL AND
        (EXISTS (SELECT 1 FROM voice_auth_connections WHERE node_boot_id = boot)
        OR EXISTS (SELECT 1 FROM voice_enforcement_sessions WHERE node_boot_id = boot)) THEN
        RAISE EXCEPTION 'boot still has live evidence' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER voice_auth_boot_state BEFORE INSERT OR UPDATE OR DELETE ON voice_auth_boot_fences
    FOR EACH ROW EXECUTE FUNCTION login_family_boot_guard();

CREATE FUNCTION login_family_connection_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM voice_auth_boot_fences WHERE node_boot_id = NEW.node_boot_id
        AND retired_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'boot is absent or retired' USING ERRCODE = 'restrict_violation';
    END IF;
    PERFORM login_family_generation_lock('connection', NEW.connection_generation);
    IF EXISTS (SELECT 1 FROM voice_auth_connection_closures
        WHERE connection_generation = NEW.connection_generation) THEN
        RAISE EXCEPTION 'connection generation is closed' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER voice_auth_connection_insert BEFORE INSERT ON voice_auth_connections
    FOR EACH ROW EXECUTE FUNCTION login_family_connection_insert();

CREATE FUNCTION login_family_binding_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE c voice_auth_connections%ROWTYPE;
BEGIN
    -- Adapter has already taken boot SHARE and the connection-generation lock.
    SELECT * INTO STRICT c FROM voice_auth_connections
        WHERE connection_generation = NEW.connection_generation FOR KEY SHARE;
    PERFORM login_family_generation_lock('room', NEW.session_generation);
    IF EXISTS (SELECT 1 FROM voice_auth_room_closures WHERE session_generation = NEW.session_generation)
        OR NOT EXISTS (SELECT 1 FROM voice_enforcement_sessions r
            WHERE r.session_generation = NEW.session_generation AND r.user_id = c.user_id
            AND r.node_boot_id = c.node_boot_id AND r.socket_id = c.socket_id
            AND r.credential_epoch = c.credential_epoch) THEN
        RAISE EXCEPTION 'closed or mismatched exact room binding' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER voice_auth_binding_insert BEFORE INSERT ON voice_auth_room_bindings
    FOR EACH ROW EXECUTE FUNCTION login_family_binding_insert();

CREATE FUNCTION login_family_closure_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE c voice_auth_connections%ROWTYPE;
BEGIN
    -- Same boot lock login_family_boot_guard takes, so a retirement committing
    -- concurrently is either seen here or waits for this transaction.
    PERFORM login_family_generation_lock('boot', NEW.node_boot_id);
    PERFORM login_family_generation_lock('connection', NEW.connection_generation);
    IF EXISTS (SELECT 1 FROM voice_auth_boot_fences
        WHERE node_boot_id = NEW.node_boot_id AND retired_at IS NOT NULL) THEN
        RAISE EXCEPTION 'retired boot cannot gain closure evidence' USING ERRCODE = 'restrict_violation';
    END IF;
    SELECT * INTO c FROM voice_auth_connections
        WHERE connection_generation = NEW.connection_generation FOR UPDATE;
    IF TG_TABLE_NAME = 'voice_auth_connection_closures' THEN
        IF NEW.variant = 'unregistered' THEN
            IF c.connection_generation IS NOT NULL THEN
                RAISE EXCEPTION 'unregistered close has live binding' USING ERRCODE = 'restrict_violation';
            END IF;
        ELSIF c.connection_generation IS NULL OR
            (c.node_boot_id, c.socket_id, c.credential_epoch, c.user_id, c.family_id,
                c.registration_token_sha256, c.registration_body_sha256)
            IS DISTINCT FROM
            (NEW.node_boot_id, NEW.socket_id, NEW.credential_epoch, NEW.user_id, NEW.family_id,
                NEW.registration_token_sha256, NEW.registration_body_sha256) THEN
            RAISE EXCEPTION 'closure does not copy exact connection' USING ERRCODE = 'restrict_violation';
        END IF;
    ELSE
        PERFORM login_family_generation_lock('room', NEW.session_generation);
        -- An absent room can be closed using a known parent Binding, including
        -- after the parent itself closed. Never invent a parent from the request.
        IF c.connection_generation IS NULL THEN
            SELECT connection_generation, node_boot_id, socket_id, credential_epoch, user_id, family_id
                INTO c.connection_generation, c.node_boot_id, c.socket_id,
                    c.credential_epoch, c.user_id, c.family_id
                FROM voice_auth_connection_closures
                WHERE connection_generation = NEW.connection_generation AND variant = 'bound';
        END IF;
        IF c.connection_generation IS NULL OR
            (c.node_boot_id, c.socket_id, c.credential_epoch, c.user_id, c.family_id)
            IS DISTINCT FROM
            (NEW.node_boot_id, NEW.socket_id, NEW.credential_epoch, NEW.user_id, NEW.family_id)
            OR EXISTS (SELECT 1 FROM voice_enforcement_sessions r
                LEFT JOIN voice_auth_room_bindings b USING (session_generation)
                WHERE r.session_generation = NEW.session_generation
                AND (b.connection_generation IS DISTINCT FROM NEW.connection_generation
                    OR r.room_id <> NEW.room_id OR r.room_kind <> NEW.room_kind)) THEN
            RAISE EXCEPTION 'closure does not copy exact room' USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION login_family_release_guard()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM login_family_generation_lock('connection', OLD.connection_generation);
    IF TG_TABLE_NAME = 'voice_auth_connections' THEN
        IF NOT EXISTS (SELECT 1 FROM voice_auth_connection_closures
            WHERE connection_generation = OLD.connection_generation AND variant = 'bound') THEN
            RAISE EXCEPTION 'connection release requires durable closure' USING ERRCODE = 'restrict_violation';
        END IF;
    ELSE
        -- Adapter holds parent connection FOR UPDATE before deleting the binding.
        PERFORM login_family_generation_lock('room', OLD.session_generation);
        IF NOT EXISTS (SELECT 1 FROM voice_auth_room_closures
            WHERE session_generation = OLD.session_generation
            AND connection_generation = OLD.connection_generation) THEN
            RAISE EXCEPTION 'room release requires durable closure' USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    RETURN OLD;
END
$$;

CREATE FUNCTION login_family_closure_check()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'voice_auth_connection_closures' THEN
        IF EXISTS (SELECT 1 FROM voice_auth_connection_closures
                WHERE connection_generation = NEW.connection_generation)
            AND EXISTS (SELECT 1 FROM voice_auth_connections
                WHERE connection_generation = NEW.connection_generation) THEN
            RAISE EXCEPTION 'closed connection remains live' USING ERRCODE = 'restrict_violation';
        END IF;
    ELSE
        IF EXISTS (SELECT 1 FROM voice_auth_room_closures WHERE session_generation = NEW.session_generation)
            AND (EXISTS (SELECT 1 FROM voice_auth_room_bindings
                    WHERE session_generation = NEW.session_generation)
                OR EXISTS (SELECT 1 FROM voice_enforcement_sessions
                    WHERE session_generation = NEW.session_generation)) THEN
            RAISE EXCEPTION 'closed room remains live' USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    RETURN NULL;
END
$$;

CREATE FUNCTION login_family_closure_gc()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM login_family_generation_lock('connection', OLD.connection_generation);
    IF TG_TABLE_NAME = 'voice_auth_room_closures' THEN
        PERFORM login_family_generation_lock('room', OLD.session_generation);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM voice_auth_boot_fences
            WHERE node_boot_id = OLD.node_boot_id AND retired_at IS NOT NULL)
        OR EXISTS (SELECT 1 FROM voice_auth_connections WHERE node_boot_id = OLD.node_boot_id)
        OR EXISTS (SELECT 1 FROM voice_enforcement_sessions WHERE node_boot_id = OLD.node_boot_id)
        OR EXISTS (SELECT 1 FROM login_families f
            LEFT JOIN login_family_revocations o ON o.operation_id = f.auth_operation_id
            WHERE f.family_id = OLD.family_id
            AND ((f.auth_operation_id IS NOT NULL AND o.completed_at IS NULL)
                OR EXISTS (SELECT 1 FROM credential_epoch_voice_ejections e
                    WHERE e.generation = f.epoch_generation))) THEN
        RAISE EXCEPTION 'closure requires retired zero-live boot' USING ERRCODE = 'restrict_violation';
    END IF;
    IF TG_TABLE_NAME = 'voice_auth_connection_closures'
        AND EXISTS (SELECT 1 FROM voice_auth_room_closures
            WHERE connection_generation = OLD.connection_generation) THEN
        RAISE EXCEPTION 'collect room closures before connection closure' USING ERRCODE = 'restrict_violation';
    END IF;
    -- Required pending/proof obligations are additionally revalidated by the
    -- deletion-only adapter under these same generation serialization locks.
    RETURN OLD;
END
$$;

CREATE TRIGGER voice_auth_connection_release BEFORE DELETE ON voice_auth_connections
    FOR EACH ROW EXECUTE FUNCTION login_family_release_guard();
CREATE TRIGGER voice_auth_room_release BEFORE DELETE ON voice_auth_room_bindings
    FOR EACH ROW EXECUTE FUNCTION login_family_release_guard();
CREATE TRIGGER login_family_sids_immutable BEFORE UPDATE ON login_family_sids
    FOR EACH ROW EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER login_family_revocation_targets_immutable BEFORE UPDATE ON login_family_revocation_targets
    FOR EACH ROW EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER voice_auth_connections_immutable BEFORE UPDATE ON voice_auth_connections
    FOR EACH ROW EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER voice_auth_room_bindings_immutable BEFORE UPDATE ON voice_auth_room_bindings
    FOR EACH ROW EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER voice_auth_connection_closures_immutable BEFORE UPDATE ON voice_auth_connection_closures
    FOR EACH ROW EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER voice_auth_room_closures_immutable BEFORE UPDATE ON voice_auth_room_closures
    FOR EACH ROW EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER voice_auth_connection_closures_insert BEFORE INSERT ON voice_auth_connection_closures
    FOR EACH ROW EXECUTE FUNCTION login_family_closure_insert();
CREATE CONSTRAINT TRIGGER voice_auth_connection_closures_closed AFTER INSERT ON voice_auth_connection_closures
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION login_family_closure_check();
CREATE TRIGGER voice_auth_connection_closures_gc BEFORE DELETE ON voice_auth_connection_closures
    FOR EACH ROW EXECUTE FUNCTION login_family_closure_gc();
CREATE TRIGGER voice_auth_room_closures_insert BEFORE INSERT ON voice_auth_room_closures
    FOR EACH ROW EXECUTE FUNCTION login_family_closure_insert();
CREATE CONSTRAINT TRIGGER voice_auth_room_closures_closed AFTER INSERT ON voice_auth_room_closures
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION login_family_closure_check();
CREATE TRIGGER voice_auth_room_closures_gc BEFORE DELETE ON voice_auth_room_closures
    FOR EACH ROW EXECUTE FUNCTION login_family_closure_gc();

-- Statement-level TRUNCATE guards. Row-level DELETE guards do not fire on
-- TRUNCATE, which would otherwise erase floor, boot-fence and closure evidence.
-- A disposable test database bypasses them with session_replication_role =
-- replica (superuser only); production's application role cannot.
CREATE TRIGGER login_family_rollout_truncate BEFORE TRUNCATE ON login_family_rollout
    FOR EACH STATEMENT EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER login_families_truncate BEFORE TRUNCATE ON login_families
    FOR EACH STATEMENT EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER login_family_sids_truncate BEFORE TRUNCATE ON login_family_sids
    FOR EACH STATEMENT EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER login_family_revocations_truncate BEFORE TRUNCATE ON login_family_revocations
    FOR EACH STATEMENT EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER login_family_revocation_targets_truncate BEFORE TRUNCATE ON login_family_revocation_targets
    FOR EACH STATEMENT EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER voice_auth_boot_fences_truncate BEFORE TRUNCATE ON voice_auth_boot_fences
    FOR EACH STATEMENT EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER voice_auth_connections_truncate BEFORE TRUNCATE ON voice_auth_connections
    FOR EACH STATEMENT EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER voice_auth_room_bindings_truncate BEFORE TRUNCATE ON voice_auth_room_bindings
    FOR EACH STATEMENT EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER voice_auth_connection_closures_truncate BEFORE TRUNCATE ON voice_auth_connection_closures
    FOR EACH STATEMENT EXECUTE FUNCTION login_family_immutable();
CREATE TRIGGER voice_auth_room_closures_truncate BEFORE TRUNCATE ON voice_auth_room_closures
    FOR EACH STATEMENT EXECUTE FUNCTION login_family_immutable();

COMMENT ON TABLE login_family_rollout IS 'One-way family activation floor; prepared state alone grants no productive authority.';
COMMENT ON COLUMN login_family_rollout.id IS 'Only TRUE is valid: the shared-writer/exclusive-activation barrier.';
COMMENT ON COLUMN login_family_rollout.activated_at IS 'Immutable first activation time; NULL is prepared.';
COMMENT ON COLUMN login_family_rollout.receipt IS 'Immutable current-image lifecycle receipt; inventory, raw shape and physical witnesses validated by adapter.';
COMMENT ON COLUMN login_family_rollout.created_at IS 'Immutable time this evidence row was recorded.';
COMMENT ON COLUMN login_family_rollout.updated_at IS 'Audit time; not proof of process freshness or cleanup eligibility.';

COMMENT ON TABLE login_family_revocations IS 'Sealed exact auth-owned revocation frontier; no principal FK and no dynamic user wildcard.';
COMMENT ON COLUMN login_family_revocations.operation_id IS 'Immutable auth delivery owner UUID.';
COMMENT ON COLUMN login_family_revocations.user_id IS 'Immutable principal correlation retained after erasure.';
COMMENT ON COLUMN login_family_revocations.scope IS 'Original signed session or user provenance.';
COMMENT ON COLUMN login_family_revocations.reason IS 'Original closed revocation cause.';
COMMENT ON COLUMN login_family_revocations.creation_xid IS 'Creating transaction identity, assigned by trigger; targets and seal must be created there.';
COMMENT ON COLUMN login_family_revocations.sealed_at IS 'One-way frontier seal required in creating transaction before commit.';
COMMENT ON COLUMN login_family_revocations.completed_at IS 'One-way definite delivery completion; does not authorize discarding other required evidence.';
COMMENT ON COLUMN login_family_revocations.attempts IS 'Bounded delivery attempt count.';
COMMENT ON COLUMN login_family_revocations.failure_class IS 'Closed retry diagnostic; never raw error or identity.';
COMMENT ON COLUMN login_family_revocations.reconcile_after IS 'Earliest delivery retry eligibility.';
COMMENT ON COLUMN login_family_revocations.created_at IS 'Immutable time this evidence row was recorded.';
COMMENT ON COLUMN login_family_revocations.updated_at IS 'Audit time; not proof of process freshness or cleanup eligibility.';

COMMENT ON TABLE login_families IS 'SQL authority for one logical login across rotating SIDs; terminal owner never changes.';
COMMENT ON COLUMN login_families.family_id IS 'Immutable logical login UUID.';
COMMENT ON COLUMN login_families.user_id IS 'Immutable principal identity; intentionally independent of users lifetime.';
COMMENT ON COLUMN login_families.credential_epoch IS 'Original empty legacy or lowercase hexadecimal credential epoch.';
COMMENT ON COLUMN login_families.terminal_at IS 'One-way terminal authority timestamp.';
COMMENT ON COLUMN login_families.auth_operation_id IS 'Matching-user sealed auth owner, exclusive with epoch owner.';
COMMENT ON COLUMN login_families.epoch_generation IS 'Existing epoch delivery generation locator; no restrictive FK to ephemeral parent.';
COMMENT ON COLUMN login_families.created_at IS 'Immutable time this evidence row was recorded.';
COMMENT ON COLUMN login_families.updated_at IS 'Audit time; not proof of process freshness or cleanup eligibility.';

COMMENT ON TABLE login_family_sids IS 'Immutable signed SID membership and exact access lifetime; independent of refresh cleanup.';
COMMENT ON COLUMN login_family_sids.sid IS 'Signed refresh/access SID identity, distinct from family and JTI.';
COMMENT ON COLUMN login_family_sids.family_id IS 'Owning logical login.';
COMMENT ON COLUMN login_family_sids.user_id IS 'Must match retained family principal.';
COMMENT ON COLUMN login_family_sids.access_expires_at IS 'Exact normalized JWT expiry selected before signing, never recomputed.';
COMMENT ON COLUMN login_family_sids.created_at IS 'Immutable time this evidence row was recorded.';
COMMENT ON COLUMN login_family_sids.updated_at IS 'Audit time; not proof of process freshness or cleanup eligibility.';

COMMENT ON TABLE login_family_revocation_targets IS 'Immutable captured membership of one sealed auth operation; whole-cohort GC only.';
COMMENT ON COLUMN login_family_revocation_targets.operation_id IS 'Captured operation with matching principal.';
COMMENT ON COLUMN login_family_revocation_targets.family_id IS 'Unique auth-owned terminal family.';
COMMENT ON COLUMN login_family_revocation_targets.user_id IS 'Principal equality enforced against family and operation.';
COMMENT ON COLUMN login_family_revocation_targets.created_at IS 'Immutable time this evidence row was recorded.';
COMMENT ON COLUMN login_family_revocation_targets.updated_at IS 'Audit time; not proof of process freshness or cleanup eligibility.';

COMMENT ON TABLE voice_auth_boot_fences IS 'Permanent one-way boot retirement; absent identities may be directly retired only by verified operator adapter.';
COMMENT ON COLUMN voice_auth_boot_fences.node_boot_id IS 'Immutable media-node boot identity; not a process-start timestamp.';
COMMENT ON COLUMN voice_auth_boot_fences.retired_at IS 'Permanent retirement time; never clear, replace or collect.';
COMMENT ON COLUMN voice_auth_boot_fences.created_at IS 'Immutable time this evidence row was recorded.';
COMMENT ON COLUMN voice_auth_boot_fences.updated_at IS 'Audit time; not proof of process freshness or cleanup eligibility.';

COMMENT ON TABLE voice_auth_connections IS 'Immutable registered authenticated connection; exact original request commitments survive renewal.';
COMMENT ON COLUMN voice_auth_connections.connection_generation IS 'Connection generation distinct from SID and room generation.';
COMMENT ON COLUMN voice_auth_connections.family_id IS 'CP-derived owning family.';
COMMENT ON COLUMN voice_auth_connections.user_id IS 'CP-derived principal retained after erasure.';
COMMENT ON COLUMN voice_auth_connections.credential_epoch IS 'Must match immutable family epoch.';
COMMENT ON COLUMN voice_auth_connections.node_boot_id IS 'Nonretired bootstrap identity at creation.';
COMMENT ON COLUMN voice_auth_connections.socket_id IS 'Exact bounded Socket.IO identity within boot.';
COMMENT ON COLUMN voice_auth_connections.registration_token_sha256 IS 'SHA-256 lowercase hex of original UTF-8 registration bearer bytes; no secret.';
COMMENT ON COLUMN voice_auth_connections.registration_body_sha256 IS 'SHA-256 lowercase hex of exact original raw registration body bytes.';
COMMENT ON COLUMN voice_auth_connections.created_at IS 'Immutable time this evidence row was recorded.';
COMMENT ON COLUMN voice_auth_connections.updated_at IS 'Audit time; not proof of process freshness or cleanup eligibility.';

COMMENT ON TABLE voice_auth_room_bindings IS 'Exact immutable association of existing room generation to authenticated parent; release deletes binding and room atomically.';
COMMENT ON COLUMN voice_auth_room_bindings.session_generation IS 'Existing immutable room evidence identity; no synthetic legacy binding.';
COMMENT ON COLUMN voice_auth_room_bindings.connection_generation IS 'Exact parent; row tuple must match existing room principal, boot, socket and epoch.';
COMMENT ON COLUMN voice_auth_room_bindings.created_at IS 'Immutable time this evidence row was recorded.';
COMMENT ON COLUMN voice_auth_room_bindings.updated_at IS 'Audit time; not proof of process freshness or cleanup eligibility.';

COMMENT ON TABLE voice_auth_connection_closures IS 'Immutable closure liability surviving deletion of live rows; provisional unknown boot is permitted.';
COMMENT ON COLUMN voice_auth_connection_closures.connection_generation IS 'Exact permanently closed generation.';
COMMENT ON COLUMN voice_auth_connection_closures.node_boot_id IS 'Copied or verified provisional boot; intentionally no FK.';
COMMENT ON COLUMN voice_auth_connection_closures.socket_id IS 'Exact original socket identity.';
COMMENT ON COLUMN voice_auth_connection_closures.credential_epoch IS 'Exact bound or provisional epoch.';
COMMENT ON COLUMN voice_auth_connection_closures.variant IS 'Bound copies CP Binding; unregistered forbids user/family invention.';
COMMENT ON COLUMN voice_auth_connection_closures.user_id IS 'CP-derived principal for bound close only.';
COMMENT ON COLUMN voice_auth_connection_closures.family_id IS 'CP-derived family for bound close only.';
COMMENT ON COLUMN voice_auth_connection_closures.registration_token_sha256 IS 'Original token commitment, never a renewed token or plaintext.';
COMMENT ON COLUMN voice_auth_connection_closures.registration_body_sha256 IS 'Original raw registration-body commitment.';
COMMENT ON COLUMN voice_auth_connection_closures.closed_at IS 'Durable closure time, not an age-based GC authorization.';
COMMENT ON COLUMN voice_auth_connection_closures.created_at IS 'Immutable time this evidence row was recorded.';
COMMENT ON COLUMN voice_auth_connection_closures.updated_at IS 'Audit time; not proof of process freshness or cleanup eligibility.';

COMMENT ON TABLE voice_auth_room_closures IS 'Immutable copied room Binding retained through live deletion and erasure; room-first closure GC.';
COMMENT ON COLUMN voice_auth_room_closures.session_generation IS 'Exact closed existing room generation.';
COMMENT ON COLUMN voice_auth_room_closures.connection_generation IS 'Exact parent connection generation.';
COMMENT ON COLUMN voice_auth_room_closures.node_boot_id IS 'Copied boot identity; no live-row FK.';
COMMENT ON COLUMN voice_auth_room_closures.socket_id IS 'Copied exact socket identity.';
COMMENT ON COLUMN voice_auth_room_closures.credential_epoch IS 'Copied immutable epoch.';
COMMENT ON COLUMN voice_auth_room_closures.user_id IS 'Copied CP principal; evidence only.';
COMMENT ON COLUMN voice_auth_room_closures.family_id IS 'Copied CP family; evidence only.';
COMMENT ON COLUMN voice_auth_room_closures.room_id IS 'Exact original room UUID.';
COMMENT ON COLUMN voice_auth_room_closures.room_kind IS 'Room namespace, dm or channel.';
COMMENT ON COLUMN voice_auth_room_closures.closed_at IS 'Durable closure time; no TTL retirement.';
COMMENT ON COLUMN voice_auth_room_closures.created_at IS 'Immutable time this evidence row was recorded.';
COMMENT ON COLUMN voice_auth_room_closures.updated_at IS 'Audit time; not proof of process freshness or cleanup eligibility.';
