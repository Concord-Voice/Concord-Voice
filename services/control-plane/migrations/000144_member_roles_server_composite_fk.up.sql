-- Bind a member_roles row to the server its role belongs to, and pin every
-- permission bitfield non-negative (#2869).
--
-- migration 000035 declared `role_id UUID NOT NULL REFERENCES roles(id)` beside a
-- separate composite FK to server_members(server_id, user_id). Nothing tied
-- roles.server_id to member_roles.server_id, so a row naming THIS server and
-- THAT server's role was schema-legal. Until this change every hierarchy and
-- permission aggregate joined on role_id alone, so such a row imported a foreign
-- role's position and permission bits -- including bit 62, PermAdministrator --
-- into the victim server. This is the shape migration 000082 used for
-- channel_groups.
--
-- WHY THIS FAILS CLOSED INSTEAD OF CLEANING UP, unlike 000082.
-- 000082's violating state was API-REACHABLE: CreateChannel / UpdateChannel /
-- ReorderChannels could each produce a cross-server group binding, so rows were
-- expected and a DELETE/UPDATE cleanup was the only way to make the FK
-- attachable. Here the state is API-UNREACHABLE. All four production writers are
-- server-scoped and were re-audited for this migration:
--
--   rbac/handlers.go     AssignRole  -- roleGuardQuery's WHERE r.id = $1 AND r.server_id = $2
--   members/handlers.go  AddMember   -- SELECT ... FROM roles WHERE server_id = $1 AND is_default
--   invites/handlers.go  redeem      -- same server-scoped default-role SELECT
--   servers/handlers.go  CreateServer-- creates the role in the same transaction
--
-- internal/ownership and the admin/ops console hold NO member_roles writer at
-- all. So a surviving row is not debris to sweep; it is evidence -- of an
-- unfound writer, or of direct database access. Deleting it silently would
-- destroy that evidence and convert an incident into a clean migration log.
-- Rolling one back is an operator decision made with the count in hand.
--
-- The RAISE also replaces Postgres's opaque "insert or update on table
-- ... violates foreign key constraint", which names neither the offending row
-- nor how many there are.
-- Every statement below needs ACCESS EXCLUSIVE on these tables anyway (DROP
-- CONSTRAINT and ADD CHECK take it). Taking it first closes the window in which a
-- concurrent write could land between a guard's COUNT and the constraint that
-- rejects it -- which would still fail the migration, but with Postgres's opaque
-- constraint error instead of the guard's diagnosis. Same pattern as 000098/000111.
LOCK TABLE roles, member_roles, channel_permission_overrides, category_permission_overrides
    IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    cross_server_rows BIGINT;
BEGIN
    SELECT COUNT(*) INTO cross_server_rows
      FROM member_roles mr
     WHERE NOT EXISTS (
         SELECT 1 FROM roles r
          WHERE r.id = mr.role_id
            AND r.server_id = mr.server_id
     );

    IF cross_server_rows > 0 THEN
        RAISE EXCEPTION
            'member_roles holds % row(s) naming a role from a different server; every production writer is server-scoped, so such a row is evidence of an unaudited writer or direct database access, not expected debris. Investigate before migrating -- this migration deliberately does not delete privilege-grant rows. Enumerate with: SELECT mr.id, mr.server_id, mr.user_id, mr.role_id FROM member_roles mr WHERE NOT EXISTS (SELECT 1 FROM roles r WHERE r.id = mr.role_id AND r.server_id = mr.server_id);',
            cross_server_rows;
    END IF;
END $$;

-- Permission bitfields are BIGINT. Bit 62 is PermAdministrator and bit 63 -- the
-- sign bit -- is not a permission at all, yet all three readers take it as data
-- and disagree about what it means: Go reads an int64, SQL a signed BIGINT, and
-- the client parses the field with BigInt, whose bitwise operators sign-extend it
-- into every bit above 63. The CHECKs below pin five columns to the defined
-- 63-bit range so the sign bit cannot carry meaning.
--
-- What the CHECK does NOT do is keep PermAdministrator out: 1 << 62 is positive.
-- What it refuses is the sign bit, and with it -1, the natural "every permission"
-- sentinel -- which does carry bit 62, and which in the actor's own BIT_OR makes
-- `Permission(req) &^ actorPerms` zero for every input, opening the escalation
-- subset check. Any all-bits value does that; -1 is merely the one a client is
-- likely to send.
--
-- Negative values ARE API-writable today, so neither guard below may claim an
-- unaudited writer the way the member_roles guard above does:
--
--   roles.permissions  the server owner, via UpdateRole, which exempts the owner
--                      from the subset check and writes the field unmasked
--   *.allow            a PermAdministrator holder, whom UpsertChannelOverride and
--                      UpsertCategoryOverride exempt from the same check
--   *.deny             any PermManageChannels holder; deny is never subset-checked
--
-- The two halves are therefore split by what a negative value DOES, not by who
-- could have written it.
--
-- GRANTS FAIL CLOSED. A negative permission or allow is a grant, and the common
-- one, -1, grants PermAdministrator. Clearing bit 63 would keep that grant intact
-- (bits 0-62 are untouched) while making it look like clean data. An operator
-- should decide what it was meant to be, with the rows in hand.
DO $$
DECLARE
    negative_roles    BIGINT;
    negative_channel  BIGINT;
    negative_category BIGINT;
BEGIN
    SELECT COUNT(*) INTO negative_roles    FROM roles WHERE permissions < 0;
    SELECT COUNT(*) INTO negative_channel  FROM channel_permission_overrides  WHERE allow < 0;
    SELECT COUNT(*) INTO negative_category FROM category_permission_overrides WHERE allow < 0;

    IF negative_roles + negative_channel + negative_category > 0 THEN
        RAISE EXCEPTION
            'found % role(s), % channel override(s) and % category override(s) granting a negative permission bitfield. -1 grants PermAdministrator (bit 62) along with every other bit, so review each before migrating -- this migration deliberately does not rewrite grants. Enumerate with: SELECT id, server_id, permissions FROM roles WHERE permissions < 0; SELECT id, channel_id, target_type, target_id, allow FROM channel_permission_overrides WHERE allow < 0; SELECT id, category_id, target_type, target_id, allow FROM category_permission_overrides WHERE allow < 0;',
            negative_roles, negative_channel, negative_category;
    END IF;
END $$;

-- DENIES ARE NORMALISED. The resolver applies a deny as `perms &^= deny`, so a
-- negative deny can only remove permissions. Its bit 63 removes nothing: the
-- grants guard above and the CHECK below keep bit 63 out of every value a deny
-- is subtracted from. Clearing it is therefore exact -- every permission outcome
-- is identical before and after -- and refusing to migrate over a restriction
-- would block a deploy on a row that is already fail-safe. updated_at is left
-- alone for the same reason: no permission outcome changed.
UPDATE channel_permission_overrides  SET deny = deny & 9223372036854775807 WHERE deny < 0;
UPDATE category_permission_overrides SET deny = deny & 9223372036854775807 WHERE deny < 0;

-- The FK target. roles.id is already the primary key, so this superset unique
-- always holds and the index build cannot fail on duplicates. Column order
-- mirrors 000082's channel_groups_id_server_key.
ALTER TABLE roles
    ADD CONSTRAINT roles_id_server_key UNIQUE (id, server_id);

-- Replace the existence-only FK (created inline by 000035, auto-named
-- member_roles_role_id_fkey) with a same-server composite FK.
--
-- ON DELETE CASCADE is carried over unchanged: role_id is NOT NULL, so deleting
-- the role must delete the assignment row, exactly as before. 000082 used the
-- PG15+ column-list SET NULL form only because its referencing column was
-- nullable; that does not apply here.
--
-- No new index. The cascade deletes from member_roles filtered on both FK
-- columns, and idx_member_roles_role(role_id) already leads with role_id, so it
-- serves that lookup.
--
-- Not split into NOT VALID + VALIDATE. The guard above proves the table holds no
-- violating row, so validation is a clean scan, and member_roles is one row per
-- (member, role) on a pre-GA deployment. Split it if the scans ever hold the
-- ACCESS EXCLUSIVE locks taken above long enough to matter -- which also means
-- narrowing that LOCK TABLE; the 000121/000122 pair is the shape.
ALTER TABLE member_roles DROP CONSTRAINT member_roles_role_id_fkey;
ALTER TABLE member_roles
    ADD CONSTRAINT member_roles_role_server_fkey
    FOREIGN KEY (role_id, server_id)
    REFERENCES roles (id, server_id)
    ON DELETE CASCADE;

ALTER TABLE roles
    ADD CONSTRAINT roles_permissions_non_negative CHECK (permissions >= 0);

ALTER TABLE channel_permission_overrides
    ADD CONSTRAINT channel_permission_overrides_allow_non_negative CHECK (allow >= 0),
    ADD CONSTRAINT channel_permission_overrides_deny_non_negative CHECK (deny >= 0);

-- The category table is the enforcement SOURCE for every synced channel:
-- syncCategoryOverridesToChannels copies these rows into
-- channel_permission_overrides, so constraining only the destination would turn
-- a negative category value into a failed sync rather than a refused write.
ALTER TABLE category_permission_overrides
    ADD CONSTRAINT category_permission_overrides_allow_non_negative CHECK (allow >= 0),
    ADD CONSTRAINT category_permission_overrides_deny_non_negative CHECK (deny >= 0);
