-- Add key derivation algorithm tracking to user_keys.
-- Existing rows default to 'pbkdf2'; new registrations will use 'argon2id'.
-- On next login, clients with 'pbkdf2' will re-derive with Argon2id and
-- update this column atomically with the re-wrapped private key.
ALTER TABLE user_keys
    ADD COLUMN key_derivation_alg TEXT NOT NULL DEFAULT 'pbkdf2';
