-- 000066_client_attestation_registry.down.sql
-- Reverses the up migration. DROP TABLE removes the composite primary key,
-- all columns (including revoked_by), and the partial revoked-at index.
-- release_spas carries a single html_hash column under sha7 spa_version keys
-- (no dual hash, no date keys) — symmetric drop.

DROP INDEX IF EXISTS idx_release_spas_revoked;
DROP INDEX IF EXISTS idx_release_spas_published_at;
DROP TABLE IF EXISTS release_spas;

DROP INDEX IF EXISTS idx_release_binaries_revoked;
DROP INDEX IF EXISTS idx_release_binaries_published_at;
DROP TABLE IF EXISTS release_binaries;
