-- Custom channel groups (categories) — replaces hardcoded Bulletins/Text/Voice grouping.
-- Channels reference a group via group_id FK. NULL group_id = "Uncategorized" (rendered last).

CREATE TABLE channel_groups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id UUID NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    name VARCHAR(100) NOT NULL,
    position INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_channel_groups_server ON channel_groups(server_id);

ALTER TABLE channels ADD COLUMN group_id UUID REFERENCES channel_groups(id) ON DELETE SET NULL;
