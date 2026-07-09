-- CV-CAN-010/011/012: enforce that a channel's group_id references a category
-- (channel_groups) owned by the SAME server. Without this, CreateChannel /
-- UpdateChannel / ReorderChannels could bind a channel to another server's
-- category, and the permission-sync cascade (which keys on group_id with no
-- server predicate) would copy that server's overrides into the channel.
--
-- The handlers now reject a cross-server group_id at the write boundary; this
-- migration is the structural backstop via a composite (group_id, server_id) FK.

-- 1. Purge ALL permission overrides on cross-server-bound channels. The sync
--    cascade (syncCategoryOverridesToChannels / copyCategoryOverridesToChannel)
--    replaces a channel's channel_permission_overrides with a copy of its
--    category's rows, keyed by channel_id. The RBAC resolver
--    (applyChannelOverrides / GetVisibleChannelIDs) applies those rows by
--    channel_id with no group predicate, so once step 2 nulls group_id the
--    foreign category's grants/denies would stay active and orphaned (and harder
--    to spot without the group_id trail). Delete them here, while the
--    cross-server binding is still identifiable.
--
--    We do NOT gate on sync_permissions. Disabling sync (SetChannelPermissionSync)
--    only flips the flag; it does not purge the rows a prior enable copied in, so
--    a channel can be cross-bound with sync_permissions = FALSE yet still hold
--    foreign-derived overrides. There is also no marker distinguishing derived
--    from manually-set rows. Since step 2 resets these (rare, pre-existing) rows
--    to uncategorized anyway, the fail-safe is to clear every override on a
--    cross-bound channel rather than risk leaving a foreign row behind. Like the
--    group_id cleanup below, this is a one-way security fix and is intentionally
--    not restored by the down migration.
DELETE FROM channel_permission_overrides cpo
USING channels c
WHERE cpo.channel_id = c.id
  AND c.group_id IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM channel_groups g
    WHERE g.id = c.group_id AND g.server_id = c.server_id
  );

-- 2. Null out any pre-existing cross-server bindings so the composite FK can be
--    added without violating existing rows. This is a security cleanup — the
--    foreign bindings were the vulnerability — and is intentionally not
--    restored by the down migration.
--
--    Also reset sync_permissions to FALSE on these channels. They are now
--    uncategorized (group_id nulled) with their overrides purged in step 1, so
--    leaving the sync flag TRUE would advertise an active category sync that no
--    longer has a source category. Clearing it keeps the flag consistent with
--    the cleaned state.
UPDATE channels c
SET group_id = NULL,
    sync_permissions = FALSE
WHERE c.group_id IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM channel_groups g
    WHERE g.id = c.group_id AND g.server_id = c.server_id
  );

-- 3. Composite unique on channel_groups(id, server_id) — the FK target. id is
--    already the primary key, so this superset unique always holds.
ALTER TABLE channel_groups
    ADD CONSTRAINT channel_groups_id_server_key UNIQUE (id, server_id);

-- 4. Replace the existence-only FK (created inline by migration 000022,
--    auto-named channels_group_id_fkey) with a same-server composite FK.
--    ON DELETE SET NULL (group_id) nulls ONLY group_id when the category is
--    deleted — a bare SET NULL would also try to null the NOT NULL server_id.
--    Column-list SET NULL requires PostgreSQL 15+ (we run 16).
ALTER TABLE channels DROP CONSTRAINT channels_group_id_fkey;
ALTER TABLE channels
    ADD CONSTRAINT channels_group_server_fkey
    FOREIGN KEY (group_id, server_id)
    REFERENCES channel_groups (id, server_id)
    ON DELETE SET NULL (group_id);
