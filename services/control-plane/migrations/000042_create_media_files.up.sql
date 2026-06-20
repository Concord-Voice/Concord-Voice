-- Media files table: tracks all uploaded files across both access tiers.
--
-- Tier 1 (media_tier=1): Authenticated media (avatars, banners, server icons).
--   Server-readable, processed on upload. No E2EE fields set.
--
-- Tier 2 (media_tier=2): E2EE attachments (chat files in encrypted channels/DMs).
--   Client-encrypted ciphertext. channel_id or conversation_id required.

CREATE TABLE media_files (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    uploader_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    file_type       VARCHAR(20) NOT NULL,        -- 'photo', 'animated', 'video', 'audio', 'file'
    media_tier      SMALLINT NOT NULL DEFAULT 2,  -- 1 = authenticated, 2 = e2ee
    mime_type       VARCHAR(100) NOT NULL,
    file_size       BIGINT NOT NULL,              -- bytes (ciphertext size for tier 2)
    storage_key     VARCHAR(500) NOT NULL,        -- MinIO object key (e.g. "avatars/uuid" or "attachments/uuid")

    -- E2EE context (tier 2 only) — which channel or conversation this attachment belongs to
    is_encrypted    BOOLEAN NOT NULL DEFAULT FALSE,
    key_version     INTEGER,                      -- CSK epoch used to wrap the file encryption key
    channel_id      UUID REFERENCES channels(id) ON DELETE CASCADE,
    conversation_id UUID REFERENCES dm_conversations(id) ON DELETE CASCADE,

    -- Lifecycle
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,                  -- soft delete

    -- Only allow known file types
    CONSTRAINT valid_file_type CHECK (
        file_type IN ('photo', 'animated', 'video', 'audio', 'file')
    ),

    -- Tier 1: server-readable, no E2EE fields set
    -- Tier 2: encrypted, exactly one of channel_id/conversation_id (XOR)
    CONSTRAINT valid_media_context CHECK (
        (media_tier = 1
            AND is_encrypted = FALSE
            AND key_version IS NULL
            AND channel_id IS NULL
            AND conversation_id IS NULL
        ) OR
        (media_tier = 2
            AND is_encrypted = TRUE
            AND key_version IS NOT NULL
            AND (
                (channel_id IS NOT NULL AND conversation_id IS NULL) OR
                (channel_id IS NULL AND conversation_id IS NOT NULL)
            )
        )
    )
);

-- Lookup by uploader (profile media, user's uploads)
CREATE INDEX idx_media_files_uploader ON media_files(uploader_id) WHERE deleted_at IS NULL;

-- Lookup by channel (channel attachment listings)
CREATE INDEX idx_media_files_channel ON media_files(channel_id) WHERE channel_id IS NOT NULL AND deleted_at IS NULL;

-- Lookup by conversation (DM attachment listings)
CREATE INDEX idx_media_files_conversation ON media_files(conversation_id) WHERE conversation_id IS NOT NULL AND deleted_at IS NULL;

-- Unique constraint on storage_key for active (non-deleted) files — prevents duplicate keys
CREATE UNIQUE INDEX idx_media_files_storage_key ON media_files(storage_key) WHERE deleted_at IS NULL;
