ALTER TABLE messages
    ADD CONSTRAINT messages_key_version_positive
    CHECK (key_version IS NOT NULL AND key_version >= 1) NOT VALID;
