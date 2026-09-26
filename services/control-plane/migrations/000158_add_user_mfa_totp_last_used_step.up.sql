-- TOTP step replay (RFC 6238 §5.2). The last 30-second time step (Unix time /
-- 30) at which this user's TOTP code was accepted. A code is accepted only for
-- a step strictly later than this one, so each code verifies at most once
-- instead of on every submission inside its ±1-step (~90 s) window.
--
-- Nullable, no default and no backfill: NULL means "no step accepted yet",
-- which is exactly true of every existing row. Adding a nullable column with
-- no default is catalog-only on PG16 (no table rewrite). No index: the column
-- is only ever read and written by primary key, inside the guarded UPDATE.
--
-- An older binary ignores the column entirely, so this is safe to apply ahead
-- of the code that writes it.
ALTER TABLE user_mfa_totp
    ADD COLUMN IF NOT EXISTS last_used_step BIGINT;
