import type {
  Router,
  WebRtcTransport,
  Producer,
  Consumer,
  RtpCapabilities,
  RtpParameters,
  DtlsParameters,
  AudioLevelObserver,
  MediaKind,
} from 'mediasoup/types';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import { MediasoupService } from './mediasoup.js';
import type { MetricsSample } from './mediaMetrics.js';
import { ActiveSpeakerSet } from './activeSpeakerSet.js';
import type { ActiveSpeakerDelta } from './activeSpeakerSet.js';
import {
  clampCameraLayerDemand,
  computeCameraLayeringGate,
  parseCameraLayerDemand,
  storedDemand,
  type LayeredCodecKind,
  type LayerValue,
  type StoredCameraLayerDemand,
} from './cameraLayerGovernor.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** Media source identifier — matches client-side appData.source */
export type MediaSource = 'mic' | 'camera' | 'screen' | 'screen-audio';

/**
 * Per-user media entitlement (#1300) — the joining user's server-authoritative
 * media caps, resolved by the control-plane and parsed in
 * `validateChannelAccess` (middleware/auth.ts). Carried through `joinRoom` onto
 * the Participant; consumed at the participant's OWN transport / produce
 * boundary. Never sourced from the client (`socket.handshake.auth`).
 */
export interface MediaEntitlement {
  tier: string;
  allowedAudioTiers: string[];
  minPtimeMs: number;
  maxManualBitrateBps: number;
}

export interface Participant {
  userId: string;
  socketId: string;
  username: string;
  displayName?: string;
  avatarUrl?: string;
  sendTransport: WebRtcTransport | null;
  /** Multiple recv transports (keyed by transport ID) — supports PiP windows */
  recvTransports: Map<string, WebRtcTransport>;
  producers: Map<string, { producer: Producer; source: MediaSource; kind: MediaKind }>;
  consumers: Map<string, Consumer>;
  rtpCapabilities: RtpCapabilities | null;
  joinedAt: Date;
  serverMuted: boolean;
  serverDeafened: boolean;
  /**
   * Self-deafen (#685) — the user chose to silence incoming audio. Client-driven
   * via the `set-deafen` socket event and broadcast to the room as
   * `participant-deafen-changed`. Distinct from `serverDeafened`, which is
   * moderator-enforced over NATS (`voice.enforce.deafen`).
   */
  isDeafened: boolean;
  /** Audio-device test indicator (#1163). UI signal only; identity comes from socket auth. */
  isTesting: boolean;
  // ── Per-user media entitlement (#1300) ─────────────────────────────────
  /** Subscription tier label (debug/log/forward-compat only — caps below drive enforcement). */
  tier: string;
  /** Aggregate send-bitrate ceiling (bps) applied to this peer's PRODUCING transport. */
  maxManualBitrateBps: number;
  /** Audio quality tiers this user may produce at (highest entry sets the opus bitrate ceiling). */
  allowedAudioTiers: string[];
  /** Minimum opus ptime (ms) this user may produce at (free 20, premium 10). */
  minPtimeMs: number;
  /** Media E2EE frame crypto format this participant joined with. */
  mediaFrameCryptoVersion: number;
}

// ---------------------------------------------------------------------------
// Audio-tier → opus bitrate ceiling (#1300)
//
// The free tier tops out at the "standard" audio quality tier. The opus
// maxaveragebitrate ceiling for a tier is the per-tier `maxBitrate` (bps) the
// CLIENT requests at produce time. Mirrored here as a server constant from the
// canonical client map AUDIO_QUALITY_TIERS in
// `client/desktop/src/renderer/stores/voiceStore.ts` — `standard.maxBitrate`
// is 96_000 bps (the free ceiling). This is the audio-tier analogue of
// FREE_MEDIA_ENTITLEMENT: a small, deliberately-pinned mirror of a client
// value used only to enforce the floor. If a future tier's client maxBitrate
// changes, update AUDIO_TIER_OPUS_BITRATE_CEILING_BPS to match.
//
// Only ceilings for tiers that can appear in a user's allowedAudioTiers are
// needed; the resolver below takes the MAX ceiling across the user's allowed
// tiers, so unknown tier strings are ignored (fail-closed: an unrecognised
// tier contributes no ceiling).
// ---------------------------------------------------------------------------
export const AUDIO_TIER_OPUS_BITRATE_CEILING_BPS: Readonly<Record<string, number>> = {
  minimum: 16_000,
  low: 32_000,
  moderate: 64_000,
  standard: 96_000,
  high: 192_000,
  hifi: 256_000,
  studio: 510_000,
};

/**
 * Fail-closed free-floor entitlement used as the `joinRoom` default when no
 * parsed entitlement is supplied (backward-compat callers / tests). Mirrors the
 * Go FREE floor — the same values `validateChannelAccess` falls back to via
 * `FREE_MEDIA_ENTITLEMENT`. The real join path ALWAYS passes the parsed
 * entitlement, so this default only fires for callers that predate #1300.
 */
export const FREE_MEDIA_ENTITLEMENT: Readonly<MediaEntitlement> = {
  tier: 'free',
  allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard'],
  minPtimeMs: 20,
  maxManualBitrateBps: 5_000_000,
};

export interface Room {
  id: string;
  router: Router;
  audioLevelObserver: AudioLevelObserver | null;
  participants: Map<string, Participant>;
  createdAt: Date;
  e2eeEpoch: number;
  /** Room-wide media E2EE frame crypto format. Null until first participant joins. */
  mediaFrameCryptoVersion: number | null;
  /**
   * Per-source count of producers that have passed the cap check but are not
   * yet recorded in `participant.producers` (in-flight across the `produce()`
   * await). Load-bearing for TOCTOU-safe cap enforcement — see `produce()`.
   */
  pendingProducerCounts: Map<MediaSource, number>;
  /** Audio last-N (#1544): the N-slot active-speaker decision unit. */
  activeSpeakers: ActiveSpeakerSet;
  /** Consumer IDs the SFU has paused for audio last-N — the resumeConsumer guard set. */
  lastNPausedConsumers: Set<string>;
  /** Mic-producer IDs (last-N managed set; screen-audio excluded). */
  micProducerIds: Set<string>;
  /** Last accepted keyframe request timestamp by sender user ID. */
  keyframeRequestCooldowns: Map<string, number>;
  /** Raw client camera layer demand, parsed and stored by consumer ID. */
  cameraLayerDemands: Map<string, StoredCameraLayerDemand>;
  /** Current room-level camera layering gate state. */
  cameraLayeringGateEnabled: boolean;
}

export interface ProducerInfo {
  producerId: string;
  userId: string;
  kind: MediaKind;
  source: MediaSource;
}

/** Absolute per-room camera-producer ceiling — hard upper bound, not tier-tunable. */
export const ABSOLUTE_VIDEO_PUBLISHER_CEILING = 25;

/** Per-room concurrent screen-share producer cap (unchanged from the original hardcoded 5). */
export const SCREEN_PRODUCER_CAP = 5;

/** Per-sender keyframe request cooldown; mirrors the client-side E2EE recovery cooldown. */
export const KEYFRAME_REQUEST_COOLDOWN_MS = 5000;

/**
 * AES-256-GCM media-frame format advertised by clients at join time.
 * v1 legacy AES-128; v2 keyed by ratchet keyId; v3 (#1878) stamped the channel-key
 * version; v4 (#1895) is per-codec — AV1 per-OBU payload encryption, VP9/VP8/Opus
 * whole-frame. v4 is the current target; the room ratchets UP to the MAX advertised.
 */
export const SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION = 4;

/**
 * Versions accepted at the admission gate during the v3→v4 rollout window
 * (#1895). v3 is tolerated so already-running v3 clients keep joining while the
 * fleet upgrades; DROP 3 post-rollout so the gate hard-requires v4. v1/v2 are
 * permanently rejected.
 */
const ACCEPTED_MEDIA_FRAME_CRYPTO_VERSIONS = new Set<number>([3, 4]);

/**
 * Thrown when a participant advertises a media-frame crypto version strictly
 * LOWER than the room's already-negotiated version (#1878, OQ-1). The room
 * version is the MAX of advertised versions (higher-version-wins): a higher
 * joiner ratchets the room up; an equal joiner is allowed; a lower joiner is
 * rejected with this typed error so the Socket.IO handler can return an
 * actionable `crypto_version_mismatch` ack and the client can prompt "update
 * required" instead of surfacing a generic join failure.
 */
export class CryptoVersionMismatchError extends Error {
  readonly code = 'crypto_version_mismatch' as const;
  constructor(
    public readonly roomVersion: number,
    public readonly joinVersion: number
  ) {
    super(`Media frame crypto version mismatch: room=${roomVersion}, join=${joinVersion}`);
    this.name = 'CryptoVersionMismatchError';
  }
}

function formatMediaFrameCryptoVersion(value: unknown): string {
  if (value === undefined) return 'missing';
  if (value === null) return 'null';
  if (
    typeof value === 'string' ||
    typeof value === 'number' ||
    typeof value === 'boolean' ||
    typeof value === 'bigint'
  ) {
    return `${value}`;
  }
  if (Array.isArray(value)) return 'array';
  return typeof value;
}

export function parseMediaFrameCryptoVersion(value: unknown): number {
  if (typeof value !== 'number' || !ACCEPTED_MEDIA_FRAME_CRYPTO_VERSIONS.has(value)) {
    const received = formatMediaFrameCryptoVersion(value);
    const accepted = [...ACCEPTED_MEDIA_FRAME_CRYPTO_VERSIONS].join(', ');
    throw new Error(
      `Unsupported media frame crypto version ${received}; expected one of ${accepted}`
    );
  }
  return value;
}

