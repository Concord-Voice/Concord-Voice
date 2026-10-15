-- Account authority, migration 000001 (#3587 PR-B1, DoR §3.2.1).
-- The authority's own database. The bootstrap (or #3593's provisioner) has
-- already created schema `authority` owned by authority_owner and set the
-- database search_path to it (B1-D2). Every constraint is VALID. This file runs
-- as one implicit transaction ([internal]rules/migrations.md § Transaction shape).

CREATE TABLE authority.meta (
    id                      smallint PRIMARY KEY CHECK (id = 1),
    realm_id                uuid NOT NULL,
    restore_epoch           bigint NOT NULL DEFAULT 0 CHECK (restore_epoch >= 0),
    reconciled              boolean NOT NULL DEFAULT true,
    restore_started_at      timestamptz NULL,
    backup_max_commit_id    bigint NULL,
    backup_max_issued_at_ms bigint NULL,
    reconcile_completed_at  timestamptz NULL,
    legacy_sealed_at        timestamptz NULL,
    population_digest       bytea NULL CHECK (population_digest IS NULL OR octet_length(population_digest) = 48),
    last_tick_at            timestamptz NOT NULL,
    last_peer_contact_at    timestamptz NULL,
    cancel_unavailable_ms   bigint NOT NULL DEFAULT 0 CHECK (cancel_unavailable_ms >= 0),
    replication_required    boolean NOT NULL DEFAULT true,
    last_purge_at           timestamptz NULL
);

CREATE FUNCTION authority.meta_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = authority, pg_temp AS $$
BEGIN
    IF NEW.realm_id IS DISTINCT FROM OLD.realm_id THEN
        RAISE EXCEPTION 'authority.meta: realm_id is immutable';
    END IF;
    IF OLD.legacy_sealed_at IS NOT NULL AND NEW.legacy_sealed_at IS DISTINCT FROM OLD.legacy_sealed_at THEN
        RAISE EXCEPTION 'authority.meta: legacy_sealed_at is set once';
    END IF;
    IF OLD.population_digest IS NOT NULL AND NEW.population_digest IS DISTINCT FROM OLD.population_digest THEN
        RAISE EXCEPTION 'authority.meta: population_digest is set once';
    END IF;
    IF NEW.cancel_unavailable_ms < OLD.cancel_unavailable_ms THEN
        RAISE EXCEPTION 'authority.meta: cancel_unavailable_ms never decreases';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER meta_guard BEFORE UPDATE ON authority.meta FOR EACH ROW EXECUTE FUNCTION authority.meta_guard();

-- Row triggers never fire on TRUNCATE, so each table whose rows are never
-- deleted refuses TRUNCATE with a statement trigger too, for the owner as well
-- (PR review). The owner can still drop a trigger: these stop a command, not
-- DDL. The test harness resets the tables as a replica session.
CREATE FUNCTION authority.never_deleted() RETURNS trigger
LANGUAGE plpgsql SET search_path = authority, pg_temp AS $$
BEGIN
    RAISE EXCEPTION 'authority.%: never deleted', TG_TABLE_NAME;
END $$;
-- A deleted and re-inserted meta row would change realm_id.
CREATE TRIGGER meta_never_deleted BEFORE DELETE ON authority.meta
    FOR EACH ROW EXECUTE FUNCTION authority.never_deleted();
CREATE TRIGGER meta_no_truncate BEFORE TRUNCATE ON authority.meta
    FOR EACH STATEMENT EXECUTE FUNCTION authority.never_deleted();

CREATE TABLE authority.delegations (
    product_root_gen bigint NOT NULL CHECK (product_root_gen >= 0),
    serial           bigint NOT NULL CHECK (serial >= 1),
    bytes            bytea NOT NULL,
    sig_a            bytea NOT NULL CHECK (octet_length(sig_a) = 96),
    sig_b            bytea NOT NULL CHECK (octet_length(sig_b) = 96),
    digest           bytea NOT NULL UNIQUE CHECK (octet_length(digest) = 48),
    installed_at     timestamptz NOT NULL,
    drain_grace_ms   bigint NOT NULL DEFAULT 660000 CHECK (drain_grace_ms IN (0, 660000)),
    PRIMARY KEY (product_root_gen, serial)
);

