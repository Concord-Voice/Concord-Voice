-- Migration: rbac_system (down)
-- Purpose: Remove RBAC and SBAC tables

-- Drop tables in reverse dependency order
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS server_bans;
DROP TABLE IF EXISTS key_revocations;
DROP TABLE IF EXISTS channel_permission_overrides;
DROP TABLE IF EXISTS member_roles;
DROP TABLE IF EXISTS roles;