/**
 * Resolve the per-room concurrent camera-producer cap.
 *
 * Tier-seam (#1294): when the entitlement resolver (#1521) + /voice/join tier
 * plumbing (#1300) land, read the room/owner subscription tier here and return
 * the paid value for paid rooms. Until then every room resolves to the free
 * default, which is correct for the Beta window (no paid users exist pre-Beta).
 *
 * Clamped at BOTH ends: the upper bound is ABSOLUTE_VIDEO_PUBLISHER_CEILING, so
 * the free cap can only ever be a stricter floor and never raise the global
 * ceiling; the lower bound is 1, so a future #1294 tier value of 0/negative —
 * which does NOT flow through config's fail-safe parsePositiveIntEnv — can never
 * disable the cap (Math.min(0, 25) = 0 would reject every camera produce, a DoS).
 */
export function resolveVideoPublisherCap(
  _room: Room,
  freeCap: number = config.freeVideoPublisherCap
): number {
  return Math.max(1, Math.min(freeCap, ABSOLUTE_VIDEO_PUBLISHER_CEILING));
}

/** Absolute per-room audio last-N ceiling — hard upper bound, not tier-tunable. */
export const ABSOLUTE_AUDIO_LAST_N_CEILING = 16;

/**
 * Resolve the per-room audio last-N forwarded-speaker cap.
 *
 * TODO(#1294): when the entitlement resolver (#1521) + /voice/join tier plumbing
 * (#1300) land, read room/owner tier here and return the paid value (16). Until
 * then every room resolves to the free default — correct for Beta (no paid users
 * pre-Beta), mirroring resolveVideoPublisherCap.
 *
 * `room` is optional because the cap is resolved at room-creation time, BEFORE
 * the Room object exists (the observer must be sized first). It is unused today
 * (tier deferred); when tier lands, pass the room/owner tier in instead.
 *
 * Clamped at BOTH ends: ceiling stops a tier value raising the global bound; the
 * floor of 1 stops a future #1294 tier value of 0/negative — which bypasses
 * config's fail-safe parsePositiveIntEnv — from disabling the cap.
 */
export function resolveAudioLastN(_room?: Room, freeN: number = config.freeAudioLastN): number {
  return Math.max(1, Math.min(freeN, ABSOLUTE_AUDIO_LAST_N_CEILING));
}

export interface TransportOptions {
  id: string;
  iceParameters: unknown;
  iceCandidates: unknown;
  dtlsParameters: unknown;
}

// ---------------------------------------------------------------------------
// Events emitted by RoomManager for NATS/Redis integration (A5-A7)
// ---------------------------------------------------------------------------

export type RoomEvent =
  | {
      type: 'user-joined';
      roomId: string;
      userId: string;
      username: string;
      displayName?: string;
      e2eeEpoch: number;
    }
  | { type: 'user-left'; roomId: string; userId: string; socketId: string; e2eeEpoch: number }
  | { type: 'room-empty'; roomId: string }
  | {
      type: 'producer-added';
      roomId: string;
      userId: string;
      producerId: string;
      kind: MediaKind;
      source: MediaSource;
    }
  | {
      type: 'producer-removed';
      roomId: string;
      userId: string;
      producerId: string;
      kind: MediaKind;
      source: MediaSource;
    }
  | { type: 'camera-layering-gate'; roomId: string; enabled: boolean }
  | { type: 'active-speaker'; roomId: string; userId: string; volume: number };

export type RoomEventHandler = (event: RoomEvent) => void;
type CameraLayerSelection = { spatialLayer: 0 | 1 | 2; temporalLayer: 0 | 1 | 2 };

// ---------------------------------------------------------------------------
// Pure helpers (extracted to reduce cognitive complexity of computeCodecFloor)
// ---------------------------------------------------------------------------

/** Extract lowercase video mimeTypes from RTP capabilities. */
function extractVideoMimeTypes(caps: RtpCapabilities): Set<string> {
  const mimes = new Set<string>();
  for (const c of caps.codecs ?? []) {
    if (c.kind === 'video') mimes.add(c.mimeType.toLowerCase());
  }
  return mimes;
}

/** Compute the intersection of an array of string Sets. */
function intersectMimeSets(sets: Set<string>[]): Set<string> {
  if (sets.length === 0) return new Set();
  const result = new Set(sets[0]);
  for (let i = 1; i < sets.length; i++) {
    for (const m of result) {
      if (!sets[i].has(m)) result.delete(m);
    }
  }
  return result;
}

// ---------------------------------------------------------------------------
// RoomManager
//
// Manages the lifecycle of voice/video rooms (one per voice channel):
//   - Router creation (1 per room, assigned round-robin to mediasoup workers)
//   - Participant join/leave with proper transport cleanup
//   - Per-participant send/recv transports (fixes the consumer transport bug)
//   - Producer/consumer management with source tracking (mic/camera/screen)
//   - AudioLevelObserver for active speaker detection
//   - Event emission for NATS/Redis integration
// ---------------------------------------------------------------------------

export class RoomManager {
  private readonly rooms: Map<string, Room> = new Map();
  private readonly mediasoup: MediasoupService;
  private readonly eventHandlers: RoomEventHandler[] = [];

  constructor(mediasoup: MediasoupService) {
    this.mediasoup = mediasoup;
  }

  /** Register an event handler for room events (used by NATS/Redis integration) */
  onEvent(handler: RoomEventHandler): void {
    this.eventHandlers.push(handler);
  }

  private emitEvent(event: RoomEvent): void {
    for (const handler of this.eventHandlers) {
      try {
        handler(event);
      } catch (err) {
        logger.error('Room event handler error', {
          event: event.type,
          error: err,
        });
      }
    }
  }

  // ─── Room lifecycle ──────────────────────────────────────────────────

  /** Get or create a room for the given channel ID */
  private async getOrCreateRoom(roomId: string): Promise<Room> {
    let room = this.rooms.get(roomId);
    if (room) {
      // Defensive: if the router was closed (e.g. race between leave and rejoin),
      // discard the stale room and recreate it below.
      if (room.router.closed) {
        logger.warn('Room has closed router, discarding stale room', {
          roomId,
        });
        this.rooms.delete(roomId);
        room = undefined;
      } else {
        return room;
      }
    }

    const router = await this.mediasoup.getOrCreateRouter(roomId);

    // Audio last-N (#1544): resolve the forwarded-speaker cap (free 8 for Beta;
    // tier deferred). The observer is sized to N so `volumes` surfaces the top-N.
    const lastN = resolveAudioLastN();

    // Create AudioLevelObserver for active speaker detection
    let audioLevelObserver: AudioLevelObserver | null = null;
    try {
      audioLevelObserver = await router.createAudioLevelObserver({
        maxEntries: lastN,
        threshold: config.audioLevelObserver.threshold,
        interval: config.audioLevelObserver.interval,
      });
    } catch (err) {
      logger.warn('Failed to create AudioLevelObserver', {
        roomId,
        error: err,
      });
    }

    room = {
      id: roomId,
      router,
      audioLevelObserver,
      participants: new Map(),
      createdAt: new Date(),
      e2eeEpoch: 0,
      mediaFrameCryptoVersion: null,
      pendingProducerCounts: new Map(),
      activeSpeakers: new ActiveSpeakerSet(lastN, config.audioLastNHoldMs),
      lastNPausedConsumers: new Set(),
      micProducerIds: new Set(),
      keyframeRequestCooldowns: new Map(),
      cameraLayerDemands: new Map(),
      cameraLayeringGateEnabled: false,
    };

    // Wire up active speaker events
    if (audioLevelObserver) {
      audioLevelObserver.on('volumes', (volumes: Array<{ producer: Producer; volume: number }>) => {
        // Preserve the single-speaker UI broadcast (loudest = volumes[0]).
        if (volumes.length > 0) {
          const { producer, volume } = volumes[0];
          // Find the participant who owns this producer
          for (const [, participant] of room.participants) {
            for (const [, entry] of participant.producers) {
              if (entry.producer.id === producer.id) {
                this.emitEvent({
                  type: 'active-speaker',
                  roomId,
                  userId: participant.userId,
                  volume,
                });
                break;
              }
            }
          }
        }
        // Drive last-N from the full ranked top-N.
        const ranked = volumes.map((v) => v.producer.id);
        const delta = room.activeSpeakers.update(ranked, Date.now());
        this.applyLastNDelta(room, delta);
      });

      // Clear active speaker when everyone stops talking
      audioLevelObserver.on('silence', () => {
        this.emitEvent({
          type: 'active-speaker',
          roomId,
          userId: '',
          volume: -Infinity,
        });
        const delta = room.activeSpeakers.update([], Date.now());
        this.applyLastNDelta(room, delta);
      });
    }

    this.rooms.set(roomId, room);

    logger.info('Room created', { roomId });
    return room;
  }

  /** Close and remove a room (called when last participant leaves) */
  private async closeRoom(roomId: string): Promise<void> {
    const room = this.rooms.get(roomId);
    if (!room) return;

    // Close AudioLevelObserver
    if (room.audioLevelObserver && !room.audioLevelObserver.closed) {
      room.audioLevelObserver.close();
    }

    // Close the router (closes all transports, producers, consumers)
    if (!room.router.closed) {
      room.router.close();
    }

    this.rooms.delete(roomId);

    // Remove the stale router from the mediasoup cache so rejoin creates a fresh one
    this.mediasoup.removeRouter(roomId);

    logger.info('Room closed', { roomId });
    this.emitEvent({ type: 'room-empty', roomId });
  }

