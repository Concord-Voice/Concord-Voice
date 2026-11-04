-- 000196_accountguard_requests.up.sql
-- #3680 (C4 account guard), PR3, spec section 6.2. Ships DORMANT: nothing reads or
-- writes any table created here until PR3's runner is activated (PR7b).
--
-- What it adds (five new tables; no existing table is touched)
--   * account_pending_requests: the pending-request state machine. One row per
--     request (credential removal, address change, break-glass), carrying its
--     status, hold accounting, origin and request fingerprints, cancellation
--     provenance and the sealed payloads a completion needs.
--   * account_pending_request_origins: the credential versions that opened a
--     request (its provenance), one row per (credential, version).
--   * user_credential_history: retired passwords and addresses, so a reset or a
--     contest can restore the credential the owner held.
--   * account_address_reservations: a displaced primary address, held back from
--     reuse until its floor passes.
--   * accountguard_hmac_keys: the HMAC key-id registry. fingerprint_key_id names a
--     row here; there is no foreign key, because the registry is not user data and
--     a fingerprint with an unregistered or stale key id must read as "does not
--     match" (it falls back to the coarse fingerprint), never as a write error.
--
-- Named CHECKs on account_pending_requests (the per-status field rules)
--   apr_disputed_fields       disputed        => disputed_at and dispute_group_id
--   apr_completed_fields      completed       => completed_at
--   apr_cancelled_fields      two-directional: cancelled <=> cancel_reason set, and
--                             cancelled_at set only on a cancelled row (cancelled is
--                             terminal, so no writer clears it)
--   apr_canceller_class       canceller_class is set exactly when cancel_reason is
--                             proof or dispute_resolved
--   apr_terminal_payload      completed or cancelled => both payloads and
--                             held_since are NULL
--   apr_fingerprint_retention request_fingerprint and coarse_fingerprint are NULL
--                             unless the row is open (pending or disputed) or was
--                             cancelled with reason proof or dispute_resolved.
--                             origin_fingerprint is NOT NULL always.
--   apr_target_shape          break_glass => no target; credential_remove => all
--                             three target columns. address_change is unconstrained
--                             here (C5 owns its target shape).
--   apr_key_id_pairs          request_fingerprint and fingerprint_key_id are set
--                             together, so a keyed fingerprint always names its key.
--   The column CHECKs (kind, status, origin_class, cancel_reason, canceller_class,
--   held_ms, origin_fingerprint length) carry auto-generated names.
--
-- There is deliberately NO index on (dispute_group_id, origin_fingerprint), unique
-- or not (spec section 11, amendment A-20). The parent design's partial UNIQUE over
-- that pair, WHERE status = 'disputed', would refuse a second disputed row that
-- shares a group and an origin set, and the merge rule (PR2 ruling 12) needs those
-- rows to coexist. Do not add it back.
--
-- One pending break-glass per user is a partial UNIQUE index, not a CHECK: it is a
-- cross-row rule. A disputed or terminal break-glass does not count.
--
-- Addresses are stored already normalized, and the schema enforces it. Every writer
-- stores `address` (user_credential_history.address, account_address_reservations.address,
-- account_pending_requests.pending_address) folded by the application's normalizer
-- (email.NormalizeAddress: Unicode white space trimmed, then lower-cased) and then by
-- lower(btrim(x)). Three CHECKs refuse any other form:
--   aar_address_normalized          account_address_reservations: address =
--                                   lower(btrim(address)), with no white space
--   uch_address_normalized          user_credential_history: address is NULL or
--                                   address = lower(btrim(address)), with no white space
--   apr_pending_address_normalized  account_pending_requests: pending_address is NULL
--                                   or pending_address = lower(btrim(pending_address)),
--                                   with no white space
-- "No white space" is anywhere in the value, not only at its ends: a stored address is
-- already trimmed and an email address has no interior space. btrim strips ASCII spaces
-- only, so each CHECK also refuses [[:space:]] and, named explicitly because the class
-- follows the database's ctype and can miss them, U+0085, U+00A0, U+1680, U+2000-U+200B,
-- U+2028, U+2029, U+202F, U+205F, U+3000 and U+FEFF. The raw-text PK on
-- account_address_reservations and the (kind, address) index on user_credential_history
-- rely on that: the database does not fold case or Unicode space, so an un-normalized
-- write would split one address into two keys. The CHECK pins the normal form, not the
-- full identifier rule; the writer's normalizer stays the authority.
--
-- Deploy and recovery: [internal]deploys.md, "Account-guard request tables
-- (migration 000196, #3680)".
--
-- Safety
--   * One file, one implicit transaction; SET LOCAL lock_timeout governs every
--     statement below. A timeout fails the file and golang-migrate records 196 as
--     dirty (recovery: [internal]deploys.md). The user_id foreign keys take a
--     brief SHARE ROW EXCLUSIVE lock on users while each empty table is created
--     (the 000111 note), so deploy this migration ahead of the binary.
--   * The tables are new and empty, so every index is a plain CREATE INDEX and every
--     CHECK is validated by construction; none is NOT VALID.
--   * Retention is the sweeper's job (history 30 d, reservations by floor, terminal
--     requests by policy), not the schema's: no trigger and no expiry here.
--   * No secret is stored in the clear. pending_password_hash is an Argon2id hash;
--     the fingerprints are SHA-256 or HMAC digests; no column holds an address
--     except pending_address and the history and reservation rows, which the spec
--     names (the addresses are the data the guard exists to protect).

