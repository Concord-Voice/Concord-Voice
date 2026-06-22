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
      socketData.tier = typeof decoded.tier === 'string' && decoded.tier ? decoded.tier : 'free';
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
  // ── Per-user media entitlements (#1300) ────────────────────────────────
  // The joining user's server-authoritative media caps, resolved by the
  // control-plane (entitlements.For(GetTier(userID))) and carried in the
  // join-authorize response. Consumed at the participant's own transport /
  // produce boundary in RoomManager — NEVER sourced from socket.handshake.auth.
  userTier: string;
  allowedAudioTiers: string[];
  minPtimeMs: number;
  maxManualBitrateBps: number;
  error?: string;
}

// ---------------------------------------------------------------------------
// FREE_MEDIA_ENTITLEMENT — the fail-closed floor for per-user media caps.
//
// This is the ONLY place the Node media-plane mirrors Go entitlement values.
// It is justified as the missing-field fallback ONLY: it mirrors the Go FREE
// floor (`entitlements.For("")` → free) so a malformed / absent
// `media_entitlements` object resolves to the strictest tier. Because free IS
// the floor, value drift between Go and Node can only ever make Node STRICTER
// than intended — never escalate a user to a higher tier. (Premium values are
// deliberately NOT mirrored here: they only ever arrive from the trusted
// control-plane response, never reconstructed locally.)
//
// Source of truth: services/control-plane/internal/entitlements/entitlements.go
// (`free` Entitlement). Keep these in sync with that file's FREE values.
// ---------------------------------------------------------------------------
export const FREE_MEDIA_ENTITLEMENT: {
  tier: string;
  allowedAudioTiers: string[];
  minPtimeMs: number;
  maxManualBitrateBps: number;
} = {
  tier: 'free',
  allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard'],
  minPtimeMs: 20,
  maxManualBitrateBps: 5_000_000,
};

/**
 * Wire shape of the `media_entitlements` object in the control-plane
 * join-authorize response (snake_case per the Go MarshalJSON tags). All fields
 * are validated structurally before use; anything that fails validation falls
 * through to FREE_MEDIA_ENTITLEMENT (fail-closed).
 */
interface MediaEntitlementsWire {
  tier?: unknown;
  allowed_audio_tiers?: unknown;
  min_ptime_ms?: unknown;
  max_manual_bitrate_bps?: unknown;
}

/**
 * Parse the control-plane `media_entitlements` object into the typed
 * ChannelAccessResult fields, FAIL-CLOSED to FREE_MEDIA_ENTITLEMENT on any
 * absent or malformed field. A partially-malformed object does not partially
 * escalate: each field independently falls back to the free floor, so a
 * tampered/garbage value can never raise a cap above the free value.
 */