CREATE FUNCTION authority.delegations_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = authority, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- DoR:1376: drain_grace_ms "can only go 660000 -> 0", so a row is born at
        -- 660000; only the owner's end-drain takes it to 0 (review round 3).
        IF NEW.drain_grace_ms <> 660000 THEN
            RAISE EXCEPTION 'authority.delegations: drain_grace_ms starts at 660000 (D-164)';
        END IF;
        NEW.installed_at := clock_timestamp();
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'authority.delegations: never deleted';
    END IF;
    IF (NEW.product_root_gen, NEW.serial, NEW.bytes, NEW.sig_a, NEW.sig_b, NEW.digest, NEW.installed_at)
       IS DISTINCT FROM (OLD.product_root_gen, OLD.serial, OLD.bytes, OLD.sig_a, OLD.sig_b, OLD.digest, OLD.installed_at) THEN
        RAISE EXCEPTION 'authority.delegations: only drain_grace_ms may change';
    END IF;
    IF NEW.drain_grace_ms <> OLD.drain_grace_ms
       AND NOT (OLD.drain_grace_ms = 660000 AND NEW.drain_grace_ms = 0 AND current_user = 'authority_owner') THEN
        RAISE EXCEPTION 'authority.delegations: drain_grace_ms goes 660000 -> 0 only, as authority_owner (D-164)';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER delegations_guard BEFORE INSERT OR UPDATE OR DELETE ON authority.delegations
    FOR EACH ROW EXECUTE FUNCTION authority.delegations_guard();
CREATE TRIGGER delegations_no_truncate BEFORE TRUNCATE ON authority.delegations
    FOR EACH STATEMENT EXECUTE FUNCTION authority.never_deleted();

CREATE TABLE authority.successions (
    prev_root_gen bigint PRIMARY KEY CHECK (prev_root_gen >= 0),
    bytes         bytea NOT NULL,
    sig_a         bytea NOT NULL CHECK (octet_length(sig_a) = 96),
    sig_b         bytea NOT NULL CHECK (octet_length(sig_b) = 96)
);

CREATE FUNCTION authority.insert_only() RETURNS trigger
LANGUAGE plpgsql SET search_path = authority, pg_temp AS $$
BEGIN
    RAISE EXCEPTION 'authority.%: insert only', TG_TABLE_NAME;
END $$;
CREATE TRIGGER successions_insert_only BEFORE UPDATE OR DELETE ON authority.successions
    FOR EACH ROW EXECUTE FUNCTION authority.insert_only();
CREATE TRIGGER successions_no_truncate BEFORE TRUNCATE ON authority.successions
    FOR EACH STATEMENT EXECUTE FUNCTION authority.insert_only();

CREATE TABLE authority.legacy_population (
    account_id       uuid PRIMARY KEY,
    legacy_ek_digest bytea NOT NULL CHECK (octet_length(legacy_ek_digest) = 48)
);

-- B1-D30: FOR SHARE serializes a change with the seal, which locks meta FOR
-- UPDATE before it digests the population. A change either commits before the
-- seal reads the rows, or waits and then sees legacy_sealed_at.
-- B1-D32: the seal freezes the population, for the owner too.
CREATE FUNCTION authority.legacy_population_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = authority, pg_temp AS $$
DECLARE
    sealed timestamptz;
BEGIN
    SELECT legacy_sealed_at INTO sealed FROM authority.meta WHERE id = 1 FOR SHARE;
    IF sealed IS NOT NULL THEN
        RAISE EXCEPTION 'authority.legacy_population: sealed';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER legacy_population_guard BEFORE INSERT OR UPDATE OR DELETE ON authority.legacy_population
    FOR EACH ROW EXECUTE FUNCTION authority.legacy_population_guard();
-- The same guard as a statement trigger: TRUNCATE is refused once sealed only.
CREATE TRIGGER legacy_population_no_truncate BEFORE TRUNCATE ON authority.legacy_population
    FOR EACH STATEMENT EXECUTE FUNCTION authority.legacy_population_guard();

