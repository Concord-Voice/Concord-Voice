-- Old handlers may submit an arbitrary positive key_version. Once the
-- fingerprint guard is installed, only epoch 1 (the unrotated base epoch) or
-- an explicitly issued successor epoch may receive wrapped keys.
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
        IF NEW.key_version > 1 THEN
            RAISE EXCEPTION 'channel key rotation epoch has not been issued';
        END IF;
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
