-- 000152_extend_voice_lifecycle_rollout_grace.up.sql
-- Existing rows received their first database-observed lease at migration 000127.
-- Refresh eligible observations so the normal 90-second stale threshold gives
-- active rows one rollout grace interval in which to heartbeat again.
UPDATE voice_participants
SET lifecycle_observed_at = clock_timestamp()
WHERE lifecycle_observed_at <= clock_timestamp();
