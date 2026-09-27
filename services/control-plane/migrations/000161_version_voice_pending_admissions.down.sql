ALTER TABLE public.voice_pending_admissions
    DROP COLUMN IF EXISTS socket_id,
    DROP COLUMN IF EXISTS admission_id;