  // ─── Participant management ──────────────────────────────────────────

  /** Add a participant to a room. Returns the router's RTP capabilities and existing producers. */
  async joinRoom(
    roomId: string,
    userId: string,
    socketId: string,
    identity: { username: string; displayName?: string; avatarUrl?: string },
    rtpCapabilities: RtpCapabilities | undefined,
    entitlement: MediaEntitlement | undefined,
    mediaFrameCryptoVersion: unknown
  ): Promise<{
    rtpCapabilities: RtpCapabilities;
    mediaFrameCryptoVersion: number;
    existingProducers: ProducerInfo[];
    participants: Array<{
      userId: string;
      username: string;
      displayName?: string;
      avatarUrl?: string;
      isDeafened: boolean;
    }>;
    e2eeEpoch: number;
  }> {
    const parsedMediaFrameCryptoVersion = parseMediaFrameCryptoVersion(mediaFrameCryptoVersion);
    const { username, displayName, avatarUrl } = identity;
    let room = await this.getOrCreateRoom(roomId);

    // Check if user is already in this room (reconnect scenario)
    const existing = room.participants.get(userId);
    if (existing) {
      logger.warn('User already in room, cleaning up old session', {
        roomId,
        userId,
        oldSocketId: existing.socketId,
        newSocketId: socketId,
      });
      await this.leaveRoom(roomId, userId);
      // Room may have been closed if user was the only participant — re-create
      room = await this.getOrCreateRoom(roomId);
      // NOTE: the crypto-version gate below runs AFTER this teardown. A reconnect
      // that DOWNGRADES the user's crypto version (lower than the room's) is
      // rejected at the gate — so the user's stale session is fully cleaned up
      // here (no leak) and they are then ejected+rejected. That is intentional:
      // a downgraded client must update to rejoin (the gate is the authority).
    }

    // Media-frame crypto-version admission gate (#1878, OQ-1: higher-version-wins).
    // The room version is the MAX of advertised versions:
    //   - empty room  → seed with the joiner's version.
    //   - joiner > room → ratchet the room UP to the joiner's version.
    //   - joiner < room → reject with a typed CryptoVersionMismatchError so the
    //     Socket.IO handler can return an actionable `crypto_version_mismatch`
    //     ack (client → "update required") rather than a generic join failure.
    //   - joiner == room → allow.
    // This runs BEFORE participant storage and the e2eeEpoch++ below — do NOT
    // move it past either (see [internal]rules/media-plane.md admission-gate rule).
    if (room.mediaFrameCryptoVersion === null) {
      room.mediaFrameCryptoVersion = parsedMediaFrameCryptoVersion;
    } else if (parsedMediaFrameCryptoVersion > room.mediaFrameCryptoVersion) {
      room.mediaFrameCryptoVersion = parsedMediaFrameCryptoVersion;
    } else if (parsedMediaFrameCryptoVersion < room.mediaFrameCryptoVersion) {
      throw new CryptoVersionMismatchError(
        room.mediaFrameCryptoVersion,
        parsedMediaFrameCryptoVersion
      );
    }
    // equal → allow (fall through)

    // Per-user media caps (#1300): from the parsed control-plane entitlement,
    // or the fail-closed free floor for pre-#1300 callers. Copy the tiers array
    // so a later mutation of the source can't leak into the participant.
    const ent = entitlement ?? FREE_MEDIA_ENTITLEMENT;

    const participant: Participant = {
      userId,
      socketId,
      username,
      displayName,
      avatarUrl,
      sendTransport: null,
      recvTransports: new Map(),
      producers: new Map(),
      consumers: new Map(),
      rtpCapabilities: rtpCapabilities ?? null,
      joinedAt: new Date(),
      serverMuted: false,
      serverDeafened: false,
      isDeafened: false,
      isTesting: false,
      tier: ent.tier,
      maxManualBitrateBps: ent.maxManualBitrateBps,
      allowedAudioTiers: [...ent.allowedAudioTiers],
      minPtimeMs: ent.minPtimeMs,
      mediaFrameCryptoVersion: parsedMediaFrameCryptoVersion,
    };

    room.participants.set(userId, participant);

    // E2EE media-epoch (keyId) sync nudge — incremented on every join so
    // receivers ratchet their per-frame keyId in lockstep. NOT a forward-secrecy
    // boundary: the access boundary is the channel-key version (key_version) the
    // frame now carries (#1878, Decision B). Inter-version forward secrecy lives
    // in the CSK rotation/re-wrap on membership change, not here.
    room.e2eeEpoch++;

    logger.info('Participant joined room', {
      roomId,
      userId,
      username,
      participantCount: room.participants.size,
    });

    this.emitEvent({
      type: 'user-joined',
      roomId,
      userId,
      username,
      displayName,
      e2eeEpoch: room.e2eeEpoch,
    });

    // Collect existing producers for the new joiner to consume
    const existingProducers: ProducerInfo[] = [];
    for (const [, p] of room.participants) {
      if (p.userId === userId) continue;
      for (const [, entry] of p.producers) {
        existingProducers.push({
          producerId: entry.producer.id,
          userId: p.userId,
          kind: entry.kind,
          source: entry.source,
        });
      }
    }

    // Collect participant list
    const participants = Array.from(room.participants.values()).map((p) => ({
      userId: p.userId,
      username: p.username,
      displayName: p.displayName,
      avatarUrl: p.avatarUrl,
      isDeafened: p.isDeafened,
      isTesting: p.isTesting,
    }));

    return {
      rtpCapabilities: room.router.rtpCapabilities,
      mediaFrameCryptoVersion: room.mediaFrameCryptoVersion,
      existingProducers,
      participants,
      e2eeEpoch: room.e2eeEpoch,
    };
  }

  /** Remove a participant from a room. Cleans up all their transports, producers, consumers. */
  async leaveRoom(roomId: string, userId: string): Promise<void> {
    const room = this.rooms.get(roomId);
    if (!room) return;

    const participant = room.participants.get(userId);
    if (!participant) return;

    this.clearParticipantCameraLayerDemands(room, participant);
    this.closeParticipantConsumers(participant);
    const removedCameraProducer = this.closeParticipantProducers(room, participant, roomId, userId);
    room.keyframeRequestCooldowns.delete(userId);

    this.closeParticipantTransports(participant);

    room.participants.delete(userId);
    if (room.participants.size > 0 && (removedCameraProducer || room.cameraLayerDemands.size > 0)) {
      this.recomputeCameraLayeringGate(room);
    }

    // E2EE media-epoch (keyId) sync nudge — incremented on leave for the same
    // lockstep-ratchet reason as the join site above; NOT a forward-secrecy
    // boundary (#1878, Decision B). Forward secrecy on membership change is the
    // CSK rotation/re-wrap, not this counter.
    room.e2eeEpoch++;

    logger.info('Participant left room', {
      roomId,
      userId,
      remainingParticipants: room.participants.size,
    });

    this.emitEvent({
      type: 'user-left',
      roomId,
      userId,
      socketId: participant.socketId,
      e2eeEpoch: room.e2eeEpoch,
    });

    // Tear down room if empty
    if (room.participants.size === 0) {
      await this.closeRoom(roomId);
    }
  }

  private closeParticipantConsumers(participant: Participant): void {
    for (const [, consumer] of participant.consumers) {
      if (!consumer.closed) consumer.close();
    }
    participant.consumers.clear();
  }

  private closeParticipantProducers(
    room: Room,
    participant: Participant,
    roomId: string,
    userId: string
  ): boolean {
    let removedCameraProducer = false;
    for (const [producerId, entry] of participant.producers) {
      if (!entry.producer.closed) entry.producer.close();
      if (entry.source === 'camera') removedCameraProducer = true;
      this.removeMicProducer(room, producerId);
      this.emitEvent({
        type: 'producer-removed',
        roomId,
        userId,
        producerId,
        kind: entry.kind,
        source: entry.source,
      });
    }
    participant.producers.clear();
    return removedCameraProducer;
  }

  private closeParticipantTransports(participant: Participant): void {
    if (participant.sendTransport && !participant.sendTransport.closed) {
      participant.sendTransport.close();
    }
    for (const [, transport] of participant.recvTransports) {
      if (!transport.closed) transport.close();
    }
    participant.recvTransports.clear();
  }

  // ─── Transport management ────────────────────────────────────────────

  /** Create a WebRTC transport for a participant */
  async createTransport(
    roomId: string,
    userId: string,
    direction: 'send' | 'recv'
  ): Promise<TransportOptions> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');

    const participant = room.participants.get(userId);
    if (!participant) throw new Error('Participant not found in room');

    const transport = await room.router.createWebRtcTransport({
      listenIps: config.mediasoup.webRtcTransport.listenIps,
      enableUdp: config.mediasoup.webRtcTransport.enableUdp,
      enableTcp: config.mediasoup.webRtcTransport.enableTcp,
      preferUdp: config.mediasoup.webRtcTransport.preferUdp,
      initialAvailableOutgoingBitrate:
        config.mediasoup.webRtcTransport.initialAvailableOutgoingBitrate,
    });

