-- Recreate the prior guard without scanning dm_messages under the table lock.
ALTER TABLE dm_messages
    DROP CONSTRAINT IF EXISTS dm_messages_key_version_positive;
ALTER TABLE dm_messages
    ADD CONSTRAINT dm_messages_key_version_positive
    CHECK (key_version IS NOT NULL AND key_version >= 1) NOT VALID;
