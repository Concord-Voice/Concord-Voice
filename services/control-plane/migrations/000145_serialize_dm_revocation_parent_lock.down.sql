-- Restore migration 000085's trigger definition.  FOR SHARE is compatible
-- with concurrent revocation inserts and does not serialize them against the
-- ciphertext writers' FOR SHARE lock; the forward migration deliberately
-- changes this boundary to FOR NO KEY UPDATE.
CREATE OR REPLACE FUNCTION public.lock_dm_key_revocation_parent()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM 1 FROM public.dm_conversations WHERE id = NEW.conversation_id FOR SHARE;
    RETURN NEW;
END;
$$;
