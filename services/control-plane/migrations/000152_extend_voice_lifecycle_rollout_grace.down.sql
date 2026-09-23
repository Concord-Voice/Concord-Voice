-- 000152_extend_voice_lifecycle_rollout_grace.down.sql
-- WARNING: this data repair is intentionally irreversible because the prior
-- observation timestamps are not retained. This follows the documented
-- 000134 irreversible-repair exception in .codex/rules/migrations.md and the
-- migration README; restoring fabricated timestamps would reintroduce stale
-- voice leases during rollback.
SELECT 1;
