-- Reverse migration 000082. The up migration's NULLing of pre-existing
-- cross-server group bindings is a security cleanup and is intentionally NOT
-- restored (the original foreign bindings were the vulnerability).

ALTER TABLE channels DROP CONSTRAINT channels_group_server_fkey;
ALTER TABLE channels
    ADD CONSTRAINT channels_group_id_fkey
    FOREIGN KEY (group_id)
    REFERENCES channel_groups (id)
    ON DELETE SET NULL;
ALTER TABLE channel_groups DROP CONSTRAINT channel_groups_id_server_key;
