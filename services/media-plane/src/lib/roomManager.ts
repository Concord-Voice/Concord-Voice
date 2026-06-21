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

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** Media source identifier — matches client-side appData.source */
export type MediaSource = 'mic' | 'camera' | 'screen' | 'screen-audio';

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
}

export interface Room {
  id: string;
  router: Router;
  audioLevelObserver: AudioLevelObserver | null;
  participants: Map<string, Participant>;
  createdAt: Date;
  e2eeEpoch: number;
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
    }
  | { type: 'user-left'; roomId: string; userId: string }
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
  | { type: 'active-speaker'; roomId: string; userId: string; volume: number };

export type RoomEventHandler = (event: RoomEvent) => void;

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
      pendingProducerCounts: new Map(),
      activeSpeakers: new ActiveSpeakerSet(lastN, config.audioLastNHoldMs),
      lastNPausedConsumers: new Set(),
      micProducerIds: new Set(),
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
    username: string,
    displayName?: string,
    avatarUrl?: string,
    rtpCapabilities?: RtpCapabilities
  ): Promise<{
    rtpCapabilities: RtpCapabilities;
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
    }

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
    };

    room.participants.set(userId, participant);

    // E2EE epoch tracking: increment epoch on every join (forward secrecy)
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
    }));

    return {
      rtpCapabilities: room.router.rtpCapabilities,
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

    // Close all consumers
    for (const [, consumer] of participant.consumers) {
      if (!consumer.closed) consumer.close();
    }
    participant.consumers.clear();

    // Close all producers (also removes from AudioLevelObserver)
    for (const [producerId, entry] of participant.producers) {
      if (!entry.producer.closed) entry.producer.close();
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

    // Close transports
    if (participant.sendTransport && !participant.sendTransport.closed) {
      participant.sendTransport.close();
    }
    for (const [, transport] of participant.recvTransports) {
      if (!transport.closed) transport.close();
    }
    participant.recvTransports.clear();

    room.participants.delete(userId);

    // E2EE: increment epoch on leave (forward secrecy)
    room.e2eeEpoch++;

    logger.info('Participant left room', {
      roomId,
      userId,
      remainingParticipants: room.participants.size,
    });

    this.emitEvent({ type: 'user-left', roomId, userId });

    // Tear down room if empty
    if (room.participants.size === 0) {
      await this.closeRoom(roomId);
    }
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

    // Set max incoming bitrate
    if (config.mediasoup.webRtcTransport.maxIncomingBitrate) {
      try {
        await transport.setMaxIncomingBitrate(config.mediasoup.webRtcTransport.maxIncomingBitrate);
      } catch {
        // Some transports don't support this
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

    // Enforce per-room concurrent-producer caps (camera: tier-resolved free
    // default 8; screen: SCREEN_PRODUCER_CAP). TOCTOU-safe (#1539 review): the
    // producer is only recorded in participant.producers AFTER the await below,
    // so a bare synchronous count check lets concurrent produce() calls all pass
    // against a stale count and overrun the cap. We reserve a slot synchronously
    // — the check + reservation run with no intervening await, so concurrent
    // invocations observe each other's reservations — and release it once the
    // producer is recorded or if produce() fails.
    const producerCap = this.resolveProducerCap(room, source);
    if (producerCap !== null) {
      const pending = room.pendingProducerCounts.get(source) ?? 0;
      if (this.countProducersBySource(room, source) + pending >= producerCap) {
        throw new Error(this.capExceededMessage(source, producerCap));
      }
      room.pendingProducerCounts.set(source, pending + 1);
    }

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
    });

    consumer.consumers.set(newConsumer.id, newConsumer);

    // Audio last-N (#1544): if this is a MIC audio consumer whose producer is not
    // currently in the top-N, start it last-N-paused (it was created paused:true)
    // so the client's unconditional resume is refused by the guard until the
    // speaker enters the top-N. Screen-audio (not a mic producer) and video are
    // never managed.
    if (
      newConsumer.kind === 'audio' &&
      room.micProducerIds.has(producerId) &&
      !room.activeSpeakers.current().has(producerId)
    ) {
      room.lastNPausedConsumers.add(newConsumer.id);
    }

    // Clean up on close
    newConsumer.on('transportclose', () => {
      consumer.consumers.delete(newConsumer.id);
      room.lastNPausedConsumers.delete(newConsumer.id);
    });
    newConsumer.on('producerclose', () => {
      consumer.consumers.delete(newConsumer.id);
      room.lastNPausedConsumers.delete(newConsumer.id);
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
    for (const producerId of delta.removed) this.setAudioForwarding(room, producerId, false);
    for (const producerId of delta.added) this.setAudioForwarding(room, producerId, true);
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
    const participant = this.rooms.get(roomId)?.participants.get(userId);
    if (participant) {
      participant.rtpCapabilities = rtpCapabilities;
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

  /** Per-room concurrent-producer cap for a capped source, or null if uncapped. */
  private resolveProducerCap(room: Room, source: MediaSource): number | null {
    if (source === 'camera') return resolveVideoPublisherCap(room);
    if (source === 'screen') return SCREEN_PRODUCER_CAP;
    return null; // mic, screen-audio — no count cap
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