SET LOCAL lock_timeout = '3s';

CREATE TABLE account_pending_requests (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                 uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind                    text        NOT NULL
        CHECK (kind IN ('credential_remove', 'address_change', 'break_glass')),
    target_kind             text,
    target_id               text,
    target_material_version bigint,
    reset_factors           boolean     NOT NULL DEFAULT false,
    status                  text        NOT NULL
        CHECK (status IN ('pending', 'disputed', 'completed', 'cancelled')),
    origin_class            smallint    NOT NULL
        CHECK (origin_class BETWEEN 1 AND 3),
    origin_fingerprint      bytea       NOT NULL
        CHECK (octet_length(origin_fingerprint) = 32),
    request_fingerprint     bytea,
    coarse_fingerprint      bytea,
    fingerprint_key_id      smallint,
    dispute_group_id        uuid,
    opened_at               timestamptz NOT NULL,
    completes_at            timestamptz NOT NULL,
    held_ms                 bigint      NOT NULL DEFAULT 0
        CHECK (held_ms >= 0),
    held_since              timestamptz,
    disputed_at             timestamptz,
    completed_at            timestamptz,
    cancelled_at            timestamptz,
    cancel_reason           text
        CHECK (cancel_reason IN ('proof', 'dispute_resolved', 'superseded', 'hold_cap', 'dispute_expired')),
    canceller_class         smallint
        CHECK (canceller_class BETWEEN 1 AND 3),
    pending_password_hash   text,
    pending_address         text,

    CONSTRAINT apr_disputed_fields CHECK (
        status <> 'disputed' OR (disputed_at IS NOT NULL AND dispute_group_id IS NOT NULL)
    ),
    CONSTRAINT apr_completed_fields CHECK (
        status <> 'completed' OR completed_at IS NOT NULL
    ),
    -- Every operand is a boolean that cannot be NULL (status is NOT NULL and
    -- IS [NOT] NULL never yields NULL), so no arm can pass by evaluating to NULL.
    CONSTRAINT apr_cancelled_fields CHECK (
        ((status = 'cancelled') = (cancel_reason IS NOT NULL))
        AND (status <> 'cancelled' OR cancelled_at IS NOT NULL)
        AND (status = 'cancelled' OR cancelled_at IS NULL)
    ),
    CONSTRAINT apr_canceller_class CHECK (
        (canceller_class IS NOT NULL) = COALESCE(cancel_reason IN ('proof', 'dispute_resolved'), false)
    ),
    CONSTRAINT apr_terminal_payload CHECK (
        status NOT IN ('completed', 'cancelled')
        OR (pending_password_hash IS NULL AND pending_address IS NULL AND held_since IS NULL)
    ),
    CONSTRAINT apr_fingerprint_retention CHECK (
        status IN ('pending', 'disputed')
        OR (status = 'cancelled' AND COALESCE(cancel_reason IN ('proof', 'dispute_resolved'), false))
        OR (request_fingerprint IS NULL AND coarse_fingerprint IS NULL)
    ),
    CONSTRAINT apr_target_shape CHECK (
        (kind <> 'break_glass'
            OR (target_kind IS NULL AND target_id IS NULL AND target_material_version IS NULL))
        AND (kind <> 'credential_remove'
            OR (target_kind IS NOT NULL AND target_id IS NOT NULL AND target_material_version IS NOT NULL))
    ),
    CONSTRAINT apr_key_id_pairs CHECK (
        (request_fingerprint IS NOT NULL) = (fingerprint_key_id IS NOT NULL)
    ),
    CONSTRAINT apr_pending_address_normalized CHECK (
        pending_address IS NULL OR (
            pending_address = lower(btrim(pending_address))
            AND pending_address !~ '[[:space:]\u0085\u00A0\u1680\u2000-\u200B\u2028\u2029\u202F\u205F\u3000\uFEFF]'
        )
    )
);

