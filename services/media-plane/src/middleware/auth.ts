import jwt from 'jsonwebtoken';
import type { Socket, ExtendedError } from 'socket.io';
import { config } from '../config/index.js';
import { logger } from '../lib/logger.js';

// ---------------------------------------------------------------------------
// JWT claims — mirrors the Go control plane's auth.Claims struct.
// The control plane signs JWTs with HS256 using the shared jwtSecret.
// ---------------------------------------------------------------------------
interface JwtClaims {
  user_id: string;
  tier?: string;
  jti?: string;
  iss?: string;
  iat?: number;
  exp?: number;
  nbf?: number;
}

// ---------------------------------------------------------------------------
// Augment socket.data with authenticated user fields.
// These are populated by the auth middleware and available in all handlers.
// ---------------------------------------------------------------------------
export interface AuthenticatedSocketData {
  userId: string;
  username: string;
  tier: string;
  displayName?: string;
  avatarUrl?: string;
  roomId?: string;
  rtpCapabilities?: unknown;
}

// ---------------------------------------------------------------------------
// Socket.IO authentication middleware
//
// Validates the JWT from socket.handshake.auth.token, verifies the HMAC-SHA256
// signature against the shared secret, and attaches user metadata to socket.data.
//
// The client must provide:
//   auth.token      — JWT access token (required)
//   auth.username   — display username (required, since JWT only has user_id)
//   auth.displayName — optional display name
//   auth.avatarUrl  — optional avatar URL
// ---------------------------------------------------------------------------
export function createAuthMiddleware() {
  return (socket: Socket, next: (err?: ExtendedError) => void) => {
    const { token, username, displayName, avatarUrl } = socket.handshake.auth;

    // Require token
    if (!token || typeof token !== 'string') {
      logger.warn('Socket connection rejected: missing token', {
        socketId: socket.id,
        address: socket.handshake.address,
      });
      return next(new Error('Authentication required'));
    }

    // Require username (JWT only carries user_id)
    if (!username || typeof username !== 'string') {
      logger.warn('Socket connection rejected: missing username', {
        socketId: socket.id,
      });
      return next(new Error('Username required'));
    }

    try {
      // Verify JWT signature and expiry
      const decoded = jwt.verify(token, config.jwtSecret, {
        algorithms: ['HS256'],
        issuer: 'concordvoice-control-plane',
      }) as JwtClaims;

      if (!decoded.user_id) {
        logger.warn('Socket connection rejected: missing user_id in token', {
          socketId: socket.id,
        });
        return next(new Error('Invalid token: missing user_id'));
      }

      // Attach authenticated user data to socket
      const socketData = socket.data as AuthenticatedSocketData;
      socketData.userId = decoded.user_id;
      // Entitlement tier rides the signed JWT claim (control-plane auth.Claims.Tier).
      // Absent or blank → 'free' (fail-closed). Consumption at the join/enforcement
      // boundary is #1300/#1542; here we only plumb it onto socket.data.
      socketData.tier =
        typeof decoded.tier === 'string' && decoded.tier ? decoded.tier : 'free';
      socketData.username = username;
      socketData.displayName = typeof displayName === 'string' ? displayName : undefined;
      socketData.avatarUrl = typeof avatarUrl === 'string' ? avatarUrl : undefined;

      logger.debug('Socket authenticated', {
        socketId: socket.id,
        userId: decoded.user_id,
        username,
      });

      next();
    } catch (err) {
      if (err instanceof jwt.TokenExpiredError) {
        logger.warn('Socket connection rejected: token expired', {
          socketId: socket.id,
        });
        return next(new Error('Token expired'));
      }

      if (err instanceof jwt.JsonWebTokenError) {
        logger.warn('Socket connection rejected: invalid token', {
          socketId: socket.id,
          error: (err as Error).message,
        });
        return next(new Error('Invalid token'));
      }

      logger.error('Socket auth unexpected error', {
        socketId: socket.id,
        error: err,
      });
      return next(new Error('Authentication failed'));
    }
  };
}

// ---------------------------------------------------------------------------
// Channel access validation
//
// Called on join-room to verify the user has permission to access the channel.
// Queries the control plane API to check server membership — the channel
// belongs to a server, and the user must be a member of that server.
//
// Returns channel metadata and server enforcement flags for the joining user.
// ---------------------------------------------------------------------------
export interface ChannelAccessResult {
  allowed: boolean;
  channelId: string;
  serverId: string;
  channelName: string;
  serverMuted: boolean;
  serverDeafened: boolean;
  error?: string;
}