CREATE TABLE authority.account_heads (
    account_id                  uuid PRIMARY KEY,
    state                       text NOT NULL CHECK (state IN ('absent','active','suspended','pending','disputed','erased')),
    seq                         bigint NOT NULL CHECK (seq >= 0),
    recovery_gen                bigint NOT NULL CHECK (recovery_gen >= 0),
    head_entry_digest           bytea NULL CHECK (head_entry_digest IS NULL OR octet_length(head_entry_digest) = 48),
    head_issued_at_ms           bigint NULL,
    prior_state                 text NULL CHECK (prior_state IS NULL OR prior_state IN ('absent','active','suspended')),
    susp_operator               boolean NOT NULL,
    susp_user                   boolean NOT NULL,
    active_root                 bytea NULL CHECK (active_root IS NULL OR octet_length(active_root) = 97),
    root_revoked                boolean NOT NULL,
    ek_binding_digest           bytea NULL CHECK (ek_binding_digest IS NULL OR octet_length(ek_binding_digest) = 48),
    ek_generation               integer NOT NULL DEFAULT 0 CHECK (ek_generation >= 0),
    pending_root                bytea NULL CHECK (pending_root IS NULL OR octet_length(pending_root) = 97),
    pending_erase               boolean NOT NULL DEFAULT false,
    pending_ek_binding_digest   bytea NULL CHECK (pending_ek_binding_digest IS NULL OR octet_length(pending_ek_binding_digest) = 48),
    pending_ek_generation       integer NULL,
    pending_epoch_digest        bytea NULL CHECK (pending_epoch_digest IS NULL OR octet_length(pending_epoch_digest) = 48),
    activation_base_ms          bigint NULL,
    activation_g_ms             bigint NULL,
    activation_unavail_base_ms  bigint NULL,
    activation_floor_ms         bigint NULL,
    legacy_ek_digest            bytea NULL CHECK (legacy_ek_digest IS NULL OR octet_length(legacy_ek_digest) = 48),
    legacy_pin_revoked          boolean NOT NULL,
    last_cancel_at              timestamptz NULL,
    last_erase_cancel_at        timestamptz NULL,
    held                        boolean NOT NULL DEFAULT false,
    force_unverified            boolean NOT NULL DEFAULT false,
    segment_assurance           text NULL CHECK (segment_assurance IS NULL OR segment_assurance IN ('initial','continuity','recovery','unverified')),
    segment_root                bytea NULL CHECK (segment_root IS NULL OR octet_length(segment_root) = 97),
    erased_at                   timestamptz NULL,
    dseq                        bigint NOT NULL DEFAULT 0 CHECK (dseq >= 0),
    device_head_digest          bytea NULL CHECK (device_head_digest IS NULL OR octet_length(device_head_digest) = 48),
    device_set                  bytea NULL,
    device_set_digest           bytea NULL CHECK (device_set_digest IS NULL OR octet_length(device_set_digest) = 48),
    recovery_bind_assurance     text NULL CHECK (recovery_bind_assurance IS NULL OR recovery_bind_assurance IN ('initial','continuity','recovery','unverified')),
    replacement_bind_assurance  text NULL CHECK (replacement_bind_assurance IS NULL OR replacement_bind_assurance IN ('initial','continuity','recovery','unverified')),
    replacement_unavail_base_ms bigint NULL,
    replacement_g_ms            bigint NULL,
    replacement_floor_ms        bigint NULL,
    recover_guard_until_ms      bigint NULL,
    pending_recover             boolean NOT NULL DEFAULT false,
    CONSTRAINT ah_seq_head CHECK ((seq = 0) = (head_entry_digest IS NULL)),
    CONSTRAINT ah_susp_set CHECK (CASE
        WHEN state = 'suspended' THEN susp_operator OR susp_user
        WHEN state IN ('pending','disputed') AND prior_state = 'suspended' THEN susp_operator OR susp_user
        ELSE NOT susp_operator AND NOT susp_user END),
    CONSTRAINT ah_pending CHECK (state <> 'pending' OR (((pending_root IS NOT NULL) <> pending_erase)
        AND activation_base_ms IS NOT NULL AND activation_g_ms IS NOT NULL AND activation_unavail_base_ms IS NOT NULL)),
    CONSTRAINT ah_pending_erase CHECK (NOT pending_erase OR state = 'pending'),
    CONSTRAINT ah_pending_recover CHECK (NOT pending_recover OR (state = 'pending' AND pending_root IS NOT NULL)),
    CONSTRAINT ah_prior CHECK ((prior_state IS NOT NULL) = (state IN ('pending','disputed'))),
    CONSTRAINT ah_root_revoked CHECK (NOT root_revoked OR active_root IS NOT NULL),
    CONSTRAINT ah_pin_revoked CHECK (NOT legacy_pin_revoked OR legacy_ek_digest IS NOT NULL),
    CONSTRAINT ah_erased CHECK ((state = 'erased') = (erased_at IS NOT NULL)),
    CONSTRAINT ah_dseq CHECK ((dseq = 0) = (device_head_digest IS NULL)),
    CONSTRAINT ah_device_set CHECK ((device_set IS NULL) = (device_set_digest IS NULL)),
    CONSTRAINT ah_slot CHECK ((replacement_unavail_base_ms IS NULL) = (replacement_g_ms IS NULL)),
    CONSTRAINT ah_slot_floor CHECK (replacement_floor_ms IS NULL OR replacement_unavail_base_ms IS NOT NULL)
);

