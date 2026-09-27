-- Migration 000160 creates this table immediately before this migration, so it
-- is empty and both NOT NULL additions are metadata-only.
ALTER TABLE public.voice_pending_admissions
    ADD COLUMN admission_id UUID NOT NULL,
    ADD COLUMN socket_id TEXT NOT NULL
        CHECK (socket_id <> '' AND length(socket_id) <= 128);
