-- 000079 DOWN: drop the case-insensitive unique index (#1931).
--
-- IRREVERSIBLE DATA NOTE: the up migration's `UPDATE users SET username =
-- LOWER(username)` case-fold cannot be reversed — the original mixed-case form is
-- not recorded anywhere, so there is nothing to restore. This is an accepted
-- irreversibility per [internal]rules/migrations.md (identity is case-insensitive,
-- so the displayed-case change is cosmetic). The down migration therefore only
-- reverses the schema change (the index); it does NOT attempt to un-fold case.
DROP INDEX IF EXISTS users_username_lower_key;