-- Purge candidates (§3.2.4 Purge leg): effectively erased or pending erase.
CREATE INDEX account_heads_purge_candidates ON authority.account_heads (account_id)
    WHERE state = 'erased' OR pending_erase;

CREATE FUNCTION authority.account_heads_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = authority, pg_temp AS $$
BEGIN
    IF OLD.held AND NOT NEW.held AND current_user <> 'authority_owner' THEN
        RAISE EXCEPTION 'authority.account_heads: held true -> false only as authority_owner';
    END IF;
    IF NOT OLD.force_unverified AND NEW.force_unverified AND current_user <> 'authority_owner' THEN
        RAISE EXCEPTION 'authority.account_heads: force_unverified false -> true only as authority_owner';
    END IF;
    IF OLD.force_unverified AND NOT NEW.force_unverified AND NEW.active_root IS NOT DISTINCT FROM OLD.active_root THEN
        RAISE EXCEPTION 'authority.account_heads: force_unverified true -> false only with an active_root change';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER account_heads_guard BEFORE UPDATE ON authority.account_heads
    FOR EACH ROW EXECUTE FUNCTION authority.account_heads_guard();

CREATE TABLE authority.key_bindings (
    key_digest bytea PRIMARY KEY CHECK (octet_length(key_digest) = 48),
    account_id uuid NOT NULL,
    role       text NOT NULL CHECK (role IN ('root','dsk','dkx','ek','recovery')),
    first_seq  bigint NOT NULL CHECK (first_seq >= 0)
);

