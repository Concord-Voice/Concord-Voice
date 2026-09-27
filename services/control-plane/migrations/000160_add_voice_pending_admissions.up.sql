CREATE TABLE public.voice_pending_admissions (
    channel_id UUID NOT NULL REFERENCES public.channels(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (channel_id, user_id)
);

CREATE INDEX idx_voice_pending_admissions_expires_at
    ON public.voice_pending_admissions (expires_at);
