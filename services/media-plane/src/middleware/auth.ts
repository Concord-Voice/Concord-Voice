import jwt from 'jsonwebtoken';
import { createHash, createHmac } from 'node:crypto';
import type { Socket, ExtendedError } from 'socket.io';
import { config } from '../config/index.js';
import { logger } from '../lib/logger.js';

// CV-CAN-007: upper bound of a valid effective-permission bitfield. The
// control-plane serializes it via strconv.FormatInt(int64(perms), 10) where
// rbac.Permission is an int64, so any legitimate non-negative value is at most
// the max positive int64 (2^63 - 1). A larger decimal (e.g. "18446744073709551615")
// cannot come from a valid rbac.Permission, so it is treated as malformed and
// fails closed rather than having its low Speak/Video/ScreenShare bits honored.
const MAX_PERMISSION_BITFIELD = (1n << 63n) - 1n;

/**
 * Strict fail-closed parser for the server-authoritative permission bitfield
 * wire format (decimal string, non-negative, at most max int64). Returns
 * undefined for anything else — hex, arrays, numbers, negatives, and
 * out-of-range decimals all fail closed. Shared by the join-authorize response
 * parse below and the voice.enforce.permissions NATS consumer (CV-CAN-007).
 */
export function parsePermissionBitfield(raw: unknown): bigint | undefined {
  if (typeof raw !== 'string' || !/^\d+$/.test(raw)) return undefined;
  const parsed = BigInt(raw);
  if (parsed > MAX_PERMISSION_BITFIELD) return undefined;
  return parsed;
}

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
  /** Server-validated DM call instance ID; undefined only for server channels. */
  callId?: string;
  /** Server-validated ring that originated the DM call, when one exists. */
  callRingId?: string;
  /** Server-validated caller (ring initiator or direct /voice/join reserver). */
  callCallerUserId?: string;
  /**
   * Room-scoped cap tier (#1542) — the server OWNER's tier for channel rooms,
   * parsed from the join-authorize `room_owner_tier` field, fail-closed to 'free'.
   * `undefined` for DM rooms (the media-plane uses max present-participant tier)
   * and on denial paths.
   */
  roomOwnerTier?: string;
  /**
   * Whether the control-plane join-authorize response carried a
   * server-authoritative identity (CV-CAN-017): TRUE when the response included
   * a string `username` field (even empty), FALSE for a pre-CV-CAN-017
   * control-plane that omits it. This is the identity-awareness discriminator
   * consumed by `resolveParticipantIdentity` — NOT the non-emptiness of
   * `authUsername` — so an identity-aware control-plane that returns an empty
   * username fails CLOSED to the authoritative (empty) identity instead of
   * re-opening every field to the spoofable handshake values.
   */
  authIdentityPresent?: boolean;
  /**
   * Server-authoritative display identity, resolved by the control-plane from
   * the authenticated user_id and returned on BOTH the channel and DM
   * join-authorize responses (CV-CAN-017). The join handler prefers these over
   * the client-supplied socket.handshake.auth values so a member cannot spoof
   * its display identity to peers. Each is `undefined` when the user genuinely
   * has none (empty/absent from the server) — never re-opened to the handshake
   * value. The handshake fallback is gated on `authIdentityPresent`, NOT on
   * these individual fields being set.
   */
  authUsername?: string;
  authDisplayName?: string;
  authAvatarUrl?: string;
  /**
   * The joining user's effective voice permission bitfield, resolved
   * server-side by the control-plane and returned in the channel join-authorize
   * response (CV-CAN-007). A `bigint` because the full permission field uses
   * bits beyond JS Number precision. Present (fail-closed to 0n) for CHANNEL
   * joins; `undefined` for DM joins, which have no server permission model.
   * Consumed at the participant's own produce boundary in RoomManager to reject
   * publishing without Speak / Video / ScreenShare — never from
   * socket.handshake.auth.
   */
  permissions?: bigint;
  error?: string;
}

/**
 * Parse server-authoritative display identity from a join-authorize response
 * (CV-CAN-017). Non-string / empty / absent values become `undefined` so an
 * empty display_name/avatar_url from the server reads as "genuinely none".
 * `authIdentityPresent` reports whether the response carried a string
 * `username` field at all (even empty) — the identity-awareness signal — so an
 * identity-aware control-plane that returns an empty username still fails
 * closed to the authoritative identity rather than the spoofable handshake.
 */
function parseAuthoritativeIdentity(resp: {
  username?: unknown;
  display_name?: unknown;
  avatar_url?: unknown;
}): {
  authIdentityPresent: boolean;
  authUsername?: string;
  authDisplayName?: string;
  authAvatarUrl?: string;
} {
  const nonEmpty = (v: unknown): string | undefined =>
    typeof v === 'string' && v.length > 0 ? v : undefined;
  return {
    authIdentityPresent: typeof resp.username === 'string',
    authUsername: nonEmpty(resp.username),
    authDisplayName: nonEmpty(resp.display_name),
    authAvatarUrl: nonEmpty(resp.avatar_url),
  };
}