-- The sweeper's due scan.
CREATE INDEX idx_apr_completes_at_pending
    ON account_pending_requests (completes_at)
    WHERE status = 'pending';

CREATE INDEX idx_apr_user_status
    ON account_pending_requests (user_id, status);

-- The re-open lookup: a prior proof or dispute-resolved cancel of the same request.
CREATE INDEX idx_apr_cancel_lookup
    ON account_pending_requests (user_id, request_fingerprint, cancelled_at)
    WHERE cancel_reason IN ('proof', 'dispute_resolved');

-- At most one pending break-glass per user.
CREATE UNIQUE INDEX idx_apr_one_pending_break_glass
    ON account_pending_requests (user_id)
    WHERE kind = 'break_glass' AND status = 'pending';

-- Key retirement: find the rows still stamped with a key id.
CREATE INDEX idx_apr_fingerprint_key_id
    ON account_pending_requests (fingerprint_key_id);

CREATE TABLE account_pending_request_origins (
    request_id       uuid   NOT NULL REFERENCES account_pending_requests (id) ON DELETE CASCADE,
    credential_kind  text   NOT NULL,
    credential_id    text   NOT NULL,
    material_version bigint NOT NULL,
    PRIMARY KEY (request_id, credential_kind, credential_id, material_version)
);

CREATE TABLE user_credential_history (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind          text        NOT NULL
        CHECK (kind IN ('password', 'address')),
    password_hash text,
    address       text,
    retired_at    timestamptz NOT NULL,
    was_mature    boolean     NOT NULL,
    was_primary   boolean,
    was_verified  boolean,

    CONSTRAINT uch_kind_fields CHECK (
        (kind = 'password'
            AND password_hash IS NOT NULL AND address IS NULL)
        OR (kind = 'address'
            AND address IS NOT NULL AND was_primary IS NOT NULL AND was_verified IS NOT NULL
            AND password_hash IS NULL)
    ),
    CONSTRAINT uch_address_normalized CHECK (
        address IS NULL OR (
            address = lower(btrim(address))
            AND address !~ '[[:space:]\u0085\u00A0\u1680\u2000-\u200B\u2028\u2029\u202F\u205F\u3000\uFEFF]'
        )
    )
);

CREATE INDEX idx_uch_address
    ON user_credential_history (kind, address)
    WHERE kind = 'address';

CREATE INDEX idx_uch_user_kind_retired
    ON user_credential_history (user_id, kind, retired_at);

-- request_id has no foreign key: a reservation outlives the request that displaced
-- the address (it is released by its floor and the current primary, not by the
-- request's terminal state).
CREATE TABLE account_address_reservations (
    address      text        PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    request_id   uuid,
    reason       text        NOT NULL
        CHECK (reason IN ('displaced_primary')),
    displaced_at timestamptz NOT NULL,
    floor_until  timestamptz NOT NULL,

    CONSTRAINT aar_address_normalized CHECK (
        address = lower(btrim(address))
        AND address !~ '[[:space:]\u0085\u00A0\u1680\u2000-\u200B\u2028\u2029\u202F\u205F\u3000\uFEFF]'
    )
);

CREATE INDEX idx_aar_user_id
    ON account_address_reservations (user_id);

-- Key ids are small and stamped on keyed rows. GENERATED ALWAYS: only the database
-- assigns an id. key_check is a 32-byte digest that identifies the key without
-- revealing it.
CREATE TABLE accountguard_hmac_keys (
    id         smallint    GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    key_check  bytea       NOT NULL UNIQUE
        CHECK (octet_length(key_check) = 32),
    created_at timestamptz NOT NULL DEFAULT now()
);
