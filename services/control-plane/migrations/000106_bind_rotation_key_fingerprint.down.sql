-- Do not silently discard an established CSK fingerprint: 000105's user-only
-- guard would let a second device on that account split a live epoch again.
LOCK TABLE key_revocations IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM key_revocations
        WHERE rotation_key_fingerprint IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'refusing to drop established rotation key fingerprints; restore a pre-000106 backup or complete a safe rotation first';
    END IF;
END
$$;

-- Restore the 000105 guard only before any fingerprint has been used, so a
-- sequential 000106 → 000105 rollback remains valid.
CREATE OR REPLACE FUNCTION enforce_rotation_distributor_writer() RETURNS TRIGGER AS $$
DECLARE
    claimed BOOLEAN;
    distributor_id UUID;
BEGIN
    SELECT rotation_distributor_claimed, rotation_distributor_id
     INTO claimed, distributor_id
      FROM key_revocations
     WHERE channel_id = NEW.channel_id
       AND revoked_epoch = NEW.key_version - 1
       AND successor_epoch = NEW.key_version;

    IF NOT FOUND OR EXISTS (
        SELECT 1
        FROM channel_initial_key_distributions
        WHERE channel_id = NEW.channel_id
          AND key_version = NEW.key_version
    ) THEN
        RETURN NEW;
    END IF;

    IF claimed IS DISTINCT FROM TRUE
       OR current_setting('concord.rotation_distributor_id', TRUE) IS DISTINCT FROM distributor_id::TEXT THEN
        RAISE EXCEPTION 'channel key rotation distribution requires its claimed distributor';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE key_revocations
    ALTER COLUMN rotation_distributor_claimed DROP DEFAULT,
    DROP COLUMN rotation_key_fingerprint;
