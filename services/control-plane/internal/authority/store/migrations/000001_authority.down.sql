-- Refuses while any authority row exists: DoR §3.2.1 names events, and B1-D27
-- widens it to every table, because a recorded refusal commits a head and a
-- ledger row with no event. The down migration exists for an empty test
-- database. The schema belongs to the bootstrap and is kept.
-- The lock comes first: a writer that commits after its table's check but
-- before its DROP would otherwise have its row dropped.
LOCK TABLE authority.meta, authority.delegations, authority.successions, authority.legacy_population,
    authority.account_heads, authority.key_bindings, authority.revoked_keys, authority.account_events,
    authority.device_events, authority.request_ledger IN ACCESS EXCLUSIVE MODE;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM authority.meta) OR EXISTS (SELECT 1 FROM authority.delegations)
       OR EXISTS (SELECT 1 FROM authority.successions) OR EXISTS (SELECT 1 FROM authority.legacy_population)
       OR EXISTS (SELECT 1 FROM authority.account_heads) OR EXISTS (SELECT 1 FROM authority.key_bindings)
       OR EXISTS (SELECT 1 FROM authority.revoked_keys) OR EXISTS (SELECT 1 FROM authority.account_events)
       OR EXISTS (SELECT 1 FROM authority.device_events) OR EXISTS (SELECT 1 FROM authority.request_ledger) THEN
        RAISE EXCEPTION 'authority 000001 down refused: authority rows exist';
    END IF;
END $$;

DROP FUNCTION authority.prune_ledger(integer);
DROP FUNCTION authority.purge_erased(uuid);
DROP FUNCTION authority.replication_state();
DROP FUNCTION authority.replicated(pg_lsn);
DROP FUNCTION authority.enter_unreconciled();
DROP TABLE authority.request_ledger;
DROP TABLE authority.device_events;
DROP TABLE authority.account_events;
DROP FUNCTION authority.force_committed_at();
DROP TABLE authority.revoked_keys;
DROP TABLE authority.key_bindings;
DROP TABLE authority.account_heads;
DROP FUNCTION authority.account_heads_guard();
DROP TABLE authority.legacy_population;
DROP FUNCTION authority.legacy_population_guard();
DROP TABLE authority.successions;
DROP FUNCTION authority.insert_only();
DROP TABLE authority.delegations;
DROP FUNCTION authority.delegations_guard();
DROP TABLE authority.meta;
DROP FUNCTION authority.never_deleted();
DROP FUNCTION authority.meta_guard();
DROP FUNCTION authority.standby_names(text);
DROP FUNCTION authority.visible_ms(bigint, bigint, bigint, bigint);
