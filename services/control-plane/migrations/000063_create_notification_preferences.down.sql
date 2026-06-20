-- Drops the entire mute-preferences table. Safe under [internal]rules/migrations.md
-- "No DROP TABLE without confirmed data migration" because this table is
-- net-new in the corresponding up migration and rolling back means the
-- feature is being removed entirely. IF EXISTS makes the down idempotent.
DROP TABLE IF EXISTS notification_preferences;
