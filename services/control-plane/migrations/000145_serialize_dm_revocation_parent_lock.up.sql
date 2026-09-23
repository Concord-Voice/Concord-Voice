-- Serialize DM revocation-ledger inserts with DM ciphertext writers.
-- FOR NO KEY UPDATE conflicts with the writers' FOR SHARE lock, making the
-- conversation row the serialization boundary for the revoked-epoch check
-- and the revocation insert.  (The prior FOR SHARE trigger did not conflict.)
CREATE OR REPLACE FUNCTION public.lock_dm_key_revocation_parent()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM 1 FROM public.dm_conversations WHERE id = NEW.conversation_id FOR NO KEY UPDATE;
    RETURN NEW;
END;
$$;
