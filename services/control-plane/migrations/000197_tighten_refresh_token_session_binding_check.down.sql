-- 000197_tighten_refresh_token_session_binding_check.down.sql
-- Restores 000195's CHECK, NOT VALID as 000195 added it. The old check admits
-- every row the new one admits, so the down cannot fail on data.
SET LOCAL lock_timeout = '3s';

ALTER TABLE refresh_tokens
    DROP CONSTRAINT refresh_tokens_pop_binding_check,
    ADD CONSTRAINT refresh_tokens_pop_binding_check
    CHECK ((pop_jkt IS NULL AND pop_alg IS NULL)
        OR (pop_alg = 'ES256' AND char_length(pop_jkt) = 43)) NOT VALID;