    // Set max incoming bitrate.
    //
    // Per-user tier gating (#1300): `setMaxIncomingBitrate` caps the aggregate
    // bitrate the SFU will ACCEPT from this transport — i.e. it caps what the
    // peer can SEND. The producing direction is the `send` transport (the peer
    // calls produce() on it — see produce() below), so for `send` we apply the
    // joining user's tier ceiling `participant.maxManualBitrateBps` (free
    // 5_000_000, premium 10_000_000), replacing the global ~50 Mbps default.
    // The `recv` transport carries media TO the peer (the SFU never produces on
    // it), so its incoming cap is irrelevant to send-side abuse — it keeps the
    // existing global default. This is the workhorse server-authoritative lever
    // (spec §1): it bounds aggregate cost/abuse. It is NOT a pixel/fps limit —
    // a patched client can still encode low-quality 4K inside the envelope.
    //
    // Honest scope: this is a BWE/congestion-control ADVISORY, not a hard RTP
    // policer. The SFU advertises the cap via REMB / transport-cc and a
    // COOPERATIVE client (stock browser / Electron WebRTC) honours it and
    // throttles its encoder. A patched client that ignores congestion-control
    // feedback can still send above the cap and the SFU will forward what
    // arrives — so this bounds real-world / cooperative cost, it does not
    // hard-stop a deliberately misbehaving peer. Hard policing against a
    // non-cooperative client is FUTURE work (server-side getStats() sampling +
    // the #1542 voice.enforce.disconnect path), explicitly out of #1300 scope.
    const incomingBitrateCap =
      direction === 'send'
        ? participant.maxManualBitrateBps
        : config.mediasoup.webRtcTransport.maxIncomingBitrate;
    if (incomingBitrateCap) {
      try {
        await transport.setMaxIncomingBitrate(incomingBitrateCap);
      } catch (err) {
        // WebRtcTransport ALWAYS supports setMaxIncomingBitrate (unlike Plain/
        // Pipe transports, which we never create here), so a failure is not a
        // capability gap — it is a transient worker error. We do NOT fail the
        // join closed: a legitimate peer should not be blocked by a flaky
        // worker call. But the fail-open IS observable — the send transport is
        // left at the mediasoup default (effectively uncapped), so the per-user
        // bitrate cap silently does not apply for this session. Log it PII-safe
        // (ids + direction only — NEVER identity/display fields or the err
        // object, per observability.md #1/#2) so the uncapped state can be
        // monitored, then proceed.
        logger.warn(
          'setMaxIncomingBitrate failed — send transport left uncapped (#1300 fail-open)',
          {
            roomId,
            userId,
            direction,
            transportId: transport.id,
            error: err instanceof Error ? err.message : 'unknown',
          }
        );
      }
    }

    // Store on participant
    if (direction === 'send') {
      if (participant.sendTransport && !participant.sendTransport.closed) {
        participant.sendTransport.close();
      }
      participant.sendTransport = transport;
    } else {
      // Multiple recv transports are allowed (main window + PiP windows)
      participant.recvTransports.set(transport.id, transport);
      // Clean up map entry when router closes the transport
      transport.on('routerclose', () => {
        participant.recvTransports.delete(transport.id);
      });
    }

    // Log ICE/DTLS state transitions for diagnostics
    const iceLogLevel: Record<string, string> = {
      completed: 'info',
      disconnected: 'warn',
      closed: 'warn',
    };
    const dtlsLogLevel: Record<string, string> = {
      connected: 'info',
      failed: 'error',
      closed: 'warn',
    };
    transport.on('icestatechange', (iceState) => {
      logger.log(iceLogLevel[iceState] || 'debug', 'Transport ICE state change', {
        transportId: transport.id,
        roomId,
        userId,
        direction,
        iceState,
      });
    });
    transport.on('dtlsstatechange', (dtlsState) => {
      logger.log(dtlsLogLevel[dtlsState] || 'debug', 'Transport DTLS state change', {
        transportId: transport.id,
        roomId,
        userId,
        direction,
        dtlsState,
      });
    });

    // Clean up on transport close
    transport.on('routerclose', () => {
      logger.debug('Transport router closed', {
        transportId: transport.id,
        roomId,
        userId,
      });
    });

    logger.debug('Transport created', {
      transportId: transport.id,
      roomId,
      userId,
      direction,
    });

