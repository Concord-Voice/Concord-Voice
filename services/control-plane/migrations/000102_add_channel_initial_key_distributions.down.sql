DROP TRIGGER IF EXISTS delete_incomplete_channels_for_erased_creator ON users;
DROP FUNCTION IF EXISTS delete_incomplete_channels_for_erased_creator();

LOCK TABLE channel_initial_key_distributions IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM channel_initial_key_distributions) THEN
        RAISE EXCEPTION 'refusing to drop initial key distribution fence while incomplete distributions exist';
    END IF;
END
$$;

DROP TABLE channel_initial_key_distributions;