/** A voice participant's display identity as stored + broadcast. */
export interface ParticipantIdentity {
  username: string;
  displayName?: string;
  avatarUrl?: string;
}

/**
 * Resolve the identity to store on the Participant and broadcast to peers
 * (CV-CAN-017). This is the load-bearing security decision: prefer the
 * server-authoritative identity from the join-authorize response, and fall back
 * to the client-supplied handshake identity ONLY when the control-plane did not
 * supply one (a pre-CV-CAN-017 control-plane, detected by `authIdentityPresent`
 * being false). The discriminator is identity PRESENCE — NOT the non-emptiness
 * of `authUsername` — so an identity-aware control-plane that returns an empty
 * username fails closed to the authoritative (empty) identity, and a genuinely
 * empty server display_name/avatar is never re-opened to the spoofable
 * handshake value.
 */
export function resolveParticipantIdentity(
  access: Pick<
    ChannelAccessResult,
    'authIdentityPresent' | 'authUsername' | 'authDisplayName' | 'authAvatarUrl'
  >,
  handshake: ParticipantIdentity
): ParticipantIdentity {
  if (access.authIdentityPresent) {
    return {
      username: access.authUsername ?? '',
      displayName: access.authDisplayName,
      avatarUrl: access.authAvatarUrl,
    };
  }
  return {
    username: handshake.username,
    displayName: handshake.displayName,
    avatarUrl: handshake.avatarUrl,
  };
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
  channel_audio_uplift?: unknown;
}

const CHANNEL_AUDIO_UPLIFT_MIN_PTIME_MS = 10;

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

  const channelAudioUplift = me.channel_audio_uplift === true;

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
  // A fixed channel audio standard (#179) is the narrow exception: the
  // control-plane marks channel_audio_uplift=true when it has bounded the grant
  // by the server tier, so only the audio tier list and ptime floor may widen.
  if (userTier !== 'premium') {
    return {
      userTier,
      allowedAudioTiers: channelAudioUplift
        ? allowedAudioTiers
        : [...FREE_MEDIA_ENTITLEMENT.allowedAudioTiers],
      minPtimeMs: channelAudioUplift
        ? Math.max(minPtimeMs, CHANNEL_AUDIO_UPLIFT_MIN_PTIME_MS)
        : Math.max(minPtimeMs, FREE_MEDIA_ENTITLEMENT.minPtimeMs),
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

interface DmVoiceAuthorizationResponse {
  authorized: boolean;
  is_group: boolean;
  server_muted?: unknown;
  server_deafened?: unknown;
  media_entitlements?: unknown;
  username?: unknown;
  display_name?: unknown;
  avatar_url?: unknown;
  call_id?: unknown;
  call_ring_id?: unknown;
  call_caller_user_id?: unknown;
}

interface ChannelJoinAuthorizationResponse {
  allowed: boolean;
  media_server_url: string;
  permissions?: unknown;
  server_muted: boolean;
  server_deafened: boolean;
  channel: {
    id: string;
    server_id: string;
    name: string;
  };
  media_entitlements?: unknown;
  room_owner_tier?: unknown;
  username?: unknown;
  display_name?: unknown;
  avatar_url?: unknown;
}

const DM_VOICE_CONTROL_PLANE_TIMEOUT_MS = 5_000;

function deniedChannelAccess(channelId: string, error: string): ChannelAccessResult {
  return {
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
  };
}

function channelAccessDenial(status: number): string {
  if (status === 401 || status === 403) return 'Not authorized to access this channel';
  if (status === 404) return 'Channel not found';
  return `Control plane returned ${status}`;
}

function dmVoiceMediaRequest(
  method: 'POST' | 'DELETE',
  channelId: string,
  token: string,
  callId?: string
): RequestInit {
  const timestamp = Math.floor(Date.now() / 1000).toString();
  const tokenDigest = createHash('sha256').update(token).digest('hex');
  const payload = ['v1', timestamp, method, channelId, callId ?? '', tokenDigest].join('\n');
  const proofKey = createHmac('sha256', config.jwtSecret)
    .update('concord/dm-voice-media-authorization/v1')
    .digest();
  return {
    method,
    // Both admission and rollback run under a per-room fence. Bound either
    // network hop so a stalled control plane cannot head-of-line block the room.
    signal: AbortSignal.timeout(DM_VOICE_CONTROL_PLANE_TIMEOUT_MS),
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      'X-Concord-Media-Timestamp': timestamp,
      'X-Concord-Media-Proof': createHmac('sha256', proofKey).update(payload).digest('hex'),
    },
    body: JSON.stringify({ call_id: callId }),
  };
}

function channelAccessRequest(
  channelId: string,
  token: string,
  roomKind: RoomKind,
  requestedCallId?: string
): { endpoint: string; request: RequestInit } {
  const endpoint =
    roomKind === 'dm'
      ? `${config.controlPlaneUrl}/api/v1/dm/conversations/${channelId}/voice/authorize`
      : `${config.controlPlaneUrl}/api/v1/channels/${channelId}/voice/join`;
  if (roomKind === 'dm') {
    return {
      endpoint,
      request: dmVoiceMediaRequest('POST', channelId, token, requestedCallId),
    };
  }
  return {
    endpoint,
    request: {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/json',
      },
    },
  };
}

