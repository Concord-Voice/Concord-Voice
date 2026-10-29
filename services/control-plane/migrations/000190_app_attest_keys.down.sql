-- 000190_app_attest_keys.down.sql
-- Unguarded on purpose: a row is a device's attested key, not evidence. If the
-- table goes, the server answers the next assertion with an unknown key and
-- the app generates and attests a fresh key (ADR-0010 D17 recovery path).
DROP TABLE IF EXISTS app_attest_keys;
