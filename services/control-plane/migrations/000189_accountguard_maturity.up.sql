-- 000189_accountguard_maturity.up.sql
-- #3680 (C4 account guard), PR1, spec section 6.1. Ships DORMANT: nothing reads
-- any column or table created here before PR3's runner.
--
-- What it adds
--   * accountguard_maturity_stamp(): the one function every maturity DEFAULT
--     calls. It returns now() + 72 hours (DeltaCP). PR7b replaces only its body
--     (with the awaiting-notice sentinel 2999-01-01Z), so no default is ever
--     altered twice. Never ALTER ... SET DEFAULT a maturity column again.
--   * Maturity, version and enrolment-provenance columns on users (password and
--     primary email, each on its own) and on the four factor tables.
--   * accountguard_material_version_seq: the one source of every credential
--     version after the legacy 1, on users and the four factor tables alike
--     (Versions, below).
--   * accountguard_is_rewrap(): true only inside a transaction that set the
--     transaction-local GUC concord.credential_rewrap to exactly 'mfa-rekey'
--     (set only by internal/mfa/rekey.go).
--   * One trigger per table, named accountguard_material, BEFORE INSERT OR
--     UPDATE OF its material and version columns. On an INSERT it sets only the
--     version(s). On a material change it moves the
--     version, re-stamps maturity and resets enrolment provenance. "Material" is
--     exactly "this trigger moved a version on an UPDATE"; #2627 keys its change
--     record on that.
--   * accountguard_state: a singleton activation marker, set by PR7b.
--
-- Sentinels (finite, because lib/pq does not scan +/-infinity)
--   1970-01-01Z  legacy: every existing row reads mature and enrolled long ago.
--                Added as the constant "fast default", so no table is rewritten.
--   2999-01-01Z  awaiting notice: reserved for PR7b's stamp function body.
--
-- Material rules (spec H1-a; columns marked * are decisions the spec leaves open)
--   users               password_hash distinct -> password pair;
--                       email distinct -> email pair (each independently)
--   user_mfa_totp       user_id*; enabled false->true; confirmed false->true;
--                       totp_secret_enc / totp_secret_nonce distinct, UNLESS
--                       accountguard_is_rewrap(), key_version moved, and nothing
--                       but totp_secret_enc, totp_secret_nonce, key_version,
--                       updated_at changed
--   user_mfa_webauthn   id*, user_id*, credential_id, public_key
--   user_sso_identities id*, user_id*, provider, provider_user_id
--   user_recovery_keys  user_id*, recovery_wrapped_private_key, recovery_key_salt,
--                       recovery_wrapped_prefs_key, recovery_prefs_key_salt
--   NOT material (H1): re-encryption of the same secret under the rewrap flag,
--   last_used_step, backup_codes_used, the WebAuthn sign count, SSO last_used_at.
-- On a material change the version moves (Versions, below) and maturity := the
-- stamp function when the writer left matures_at IS NOT DISTINCT FROM OLD (a
-- writer-supplied value is kept). On a non-material UPDATE the version is forced
-- back to OLD (the mfa-rekey rewrap included), so a version moves only through
-- the trigger. INSERT leaves maturity and provenance to the column defaults or
-- the writer (H3-b's registration stamp needs that).
--
-- Versions (spec 6.1, amendment A-16; red-team F1)
--   All six version slots (users.password_version, users.email_version and the
--   four factor tables' material_version) draw from
--   accountguard_material_version_seq. START WITH 2 keeps every drawn value
--   apart from the legacy 1, which stays each column's DEFAULT.
--   users               password_version / email_version := nextval on EVERY
--                       INSERT (both slots, discarding any writer-supplied
--                       value, which could equal a later draw for the same row)
--                       and on a material change to that slot. A users row is
--                       never re-inserted under its id.
--   the factor tables   material_version := nextval on EVERY INSERT, discarding
--                       any writer-supplied value, and on every material UPDATE.
--                       user_mfa_totp and user_recovery_keys are keyed by
--                       user_id, so remove-then-re-enrol reuses the row identity,
--                       and a per-row counter (INSERT at 1, then OLD + 1) handed
--                       the new credential a (kind, id, version) its predecessor
--                       held. A material UPDATE draws from the sequence too:
--                       OLD + 1 can equal the value the sequence next hands a
--                       re-inserted slot.
--   Versions are compared for equality only, never with <, and never counted.
--   They have gaps (a rolled-back write, and the BEFORE INSERT that fires on an
--   upsert's conflict path, each spend a value) and no order across slots. The
--   counter is global, so no version, users or factor, ever leaves the server.
--   The triggers name the sequence through TG_TABLE_SCHEMA, never through
--   search_path: a relation lookup searches pg_temp first, so a session's TEMP
--   SEQUENCE of the same name would otherwise choose the version.
--   accountguard_is_rewrap() and accountguard_maturity_stamp() are, by design,
--   resolved through search_path: pg_temp is never searched for functions, and
--   shadowing either from another schema needs arbitrary SQL as the role that
--   owns these tables, which can already DISABLE TRIGGER. That is the accepted
--   boundary of these triggers.
--
-- Provenance: a writer that sets enrolled_at in the statement (IS DISTINCT FROM
-- OLD) has made its provenance statement, so its enrolled_at AND its enrol_class
-- are kept, even when the class equals OLD (a same-class re-enrolment keeps its
-- class). A writer that leaves enrolled_at untouched gets enrolled_at := now()
-- and enrol_class := NULL, except that a class the writer changed alone is kept.
-- enrol_class NULL means unknown (a legacy row, or a writer that bypassed the
-- runner); PR2 reads unknown at the rank floor, so it fails closed. Two material
-- writes to one slot in one transaction see OLD.enrolled_at = now() on the second
-- and reset the class; the runner writes each slot once per transaction.
--
-- Safety
--   * One file, one implicit transaction; SET LOCAL lock_timeout governs every
--     ALTER below. A timeout fails the file and golang-migrate records 189 as
--     dirty (recovery: [internal]deploys.md).
--   * Lock order: users first, then user_mfa_totp, user_mfa_webauthn,
--     user_sso_identities, user_recovery_keys.
--   * Every accountguard_material trigger is ENABLE ALWAYS, so it fires under
--     session_replication_role = replica too (red-team RT-3, at the triggers).
--   * Catalog-only on every existing table: constant defaults, and every new
--     CHECK is NOT VALID, permanently (no validating migration; 000168 precedent).
--   * Every GUC read uses NULLIF(current_setting(name, true), ''), because after a
--     transaction-local set_config commits the same session reads '' and not NULL.
--   * account_auth_changes, its trigger and its CHECK belong to #2627, not here.

SET LOCAL lock_timeout = '3s';

-- The single source of every credential version after the legacy 1: users'
-- password_version and email_version and each factor table's material_version.
-- NO CYCLE is the default and is load-bearing: a cycling sequence would hand a
-- value out twice. It is deliberately not OWNED BY a column: TRUNCATE ...
-- RESTART IDENTITY restarts only owned sequences. The trigger functions run with
-- the writer's rights, so the application role needs USAGE (nextval); it is
-- granted explicitly, as 000159 does, rather than left to owner defaults. Like
-- 000159's grant, it assumes the runtime role is the role that runs this
-- migration; a separate runtime role would fail every material write, password
-- and email changes included, until granted USAGE.
CREATE SEQUENCE accountguard_material_version_seq AS bigint START WITH 2 NO CYCLE;

GRANT USAGE ON SEQUENCE accountguard_material_version_seq TO current_user;

COMMENT ON SEQUENCE accountguard_material_version_seq IS
    'Credential material versions for users and the four factor tables (000189, #3680). Equality only: gapped, unordered across slots, never shown outside the server.';

-- The single source of a new credential's maturity. STABLE: now() is fixed for
-- the statement, so a multi-row INSERT stamps one instant.
CREATE FUNCTION accountguard_maturity_stamp() RETURNS timestamptz
    LANGUAGE sql STABLE
    AS $$ SELECT now() + interval '72 hours' $$;

CREATE FUNCTION accountguard_is_rewrap() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT COALESCE(NULLIF(current_setting('concord.credential_rewrap', true), '') = 'mfa-rekey', false) $$;

CREATE FUNCTION accountguard_material_users() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    -- INSERT discards any writer-supplied version, as on the factor tables: a
    -- kept value could equal a later draw for the same row.
    IF TG_OP = 'INSERT' THEN
        NEW.password_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        NEW.email_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        RETURN NEW;
    END IF;
    IF NEW.password_hash IS DISTINCT FROM OLD.password_hash THEN
        NEW.password_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        IF NEW.password_matures_at IS NOT DISTINCT FROM OLD.password_matures_at THEN
            NEW.password_matures_at := accountguard_maturity_stamp();
        END IF;
        IF NEW.password_enrolled_at IS NOT DISTINCT FROM OLD.password_enrolled_at THEN
            NEW.password_enrolled_at := now();
            IF NEW.password_enrol_class IS NOT DISTINCT FROM OLD.password_enrol_class THEN
                NEW.password_enrol_class := NULL;
            END IF;
        END IF;
    ELSE
        NEW.password_version := OLD.password_version;
    END IF;

    IF NEW.email IS DISTINCT FROM OLD.email THEN
        NEW.email_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        IF NEW.email_matures_at IS NOT DISTINCT FROM OLD.email_matures_at THEN
            NEW.email_matures_at := accountguard_maturity_stamp();
        END IF;
        IF NEW.email_enrolled_at IS NOT DISTINCT FROM OLD.email_enrolled_at THEN
            NEW.email_enrolled_at := now();
            IF NEW.email_enrol_class IS NOT DISTINCT FROM OLD.email_enrol_class THEN
                NEW.email_enrol_class := NULL;
            END IF;
        END IF;
    ELSE
        NEW.email_version := OLD.email_version;
    END IF;

    RETURN NEW;
END
$$;

CREATE FUNCTION accountguard_material_user_mfa_totp() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    -- The only columns a rekey rewrap may change. Comparing whole-row images minus
    -- these means a column added later counts as non-rewrap, so a rekey that
    -- touches it falls back to material (fail closed).
    rewrap_cols CONSTANT text[] := ARRAY['totp_secret_enc', 'totp_secret_nonce', 'key_version', 'updated_at'];
    v_material boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.material_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        RETURN NEW;
    END IF;

    -- A rewrap must also move key_version (red-team RT-2). A genuine rekey
    -- always does: mfa-rekey selects rows whose key_version <> the active one
    -- and writes the active one. A flagged write that keeps key_version is a new
    -- secret under the same key, not a re-encryption of the old one, so it is
    -- material.
    v_material :=
        NEW.user_id IS DISTINCT FROM OLD.user_id
        OR COALESCE(NOT OLD.enabled AND NEW.enabled, false)
        OR COALESCE(NOT OLD.confirmed AND NEW.confirmed, false)
        OR ((NEW.totp_secret_enc IS DISTINCT FROM OLD.totp_secret_enc
             OR NEW.totp_secret_nonce IS DISTINCT FROM OLD.totp_secret_nonce)
            AND NOT (accountguard_is_rewrap()
                     AND NEW.key_version IS DISTINCT FROM OLD.key_version
                     AND (to_jsonb(NEW) - rewrap_cols) = (to_jsonb(OLD) - rewrap_cols)));

    IF v_material THEN
        NEW.material_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        IF NEW.matures_at IS NOT DISTINCT FROM OLD.matures_at THEN
            NEW.matures_at := accountguard_maturity_stamp();
        END IF;
        IF NEW.enrolled_at IS NOT DISTINCT FROM OLD.enrolled_at THEN
            NEW.enrolled_at := now();
            IF NEW.enrol_class IS NOT DISTINCT FROM OLD.enrol_class THEN
                NEW.enrol_class := NULL;
            END IF;
        END IF;
    ELSE
        NEW.material_version := OLD.material_version;
    END IF;

    RETURN NEW;
END
$$;

CREATE FUNCTION accountguard_material_user_mfa_webauthn() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.material_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        RETURN NEW;
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.user_id IS DISTINCT FROM OLD.user_id
       OR NEW.credential_id IS DISTINCT FROM OLD.credential_id
       OR NEW.public_key IS DISTINCT FROM OLD.public_key THEN
        NEW.material_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        IF NEW.matures_at IS NOT DISTINCT FROM OLD.matures_at THEN
            NEW.matures_at := accountguard_maturity_stamp();
        END IF;
        IF NEW.enrolled_at IS NOT DISTINCT FROM OLD.enrolled_at THEN
            NEW.enrolled_at := now();
            IF NEW.enrol_class IS NOT DISTINCT FROM OLD.enrol_class THEN
                NEW.enrol_class := NULL;
            END IF;
        END IF;
    ELSE
        NEW.material_version := OLD.material_version;
    END IF;

    RETURN NEW;
END
$$;

CREATE FUNCTION accountguard_material_user_sso_identities() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.material_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        RETURN NEW;
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.user_id IS DISTINCT FROM OLD.user_id
       OR NEW.provider IS DISTINCT FROM OLD.provider
       OR NEW.provider_user_id IS DISTINCT FROM OLD.provider_user_id THEN
        NEW.material_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        IF NEW.matures_at IS NOT DISTINCT FROM OLD.matures_at THEN
            NEW.matures_at := accountguard_maturity_stamp();
        END IF;
        IF NEW.enrolled_at IS NOT DISTINCT FROM OLD.enrolled_at THEN
            NEW.enrolled_at := now();
            IF NEW.enrol_class IS NOT DISTINCT FROM OLD.enrol_class THEN
                NEW.enrol_class := NULL;
            END IF;
        END IF;
    ELSE
        NEW.material_version := OLD.material_version;
    END IF;

    RETURN NEW;
END
$$;

CREATE FUNCTION accountguard_material_user_recovery_keys() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.material_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        RETURN NEW;
    END IF;

    IF NEW.user_id IS DISTINCT FROM OLD.user_id
       OR NEW.recovery_wrapped_private_key IS DISTINCT FROM OLD.recovery_wrapped_private_key
       OR NEW.recovery_key_salt IS DISTINCT FROM OLD.recovery_key_salt
       OR NEW.recovery_wrapped_prefs_key IS DISTINCT FROM OLD.recovery_wrapped_prefs_key
       OR NEW.recovery_prefs_key_salt IS DISTINCT FROM OLD.recovery_prefs_key_salt THEN
        NEW.material_version := nextval(format('%I.accountguard_material_version_seq', TG_TABLE_SCHEMA)::regclass);
        IF NEW.matures_at IS NOT DISTINCT FROM OLD.matures_at THEN
            NEW.matures_at := accountguard_maturity_stamp();
        END IF;
        IF NEW.enrolled_at IS NOT DISTINCT FROM OLD.enrolled_at THEN
            NEW.enrolled_at := now();
            IF NEW.enrol_class IS NOT DISTINCT FROM OLD.enrol_class THEN
                NEW.enrol_class := NULL;
            END IF;
        END IF;
    ELSE
        NEW.material_version := OLD.material_version;
    END IF;

    RETURN NEW;
END
$$;

-- users. Two statements: the first stores the constant legacy value as the fast
-- default (existing rows read mature, nothing is rewritten); the second points
-- the defaults at the live stamp for every row inserted from now on.
ALTER TABLE users
    ADD COLUMN password_matures_at   timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN email_matures_at      timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN email_contested_at    timestamptz,
    ADD COLUMN password_version      bigint      NOT NULL DEFAULT 1,
    ADD COLUMN email_version         bigint      NOT NULL DEFAULT 1,
    ADD COLUMN password_enrolled_at  timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN email_enrolled_at     timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN password_enrol_class  smallint,
    ADD COLUMN email_enrol_class     smallint,
    ADD CONSTRAINT users_matures_at_finite
        CHECK (isfinite(password_matures_at) AND isfinite(email_matures_at)) NOT VALID,
    ADD CONSTRAINT users_enrolled_at_finite
        CHECK (isfinite(password_enrolled_at) AND isfinite(email_enrolled_at)) NOT VALID,
    ADD CONSTRAINT users_enrol_class_range
        CHECK (password_enrol_class BETWEEN 1 AND 3 AND email_enrol_class BETWEEN 1 AND 3) NOT VALID;

ALTER TABLE users
    ALTER COLUMN password_matures_at  SET DEFAULT accountguard_maturity_stamp(),
    ALTER COLUMN email_matures_at     SET DEFAULT accountguard_maturity_stamp(),
    ALTER COLUMN password_enrolled_at SET DEFAULT now(),
    ALTER COLUMN email_enrolled_at    SET DEFAULT now();

-- user_mfa_totp
ALTER TABLE user_mfa_totp
    ADD COLUMN matures_at             timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN maturity_contested_at  timestamptz,
    ADD COLUMN material_version       bigint      NOT NULL DEFAULT 1,
    ADD COLUMN enrolled_at            timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN enrol_class            smallint,
    ADD CONSTRAINT user_mfa_totp_matures_at_finite   CHECK (isfinite(matures_at)) NOT VALID,
    ADD CONSTRAINT user_mfa_totp_enrolled_at_finite  CHECK (isfinite(enrolled_at)) NOT VALID,
    ADD CONSTRAINT user_mfa_totp_enrol_class_range   CHECK (enrol_class BETWEEN 1 AND 3) NOT VALID;

ALTER TABLE user_mfa_totp
    ALTER COLUMN matures_at  SET DEFAULT accountguard_maturity_stamp(),
    ALTER COLUMN enrolled_at SET DEFAULT now();

-- user_mfa_webauthn
ALTER TABLE user_mfa_webauthn
    ADD COLUMN matures_at             timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN maturity_contested_at  timestamptz,
    ADD COLUMN material_version       bigint      NOT NULL DEFAULT 1,
    ADD COLUMN enrolled_at            timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN enrol_class            smallint,
    ADD CONSTRAINT user_mfa_webauthn_matures_at_finite   CHECK (isfinite(matures_at)) NOT VALID,
    ADD CONSTRAINT user_mfa_webauthn_enrolled_at_finite  CHECK (isfinite(enrolled_at)) NOT VALID,
    ADD CONSTRAINT user_mfa_webauthn_enrol_class_range   CHECK (enrol_class BETWEEN 1 AND 3) NOT VALID;

ALTER TABLE user_mfa_webauthn
    ALTER COLUMN matures_at  SET DEFAULT accountguard_maturity_stamp(),
    ALTER COLUMN enrolled_at SET DEFAULT now();

-- user_sso_identities
ALTER TABLE user_sso_identities
    ADD COLUMN matures_at             timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN maturity_contested_at  timestamptz,
    ADD COLUMN material_version       bigint      NOT NULL DEFAULT 1,
    ADD COLUMN enrolled_at            timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN enrol_class            smallint,
    ADD CONSTRAINT user_sso_identities_matures_at_finite   CHECK (isfinite(matures_at)) NOT VALID,
    ADD CONSTRAINT user_sso_identities_enrolled_at_finite  CHECK (isfinite(enrolled_at)) NOT VALID,
    ADD CONSTRAINT user_sso_identities_enrol_class_range   CHECK (enrol_class BETWEEN 1 AND 3) NOT VALID;

ALTER TABLE user_sso_identities
    ALTER COLUMN matures_at  SET DEFAULT accountguard_maturity_stamp(),
    ALTER COLUMN enrolled_at SET DEFAULT now();

-- user_recovery_keys
ALTER TABLE user_recovery_keys
    ADD COLUMN matures_at             timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN maturity_contested_at  timestamptz,
    ADD COLUMN material_version       bigint      NOT NULL DEFAULT 1,
    ADD COLUMN enrolled_at            timestamptz NOT NULL DEFAULT '1970-01-01Z',
    ADD COLUMN enrol_class            smallint,
    ADD CONSTRAINT user_recovery_keys_matures_at_finite   CHECK (isfinite(matures_at)) NOT VALID,
    ADD CONSTRAINT user_recovery_keys_enrolled_at_finite  CHECK (isfinite(enrolled_at)) NOT VALID,
    ADD CONSTRAINT user_recovery_keys_enrol_class_range   CHECK (enrol_class BETWEEN 1 AND 3) NOT VALID;

ALTER TABLE user_recovery_keys
    ALTER COLUMN matures_at  SET DEFAULT accountguard_maturity_stamp(),
    ALTER COLUMN enrolled_at SET DEFAULT now();

-- Triggers. PR6's guard triggers must sort BEFORE "accountguard_material" (same-
-- event row triggers fire in name order) so a refused write is refused before it
-- is stamped or spends a version. Neither provenance column is in any UPDATE OF
-- list, so a runner's follow-up write to enrol_class fires nothing. A column list
-- narrows only the UPDATE event, so every factor-table INSERT (COPY and an
-- upsert's insert attempt included) fires the trigger.
CREATE TRIGGER accountguard_material
    BEFORE INSERT OR UPDATE OF password_hash, email, password_version, email_version ON users
    FOR EACH ROW EXECUTE FUNCTION accountguard_material_users();

CREATE TRIGGER accountguard_material
    BEFORE INSERT OR UPDATE OF user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed, material_version ON user_mfa_totp
    FOR EACH ROW EXECUTE FUNCTION accountguard_material_user_mfa_totp();

CREATE TRIGGER accountguard_material
    BEFORE INSERT OR UPDATE OF id, user_id, credential_id, public_key, material_version ON user_mfa_webauthn
    FOR EACH ROW EXECUTE FUNCTION accountguard_material_user_mfa_webauthn();

CREATE TRIGGER accountguard_material
    BEFORE INSERT OR UPDATE OF id, user_id, provider, provider_user_id, material_version ON user_sso_identities
    FOR EACH ROW EXECUTE FUNCTION accountguard_material_user_sso_identities();

CREATE TRIGGER accountguard_material
    BEFORE INSERT OR UPDATE OF user_id, recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt, material_version ON user_recovery_keys
    FOR EACH ROW EXECUTE FUNCTION accountguard_material_user_recovery_keys();

-- ALWAYS (red-team RT-3): an ordinary trigger does not fire in a session with
-- session_replication_role = replica. Setting that takes a superuser or a
-- GRANT SET ON PARAMETER, and the application role is a superuser in compose,
-- so one SET would let a session write credential material with no version
-- move and no maturity stamp. internal/authority/store/serve.go (CheckAppRole,
-- lines 140-153) refuses replica for the same reason on the authority's guard
-- triggers. ALWAYS does not stop the table owner's DISABLE TRIGGER; that is the
-- boundary named in the header. Same lock order as above.
ALTER TABLE users               ENABLE ALWAYS TRIGGER accountguard_material;
ALTER TABLE user_mfa_totp       ENABLE ALWAYS TRIGGER accountguard_material;
ALTER TABLE user_mfa_webauthn   ENABLE ALWAYS TRIGGER accountguard_material;
ALTER TABLE user_sso_identities ENABLE ALWAYS TRIGGER accountguard_material;
ALTER TABLE user_recovery_keys  ENABLE ALWAYS TRIGGER accountguard_material;

-- Activation marker: exactly one row. PR3's runner must treat a missing row as an
-- error, never as "not activated". PR7b sets activated_at.
CREATE TABLE accountguard_state (
    singleton    boolean PRIMARY KEY DEFAULT true,
    activated_at timestamptz,
    CONSTRAINT accountguard_state_singleton CHECK (singleton)
);

INSERT INTO accountguard_state DEFAULT VALUES;