function parseMediaEntitlements(raw: unknown): {
  userTier: string;
  allowedAudioTiers: string[];
  minPtimeMs: number;
  maxManualBitrateBps: number;
} {
  if (typeof raw !== 'object' || raw === null) {
    return {
      userTier: FREE_MEDIA_ENTITLEMENT.tier,
      allowedAudioTiers: [...FREE_MEDIA_ENTITLEMENT.allowedAudioTiers],
      minPtimeMs: FREE_MEDIA_ENTITLEMENT.minPtimeMs,
      maxManualBitrateBps: FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps,
    };
  }

  const me = raw as MediaEntitlementsWire;

  const userTier = typeof me.tier === 'string' && me.tier ? me.tier : FREE_MEDIA_ENTITLEMENT.tier;

  // allowed_audio_tiers must be a non-empty array of strings; otherwise floor.
  const allowedAudioTiers =
    Array.isArray(me.allowed_audio_tiers) &&
    me.allowed_audio_tiers.length > 0 &&
    me.allowed_audio_tiers.every((t) => typeof t === 'string')
      ? me.allowed_audio_tiers
      : [...FREE_MEDIA_ENTITLEMENT.allowedAudioTiers];

  // Numeric caps must be finite positive numbers; otherwise floor.
  const minPtimeMs =
    typeof me.min_ptime_ms === 'number' && Number.isFinite(me.min_ptime_ms) && me.min_ptime_ms > 0
      ? me.min_ptime_ms
      : FREE_MEDIA_ENTITLEMENT.minPtimeMs;

  const maxManualBitrateBps =
    typeof me.max_manual_bitrate_bps === 'number' &&
    Number.isFinite(me.max_manual_bitrate_bps) &&
    me.max_manual_bitrate_bps > 0
      ? me.max_manual_bitrate_bps
      : FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps;

  // Atomic-free clamp (#1300 adversarial-review defence-in-depth). The
  // per-field validation above floors INVALID fields but keeps any VALID
  // premium-shaped value regardless of the resolved tier. That leaves a
  // (client-unreachable, but possible via a control-plane bug or a
  // downgrade race) cross-field inconsistency: a `tier: "free"` object that
  // still carries a valid premium `min_ptime_ms`/`max_manual_bitrate_bps`/
  // `allowed_audio_tiers`. Tie the caps to the tier: only the explicit
  // PREMIUM tier may carry premium values; anything else (free OR an unknown
  // tier string — fail-closed) is clamped to the free floor, so a free tier
  // can never carry a premium cap. Clamp DIRECTION matters — minPtime floors
  // UP (lower ptime is the premium lever), the others floor DOWN/to the free
  // set. Premium values flow through untouched only when the tier is premium.
  if (userTier !== 'premium') {
    return {
      userTier,
      allowedAudioTiers: [...FREE_MEDIA_ENTITLEMENT.allowedAudioTiers],
      minPtimeMs: Math.max(minPtimeMs, FREE_MEDIA_ENTITLEMENT.minPtimeMs),
      maxManualBitrateBps: Math.min(
        maxManualBitrateBps,
        FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps
      ),
    };
  }

  return { userTier, allowedAudioTiers, minPtimeMs, maxManualBitrateBps };
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
  // Denial-path results carry the fail-closed free floor for the per-user media
  // caps (#1300) — a denied join never grants a media entitlement above free.
  const deniedResult = (error: string): ChannelAccessResult => ({
    allowed: false,
    channelId,
    serverId: '',
    channelName: '',
    serverMuted: false,
    serverDeafened: false,
    userTier: FREE_MEDIA_ENTITLEMENT.tier,
    allowedAudioTiers: [...FREE_MEDIA_ENTITLEMENT.allowedAudioTiers],
    minPtimeMs: FREE_MEDIA_ENTITLEMENT.minPtimeMs,
    maxManualBitrateBps: FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps,
    error,
  });

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
        return deniedResult('Not authorized to access this channel');
      }
      if (channelRes.status === 404) {
        return deniedResult('Channel not found');
      }
      return deniedResult(`Control plane returned ${channelRes.status}`);
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
        media_entitlements?: unknown;
      };
      const allowed = dmResponse.authorized === true;
      // Per-user media caps are room-kind-independent (spec §3): the DM
      // authorize response carries media_entitlements too. Parse fail-closed.
      const ent = parseMediaEntitlements(dmResponse.media_entitlements);
      return {
        allowed,
        channelId,
        serverId: '',
        channelName: '',
        serverMuted: false,
        serverDeafened: false,
        userTier: ent.userTier,
        allowedAudioTiers: ent.allowedAudioTiers,
        minPtimeMs: ent.minPtimeMs,
        maxManualBitrateBps: ent.maxManualBitrateBps,
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
      media_entitlements?: unknown;
    };

    const channel = responseData.channel;

    // Per-user media caps (#1300): parse the server-authoritative
    // media_entitlements object, FAIL-CLOSED to the free floor if absent /
    // malformed. The tier is never taken from socket.handshake.auth.
    const ent = parseMediaEntitlements(responseData.media_entitlements);

    // The voice join endpoint validates channel type, membership, and permissions.
    // If we got a 200 with allowed=true, the user has access.
    return {
      allowed: true,
      channelId: channel.id,
      serverId: channel.server_id,
      channelName: channel.name,
      serverMuted: responseData.server_muted ?? false,
      serverDeafened: responseData.server_deafened ?? false,
      userTier: ent.userTier,
      allowedAudioTiers: ent.allowedAudioTiers,
      minPtimeMs: ent.minPtimeMs,
      maxManualBitrateBps: ent.maxManualBitrateBps,
    };
  } catch (err) {
    logger.error('Failed to validate channel access', {
      userId,
      channelId,
      error: err,
    });
    return deniedResult('Failed to validate channel access');
  }
}