export async function releaseDMVoiceAuthorization(
  conversationId: string,
  token: string,
  callId: string
): Promise<void> {
  const response = await fetch(
    `${config.controlPlaneUrl}/api/v1/dm/conversations/${conversationId}/voice/authorize`,
    dmVoiceMediaRequest('DELETE', conversationId, token, callId)
  );
  if (!response.ok) {
    throw new Error(`Control plane rejected DM voice authorization release (${response.status})`);
  }
}

function nonEmptyResponseString(value: unknown): string | undefined {
  return typeof value === 'string' && value.length > 0 ? value : undefined;
}

function dmChannelAccessResult(
  channelId: string,
  response: DmVoiceAuthorizationResponse
): ChannelAccessResult {
  const moderationStateValid =
    typeof response.server_muted === 'boolean' && typeof response.server_deafened === 'boolean';
  const allowed = response.authorized === true && moderationStateValid;
  const ent = parseMediaEntitlements(response.media_entitlements);
  const identity = parseAuthoritativeIdentity(response);
  return {
    allowed,
    channelId,
    serverId: '',
    channelName: '',
    serverMuted: response.server_muted === true,
    serverDeafened: response.server_deafened === true,
    userTier: ent.userTier,
    allowedAudioTiers: ent.allowedAudioTiers,
    minPtimeMs: ent.minPtimeMs,
    maxManualBitrateBps: ent.maxManualBitrateBps,
    callId: nonEmptyResponseString(response.call_id),
    callRingId: nonEmptyResponseString(response.call_ring_id),
    callCallerUserId: nonEmptyResponseString(response.call_caller_user_id),
    ...identity,
    // A 200 with authorized=false is rare; preserve a specific denial reason
    // instead of making the join handler fall back to generic "Access denied".
    ...(allowed
      ? {}
      : {
          error: moderationStateValid
            ? 'DM voice join not authorized'
            : 'Invalid DM voice moderation state',
        }),
  };
}

function serverChannelAccessResult(
  response: ChannelJoinAuthorizationResponse
): ChannelAccessResult {
  const ent = parseMediaEntitlements(response.media_entitlements);
  const identity = parseAuthoritativeIdentity(response);
  const permissions = parsePermissionBitfield(response.permissions) ?? 0n;
  return {
    allowed: true,
    channelId: response.channel.id,
    serverId: response.channel.server_id,
    channelName: response.channel.name,
    serverMuted: response.server_muted ?? false,
    serverDeafened: response.server_deafened ?? false,
    userTier: ent.userTier,
    allowedAudioTiers: ent.allowedAudioTiers,
    minPtimeMs: ent.minPtimeMs,
    maxManualBitrateBps: ent.maxManualBitrateBps,
    roomOwnerTier: response.room_owner_tier === 'premium' ? 'premium' : 'free',
    ...identity,
    permissions,
  };
}

export async function validateChannelAccess(
  userId: string,
  channelId: string,
  token: string,
  roomKind: RoomKind = 'channel',
  requestedCallId?: string
): Promise<ChannelAccessResult> {
  try {
    // Route to the appropriate control-plane endpoint based on room kind.
    // The two endpoints have different response shapes (server-channel
    // join returns full channel + enforcement state; DM authorize returns
    // only { authorized, is_group }). Both serve the same purpose:
    // defense-in-depth re-validation of the user's access to the room.
    const { endpoint, request } = channelAccessRequest(channelId, token, roomKind, requestedCallId);
    const channelRes = await fetch(endpoint, request);

    if (!channelRes.ok) {
      return deniedChannelAccess(channelId, channelAccessDenial(channelRes.status));
    }

    // Response shape differs by room kind:
    //   server channel join → { allowed, channel: {id, server_id, name}, server_muted, server_deafened, ... }
    //   DM authorize        → { authorized, is_group }
    // Both indicate access success, but DM responses don't carry server-channel-specific
    // metadata (serverId, channelName, enforcement flags). For DM rooms those fields
    // default to empty/false in the returned ChannelAccessResult.
    if (roomKind === 'dm') {
      return dmChannelAccessResult(
        channelId,
        (await channelRes.json()) as DmVoiceAuthorizationResponse
      );
    }

    return serverChannelAccessResult((await channelRes.json()) as ChannelJoinAuthorizationResponse);
  } catch (err) {
    logger.error('Failed to validate channel access', {
      userId,
      channelId,
      error: err,
    });
    return deniedChannelAccess(channelId, 'Failed to validate channel access');
  }
}
