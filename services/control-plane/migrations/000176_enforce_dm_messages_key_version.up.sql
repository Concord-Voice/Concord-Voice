ALTER TABLE dm_messages
    ALTER COLUMN key_version SET NOT NULL,
    ALTER COLUMN key_version DROP DEFAULT;