    return {
      id: transport.id,
      iceParameters: transport.iceParameters,
      iceCandidates: transport.iceCandidates,
      dtlsParameters: transport.dtlsParameters,
    };
  }

  /** Connect a transport's DTLS */
  async connectTransport(
    roomId: string,
    userId: string,
    transportId: string,
    dtlsParameters: DtlsParameters
  ): Promise<void> {
    const transport = this.findParticipantTransport(roomId, userId, transportId);
    if (!transport) throw new Error('Transport not found');

    await transport.connect({ dtlsParameters });

    logger.debug('Transport connected', { transportId, roomId, userId });
  }

  // ─── Producer management ─────────────────────────────────────────────

  /** Create a producer on the participant's send transport */
  async produce(
    roomId: string,
    userId: string,
    transportId: string,
    kind: MediaKind,
    rtpParameters: RtpParameters,
    source: MediaSource
  ): Promise<ProducerInfo> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');

    const participant = room.participants.get(userId);
    if (!participant) throw new Error('Participant not found');

    if (participant.sendTransport?.id !== transportId) {
      throw new Error('Send transport not found or mismatch');
    }

    // Validate screen-audio first: must be audio kind, participant must have an
    // active screen producer, and only one screen-audio producer is allowed per
    // participant. Ordered before the generic kind/source check so a video-kind
    // screen-audio mislabel gets the specific "must be audio kind" message.
    if (source === 'screen-audio') {
      this.validateScreenAudioSource(participant, kind);
    }

    // Validate the client-declared source. It selects which per-room cap applies,
    // so it is security-load-bearing (#1539): a video stream mislabeled as
    // 'mic'/'screen-audio' would otherwise dodge the camera cap entirely. Every
    // video producer must be a capped video source (camera/screen); every audio
    // producer must be an audio source (mic/screen-audio). The legit client only
    // ever emits these kind/source pairings (voiceService.ts). Static message —
    // never interpolate the client-supplied source into the error/log (CWE-117).
    const allowedSources: MediaSource[] =
      kind === 'video' ? ['camera', 'screen'] : ['mic', 'screen-audio'];
    if (!allowedSources.includes(source)) {
      throw new Error('Invalid media source for producer kind');
    }

    // Per-user audio-tier gating (#1300): for the joining user's OWN mic
    // producer, reject (BEFORE creating the producer) an over-tier opus stream.
    // Server-verifiable from rtpParameters at the produce boundary; the tier is
    // server-authoritative (participant entitlement parsed from the join-authorize
    // response, never socket.handshake.auth). Only mic is gated — screen-audio
    // is system audio with no per-tier quality contract. TOCTOU-safe: this is a
    // pure synchronous check before the `await produce()`, so a rejected stream
    // never yields a (briefly-existing) producer.
    if (kind === 'audio' && source === 'mic') {
      this.enforceAudioTierGate(participant, rtpParameters, source);
    }

    // Enforce per-room concurrent-producer caps (camera: tier-resolved free
    // default 8; screen: SCREEN_PRODUCER_CAP). TOCTOU-safe (#1539 review): the
    // check + slot reservation run synchronously with no intervening await, so
    // concurrent invocations observe each other's reservations. The reservation
    // is released once the producer is recorded or if produce() fails.
    const producerCap = this.reserveProducerSlot(room, source);

    let producer: Producer;
    try {
      producer = await participant.sendTransport.produce({
        kind,
        rtpParameters,
        appData: { source, userId },
      });
    } catch (err) {
      this.releaseProducerReservation(room, source, producerCap);
      throw err;
    }

    participant.producers.set(producer.id, { producer, source, kind });
    // Release the reservation: the producer is now counted via participant.producers.
    this.releaseProducerReservation(room, source, producerCap);
    if (source === 'camera') this.recomputeCameraLayeringGate(room);

    // Add mic audio producers to the AudioLevelObserver (skip screen-audio to
    // avoid false active-speaker detection from system audio)
    if (
      kind === 'audio' &&
      source !== 'screen-audio' &&
      room.audioLevelObserver &&
      !room.audioLevelObserver.closed
    ) {
      // Audio last-N (#1544): track this as a last-N-managed mic producer so the
      // observed set matches exactly the producers driving the active-speaker set.
      room.micProducerIds.add(producer.id);
      try {
        await room.audioLevelObserver.addProducer({ producerId: producer.id });
      } catch (err) {
        logger.warn('Failed to add producer to AudioLevelObserver', {
          producerId: producer.id,
          error: err,
        });
      }
    }

    // Clean up when producer closes
    producer.on('transportclose', () => {
      participant.producers.delete(producer.id);
      this.removeMicProducer(room, producer.id);
      if (source === 'camera') this.recomputeCameraLayeringGate(room);
    });

    const info: ProducerInfo = {
      producerId: producer.id,
      userId,
      kind,
      source,
    };

    logger.info('Producer created', {
      producerId: producer.id,
      roomId,
      userId,
      kind,
      source,
    });

    this.emitEvent({
      type: 'producer-added',
      roomId,
      userId,
      producerId: producer.id,
      kind,
      source,
    });

    return info;
  }

  /** Pause a producer (mute without removing) */
  async pauseProducer(roomId: string, userId: string, producerId: string): Promise<void> {
    const entry = this.findProducer(roomId, userId, producerId);
    if (!entry) throw new Error('Producer not found');

    await entry.producer.pause();
    logger.debug('Producer paused', { producerId, roomId, userId });
  }

  /** Resume a paused producer */
  async resumeProducer(roomId: string, userId: string, producerId: string): Promise<void> {
    const entry = this.findProducer(roomId, userId, producerId);
    if (!entry) throw new Error('Producer not found');

    await entry.producer.resume();
    logger.debug('Producer resumed', { producerId, roomId, userId });
  }

  async requestKeyFrame(
    roomId: string,
    requesterUserId: string,
    senderUserId: string
  ): Promise<number> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');
    const requester = room.participants.get(requesterUserId);
    if (!requester) throw new Error('Requester not found');

    const sender = room.participants.get(senderUserId);
    if (!sender) throw new Error('Sender not found');

    const now = Date.now();
    const lastRequest = room.keyframeRequestCooldowns.get(senderUserId) ?? 0;
    if (now - lastRequest < KEYFRAME_REQUEST_COOLDOWN_MS) return 0;

    const videoProducerIds = [...sender.producers.values()]
      .filter(
        (entry) =>
          entry.kind === 'video' &&
          !entry.producer.closed &&
          (entry.source === 'camera' || entry.source === 'screen')
      )
      .map((entry) => entry.producer.id);
    if (videoProducerIds.length === 0) return 0;

    const videoConsumers = [...requester.consumers.values()].filter(
      (entry) =>
        entry.kind === 'video' && !entry.closed && videoProducerIds.includes(entry.producerId)
    );
    if (videoConsumers.length === 0) return 0;

    room.keyframeRequestCooldowns.set(senderUserId, now);
    try {
      await Promise.all(videoConsumers.map((consumer) => consumer.requestKeyFrame()));
    } catch (error) {
      room.keyframeRequestCooldowns.delete(senderUserId);
      throw error;
    }
    logger.debug('Requested producer keyframe', {
      roomId,
      requesterUserId,
      senderUserId,
      count: videoConsumers.length,
    });
    return videoConsumers.length;
  }

  /** Close and remove a producer */
  async closeProducer(
    roomId: string,
    userId: string,
    producerId: string
  ): Promise<MediaSource | null> {
    const room = this.rooms.get(roomId);
    if (!room) return null;

    const participant = room.participants.get(userId);
    if (!participant) return null;

    const entry = participant.producers.get(producerId);
    if (!entry) return null;

    if (!entry.producer.closed) entry.producer.close();
    participant.producers.delete(producerId);
    this.removeMicProducer(room, producerId);
    if (entry.source === 'camera') this.recomputeCameraLayeringGate(room);

    this.emitEvent({
      type: 'producer-removed',
      roomId,
      userId,
      producerId,
      kind: entry.kind,
      source: entry.source,
    });

    logger.info('Producer closed', {
      producerId,
      roomId,
      userId,
      kind: entry.kind,
      source: entry.source,
    });
    return entry.source;
  }

  // ─── Consumer management ─────────────────────────────────────────────

  /**
   * Create a consumer on the requesting user's recv transport for a given producer.
   * This fixes the bug in the original mediasoup.ts where consume() picked an
   * arbitrary transport from the room instead of the specific user's recv transport.
   */
  async consume(
    roomId: string,
    consumerUserId: string,
    producerId: string,
    transportId?: string
  ): Promise<{
    id: string;
    producerId: string;
    kind: MediaKind;
    rtpParameters: RtpParameters;
    producerUserId: string;
    source: MediaSource;
  } | null> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');

    const consumer = room.participants.get(consumerUserId);
    if (!consumer) throw new Error('Consumer participant not found');

    // Find the recv transport: use specified ID, or fall back to first available
    const recvTransport = transportId
      ? consumer.recvTransports.get(transportId)
      : consumer.recvTransports.values().next().value;

    logger.debug('Consume transport lookup', {
      roomId,
      consumerUserId,
      producerId,
      requestedTransportId: transportId,
      recvTransportCount: consumer.recvTransports.size,
      hasRecvTransport: !!recvTransport,
      hasRtpCapabilities: !!consumer.rtpCapabilities,
    });

    if (!recvTransport) {
      throw new Error('No receive transport — create one first');
    }

    if (!consumer.rtpCapabilities) {
      throw new Error('RTP capabilities not set — provide them on join');
    }

    // Find the producer and its owner
    const producerResult = this.findProducerInRoom(room, producerId);
    if (!producerResult) {
      throw new Error('Producer not found in room');
    }
    const { entry: producerEntry, userId: producerUserId } = producerResult;

    // Check if the router can route this producer to this consumer
    if (
      !room.router.canConsume({
        producerId,
        rtpCapabilities: consumer.rtpCapabilities,
      })
    ) {
      logger.warn('Cannot consume — incompatible codecs', {
        producerId,
        consumerUserId,
        roomId,
      });
      return null;
    }

    // Create consumer on the specified (or default) recv transport
    const newConsumer = await recvTransport.consume({
      producerId,
      rtpCapabilities: consumer.rtpCapabilities,
      paused: true, // Start paused — client resumes after setup
      appData: {
        source: producerEntry.source,
        producerUserId,
        producerId,
      },
    });

    consumer.consumers.set(newConsumer.id, newConsumer);

    // Audio last-N (#1544): if this is a MIC audio consumer whose producer is not
    // currently in the top-N, start it last-N-paused (it was created paused:true)
    // so the client's unconditional resume is refused by the guard until the
    // speaker enters the top-N. Screen-audio (not a mic producer) and video are
    // never managed.
    //
    // #1742: only seed paused when the room is GENUINELY over the cap. When every
    // mic publisher fits under N, last-N is a no-op (see applyLastNDelta), so a
    // fresh joiner must hear from the first frame — seeding it paused would clip
    // the opening of the first words. The count is server-authoritative.
    if (
      newConsumer.kind === 'audio' &&
      room.micProducerIds.has(producerId) &&
      room.micProducerIds.size > resolveAudioLastN(room) &&
      !room.activeSpeakers.current().has(producerId)
    ) {
      room.lastNPausedConsumers.add(newConsumer.id);
    }

    // Clean up on close
    newConsumer.on('transportclose', () => {
      consumer.consumers.delete(newConsumer.id);
      room.lastNPausedConsumers.delete(newConsumer.id);
      this.clearCameraLayerDemand(room, newConsumer.id);
    });
    newConsumer.on('producerclose', () => {
      consumer.consumers.delete(newConsumer.id);
      room.lastNPausedConsumers.delete(newConsumer.id);
      this.clearCameraLayerDemand(room, newConsumer.id);
    });

    logger.info('Consumer created with transport state', {
      consumerId: newConsumer.id,
      producerId,
      consumerUserId,
      producerUserId,
      roomId,
      kind: newConsumer.kind,
      recvTransportId: recvTransport.id,
      iceState: recvTransport.iceState,
      dtlsState: recvTransport.dtlsState,
    });

    return {
      id: newConsumer.id,
      producerId,
      kind: newConsumer.kind as MediaKind,
      rtpParameters: newConsumer.rtpParameters,
      producerUserId,
      source: producerEntry.source,
    };
  }

  /** Resume a consumer (called after client has set up its decoder) */
  async resumeConsumer(roomId: string, userId: string, consumerId: string): Promise<void> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');

    const participant = room.participants.get(userId);
    if (!participant) throw new Error('Participant not found');

    const consumer = participant.consumers.get(consumerId);
    if (!consumer) throw new Error('Consumer not found');

    // Audio last-N (#1544) enforcement boundary: a consumer the SFU has paused
    // for last-N must NOT be resumed on a client request — that is the egress-cap
    // bypass. The server's own last-N resume path clears the set first.
    if (room.lastNPausedConsumers.has(consumerId)) {
      logger.debug('Refusing client resume of last-N-paused consumer', {
        consumerId,
        roomId,
        userId,
      });
      return;
    }
    await consumer.resume();
    logger.debug('Consumer resumed', { consumerId, roomId, userId });
  }

  /** Pause a consumer (stops SFU from forwarding RTP, saving bandwidth) */
  async pauseConsumer(roomId: string, userId: string, consumerId: string): Promise<void> {
    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');

    const participant = room.participants.get(userId);
    if (!participant) throw new Error('Participant not found');

    const consumer = participant.consumers.get(consumerId);
    if (!consumer) throw new Error('Consumer not found');

    await consumer.pause();
    logger.debug('Consumer paused', { consumerId, roomId, userId });
  }

  async setPreferredCameraLayers(
    roomId: string,
    userId: string,
    raw: unknown
  ): Promise<{
    effectiveLayers: CameraLayerSelection;
  }> {
    const parsed = parseCameraLayerDemand(raw);
    if (!parsed.ok) throw new Error(parsed.error);

    const room = this.rooms.get(roomId);
    if (!room) throw new Error('Room not found');
    const participant = room.participants.get(userId);
    if (!participant) throw new Error('Participant not found');
    const consumer = participant.consumers.get(parsed.value.consumerId);
    if (!consumer) throw new Error('Consumer not found');
    if (consumer.kind !== 'video') throw new Error('Consumer is not video');

    const appData = consumer.appData as { source?: unknown } | undefined;
    if (appData?.source !== 'camera') throw new Error('Consumer is not camera');

    const maxSpatialLayer = this.maxCameraSpatialLayerForParticipant(participant);
    const effectiveLayers = clampCameraLayerDemand(parsed.value, maxSpatialLayer);
    room.cameraLayerDemands.set(
      parsed.value.consumerId,
      storedDemand(parsed.value, maxSpatialLayer)
    );
    this.recomputeCameraLayeringGate(room);
    const consumerType = (consumer as { type?: unknown }).type;
    if (consumerType === 'simulcast' || consumerType === 'svc') {
      await consumer.setPreferredLayers(effectiveLayers);
    }
    return { effectiveLayers };
  }

  /** Close and remove a consumer (client-initiated, e.g. tune-out of screen share) */
  closeConsumer(roomId: string, userId: string, consumerId: string): boolean {
    const room = this.rooms.get(roomId);
    if (!room) return false;

    const participant = room.participants.get(userId);
    if (!participant) return false;

    const consumer = participant.consumers.get(consumerId);
    if (!consumer) return false;

    if (!consumer.closed) consumer.close();
    participant.consumers.delete(consumerId);
    // Explicit close does NOT fire the consumer's transportclose/producerclose
    // handlers, so clean up the last-N guard set here too (else the entry leaks).
    room.lastNPausedConsumers.delete(consumerId);
    this.clearCameraLayerDemand(room, consumerId);
    logger.debug('Consumer closed', { consumerId, roomId, userId });
    return true;
  }

  // ─── Server enforcement (mute/deafen) ────────────────────────────────

  /** Pause all audio producers for a participant */
  private async pauseAudioProducers(participant: Participant): Promise<void> {
    for (const [, entry] of participant.producers) {
      if (entry.kind === 'audio') await entry.producer.pause();
    }
  }

  /** Pause all audio consumers for a participant */
  private async pauseAudioConsumers(participant: Participant): Promise<void> {
    for (const [, consumer] of participant.consumers) {
      if (consumer.kind === 'audio') await consumer.pause();
    }
  }

  /**
   * Apply an active-speaker delta: pause each subscriber's audio consumers for
   * producers that just left the top-N, resume those that just entered. The
   * `lastNPausedConsumers` Set is mutated SYNCHRONOUSLY (before any await) so the
   * resumeConsumer guard's view is always consistent under overlapping ticks;
   * the mediasoup pause/resume calls reconcile toward it idempotently.
   * The PRODUCER is never paused — only consumers.
   */
  private applyLastNDelta(room: Room, delta: ActiveSpeakerDelta): void {
    // #1742: last-N is a capacity/DoS guardrail for rooms LARGER than the
    // forwarded-speaker cap. When every mic publisher already fits under N,
    // applying the cap is pure regression — the evict-on-silence pause/resume
    // churn loses the leading frames of each utterance during the resume gap,
    // producing Opus PLC crackle. So when the room is at/under the cap, never
    // pause, and drain any consumer left paused from a prior over-cap window
    // (resume-on-shrink). The count is server-authoritative (no new trust
    // surface). The over-cap path below is unchanged: the #1544/#1632
    // enforcement invariants only apply when the room genuinely exceeds N.
    if (room.micProducerIds.size <= resolveAudioLastN(room)) {
      this.drainLastNPaused(room);
      return;
    }
    for (const producerId of delta.removed) this.setAudioForwarding(room, producerId, false);
    for (const producerId of delta.added) this.setAudioForwarding(room, producerId, true);
  }

  /**
   * #1742: resume every consumer currently last-N-paused, because last-N no
   * longer applies (the room is at/under the cap — either always was, or just
   * shrank back). `lastNPausedConsumers` is mutated SYNCHRONOUSLY before the
   * async resume, mirroring setAudioForwarding's TOCTOU discipline so the
   * resumeConsumer guard's view stays consistent under overlapping observer
   * ticks. A server-deafened subscriber is cleared from the set (last-N no
   * longer owns its pause) but is NOT resumed — the deafen pause must hold
   * (moderation is not bypassed); server-undeafen + the client resume restores
   * it, exactly as in setAudioForwarding's resume branch.
   */
  private drainLastNPaused(room: Room): void {
    if (room.lastNPausedConsumers.size === 0) return;
    for (const [, participant] of room.participants) {
      for (const [consumerId, consumer] of participant.consumers) {
        if (!room.lastNPausedConsumers.has(consumerId)) continue;
        room.lastNPausedConsumers.delete(consumerId); // delete BEFORE resume
        if (participant.serverDeafened) continue;
        consumer
          .resume()
          .catch((err) => logger.warn('last-N drain resume failed', { consumerId, error: err }));
      }
    }
  }

  /**
   * Pause (forward=false) or resume (forward=true) every subscriber's audio
   * consumer for `producerId`. `lastNPausedConsumers` (the resumeConsumer guard's
   * source of truth) is mutated SYNCHRONOUSLY before the async pause/resume so the
   * guard's view stays consistent under overlapping observer ticks. The PRODUCER
   * is never touched — only consumers.
   */
  private setAudioForwarding(room: Room, producerId: string, forward: boolean): void {
    for (const [, participant] of room.participants) {
      for (const [consumerId, consumer] of participant.consumers) {
        if (consumer.kind !== 'audio' || consumer.producerId !== producerId) continue;
        if (forward) {
          room.lastNPausedConsumers.delete(consumerId); // no longer last-N-paused (producer is top-N)
          // Server-deafen is a server-ENFORCED moderation control: a deafened
          // subscriber must not receive audio regardless of client behavior.
          // Last-N must NOT resume their consumer (that would bypass the deafen).
          // The deafen pause holds; on server-undeafen the client's resume — now
          // allowed by the guard since this consumer is out of lastNPausedConsumers —
          // restores forwarding.
          if (participant.serverDeafened) continue;
          consumer
            .resume()
            .catch((err) => logger.warn('last-N resume failed', { consumerId, error: err }));
        } else {
          room.lastNPausedConsumers.add(consumerId); // add BEFORE pause
          consumer
            .pause()
            .catch((err) => logger.warn('last-N pause failed', { consumerId, error: err }));
        }
      }
    }
  }

  /** Server-mute a participant: pause all audio producers and set flag */
  async serverMuteUser(roomId: string, userId: string): Promise<void> {
    const participant = this.getParticipant(roomId, userId);
    if (!participant) return;

    participant.serverMuted = true;
    await this.pauseAudioProducers(participant);
    logger.info('Server-muted user', { roomId, userId });
  }

  /** Server-unmute a participant: clear flag but do NOT resume producers */
  async serverUnmuteUser(roomId: string, userId: string): Promise<void> {
    const participant = this.getParticipant(roomId, userId);
    if (!participant) return;

    participant.serverMuted = false;
    logger.info('Server-unmuted user', { roomId, userId });
  }

  /** Server-deafen a participant: mute + deafen, pause audio producers and consumers */
  async serverDeafenUser(roomId: string, userId: string): Promise<void> {
    const participant = this.getParticipant(roomId, userId);
    if (!participant) return;

    participant.serverDeafened = true;
    participant.serverMuted = true;
    await this.pauseAudioProducers(participant);
    await this.pauseAudioConsumers(participant);
    logger.info('Server-deafened user', { roomId, userId });
  }

  /** Server-undeafen a participant: clear deafen and mute flags, do NOT resume anything */
  async serverUndeafenUser(roomId: string, userId: string): Promise<void> {
    const participant = this.getParticipant(roomId, userId);
    if (!participant) return;

    participant.serverDeafened = false;
    participant.serverMuted = false;
    logger.info('Server-undeafened user', { roomId, userId });
  }

  /**
   * Self-deafen (#685): record the participant's self-chosen deafen state so the
   * room snapshot and the `participant-deafen-changed` broadcast carry it. Deafen
   * silences INCOMING audio, which the client handles by pausing its own consumers
   * locally — so unlike `serverDeafenUser` there is no server-side producer/consumer
   * op here; this only tracks the authoritative flag for other participants' UIs.
   */
  setParticipantDeafen(roomId: string, userId: string, isDeafened: boolean): void {
    const participant = this.getParticipant(roomId, userId);
    if (!participant) return;
    participant.isDeafened = isDeafened;
    logger.info('User-level deafen state changed', { roomId, userId, isDeafened });
  }

  setParticipantTestingStatus(roomId: string, userId: string, isTesting: boolean): void {
    const participant = this.getParticipant(roomId, userId);
    if (!participant) return;
    participant.isTesting = isTesting;
    logger.info('Audio test state changed', { roomId, userId, isTesting });
  }

  /** User-level mute: pause all audio producers without setting server flags */
  async userMuteParticipant(roomId: string, userId: string): Promise<void> {
    const participant = this.getParticipant(roomId, userId);
    if (!participant) return;

    await this.pauseAudioProducers(participant);
    logger.info('User-level mute applied', { roomId, userId });
  }

  /** User-level deafen: pause all audio producers and consumers without setting server flags */
  async userDeafenParticipant(roomId: string, userId: string): Promise<void> {
    const participant = this.getParticipant(roomId, userId);
    if (!participant) return;

    await this.pauseAudioProducers(participant);
    await this.pauseAudioConsumers(participant);
    logger.info('User-level deafen applied', { roomId, userId });
  }

  // ─── Query methods ───────────────────────────────────────────────────

  /** Get a room by ID */
  getRoom(roomId: string): Room | undefined {
    return this.rooms.get(roomId);
  }

  /** Get all active room IDs */
  getActiveRoomIds(): string[] {
    return Array.from(this.rooms.keys());
  }

  /** Get a participant by userId in a specific room */
  /** Update a participant's RTP capabilities (after device.load on client) */
  updateRtpCapabilities(roomId: string, userId: string, rtpCapabilities: RtpCapabilities): void {
    const room = this.rooms.get(roomId);
    if (!room) return;
    const participant = room.participants.get(userId);
    if (participant) {
      participant.rtpCapabilities = rtpCapabilities;
      if (room.cameraLayerDemands.size > 0) this.recomputeCameraLayeringGate(room);
    }
  }

  /**
   * Compute the "codec floor" — the intersection of all participants' video
   * decode capabilities. Returns lowercase mimeTypes that every participant
   * with non-null rtpCapabilities can decode.
   * Returns null if <2 participants have capabilities (no constraint needed).
   *
   * Floor operates at mimeType level (e.g., "video/h264") — not profile level.
   * This is intentional: canConsume() handles profile matching, and all modern
   * browsers decode all profiles within a codec family. Profile-level floor
   * can be added if a real-world decode failure is observed.
   */
  computeCodecFloor(roomId: string): string[] | null {
    const room = this.rooms.get(roomId);
    if (!room) return null;

    const capableParts: RtpCapabilities[] = [];
    for (const [, p] of room.participants) {
      if (p.rtpCapabilities) capableParts.push(p.rtpCapabilities);
    }

    if (capableParts.length < 2) return null;

    const mimeSets = capableParts.map(extractVideoMimeTypes);
    const floor = intersectMimeSets(mimeSets);
    return Array.from(floor);
  }

  getParticipant(roomId: string, userId: string): Participant | undefined {
    return this.rooms.get(roomId)?.participants.get(userId);
  }

  /** Find which room a user is in (by socketId) */
  findRoomBySocketId(socketId: string): { roomId: string; userId: string } | null {
    for (const [roomId, room] of this.rooms) {
      for (const [userId, participant] of room.participants) {
        if (participant.socketId === socketId) {
          return { roomId, userId };
        }
      }
    }
    return null;
  }

  /** Get all socket IDs in a room (for broadcasting) */
  getRoomSocketIds(roomId: string): string[] {
    const room = this.rooms.get(roomId);
    if (!room) return [];
    return Array.from(room.participants.values()).map((p) => p.socketId);
  }

  /** Get aggregate stats for health endpoint */
  getStats(): {
    activeRooms: number;
    totalParticipants: number;
    totalProducers: number;
    totalConsumers: number;
  } {
    let totalParticipants = 0;
    let totalProducers = 0;
    let totalConsumers = 0;

    for (const [, room] of this.rooms) {
      totalParticipants += room.participants.size;
      for (const [, p] of room.participants) {
        totalProducers += p.producers.size;
        totalConsumers += p.consumers.size;
      }
    }

    return {
      activeRooms: this.rooms.size,
      totalParticipants,
      totalProducers,
      totalConsumers,
    };
  }

  /**
   * Walk live rooms and gather one measurement sample (#1553). The impure mediasoup
   * boundary (producer counts + recv-transport egress bytes) — fed to MediaMetrics,
   * which does the pure accumulation. getStats() failures per transport are swallowed
   * so sampling never throws into the heartbeat.
   */
  async collectMetricsSample(): Promise<MetricsSample> {
    const counts = { camera: 0, screen: 0, audio: 0, webcam: 0, screenshare: 0 };
    const egressBytesByTransport = new Map<string, number>();
    const liveTransportIds = new Set<string>();
    const perRoomVideoPublishers: number[] = [];
    const egressTasks: Promise<void>[] = [];

    for (const [, room] of this.rooms) {
      let roomVideo = 0;
      for (const [, p] of room.participants) {
        roomVideo += RoomManager.countParticipantSources(p, counts);
        for (const [tid, transport] of p.recvTransports) {
          liveTransportIds.add(tid); // every live recv transport, regardless of getStats success
          egressTasks.push(
            RoomManager.gatherTransportEgress(tid, transport, egressBytesByTransport)
          );
        }
      }
      perRoomVideoPublishers.push(roomVideo);
    }
    // Concurrent getStats — sampling latency is the slowest call, not the sum (#1553 review).
    await Promise.all(egressTasks);

    return {
      publishers: { camera: counts.camera, screen: counts.screen },
      activeByKind: { audio: counts.audio, webcam: counts.webcam, screenshare: counts.screenshare },
      egressBytesByTransport,
      liveTransportIds,
      perRoomVideoPublishers,
    };
  }

  /** Tally a participant's producers into `counts`; returns its video-publisher count. */
  private static countParticipantSources(
    p: Participant,
    counts: { camera: number; screen: number; audio: number; webcam: number; screenshare: number }
  ): number {
    let video = 0;
    for (const [, entry] of p.producers) {
      switch (entry.source) {
        case 'camera':
          counts.camera++;
          counts.webcam++;
          video++;
          break;
        case 'screen':
          counts.screen++;
          counts.screenshare++;
          video++;
          break;
        case 'mic':
        case 'screen-audio':
          counts.audio++;
          break;
      }
    }
    return video;
  }

  /**
   * Sum one recv transport's egress `bytesSent` into `out`. On a transient/closing
   * getStats() error the transport is OMITTED from `out` (but the caller keeps it in
   * `liveTransportIds`, so the accumulator does not prune + re-count it from zero).
   */
  private static async gatherTransportEgress(
    tid: string,
    transport: WebRtcTransport,
    out: Map<string, number>
  ): Promise<void> {
    try {
      const stats = await transport.getStats();
      let bytesSent = 0;
      for (const s of stats) {
        const b = (s as { bytesSent?: number }).bytesSent;
        if (typeof b === 'number') bytesSent += b;
      }
      out.set(tid, bytesSent);
    } catch {
      // transient/closing — omit; never throw into the heartbeat
    }
  }

  /** Close all rooms (graceful shutdown) */
  async closeAll(): Promise<void> {
    logger.info('Closing all rooms', { count: this.rooms.size });
    const roomIds = Array.from(this.rooms.keys());
    for (const roomId of roomIds) {
      await this.closeRoom(roomId);
    }
  }

  // ─── Private helpers ─────────────────────────────────────────────────

  /**
   * Per-user audio-tier gate (#1300). REJECTS (throws — so the producer is never
   * created) an over-tier mic opus stream for the participant's tier. Two
   * server-verifiable checks against the inbound `rtpParameters`:
   *
   *  1. **ptime guard** — the effective opus packet duration. Reject when the
   *     declared ptime is below `participant.minPtimeMs` (free 20 ms; premium
   *     10 ms). Lower ptime = more packets/sec = higher overhead, a premium
   *     lever.
   *  2. **audio-tier (bitrate) guard** — the opus `maxaveragebitrate` fmtp.
   *     Reject when it exceeds the ceiling implied by the highest tier in
   *     `participant.allowedAudioTiers` (free tops out at `standard` = 96 kbps).
   *
   * PII-safe: on reject we log ONLY `{ userId, kind, source, reason }` — never
   * the rtpParameters payload, codec parameters, or any media content
   * (observability.md core principle #4). The thrown error is generic.
   *
   * Honest scope (BEST-EFFORT, NOT a hard guarantee). The values inspected are
   * client-DECLARED, OPTIONAL opus `fmtp` parameters (`maxaveragebitrate`,
   * `ptime`/`minptime`). They describe what the client SAYS it will send; they
   * do not bind the local encoder. The gate therefore reliably rejects an
   * HONEST/stock client that truthfully declares an over-tier opus, but a
   * patched client evades it by simply OMITTING the fmtp (we admit-on-absence
   * by design — the stock client legitimately omits these, so we cannot
   * fail-closed on a missing fmtp without rejecting legitimate free users) or
   * by mislabelling mic audio as `source: screen-audio` (only `mic` reaches
   * here — screen-audio is intentionally ungated, bounded only by the advisory
   * transport bitrate cap). This is a produce-time TRIPWIRE on declared intent,
   * not a hard policer. Hard enforcement against a non-cooperative client is
   * FUTURE work (server-side getStats() sampling + the #1542
   * voice.enforce.disconnect path), explicitly out of #1300 scope. Mirrors the
   * honest "NOT a pixel/fps limit" framing on createTransport — and like there,
   * this gates AUDIO quality only, never video pixel/fps (client-enforced +
   * bitrate-backstopped, see `[internal]rules/media-plane.md`). Only `mic`
   * reaches here.
   */
  private enforceAudioTierGate(
    participant: Participant,
    rtpParameters: RtpParameters,
    source: MediaSource
  ): void {
    const opus = RoomManager.findOpusCodec(rtpParameters);
    // No opus codec entry → nothing tier-gated to inspect (e.g. a non-opus
    // audio codec); the bitrate transport cap remains the backstop.
    if (!opus) return;

    const ptime = RoomManager.extractEffectivePtimeMs(opus, rtpParameters);
    if (ptime !== null && ptime < participant.minPtimeMs) {
      this.rejectAudioTier(participant.userId, source, 'ptime_below_tier_minimum');
    }

    const maxAvgBitrate = RoomManager.extractOpusMaxAverageBitrate(opus);
    if (maxAvgBitrate !== null) {
      const ceiling = RoomManager.resolveAllowedOpusBitrateCeiling(participant.allowedAudioTiers);
      if (maxAvgBitrate > ceiling) {
        this.rejectAudioTier(participant.userId, source, 'opus_bitrate_above_tier_ceiling');
      }
    }
  }

  /** Emit the PII-safe violation log and throw a generic produce error (#1300). */
  private rejectAudioTier(userId: string, source: MediaSource, reason: string): never {
    // PII-safe: userId (opaque id, no display fields), kind, source, reason ONLY.
    // NEVER the rtpParameters / codec params / media (observability.md #4).
    logger.warn('Audio producer rejected: over-tier media (#1300)', {
      userId,
      kind: 'audio',
      source,
      reason,
    });
    throw new Error('Audio producer exceeds tier media limits');
  }

  /** Find the opus codec entry in rtpParameters (case-insensitive mimeType match). */
  private static findOpusCodec(
    rtpParameters: RtpParameters
  ): RtpParameters['codecs'][number] | undefined {
    return rtpParameters.codecs?.find((c) => c.mimeType?.toLowerCase() === 'audio/opus');
  }

  /**
   * Effective opus ptime in ms. Precedence: the codec-level `ptime` fmtp, else
   * the transport-level `rtpParameters` ptime (some clients put it there).
   * Returns null when neither is present (admit-on-absence by design).
   *
   * We deliberately DO NOT fall back to `minptime`. That fmtp is only the LOWER
   * BOUND the encoder is permitted to drop to — NOT the actual packetization
   * interval. Stock WebRTC stacks (Chromium / Electron, which the Concord
   * desktop client is built on) emit `a=fmtp:111 minptime=10;useinbandfec=1` by
   * DEFAULT while actually packetizing at 20 ms. Treating `minptime` as the
   * effective ptime would make the gate read 10 ms for a normal free client and
   * falsely reject EVERY stock free-tier mic (10 < 20) — breaking the core
   * feature for the very tier this gate only means to constrain. So an
   * undeclared ptime is not enforced (the transport bitrate cap remains the
   * backstop); only an explicit, actual `ptime` is checked. (Gitar review, #1300.)
   */
  private static extractEffectivePtimeMs(
    opus: RtpParameters['codecs'][number],
    rtpParameters: RtpParameters
  ): number | null {
    const params = (opus.parameters ?? {}) as Record<string, unknown>;
    const codecPtime = RoomManager.toFiniteNumber(params.ptime);
    if (codecPtime !== null) return codecPtime;
    return RoomManager.toFiniteNumber((rtpParameters as { ptime?: unknown }).ptime);
  }

  /** Opus `maxaveragebitrate` fmtp (bps), or null when not declared. */
  private static extractOpusMaxAverageBitrate(
    opus: RtpParameters['codecs'][number]
  ): number | null {
    const params = (opus.parameters ?? {}) as Record<string, unknown>;
    return RoomManager.toFiniteNumber(params.maxaveragebitrate);
  }

  /**
   * The opus bitrate ceiling (bps) a user may produce at = the MAX per-tier
   * ceiling across their `allowedAudioTiers`. Unknown tier strings contribute
   * no ceiling (fail-closed). An empty / all-unknown allow-list floors at the
   * `standard` free ceiling so a malformed entitlement can never escalate.
   */
  private static resolveAllowedOpusBitrateCeiling(allowedAudioTiers: string[]): number {
    let ceiling = 0;
    for (const tier of allowedAudioTiers) {
      const tierCeiling = AUDIO_TIER_OPUS_BITRATE_CEILING_BPS[tier];
      if (typeof tierCeiling === 'number' && tierCeiling > ceiling) ceiling = tierCeiling;
    }
    // Fail-closed: no recognised tier → the free 'standard' ceiling, never 0
    // (a 0 ceiling would reject every opus stream — a self-inflicted DoS).
    return ceiling > 0 ? ceiling : AUDIO_TIER_OPUS_BITRATE_CEILING_BPS.standard;
  }

  /**
   * Coerce a fmtp value to a finite number. mediasoup fmtp parameters can arrive
   * as numbers OR numeric strings depending on the codec entry; both are valid
   * here. Returns null for absent / non-numeric / non-finite values.
   */
  private static toFiniteNumber(value: unknown): number | null {
    if (typeof value === 'number') return Number.isFinite(value) ? value : null;
    if (typeof value === 'string' && value.trim() !== '') {
      const n = Number(value);
      return Number.isFinite(n) ? n : null;
    }
    return null;
  }

  /** Per-room concurrent-producer cap for a capped source, or null if uncapped. */
  private resolveProducerCap(room: Room, source: MediaSource): number | null {
    if (source === 'camera') return resolveVideoPublisherCap(room);
    if (source === 'screen') return SCREEN_PRODUCER_CAP;
    return null; // mic, screen-audio — no count cap
  }

  /**
   * Resolve the per-room cap for `source` and, if capped, reserve a slot
   * synchronously (check + reserve with no intervening await — the #1539
   * TOCTOU-safe path). Throws the cap-exceeded error when the cap is already
   * met; returns the resolved cap (or null for uncapped sources) so the caller
   * can release the reservation once the producer is recorded or produce()
   * fails. No-op reservation for uncapped sources.
   */
  private reserveProducerSlot(room: Room, source: MediaSource): number | null {
    const producerCap = this.resolveProducerCap(room, source);
    if (producerCap === null) return null;
    const pending = room.pendingProducerCounts.get(source) ?? 0;
    if (this.countProducersBySource(room, source) + pending >= producerCap) {
      throw new Error(this.capExceededMessage(source, producerCap));
    }
    room.pendingProducerCounts.set(source, pending + 1);
    return producerCap;
  }

  /** Human-readable cap-exceeded message for a capped source (server-side ints only). */
  private capExceededMessage(source: MediaSource, cap: number): string {
    return source === 'camera'
      ? `Video participant limit reached (max ${cap})`
      : `Screen share limit reached (max ${cap})`;
  }

  /** Release a previously-reserved producer slot (no-op for uncapped sources). */
  private releaseProducerReservation(room: Room, source: MediaSource, cap: number | null): void {
    if (cap === null) return;
    const pending = room.pendingProducerCounts.get(source) ?? 0;
    room.pendingProducerCounts.set(source, Math.max(0, pending - 1));
  }

  /**
   * Audio last-N (#1544) producer-close cleanup: drop a removed producer from the
   * active-speaker decision set and the last-N-managed mic set. Idempotent and
   * safe to call for ANY producer removal — `remove`/`delete` are no-ops for
   * producers that were never mic-tracked (camera/screen/screen-audio), so every
   * producer-removal flow routes through it unconditionally.
   */
  private removeMicProducer(room: Room, producerId: string): void {
    room.activeSpeakers.remove(producerId);
    room.micProducerIds.delete(producerId);
    // #1742: a publisher leaving can drop the room from over-cap back to <= N.
    // Drive the resume-on-shrink drain from the leave EVENT here — this is the
    // convergence point every producer-removal flow routes through — rather than
    // waiting for the next AudioLevelObserver tick. Otherwise a consumer that was
    // last-N-paused while the room was over-cap stays paused until the next
    // 'volumes'/'silence' event (which may not fire if the room has gone quiet).
    // The guard mirrors applyLastNDelta: drain only once the room fits under N.
    if (room.micProducerIds.size <= resolveAudioLastN(room)) {
      this.drainLastNPaused(room);
    }
  }

  private recomputeCameraLayeringGate(room: Room): void {
    const enabled = computeCameraLayeringGate({
      codecKind: this.cameraLayeringCodecKind(room),
      cameraProducerCount: this.countProducersBySource(room, 'camera'),
      demands: Array.from(room.cameraLayerDemands.values()),
      previouslyEnabled: room.cameraLayeringGateEnabled,
    });
    if (enabled === room.cameraLayeringGateEnabled) return;
    room.cameraLayeringGateEnabled = enabled;
    this.emitEvent({ type: 'camera-layering-gate', roomId: room.id, enabled });
  }

  private maxCameraSpatialLayerForParticipant(participant: Participant): LayerValue {
    return participant.maxManualBitrateBps > FREE_MEDIA_ENTITLEMENT.maxManualBitrateBps ? 2 : 1;
  }

  private clearCameraLayerDemand(room: Room, consumerId: string): void {
    if (!room.cameraLayerDemands.delete(consumerId)) return;
    this.recomputeCameraLayeringGate(room);
  }

  private clearParticipantCameraLayerDemands(room: Room, participant: Participant): void {
    let changed = false;
    for (const consumerId of participant.consumers.keys()) {
      if (room.cameraLayerDemands.delete(consumerId)) changed = true;
    }
    if (changed) this.recomputeCameraLayeringGate(room);
  }

  private cameraLayeringCodecKind(room: Room): LayeredCodecKind {
    const floor = this.computeCodecFloor(room.id);
    if (!floor) return 'svc';
    if (floor.some((codec) => codec === 'video/av1' || codec === 'video/vp9')) return 'svc';
    return 'simulcast';
  }

  private countProducersBySource(room: Room, source: MediaSource): number {
    let count = 0;
    for (const [, p] of room.participants) {
      for (const [, info] of p.producers) {
        if (info.source === source) count++;
      }
    }
    return count;
  }

  private validateScreenAudioSource(participant: Participant, kind: MediaKind): void {
    if (kind !== 'audio') {
      throw new Error('screen-audio source must be audio kind');
    }
    const hasScreenProducer = [...participant.producers.values()].some(
      (info) => info.source === 'screen'
    );
    if (!hasScreenProducer) {
      throw new Error('screen-audio requires an active screen producer');
    }
    const hasActiveScreenAudio = [...participant.producers.values()].some(
      (info) => info.source === 'screen-audio' && !info.producer.closed
    );
    if (hasActiveScreenAudio) {
      throw new Error('Only one active screen-audio producer allowed per participant');
    }
  }

  private findProducerInRoom(
    room: Room,
    producerId: string
  ): {
    entry: { producer: Producer; source: MediaSource; kind: MediaKind };
    userId: string;
  } | null {
    for (const [, participant] of room.participants) {
      const entry = participant.producers.get(producerId);
      if (entry) {
        return { entry, userId: participant.userId };
      }
    }
    return null;
  }

  private findParticipantTransport(
    roomId: string,
    userId: string,
    transportId: string
  ): WebRtcTransport | null {
    const room = this.rooms.get(roomId);
    if (!room) return null;

    const participant = room.participants.get(userId);
    if (!participant) return null;

    if (participant.sendTransport?.id === transportId) return participant.sendTransport;
    const recvTransport = participant.recvTransports.get(transportId);
    if (recvTransport) return recvTransport;

    return null;
  }

  private findProducer(
    roomId: string,
    userId: string,
    producerId: string
  ): { producer: Producer; source: MediaSource; kind: MediaKind } | null {
    const room = this.rooms.get(roomId);
    if (!room) return null;

    const participant = room.participants.get(userId);
    if (!participant) return null;

    return participant.producers.get(producerId) ?? null;
  }
}
