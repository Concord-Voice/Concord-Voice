-- Reverse migration 000144.
--
-- Schema is restored exactly. One data change is NOT reversed: the up migration
-- cleared bit 63 from negative channel/category override DENY values. That bit
-- is not a permission, and it could remove nothing -- every value a deny is
-- subtracted from is non-negative -- so no permission outcome depends on it, and
-- the original sign is not recorded anywhere to restore it from. Cross-server
-- member_roles rows and negative grants were never touched: the up migration
-- refuses to proceed over them rather than deleting or rewriting a privilege
-- grant (see its guard blocks).
--
-- Statements are the up migration's, reversed. The UNIQUE must go last: it is
-- the FK's target, and dropping it first would either be refused or take the FK
-- with it.

ALTER TABLE category_permission_overrides
    DROP CONSTRAINT category_permission_overrides_deny_non_negative,
    DROP CONSTRAINT category_permission_overrides_allow_non_negative;

ALTER TABLE channel_permission_overrides
    DROP CONSTRAINT channel_permission_overrides_deny_non_negative,
    DROP CONSTRAINT channel_permission_overrides_allow_non_negative;

ALTER TABLE roles DROP CONSTRAINT roles_permissions_non_negative;

ALTER TABLE member_roles DROP CONSTRAINT member_roles_role_server_fkey;
ALTER TABLE member_roles
    ADD CONSTRAINT member_roles_role_id_fkey
    FOREIGN KEY (role_id)
    REFERENCES roles (id)
    ON DELETE CASCADE;

ALTER TABLE roles DROP CONSTRAINT roles_id_server_key;