CREATE TABLE authority.revoked_keys (
    key_digest bytea PRIMARY KEY CHECK (octet_length(key_digest) = 48),
    kind       text NOT NULL CHECK (kind IN ('cak','operator','cp_tls')),
    revoked_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
-- D-207: insert-only, for the owner too, so no command can re-enable a revoked key.
CREATE TRIGGER revoked_keys_insert_only BEFORE UPDATE OR DELETE ON authority.revoked_keys
    FOR EACH ROW EXECUTE FUNCTION authority.insert_only();
CREATE TRIGGER revoked_keys_no_truncate BEFORE TRUNCATE ON authority.revoked_keys
    FOR EACH STATEMENT EXECUTE FUNCTION authority.insert_only();

CREATE TABLE authority.account_events (
    account_id        uuid NOT NULL,
    seq               bigint NOT NULL CHECK (seq >= 1),
    commit_id         bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    restore_epoch     bigint NOT NULL,
    request_id        uuid NOT NULL,
    intent_digest     bytea NOT NULL CHECK (octet_length(intent_digest) = 48),
    op                text NOT NULL CHECK (op IN ('cancel_pending','enroll_existing','enroll_new','erase','recover',
                          'replace_root','revoke_root','rotate_ek','rotate_root','suspend','unsuspend')),
    bundle            bytea NOT NULL,
    cp_evidence       bytea NULL,
    entry_digest      bytea NOT NULL UNIQUE CHECK (octet_length(entry_digest) = 48),
    prev_entry_digest bytea NULL CHECK (prev_entry_digest IS NULL OR octet_length(prev_entry_digest) = 48),
    delegation_gen    bigint NOT NULL,
    delegation_serial bigint NOT NULL,
    issued_at_ms      bigint NOT NULL,
    asserted_at_ms    bigint NOT NULL,
    imported          boolean NOT NULL DEFAULT false,
    committed_at      timestamptz NOT NULL,
    PRIMARY KEY (account_id, seq),
    UNIQUE (account_id, request_id),
    FOREIGN KEY (delegation_gen, delegation_serial) REFERENCES authority.delegations (product_root_gen, serial)
        ON DELETE RESTRICT
);
CREATE INDEX account_events_issued_at_ms_idx ON authority.account_events (issued_at_ms);

CREATE TABLE authority.device_events (
    account_id           uuid NOT NULL,
    dseq                 bigint NOT NULL CHECK (dseq >= 1),
    device_commit_id     bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    request_id           uuid NOT NULL,
    device_intent_digest bytea NOT NULL CHECK (octet_length(device_intent_digest) = 48),
    op                   text NOT NULL CHECK (op IN ('add_device','bind_recovery','cancel_recovery_bind','enroll_existing',
                             'enroll_new','recover','replace_root','rotate_ek','rotate_root','unsuspend')),
    chain_seq            bigint NOT NULL CHECK (chain_seq >= 0),
    chain_entry_digest   bytea NULL CHECK (chain_entry_digest IS NULL OR octet_length(chain_entry_digest) = 48),
    bundle               bytea NOT NULL,
    cp_evidence          bytea NULL,
    entry_digest         bytea NOT NULL UNIQUE CHECK (octet_length(entry_digest) = 48),
    prev_entry_digest    bytea NULL CHECK (prev_entry_digest IS NULL OR octet_length(prev_entry_digest) = 48),
    issued_at_ms         bigint NOT NULL,
    delegation_gen       bigint NOT NULL,
    delegation_serial    bigint NOT NULL,
    imported             boolean NOT NULL DEFAULT false,
    committed_at         timestamptz NOT NULL,
    PRIMARY KEY (account_id, dseq),
    UNIQUE (account_id, request_id),
    FOREIGN KEY (delegation_gen, delegation_serial) REFERENCES authority.delegations (product_root_gen, serial)
        ON DELETE RESTRICT
);

-- F1: committed_at is the database's clock, never the writer's.
CREATE FUNCTION authority.force_committed_at() RETURNS trigger
LANGUAGE plpgsql SET search_path = authority, pg_temp AS $$
BEGIN
    NEW.committed_at := clock_timestamp();
    RETURN NEW;
END $$;
CREATE TRIGGER account_events_committed_at BEFORE INSERT ON authority.account_events
    FOR EACH ROW EXECUTE FUNCTION authority.force_committed_at();
CREATE TRIGGER device_events_committed_at BEFORE INSERT ON authority.device_events
    FOR EACH ROW EXECUTE FUNCTION authority.force_committed_at();

CREATE TABLE authority.request_ledger (
    account_id    uuid NOT NULL,
    request_id    uuid NOT NULL,
    intent_digest bytea NOT NULL CHECK (octet_length(intent_digest) = 48),
    code          text NOT NULL CHECK (code IN ('authz_out_of_window','device_key_bound','device_limit','device_too_new',
                      'device_unknown','ek_generation','ek_mismatch','ek_reused','expired','expiry_out_of_range','held',
                      'in_population','insufficient_authorization','legacy_mismatch','malformed_for_op','policy_disabled',
                      'predecessor_mismatch','recovery_key_bound','recovery_key_unbound','root_bound_elsewhere','root_revoked',
                      'root_reused','stale_device_seq','stale_head','stale_recovery_gen','stale_seq','wrong_state')),
    cp_evidence   bytea NOT NULL,
    recorded_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    prune_after   timestamptz NOT NULL DEFAULT clock_timestamp() + interval '2592000000 milliseconds', -- B1-D11: N_CANCEL_MS
    PRIMARY KEY (account_id, request_id)
);
CREATE INDEX request_ledger_prune_after_idx ON authority.request_ledger (prune_after);

-- §3.2.4 step 7's visible instant as SQL (EXTENSION_CAP_MS = 259 200 000).
-- Task 14 pins agreement with the Go form.
CREATE FUNCTION authority.visible_ms(p_base bigint, p_g bigint, p_growth bigint, p_floor bigint) RETURNS bigint
LANGUAGE sql IMMUTABLE SET search_path = authority, pg_temp AS $$
    SELECT GREATEST(p_base + LEAST(259200000, GREATEST(p_growth, 0)) + p_g, COALESCE(p_floor, 0));
$$;

-- B1-D14: the names in a synchronous_standby_names value. `*` never counts.
CREATE FUNCTION authority.standby_names(p text) RETURNS text[]
LANGUAGE sql IMMUTABLE SET search_path = authority, pg_temp AS $$
    SELECT COALESCE(array_agg(n ORDER BY n) FILTER (WHERE n <> '' AND n <> '*'), '{}')
    FROM (
        SELECT btrim(btrim(x), '"') AS n
        FROM regexp_split_to_table(
            regexp_replace(
                regexp_replace(COALESCE(p, ''), '^\s*(?:(?:first|any)\s+)?(?:[0-9]+\s*)?\(?', '', 'i'),
                '\)\s*$', ''),
            ',') AS x
    ) s;
$$;

CREATE FUNCTION authority.enter_unreconciled() RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = authority, pg_temp AS $$
    UPDATE authority.meta SET reconciled = false WHERE id = 1;
$$;

-- LD-2.2, D-200, D-224: a member of the current sync set has flushed p_lsn.
-- Its owner holds pg_read_all_stats (granted by #3593's provisioner).
CREATE FUNCTION authority.replicated(p_lsn pg_lsn) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = authority, pg_temp AS $$
    SELECT EXISTS (
        SELECT 1 FROM pg_stat_replication r
        WHERE r.sync_state IN ('sync','quorum')
          AND r.flush_lsn >= p_lsn
          AND r.application_name = ANY (authority.standby_names(current_setting('synchronous_standby_names')))
    );
$$;

CREATE FUNCTION authority.replication_state() RETURNS integer
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = authority, pg_temp AS $$
    SELECT count(*)::integer FROM pg_stat_replication r
    WHERE r.sync_state IN ('sync','quorum')
      AND r.application_name = ANY (authority.standby_names(current_setting('synchronous_standby_names')));
$$;

-- LD-22, D-210: the only path that deletes an account's events.
CREATE FUNCTION authority.purge_erased(p uuid) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = authority, pg_temp AS $$
DECLARE
    m       authority.meta%ROWTYPE;
    h       authority.account_heads%ROWTYPE;
    last_op text;
    now_ms  bigint := floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint;
    vis     bigint;
BEGIN
    -- B1-D30: FOR SHARE until this call commits, so a tick (cancel_unavailable_ms)
    -- or a restore (reconciled) cannot change meta between this read and the deletes.
    SELECT * INTO m FROM authority.meta WHERE id = 1 FOR SHARE;
    IF NOT FOUND OR NOT m.reconciled THEN                                        -- (1)
        RETURN false;
    END IF;
    SELECT * INTO h FROM authority.account_heads WHERE account_id = p FOR UPDATE; -- (2) L1
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    SELECT op INTO last_op FROM authority.account_events WHERE account_id = p ORDER BY seq DESC LIMIT 1;
    IF last_op IS DISTINCT FROM 'erase' THEN                                     -- (3)
        RETURN false;
    END IF;
    IF h.state = 'erased' THEN                                                   -- (4) effectively erased
        IF h.erased_at > clock_timestamp() - interval '30 days' THEN
            RETURN false;
        END IF;
    ELSIF h.state = 'pending' AND h.pending_erase AND NOT h.held THEN
        vis := authority.visible_ms(h.activation_base_ms, h.activation_g_ms,
                                    m.cancel_unavailable_ms - h.activation_unavail_base_ms, h.activation_floor_ms);
        IF vis + 2592000000 > now_ms THEN
            RETURN false;
        END IF;
        UPDATE authority.account_heads                                           -- (5) materialise E, root kept
        SET state = 'erased', erased_at = to_timestamp(vis / 1000.0), prior_state = NULL, pending_erase = false,
            susp_operator = false, susp_user = false, activation_base_ms = NULL, activation_g_ms = NULL,
            activation_unavail_base_ms = NULL, activation_floor_ms = NULL
        WHERE account_id = p;
    ELSE
        RETURN false;
    END IF;
    DELETE FROM authority.account_events WHERE account_id = p;                   -- (6)
    DELETE FROM authority.device_events WHERE account_id = p;
    DELETE FROM authority.request_ledger WHERE account_id = p;
    RETURN true;                                                                 -- (7) key_bindings and tombstone untouched
END $$;

CREATE FUNCTION authority.prune_ledger(p_limit integer) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = authority, pg_temp AS $$
DECLARE
    n integer;
BEGIN
    IF p_limit IS NULL OR p_limit < 1 THEN
        RETURN 0;
    END IF;
    WITH d AS (
        SELECT account_id, request_id FROM authority.request_ledger
        WHERE prune_after < now() ORDER BY prune_after LIMIT p_limit FOR UPDATE SKIP LOCKED
    )
    DELETE FROM authority.request_ledger r USING d
    WHERE r.account_id = d.account_id AND r.request_id = d.request_id;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END $$;

-- Grants (D-156).
REVOKE ALL ON ALL TABLES IN SCHEMA authority FROM PUBLIC;
REVOKE ALL ON FUNCTION authority.enter_unreconciled(), authority.replicated(pg_lsn), authority.replication_state(),
    authority.purge_erased(uuid), authority.prune_ledger(integer), authority.visible_ms(bigint, bigint, bigint, bigint),
    authority.standby_names(text), authority.meta_guard(), authority.delegations_guard(), authority.insert_only(),
    authority.legacy_population_guard(), authority.account_heads_guard(), authority.force_committed_at() FROM PUBLIC;
GRANT USAGE ON SCHEMA authority TO authority_app, authority_backup;
GRANT SELECT ON ALL TABLES IN SCHEMA authority TO authority_app, authority_backup;
GRANT INSERT ON authority.account_heads, authority.account_events, authority.device_events, authority.key_bindings,
    authority.request_ledger, authority.legacy_population, authority.delegations, authority.successions TO authority_app;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA authority TO authority_app;
GRANT UPDATE (state, seq, recovery_gen, head_entry_digest, head_issued_at_ms, prior_state, susp_operator, susp_user,
    active_root, root_revoked, ek_binding_digest, ek_generation, pending_root, pending_erase, pending_ek_binding_digest,
    pending_ek_generation, pending_epoch_digest, activation_base_ms, activation_g_ms, activation_unavail_base_ms,
    activation_floor_ms, legacy_ek_digest, legacy_pin_revoked, last_cancel_at, last_erase_cancel_at, held,
    force_unverified, segment_assurance, segment_root, erased_at, dseq, device_head_digest, device_set,
    device_set_digest, recovery_bind_assurance, replacement_bind_assurance, replacement_unavail_base_ms,
    replacement_g_ms, replacement_floor_ms, recover_guard_until_ms, pending_recover)
    ON authority.account_heads TO authority_app;
GRANT UPDATE (last_tick_at, last_peer_contact_at, cancel_unavailable_ms, last_purge_at) ON authority.meta TO authority_app;
GRANT EXECUTE ON FUNCTION authority.enter_unreconciled(), authority.replicated(pg_lsn), authority.replication_state(),
    authority.purge_erased(uuid), authority.prune_ledger(integer) TO authority_app;
