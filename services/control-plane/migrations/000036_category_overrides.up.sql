-- Category permission overrides: SBAC layer for category-specific permissions
-- Mirrors channel_permission_overrides but references channel_groups
CREATE TABLE IF NOT EXISTS category_permission_overrides (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    category_id UUID NOT NULL REFERENCES channel_groups(id) ON DELETE CASCADE,
    target_type VARCHAR(10) NOT NULL CHECK (target_type IN ('user', 'role')),
    target_id UUID NOT NULL,  -- user_id or role_id depending on target_type
    allow BIGINT NOT NULL DEFAULT 0,  -- Bitfield of allowed permissions
    deny BIGINT NOT NULL DEFAULT 0,   -- Bitfield of denied permissions (takes precedence)
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(category_id, target_type, target_id)
);

CREATE INDEX idx_category_overrides_category ON category_permission_overrides(category_id);
CREATE INDEX idx_category_overrides_target ON category_permission_overrides(target_type, target_id);

-- Add sync_permissions flag to channels: when true, channel inherits overrides from its parent category
ALTER TABLE channels ADD COLUMN sync_permissions BOOLEAN NOT NULL DEFAULT FALSE;
