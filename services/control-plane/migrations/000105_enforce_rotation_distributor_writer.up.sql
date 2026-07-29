-- Old binaries do not record the actor that inserts channel-key batches. The
-- database therefore rejects all successor-epoch inserts unless a current
-- handler has claimed the rotation and set its transaction-local actor ID.
ALTER TABLE key_revocations
    DROP CONSTRAINT key_revocations_rotation_distributor_sealed;

CREATE FUNCTION enforce_rotation_distributor_writer() RETURNS TRIGGER AS $$
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

CREATE TRIGGER enforce_rotation_distributor_writer_before_channel_key_insert
    BEFORE INSERT ON channel_keys
    FOR EACH ROW EXECUTE FUNCTION enforce_rotation_distributor_writer();
