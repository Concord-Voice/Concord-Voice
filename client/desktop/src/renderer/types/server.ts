export interface Server {
  id: string;
  name: string;
  icon_url?: string;
  banner_url?: string;
  owner_id: string;
  server_tier?: 'groundspeed' | 'mach'; // server-axis tier (#179/#1521); absent on stale cache
  allow_embedded_content: boolean;
  created_at: string;
  updated_at: string;
}

export interface ServerWithRole extends Server {
  role: 'owner' | 'admin' | 'member'; // Legacy — kept for backwards compat during transition
  member_count: number;
  online_count: number;
  permissions?: string; // Stringified bigint of computed server permissions
}

export interface Role {
  id: string;
  server_id: string;
  name: string;
  color?: string;
  emoji?: string;
  position: number;
  permissions: string; // Stringified bigint
  is_default: boolean;
  display_separately: boolean;
  mentionable: boolean;
  require_mfa: boolean;
  created_at: string;
  updated_at: string;
}

export interface MemberRoleInfo {
  role_id: string;
  role_name: string;
  role_color?: string;
  role_emoji?: string;
  position: number;
  display_separately?: boolean;
}

export interface CreateServerRequest {
  name: string;
  icon_url?: string;
}

export interface CreateServerResponse {
  server: Server;
  role: string;
}

export interface ListServersResponse {
  servers: ServerWithRole[];
}

// --- Invite Types ---

export interface ServerInvite {
  id: string;
  server_id: string;
  code: string;
  created_by: string;
  max_uses: number | null;
  use_count: number;
  expires_at: string | null;
  is_revoked: boolean;
  created_at: string;
}

export interface ServerInviteWithCreator extends ServerInvite {
  creator_username: string;
}

export interface CreateInviteRequest {
  max_uses?: number;
  expires_in?: number; // seconds
}

export interface JoinServerRequest {
  code: string;
}

export interface JoinServerResponse {
  server: Server;
  role: string;
}

export interface InviteInfoResponse {
  server_name: string;
  server_icon: string | null;
  server_banner: string | null;
  member_count: number;
  valid: boolean;
}
