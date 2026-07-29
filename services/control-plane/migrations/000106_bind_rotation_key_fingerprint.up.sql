-- The first batch records the client's successor-CSK assertion, not merely
-- its submitting account. SHA-256(CSK) is safe to retain because a CSK is 256
-- random bits; it lets another current holder assert the same CSK for a later
-- rewrap without storing the CSK itself.
-- Old replicas omit the claim column. Default future omitted claims to FALSE
-- so a current handler can recover them; existing NULL claims remain sealed.
ALTER TABLE key_revocations
    ADD COLUMN rotation_key_fingerprint TEXT,
    ALTER COLUMN rotation_distributor_claimed SET DEFAULT FALSE;

COMMENT ON COLUMN key_revocations.rotation_key_fingerprint IS
    'Standard-base64 SHA-256 of the successor CSK; binds rotation batches without storing the CSK.';

-- Replace the old user-only guard atomically with the fingerprint guard. The
-- initial-distribution branch also requires the transaction-local creator ID,
-- so an old replica cannot bypass the creator fence during a rolling deploy.
CREATE OR REPLACE FUNCTION enforce_rotation_distributor_writer() RETURNS TRIGGER AS $$
DECLARE
    claimed BOOLEAN;
    key_fingerprint TEXT;
    initial_creator_id UUID;
BEGIN
    SELECT creator_id
      INTO initial_creator_id
      FROM channel_initial_key_distributions
     WHERE channel_id = NEW.channel_id
       AND key_version = NEW.key_version;

    IF FOUND
       AND (initial_creator_id IS NULL
            OR current_setting('concord.rotation_distributor_id', TRUE) IS DISTINCT FROM initial_creator_id::TEXT) THEN
        RAISE EXCEPTION 'channel key initial distribution requires its creator';
    END IF;

    SELECT rotation_distributor_claimed, rotation_key_fingerprint
      INTO claimed, key_fingerprint
      FROM key_revocations
     WHERE channel_id = NEW.channel_id
       AND revoked_epoch = NEW.key_version - 1
       AND successor_epoch = NEW.key_version;

    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    IF claimed IS DISTINCT FROM TRUE
       OR key_fingerprint IS NULL
       OR current_setting('concord.rotation_key_fingerprint', TRUE) IS DISTINCT FROM key_fingerprint THEN
        RAISE EXCEPTION 'channel key rotation distribution requires its established key fingerprint';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
