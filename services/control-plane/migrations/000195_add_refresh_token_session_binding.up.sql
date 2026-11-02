-- 000195_add_refresh_token_session_binding.up.sql
-- Secure Enclave session binding (ADR-0050, option D of the mobile refresh
-- token design). A bound refresh token row records the RFC 7638 thumbprint of
-- the device key that must sign a DPoP-shaped proof on every refresh.
--
-- Both columns are nullable with no default, so ADD COLUMN is metadata-only and
-- every existing row reads as unbound. The CHECK keeps the pair whole and admits
-- only ES256, the one algorithm ADR-0050 allows. It is added NOT VALID, so it
-- holds every write from here on without a scan under the lock. No VALIDATE is
-- needed: the columns are new, so every existing row is NULL and passes.
ALTER TABLE refresh_tokens
    ADD COLUMN pop_jkt TEXT,
    ADD COLUMN pop_alg TEXT;

ALTER TABLE refresh_tokens ADD CONSTRAINT refresh_tokens_pop_binding_check
    CHECK ((pop_jkt IS NULL AND pop_alg IS NULL)
        OR (pop_alg = 'ES256' AND char_length(pop_jkt) = 43)) NOT VALID;
