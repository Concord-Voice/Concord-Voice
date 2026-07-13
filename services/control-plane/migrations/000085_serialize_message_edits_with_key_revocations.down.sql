DROP TRIGGER IF EXISTS serialize_dm_key_revocation ON public.dm_key_revocations;
DROP TRIGGER IF EXISTS serialize_channel_key_revocation ON public.key_revocations;
DROP FUNCTION IF EXISTS public.lock_dm_key_revocation_parent();
DROP FUNCTION IF EXISTS public.lock_channel_key_revocation_parent();