/**
 * RoomKind discriminates server-channel rooms from DM-conversation rooms.
 * Sourced from socket.handshake.auth.room_kind in the SFU connection
 * (per #1209 plan task C1 + spec §6.5). The renderer-side voiceService
 * sets this field at Socket.IO handshake time based on joinType.
 *
 * 'channel' (default) → hit /api/v1/channels/{id}/voice/join
 * 'dm'                → hit /api/v1/dm/conversations/{id}/voice/authorize (G7)
 */
export type RoomKind = 'channel' | 'dm';

export async function validateChannelAccess(
  userId: string,
  channelId: string,
  token: string,
  roomKind: RoomKind = 'channel'
): Promise<ChannelAccessResult> {
  try {
    // Route to the appropriate control-plane endpoint based on room kind.
    // The two endpoints have different response shapes (server-channel
    // join returns full channel + enforcement state; DM authorize returns
    // only { authorized, is_group }). Both serve the same purpose:
    // defense-in-depth re-validation of the user's access to the room.
    const endpoint =
      roomKind === 'dm'
        ? `${config.controlPlaneUrl}/api/v1/dm/conversations/${channelId}/voice/authorize`
        : `${config.controlPlaneUrl}/api/v1/channels/${channelId}/voice/join`;

    const channelRes = await fetch(endpoint, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/json',
      },
    });

    if (!channelRes.ok) {
      if (channelRes.status === 401 || channelRes.status === 403) {
        return {
          allowed: false,
          channelId,
          serverId: '',
          channelName: '',
          serverMuted: false,
          serverDeafened: false,
          error: 'Not authorized to access this channel',
        };
      }
      if (channelRes.status === 404) {
        return {
          allowed: false,
          channelId,
          serverId: '',
          channelName: '',
          serverMuted: false,
          serverDeafened: false,
          error: 'Channel not found',
        };
      }
      return {
        allowed: false,
        channelId,
        serverId: '',
        channelName: '',
        serverMuted: false,
        serverDeafened: false,
        error: `Control plane returned ${channelRes.status}`,
      };
    }

    // Response shape differs by room kind:
    //   server channel join → { allowed, channel: {id, server_id, name}, server_muted, server_deafened, ... }
    //   DM authorize        → { authorized, is_group }
    // Both indicate access success, but DM responses don't carry server-channel-specific
    // metadata (serverId, channelName, enforcement flags). For DM rooms those fields
    // default to empty/false in the returned ChannelAccessResult.
    if (roomKind === 'dm') {
      const dmResponse = (await channelRes.json()) as {
        authorized: boolean;
        is_group: boolean;
      };
      const allowed = dmResponse.authorized === true;
      return {
        allowed,
        channelId,
        serverId: '',
        channelName: '',
        serverMuted: false,
        serverDeafened: false,
        // Surface a specific reason when the control-plane returned 200
        // with authorized=false (rare — the 401/403/404 branches above
        // are the usual failure path). Without this the join-room handler
        // falls back to a generic 'Access denied' and the failure cause
        // is obscured (Copilot #1231 cycle-4 finding).
        ...(allowed ? {} : { error: 'DM voice join not authorized' }),
      };
    }

    const responseData = (await channelRes.json()) as {
      allowed: boolean;
      media_server_url: string;
      permissions: string;
      server_muted: boolean;
      server_deafened: boolean;
      channel: {
        id: string;
        server_id: string;
        name: string;
      };
    };

    const channel = responseData.channel;

    // The voice join endpoint validates channel type, membership, and permissions.
    // If we got a 200 with allowed=true, the user has access.
    return {
      allowed: true,
      channelId: channel.id,
      serverId: channel.server_id,
      channelName: channel.name,
      serverMuted: responseData.server_muted ?? false,
      serverDeafened: responseData.server_deafened ?? false,
    };
  } catch (err) {
    logger.error('Failed to validate channel access', {
      userId,
      channelId,
      error: err,
    });
    return {
      allowed: false,
      channelId,
      serverId: '',
      channelName: '',
      serverMuted: false,
      serverDeafened: false,
      error: 'Failed to validate channel access',
    };
  }
}
