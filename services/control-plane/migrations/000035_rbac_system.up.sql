-- Migration: rbac_system (up)
-- Purpose: Add RBAC (Role-Based Access Control) and SBAC (Server-Based Access Control) tables
-- Phase: 2A

-- Roles table: Define server roles with permission bitfields
CREATE TABLE IF NOT EXISTS roles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id UUID NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    name VARCHAR(100) NOT NULL,
    color VARCHAR(7),         -- Hex color for role badge (e.g., #5865F2)
    emoji VARCHAR(32),         -- Optional emoji for role display
    position INTEGER NOT NULL DEFAULT 0,  -- Higher position = higher role in hierarchy
    permissions BIGINT NOT NULL DEFAULT 0, -- Permission bitfield
    is_default BOOLEAN DEFAULT FALSE,      -- Auto-assigned to new members (@all role)
    is_managed BOOLEAN DEFAULT FALSE,      -- System-managed (cannot be deleted)
    mentionable BOOLEAN DEFAULT FALSE,     -- Can @mention this role
    display_separately BOOLEAN DEFAULT FALSE, -- Display members with this role separately in member list
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(server_id, name),
    CHECK (position >= 0)
);

-- Index for efficient role lookups by server
CREATE INDEX idx_roles_server_id ON roles(server_id);
-- Index for position-based queries (hierarchy)
CREATE INDEX idx_roles_position ON roles(server_id, position DESC);
-- Index for default role queries (used during member join)
CREATE INDEX idx_roles_default ON roles(server_id, is_default) WHERE is_default = TRUE;

-- Member roles: Many-to-many relationship between members and roles
CREATE TABLE IF NOT EXISTS member_roles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id UUID NOT NULL,
    user_id UUID NOT NULL,
    role_id UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    assigned_at TIMESTAMPTZ DEFAULT NOW(),
    assigned_by UUID REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE(server_id, user_id, role_id),
    FOREIGN KEY (server_id, user_id) REFERENCES server_members(server_id, user_id) ON DELETE CASCADE
);

-- Index for member role lookups
CREATE INDEX idx_member_roles_user ON member_roles(server_id, user_id);
CREATE INDEX idx_member_roles_role ON member_roles(role_id);

-- Channel permission overrides: SBAC layer for channel-specific permissions
CREATE TABLE IF NOT EXISTS channel_permission_overrides (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    target_type VARCHAR(10) NOT NULL CHECK (target_type IN ('user', 'role')),
    target_id UUID NOT NULL,  -- user_id or role_id depending on target_type
    allow BIGINT NOT NULL DEFAULT 0,  -- Bitfield of allowed permissions
    deny BIGINT NOT NULL DEFAULT 0,   -- Bitfield of denied permissions (takes precedence)
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(channel_id, target_type, target_id)
);

-- Index for permission override lookups
CREATE INDEX idx_channel_overrides_channel ON channel_permission_overrides(channel_id);
CREATE INDEX idx_channel_overrides_target ON channel_permission_overrides(target_type, target_id);

-- Key revocations: Track CSK rotation for E2EE forward secrecy
CREATE TABLE IF NOT EXISTS key_revocations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    revoked_epoch INTEGER NOT NULL,      -- The key version being revoked
    successor_epoch INTEGER NOT NULL,    -- The new key version replacing it
    reason VARCHAR(50) NOT NULL,         -- 'member_removal', 'permission_revocation', 'manual_rotation'
    revoked_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(channel_id, revoked_epoch),
    CHECK (successor_epoch > revoked_epoch)
);

-- Replace single-column index from migration 28 with composite index for epoch lookups
DROP INDEX IF EXISTS idx_key_revocations_channel;
CREATE INDEX idx_key_revocations_channel ON key_revocations(channel_id, revoked_epoch);

-- Server bans: Track banned users (prevents rejoin via invite)
CREATE TABLE IF NOT EXISTS server_bans (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id UUID NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    banned_by UUID REFERENCES users(id) ON DELETE SET NULL,
    reason TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(server_id, user_id)
);

-- Index for ban checks during invite redemption
CREATE INDEX idx_server_bans_server ON server_bans(server_id, user_id);

-- Audit log: Track permission-related administrative actions
CREATE TABLE IF NOT EXISTS audit_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id UUID NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    actor_id UUID REFERENCES users(id) ON DELETE SET NULL,  -- Who performed the action
    action VARCHAR(50) NOT NULL,        -- 'role_created', 'role_deleted', 'permission_granted', etc.
    target_type VARCHAR(20) NOT NULL,   -- 'role', 'member', 'channel', 'permission'
    target_id UUID,                     -- ID of the affected resource
    metadata JSONB,                     -- Additional context (old/new values, reason, etc.)
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Index for audit log queries (by server and time)
CREATE INDEX idx_audit_log_server ON audit_log(server_id, created_at DESC);
CREATE INDEX idx_audit_log_actor ON audit_log(actor_id, created_at DESC);
