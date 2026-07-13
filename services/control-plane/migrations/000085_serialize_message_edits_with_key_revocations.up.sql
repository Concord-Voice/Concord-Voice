-- Serialize message edits with key-epoch revocations on their stable parent row.
-- SHARE is compatible with FK KEY SHARE and concurrent revocations, while it
-- conflicts with the edit handlers' NO KEY UPDATE lock.
CREATE FUNCTION public.lock_channel_key_revocation_parent()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM 1 FROM public.channels WHERE id = NEW.channel_id FOR SHARE;
    RETURN NEW;
END;
$$;

CREATE TRIGGER serialize_channel_key_revocation
BEFORE INSERT ON public.key_revocations
FOR EACH ROW
EXECUTE FUNCTION public.lock_channel_key_revocation_parent();

CREATE FUNCTION public.lock_dm_key_revocation_parent()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM 1 FROM public.dm_conversations WHERE id = NEW.conversation_id FOR SHARE;
    RETURN NEW;
END;
$$;

CREATE TRIGGER serialize_dm_key_revocation
BEFORE INSERT ON public.dm_key_revocations
FOR EACH ROW
EXECUTE FUNCTION public.lock_dm_key_revocation_parent();
