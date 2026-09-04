import express from 'express';
import { Server as SocketIOServer, Socket } from 'socket.io';
import { createServer } from 'node:http';
import { config } from './config/index.js';
import { logger } from './lib/logger.js';
import { MediasoupService } from './lib/mediasoup.js';
import {
  CryptoVersionMismatchError,
  parseMediaFrameCryptoVersion,
  RoomManager,
} from './lib/roomManager.js';
import type { DMParticipantPromotion, MediaSource } from './lib/roomManager.js';
import { MediaMetrics } from './lib/mediaMetrics.js';
import {
  createAuthMiddleware,
  releaseDMVoiceAuthorization,
  validateChannelAccess,
  resolveParticipantIdentity,
} from './middleware/auth.js';
import type { AuthenticatedSocketData } from './middleware/auth.js';
import { NatsService } from './lib/nats.js';
import { OpsMetricsPublisher } from './lib/opsMetricsPublisher.js';
import { RedisService } from './lib/redis.js';
import { createExpressErrorHandler } from './lib/expressErrorHandler.js';
import { createOriginGate } from './lib/originGate.js';
import { createAdmissionGate } from './lib/admissionGate.js';
import { handleForceDisconnect } from './lib/forceDisconnect.js';
import { handleEnforcePermissionsMessage } from './lib/enforcePermissions.js';
import { handleSetDeafen } from './lib/setDeafen.js';
import {
  acknowledgeCloseRecvTransport,
  handleCloseRecvTransport,
} from './lib/closeRecvTransport.js';
import { handleSetTestingStatus } from './lib/setTestingStatus.js';
import {
  cleanupEmptyDMJoin,
  DMJoinCallIdTracker,
  KeyedJoinFence,
  reauthorizeDMAdmission,
  rollbackSocketRoomJoin,
  SocketRoomClaim,
  runSocketBoundJoin,
  withKeyedJoinFence,
} from './lib/socketJoinLifecycle.js';
import { withRateLimit as registerRateLimited } from './lib/rateLimit.js';
import type {
  RateLimitDeps,
  RateLimitedSocket,
  RoomEventName,
  SocketListener,
} from './lib/rateLimit.js';

const expectedKeyframeRequestErrors = new Set([
  'Room not found',
  'Requester not found',
  'Sender not found',
]);

// ── #2032 rate-limit rejection diagnostic ────────────────────────────────
// At most one warn per (event, userId) per minute: an attacker who can trip a
// budget can otherwise turn the limiter's own log into the flood the limiter
// exists to stop. The throttle map is bounded for the same reason — it must not
// become the unbounded growth it prevents.
const RATE_LIMIT_LOG_THROTTLE_MS = 60_000;
const RATE_LIMIT_LOG_MAX_KEYS = 1024;
const rateLimitLogLastEmittedMs = new Map<string, number>();

/**
 * True when this key has not been emitted inside the throttle window.
 *
 * Shared by BOTH limiter diagnostics (overflow rejections and handler
 * failures): a handler-crash path is reachable by the same one-packet flood as
 * an overflow, so it needs the same ceiling and the same bounded map. The key
 * is namespaced per reporter so one does not starve the other.
 */
function shouldEmitRateLimitLog(key: string): boolean {
  const nowMs = Date.now();
  const lastMs = rateLimitLogLastEmittedMs.get(key);
  if (lastMs !== undefined && nowMs - lastMs < RATE_LIMIT_LOG_THROTTLE_MS) return false;

  if (rateLimitLogLastEmittedMs.size >= RATE_LIMIT_LOG_MAX_KEYS) {
    for (const [trackedKey, emittedMs] of rateLimitLogLastEmittedMs) {
      if (nowMs - emittedMs >= RATE_LIMIT_LOG_THROTTLE_MS) {
        rateLimitLogLastEmittedMs.delete(trackedKey);
      }
    }
    if (rateLimitLogLastEmittedMs.size >= RATE_LIMIT_LOG_MAX_KEYS) {
      // Still full of in-window entries: evict the oldest insertion (Map
      // preserves insertion order) rather than growing without limit.
      const oldest = rateLimitLogLastEmittedMs.keys().next();
      if (!oldest.done) rateLimitLogLastEmittedMs.delete(oldest.value);
    }
  }

  rateLimitLogLastEmittedMs.set(key, nowMs);
  return true;
}

function logRateLimitRejection(event: RoomEventName, userId: string): void {
  if (!shouldEmitRateLimitLog(`reject|${event}|${userId}`)) return;
  // PII-safe per observability.md #4: the event NAME and the authenticated
  // userId only — never the rejected payload, never the peer address.
  logger.warn('Socket event rate-limited', { event, userId });
}

function logSocketHandlerFailure(event: RoomEventName, userId: string): void {
  if (!shouldEmitRateLimitLog(`handler-error|${event}|${userId}`)) return;
  // Same PII-safe fields, and deliberately no error value: the wrapper does not
  // hand one over, so no `Error.cause` chain can reach this sink
  // (observability.md #3). Handlers log their own failures with their own
  // context; this line reports the residue that escaped them.
  logger.error('Socket handler failed', { event, userId });
}

const rateLimitDeps: RateLimitDeps = {
  onReject: logRateLimitRejection,
  onHandlerError: logSocketHandlerFailure,
};

/**
 * Local binding of the shared #2032 wrapper that supplies the throttled
 * rejection reporter to EVERY registration, so no call site can forget it.
 *
 * Keeping the three-argument shape is deliberate: it leaves the handler as the
 * last argument, which is what lets all 18 conversions stay a pure
 * `socket.on(` → `withRateLimit(socket, ` rename with untouched handler bodies.
 * Passing `rateLimitDeps` positionally at each site would make the handler a
 * non-final argument and force Prettier to re-indent every body.
 */
function withRateLimit<H extends SocketListener>(
  socket: RateLimitedSocket,
  event: RoomEventName,
  handler: H
): void {
  registerRateLimited(socket, event, handler, rateLimitDeps);
}

// #1878: extracted from the join-room handler's catch block so that handler
// stays under the S3776 cognitive-complexity limit. Logs the failure and sends
// the structured crypto_version_mismatch ack (so a lower-version client can
// prompt "update required") or a generic failure ack to the client.
function emitJoinError(
  socket: Socket,
  roomId: string,
  userId: string,
  error: unknown,
  callback?: (payload: unknown) => void
): void {
  logger.error('Error joining room', {
    error: error instanceof Error ? error.message : error,
    stack: error instanceof Error ? error.stack : undefined,
    roomId,
    userId,
  });
  const errPayload =
    error instanceof CryptoVersionMismatchError
      ? {
          error: error.message,
          code: error.code,
          roomVersion: error.roomVersion,
          joinVersion: error.joinVersion,
        }
      : { error: 'Failed to join room' };
  if (callback) {
    callback(errPayload);
    return;
  }
  socket.emit('error', errPayload);
}

function getKeyframeSenderUserId(payload: unknown): string | undefined {
  if (!payload || typeof payload !== 'object' || !('senderUserId' in payload)) {
    return undefined;
  }

  const senderUserId = (payload as { senderUserId?: unknown }).senderUserId;
  if (typeof senderUserId !== 'string') {
    return undefined;
  }

  const trimmed = senderUserId.trim();
  return trimmed === '' ? undefined : trimmed;
}

function getExpectedKeyframeRequestError(error: unknown): string | undefined {
  if (!(error instanceof Error)) {
    return undefined;
  }

  return expectedKeyframeRequestErrors.has(error.message) ? error.message : undefined;
}

async function rollbackRegisteredRoomJoin({
  roomManager,
  cleanupDMJoin,
  roomId,
  userId,
  socketId,
  roomKind,
  callId,
}: {
  roomManager: RoomManager;
  cleanupDMJoin: (authorizedCallId?: string) => Promise<void>;
  roomId: string;
  userId: string;
  socketId: string;
  roomKind: 'channel' | 'dm';
  callId?: string;
}): Promise<void> {
  await rollbackSocketRoomJoin({
    cleanupDMJoin: () => cleanupDMJoin(callId),
    leaveChannelIfOwned: () => roomManager.leaveRoomIfSocketOwned(roomId, userId, socketId),
    removeDMParticipant: () =>
      roomManager.removeProvisionalParticipantIfSocketOwned(roomId, userId, socketId),
    roomKind,
  });
}

function registerJoinRoomHandler(
  socket: Socket,
  data: AuthenticatedSocketData,
  roomManager: RoomManager,
  dmJoinFence: KeyedJoinFence
): void {
  const socketRoomClaim = new SocketRoomClaim();

  // Client sends: { roomId, rtpCapabilities, mediaFrameCryptoVersion, callId? }
  // Server responds with: room-joined event containing router caps, existing producers, participants
  withRateLimit(
    socket,
    'join-room',
    async ({ roomId, rtpCapabilities, mediaFrameCryptoVersion, callId }, callback?) => {
      // Claim before the first await: socket.data can name only one room, so a
      // concurrent or subsequent admission must not create an untracked ghost.
      const releaseSocketRoomClaim = socketRoomClaim.claim(data.roomId, roomId);
      if (!releaseSocketRoomClaim) {
        const error = { error: 'Socket is already joining or joined to a voice room' };
        if (callback) callback(error);
        else socket.emit('error', error);
        return;
      }
      const commitSocketMembership = () => {
        socket.join(roomId);
        data.roomId = roomId;
      };

      const roomKind: 'channel' | 'dm' =
        socket.handshake.auth.room_kind === 'dm' ? 'dm' : 'channel';
      const token = socket.handshake.auth.token;
      const inputCallId = typeof callId === 'string' ? callId : undefined;
      const dmCallId = new DMJoinCallIdTracker(inputCallId);
      const cleanupDMJoin = async (authorizedCallId?: string) => {
        if (roomKind !== 'dm') return;
        const room = roomManager.getRoom(roomId);
        const cleanupCallId = dmJoinFence.callIdForCleanup(roomId, authorizedCallId);
        const cleanupResult = await cleanupEmptyDMJoin({
          authorizedCallId: cleanupCallId,
          closeEmptyRoom: (expectedCallId) => roomManager.closeEmptyDMRoom(roomId, expectedCallId),
          queuedJoinCount: dmJoinFence.pending(roomId),
          releaseAuthorization: (releaseCallId) =>
            releaseDMVoiceAuthorization(roomId, token, releaseCallId),
          room: room
            ? { callId: room.callId, participantCount: room.participants.size }
            : undefined,
        });
        if (cleanupResult === 'deferred') {
          dmJoinFence.deferCleanupCallId(roomId, cleanupCallId);
        }
      };
      const cleanupDMJoinSafely = async (message: string) => {
        try {
          await cleanupDMJoin(dmCallId.current());
        } catch (error) {
          logger.error(message, { error, roomId });
        }
      };
      const authorizeRoom = async (requestedCallId?: string) => {
        const access = await validateChannelAccess(
          data.userId,
          roomId,
          token,
          roomKind,
          requestedCallId
        );
        if (roomKind === 'dm') dmCallId.observe(access.callId);
        return access;
      };
      const executeJoin = async () => {
        try {
          if (!socket.connected) {
            await cleanupDMJoin(dmCallId.current());
            return;
          }
          const parsedMediaFrameCryptoVersion =
            parseMediaFrameCryptoVersion(mediaFrameCryptoVersion);
          // room_kind is only a routing hint: every endpoint independently
          // authorizes the JWT-derived user before any room mutation.

          const outcome = await runSocketBoundJoin({
            authorize: () => authorizeRoom(inputCallId),
            isAllowed: (access, authorizedAccess) =>
              access.allowed &&
              (roomKind === 'channel' ||
                (typeof authorizedAccess?.callId === 'string' &&
                  authorizedAccess.callId !== '' &&
                  access.callId === authorizedAccess.callId)),
            isConnected: () => socket.connected,
            join: async (access) => {
              logger.debug('Channel access validated', { roomId, userId: data.userId });
              // Prefer the server-authoritative identity returned by the
              // control plane over client-supplied display fields (CV-CAN-017).
              const identity = resolveParticipantIdentity(access, {
                username: data.username,
                displayName: data.displayName,
                avatarUrl: data.avatarUrl,
              });
              // Thread the server-authoritative entitlement and permissions
              // into the participant; neither comes from the handshake.
              const result = await roomManager.joinRoom(
                roomId,
                data.userId,
                socket.id,
                identity,
                rtpCapabilities,
                {
                  entitlement: {
                    tier: access.userTier,
                    allowedAudioTiers: access.allowedAudioTiers,
                    minPtimeMs: access.minPtimeMs,
                    maxManualBitrateBps: access.maxManualBitrateBps,
                  },
                  mediaFrameCryptoVersion: parsedMediaFrameCryptoVersion,
                  // Room kind selects the cap strategy; owner tier is absent for DMs.
                  roomContext: {
                    roomKind,
                    ownerTier: access.roomOwnerTier,
                    callId: access.callId,
                    callRingId: access.callRingId,
                    callCallerUserId: access.callCallerUserId,
                  },
                  // Effective publish permissions apply only to channel joins.
                  permissions: access.permissions,
                }
              );

              return {
                authorizedCallId: access.callId,
                identity,
                promotion: undefined as DMParticipantPromotion | undefined,
                result,
              };
            },
            // DM membership can change while the asynchronous RoomManager
            // registration is in flight. Re-read the exact server-issued call
            // ID after registration and before any socket.join/ack.
            reauthorize: (access) =>
              reauthorizeDMAdmission(roomKind, access, inputCallId, authorizeRoom),
            finalize: async (access, value) => {
              // The second authorization is authoritative for both channel and
              // Private Call moderator state. Apply it before socket.join/ack.
              if (roomKind === 'dm') {
                const identity = resolveParticipantIdentity(access, {
                  username: data.username,
                  displayName: data.displayName,
                  avatarUrl: data.avatarUrl,
                });
                value.identity = identity;
                value.promotion = {
                  callId: access.callId,
                  identity,
                  entitlement: {
                    tier: access.userTier,
                    allowedAudioTiers: access.allowedAudioTiers,
                    minPtimeMs: access.minPtimeMs,
                    maxManualBitrateBps: access.maxManualBitrateBps,
                  },
                  serverMuted: access.serverMuted,
                  serverDeafened: access.serverDeafened,
                };
                return;
              }

              if (access.serverMuted) {
                await roomManager.serverMuteUser(roomId, data.userId);
                logger.info('Applied server-mute enforcement on join', {
                  roomId,
                  userId: data.userId,
                });
              }
              if (access.serverDeafened) {
                await roomManager.serverDeafenUser(roomId, data.userId);
                logger.info('Applied server-deafen enforcement on join', {
                  roomId,
                  userId: data.userId,
                });
              }
            },
            commit: (_access, value) => {
              if (roomKind === 'channel') return value;
              if (!value.authorizedCallId || !value.promotion) {
                throw new Error('DM admission is missing its A2 promotion state');
              }
              try {
                value.result = roomManager.promoteDMParticipant(
                  roomId,
                  data.userId,
                  socket.id,
                  value.authorizedCallId,
                  value.promotion,
                  commitSocketMembership
                );
              } catch (error) {
                // A custom/failed adapter must not strand Socket.IO membership
                // or socket.data when its synchronous join boundary throws.
                data.roomId = undefined;
                try {
                  socket.leave(roomId);
                } catch (leaveError) {
                  logger.error('Failed to roll back Socket.IO room membership', {
                    error: leaveError,
                    roomId,
                    userId: data.userId,
                  });
                }
                throw error;
              }
              return value;
            },
            rollback: (access) =>
              rollbackRegisteredRoomJoin({
                roomManager,
                cleanupDMJoin,
                roomId,
                userId: data.userId,
                socketId: socket.id,
                roomKind,
                callId: access.callId,
              }),
          });

          if (outcome.status === 'denied' || outcome.status === 'revoked') {
            if (outcome.status === 'denied') {
              await cleanupDMJoinSafely('Failed to clean up a queued DM room join');
            }
            logger.warn('Channel access denied', {
              userId: data.userId,
              roomId,
              error: outcome.access.error,
            });
            const error = { error: outcome.access.error || 'Access denied' };
            if (callback) callback(error);
            else socket.emit('error', error);
            return;
          }
          if (outcome.status === 'canceled') {
            logger.info('Canceled room join for disconnected socket', {
              roomId,
              userId: data.userId,
              socketId: socket.id,
            });
            return;
          }

          const { access, value } = outcome;
          const { identity, result } = value;
          const joinedParticipant = result.participants.find(
            (participant) => participant.userId === data.userId
          );
          if (roomKind === 'channel') {
            commitSocketMembership();
          }

          // The per-sharer screen-layering gate has no room-wide late-join state.
          // A join simply recomputes each share it consumes (#1924).

          // Broadcast only the same server-authoritative identity stored above.
          socket.to(roomId).emit('user-joined', {
            userId: data.userId,
            username: identity.username,
            displayName: identity.displayName,
            avatarUrl: identity.avatarUrl,
            e2eeEpoch: result.e2eeEpoch,
            ...(joinedParticipant
              ? {
                  isDeafened: joinedParticipant.isDeafened,
                  isTesting: joinedParticipant.isTesting,
                }
              : {}),
          });

          // Respond to the joining client.
          const response = {
            rtpCapabilities: result.rtpCapabilities,
            mediaFrameCryptoVersion: result.mediaFrameCryptoVersion,
            existingProducers: result.existingProducers,
            participants: result.participants,
            channelName: access.channelName,
            e2eeEpoch: result.e2eeEpoch,
          };

          logger.info('Room join response', {
            roomId,
            userId: data.userId,
            existingProducerCount: result.existingProducers.length,
            participantCount: result.participants.length,
          });

          logger.debug('Room join existing producers', {
            roomId,
            userId: data.userId,
            existingProducers: result.existingProducers.map((p) => ({
              producerId: p.producerId,
              userId: p.userId,
              kind: p.kind,
              source: p.source,
            })),
          });

          if (callback) {
            callback(response);
          } else {
            socket.emit('room-joined', response);
          }
        } catch (error) {
          await cleanupDMJoinSafely('Failed to clean up an errored DM room join');
          emitJoinError(socket, roomId, data.userId, error, callback);
        }
      };

      try {
        if (roomKind === 'dm') await withKeyedJoinFence(dmJoinFence, roomId, executeJoin);
        else await executeJoin();
      } finally {
        releaseSocketRoomClaim();
      }
    }
  );
}

async function main() {
  // Create Express app
  const app = express();
  const httpServer = createServer(app);

  // Middleware
  app.use(express.json());

  // Initialize mediasoup
  const mediasoupService = new MediasoupService();
  await mediasoupService.init();

  logger.info('Mediasoup initialized', {
    workers: mediasoupService.getWorkerCount(),
  });

  // Initialize NATS (inter-service messaging)
  const natsService = new NatsService();
  try {
    await natsService.connect();
  } catch {
    logger.warn('NATS connection failed — running without inter-service messaging');
  }

  // Initialize Redis (room state)
  const redisService = new RedisService();
  try {
    await redisService.connect();
  } catch {
    logger.warn('Redis connection failed — running without persistent room state');
  }

  // Initialize RoomManager
  // #1553 measurement counters — accumulated from heartbeat samples, surfaced on /health.
  // Constructed BEFORE RoomManager so the #3104 ICE counters can be injected into it.
  const mediaMetrics = new MediaMetrics();
  const roomManager = new RoomManager(mediasoupService, {
    onIceSelected: (protocol) => mediaMetrics.incrementIceSelected(protocol),
    onIceTerminalWithoutConnect: () => mediaMetrics.incrementIceTerminalWithoutConnect(),
  });
  const opsMetricsPublisher = new OpsMetricsPublisher({
    enabled: config.opsMetrics.enabled,
    nodeId: config.opsMetrics.nodeId,
    secret: config.opsMetrics.sharedSecret,
    intervalMs: config.opsMetrics.intervalMs,
    natsService,
    roomManager,
    mediaMetrics,
  });
  opsMetricsPublisher.start();

  // Wire up room events
  roomManager.onEvent((event) => {
    logger.debug('Room event', { type: event.type, roomId: event.roomId });
  });
  roomManager.onEvent(natsService.createRoomEventHandler());
  roomManager.onEvent(redisService.createRoomEventHandler());

  // Health check endpoint (enhanced — A8).
  //
  // INTERNAL endpoint: intended to be reachable only on the local interface
  // (e.g. localhost:3000) by local/dev ops tooling and health/liveness checks,
  // not exposed to the public internet by the reverse proxy in a typical
  // deployment. The `metrics` field below therefore surfaces capacity counters
  // to operators only — and carries aggregate numbers only (no PII/IDs/keys).
  // Do NOT add per-room/per-user breakdowns here without revisiting this
  // exposure note.
  app.get('/health', (_req, res) => {
    const stats = roomManager.getStats();
    res.json({
      status: 'healthy',
      service: 'media-plane',
      workers: mediasoupService.getWorkerCount(),
      activeRooms: stats.activeRooms,
      totalParticipants: stats.totalParticipants,
      totalProducers: stats.totalProducers,
      totalConsumers: stats.totalConsumers,
      // #1553 measurement counters (aggregate-only; no PII/IDs/keys).
      metrics: mediaMetrics.getSnapshot(),
    });
  });

  // Express error handler — surface uncaught route errors via Winston with a
  // canonical 500 response. Per-event Socket.IO errors (post-upgrade) are
  // handled in each socket-event try/catch and logged via logger.error.
  app.use(createExpressErrorHandler(logger));

  // Initialize Socket.IO
  const io = new SocketIOServer(httpServer, {
    cors: {
      origin: createOriginGate(config.allowedOrigins),
      credentials: true,
    },
    // #2032 admission layer — bounds what per-socket budgets structurally
    // cannot: connection establishment, message size, and idle half-joins.
    allowRequest: createAdmissionGate({
      trustedProxies: config.trustedProxies,
      onReject: () => mediaMetrics.incrementAdmissionRejected(),
      // Startup-only configuration warnings (empty/invalid TRUSTED_PROXIES).
      warn: (message) => logger.warn(message),
    }),
    // 256 KB. Measured against production 2026-08-19: the largest inbound
    // frame is `update-rtp-capabilities` at 4,505 B. `join-room` does NOT
    // carry rtpCapabilities — it sends `undefined` and the client emits them
    // after `device.load` (client/desktop/src/renderer/services/voice/
    // voiceService.ts:2950), so join-room measures 94 B. The largest
    // `produce` frame is 1,933 B (screen simulcast). That is ~58x headroom
    // while cutting the 1 MB default 4x. NOTE: exceeding this CLOSES THE
    // SESSION rather than rejecting one message, so it is sized
    // conservatively on purpose.
    maxHttpBufferSize: 262_144,
    // Down from the 45 s default: this is the window a client holds a socket
    // with zero authentication.
    connectTimeout: 10_000,
    // perMessageDeflate is left at its `false` default DELIBERATELY.
    // Compression on attacker-controlled inbound payloads is a
    // decompression-amplification vector. Do not enable it for "performance".
  });

  // Socket.IO JWT authentication middleware (A2)
  io.use(createAuthMiddleware());

  // ── NATS subscriptions for enforcement commands from control plane ────

  /** Creates a NATS handler for server-level toggle enforcement (mute/deafen). */
  function createServerEnforcementHandler(
    subject: string,
    applyAction: string,
    removeAction: string,
    applyFn: (roomId: string, userId: string) => void | Promise<void>,
    removeFn: (roomId: string, userId: string) => void | Promise<void>,
    eventName: string,
    eventField: string
  ) {
    natsService.subscribe(subject, async (natsData) => {
      const channelId = natsData.channelId as string;
      const userId = natsData.userId as string;
      const action = natsData.action as string;
      if (typeof channelId !== 'string' || typeof userId !== 'string' || typeof action !== 'string')
        return;

      try {
        if (action === applyAction) {
          await applyFn(channelId, userId);
          io.to(channelId).emit(eventName, { userId, [eventField]: true });
        } else if (action === removeAction) {
          await removeFn(channelId, userId);
          io.to(channelId).emit(eventName, { userId, [eventField]: false });
        }
      } catch (err) {
        logger.error(`Failed to handle ${subject}`, { error: err, channelId, userId, action });
      }
    });
  }

  createServerEnforcementHandler(
    'voice.enforce.mute',
    'mute',
    'unmute',
    (r, u) => roomManager.serverMuteUser(r, u),
    (r, u) => roomManager.serverUnmuteUser(r, u),
    'server-mute-changed',
    'serverMuted'
  );

  createServerEnforcementHandler(
    'voice.enforce.deafen',
    'deafen',
    'undeafen',
    (r, u) => roomManager.serverDeafenUser(r, u),
    (r, u) => roomManager.serverUndeafenUser(r, u),
    'server-deafen-changed',
    'serverDeafened'
  );

  /** Creates a NATS handler for user-level enforcement (mute/deafen). */
  function createUserEnforcementHandler(
    subject: string,
    applyFn: (roomId: string, userId: string) => void | Promise<void>
  ) {
    natsService.subscribe(subject, async (natsData) => {
      const channelId = natsData.channelId as string;
      const userId = natsData.userId as string;
      if (!channelId || !userId) return;
      if (typeof channelId !== 'string' || typeof userId !== 'string') return;

      try {
        await applyFn(channelId, userId);
        const participant = roomManager.getParticipant(channelId, userId);
        if (participant) {
          for (const [producerId, entry] of participant.producers) {
            if (entry.kind === 'audio') {
              io.to(channelId).emit('producer-paused', { producerId, userId });
            }
          }
        }
      } catch (err) {
        logger.error(`Failed to handle ${subject}`, { error: err, channelId, userId });
      }
    });
  }

  createUserEnforcementHandler('voice.user_mute', (r, u) => roomManager.userMuteParticipant(r, u));
  createUserEnforcementHandler('voice.user_deafen', (r, u) =>
    roomManager.userDeafenParticipant(r, u)
  );

  // ── Force-disconnect (temporary-SBAC access revocation, #487 P3) ──────
  // Control plane publishes voice.enforce.disconnect {channelId, userId} when a
  // moved user's temporary channel access is revoked. We evict the live peer via
  // RoomManager.leaveRoom (which emits user-left -> voice.left over NATS).
  natsService.subscribe('voice.enforce.disconnect', async (natsData) => {
    const channelId = natsData.channelId as string;
    const userId = natsData.userId as string;
    if (typeof channelId !== 'string' || typeof userId !== 'string') return;

    try {
      await handleForceDisconnect(roomManager, io, channelId, userId);
    } catch (err) {
      logger.error('Failed to handle voice.enforce.disconnect', { error: err, channelId, userId });
    }
  });

  // ── Mid-session permission push (CV-CAN-007 review P1) ────────────────
  // Control plane publishes voice.enforce.permissions {channelId, userId,
  // permissions} after an RBAC mutation touching a voice-connected member. The
  // bitfield rides the same decimal-string wire format as join-authorize and
  // goes through the same strict fail-closed parser; a malformed payload is
  // ignored (the join-time snapshot stays — enforcement never fails open, and
  // a bad message never strips a legitimate peer).
  natsService.subscribe('voice.enforce.permissions', async (natsData) => {
    try {
      await handleEnforcePermissionsMessage(roomManager, io, natsData);
    } catch (err) {
      logger.error('Failed to handle voice.enforce.permissions', { error: err });
    }
  });

  // ─── Socket.IO connection handling ───────────────────────────────────

  // ponytail: process-local serialization matches single-node room ownership;
  // use a distributed admission ID before one DM room can span media nodes.
  const dmJoinFence = new KeyedJoinFence();

  io.on('connection', (socket) => {
    const data = socket.data as AuthenticatedSocketData;
    logger.info('Client connected', {
      socketId: socket.id,
      userId: data.userId,
      username: data.username,
    });

    // ── join-room ────────────────────────────────────────────────────
    registerJoinRoomHandler(socket, data, roomManager, dmJoinFence);

    // ── update-rtp-capabilities ─────────────────────────────────────
    // Client sends this after device.load() to provide its actual RTP capabilities
    withRateLimit(socket, 'update-rtp-capabilities', ({ rtpCapabilities }, callback?) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          if (callback) return callback({ error: 'Not in a room' });
          return;
        }

        roomManager.updateRtpCapabilities(roomId, data.userId, rtpCapabilities);

        // Broadcast updated codec floor to all room members
        const codecFloor = roomManager.computeCodecFloor(roomId);
        io.to(roomId).emit('room-codec-floor', { codecFloor });

        if (callback) callback({ success: true });
      } catch (error) {
        logger.error('Error updating RTP capabilities', {
          error,
          userId: data.userId,
        });
        if (callback) callback({ error: 'Failed to update RTP capabilities' });
      }
    });

    // ── create-transport ─────────────────────────────────────────────
    withRateLimit(socket, 'create-transport', async ({ direction }, callback) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          return callback({ error: 'Not in a room' });
        }

        const transportOptions = await roomManager.createTransport(roomId, data.userId, direction);

        logger.info('Transport created', {
          transportId: transportOptions.id,
          roomId,
          userId: data.userId,
          direction,
          iceCandidates: (
            transportOptions as {
              iceCandidates?: Array<{
                ip: string;
                port: number;
                protocol: string;
              }>;
            }
          ).iceCandidates?.map((c) => `${c.protocol}:${c.ip}:${c.port}`),
        });

        callback(transportOptions);
      } catch (error) {
        logger.error('Error creating transport', {
          error,
          userId: data.userId,
        });
        callback({ error: 'Failed to create transport' });
      }
    });

    // ── connect-transport ────────────────────────────────────────────
    withRateLimit(
      socket,
      'connect-transport',
      async ({ transportId, dtlsParameters }, callback) => {
        try {
          const roomId = data.roomId;
          if (!roomId) {
            return callback({ error: 'Not in a room' });
          }

          logger.info('Transport connect requested', {
            transportId,
            roomId,
            userId: data.userId,
            dtlsRole: dtlsParameters?.role,
          });

          await roomManager.connectTransport(roomId, data.userId, transportId, dtlsParameters);
          callback({ success: true });
        } catch (error) {
          logger.error('Error connecting transport', {
            error,
            userId: data.userId,
            transportId,
          });
          callback({ error: 'Failed to connect transport' });
        }
      }
    );

    // ── produce ──────────────────────────────────────────────────────
    // Client sends: { transportId, kind, rtpParameters, appData: { source } }
    withRateLimit(
      socket,
      'produce',
      async ({ transportId, kind, rtpParameters, appData }, callback) => {
        try {
          const roomId = data.roomId;
          if (!roomId) {
            return callback({ error: 'Not in a room' });
          }

          const source: MediaSource = appData?.source || 'mic';
          const producerInfo = await roomManager.produce(
            roomId,
            data.userId,
            transportId,
            kind,
            rtpParameters,
            source
          );

          // Auto-pause audio producers for server-muted participants
          const participant = roomManager.getParticipant(roomId, data.userId);
          if (participant?.serverMuted && kind === 'audio') {
            await roomManager.pauseProducer(roomId, data.userId, producerInfo.producerId);
          }

          // Notify other users in the room about the new producer
          // Screen shares require opt-in (Tune In model) — not auto-consumed
          socket.to(roomId).emit('new-producer', {
            producerId: producerInfo.producerId,
            userId: data.userId,
            kind: producerInfo.kind,
            source: producerInfo.source,
            requiresOptIn:
              producerInfo.source === 'screen' || producerInfo.source === 'screen-audio',
          });

          // Camera replace-in-place, ordered AFTER the announcement above so the
          // room learns about the replacement before it learns the old camera
          // closed. `closeProducer` puts `producer-closed` on the wire
          // immediately, so evicting inside `roomManager.produce()` would invert
          // that and show every remote a publisher with no camera for a round
          // trip. No-ops for every non-camera source.
          await roomManager.supersedeOlderCameraProducers(
            roomId,
            data.userId,
            producerInfo.producerId,
            producerInfo.source
          );

          callback({ id: producerInfo.producerId });
        } catch (error) {
          const message = error instanceof Error ? error.message : 'Failed to produce';
          logger.error('Error producing', {
            error: message,
            userId: data.userId,
          });
          // Surface limit errors to client (e.g. "Video participant limit reached (max 25)")
          callback({ error: message });
        }
      }
    );

    // ── consume ──────────────────────────────────────────────────────
    withRateLimit(socket, 'consume', async ({ producerId, transportId }, callback) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          logger.warn('Consume rejected — not in a room', {
            userId: data.userId,
            producerId,
          });
          return callback({ error: 'Not in a room' });
        }

        logger.info('Consume requested', {
          roomId,
          userId: data.userId,
          producerId,
          transportId,
        });

        const result = await roomManager.consume(roomId, data.userId, producerId, transportId);

        if (!result) {
          logger.warn('Consume failed — incompatible codecs', {
            roomId,
            userId: data.userId,
            producerId,
          });
          return callback({ error: 'Cannot consume — incompatible codecs' });
        }

        logger.info('Consumer created', {
          consumerId: result.id,
          producerId: result.producerId,
          kind: result.kind,
          source: result.source,
          producerUserId: result.producerUserId,
          consumerUserId: data.userId,
          roomId,
        });

        callback({
          id: result.id,
          producerId: result.producerId,
          kind: result.kind,
          rtpParameters: result.rtpParameters,
          producerUserId: result.producerUserId,
          source: result.source,
        });
      } catch (error) {
        logger.error('Error consuming', {
          error,
          userId: data.userId,
          producerId,
        });
        callback({ error: 'Failed to consume' });
      }
    });

    // ── request-keyframe ─────────────────────────────────────────────
    withRateLimit(socket, 'request-keyframe', async (payload, callback?) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          callback?.({ error: 'Not in a room' });
          return;
        }

        const senderUserId = getKeyframeSenderUserId(payload);
        if (!senderUserId) {
          callback?.({ error: 'senderUserId is required' });
          return;
        }

        const requested = await roomManager.requestKeyFrame(roomId, data.userId, senderUserId);
        callback?.({ success: true, requested });
      } catch (error) {
        const message = getExpectedKeyframeRequestError(error);
        if (message) {
          callback?.({ error: message });
          return;
        }

        logger.error('Error requesting keyframe', { error, userId: data.userId });
        callback?.({ error: 'Failed to request keyframe' });
      }
    });

    // ── resume-consumer ──────────────────────────────────────────────
    withRateLimit(socket, 'resume-consumer', async ({ consumerId }, callback?) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          if (callback) return callback({ error: 'Not in a room' });
          return;
        }

        // Block resume if server-deafened and this is an audio consumer
        const participant = roomManager.getParticipant(roomId, data.userId);
        if (participant?.serverDeafened) {
          const consumer = participant.consumers.get(consumerId);
          if (consumer?.kind === 'audio') {
            if (callback)
              return callback({
                error: 'server_deafened',
                message: 'Server-deafened by moderator',
              });
            return;
          }
        }

        await roomManager.resumeConsumer(roomId, data.userId, consumerId);
        logger.info('Consumer resumed', {
          roomId,
          userId: data.userId,
          consumerId,
        });
        if (callback) callback({ success: true });
      } catch (error) {
        logger.error('Error resuming consumer', { error, userId: data.userId });
        if (callback) callback({ error: 'Failed to resume consumer' });
      }
    });

    // ── set-preferred-layers ─────────────────────────────────────────
    withRateLimit(socket, 'set-preferred-layers', async (payload, callback?) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          callback?.({ error: 'Not in a room' });
          return;
        }

        const result = await roomManager.setPreferredLayers(roomId, data.userId, payload);
        callback?.({ success: true, effectiveLayers: result.effectiveLayers });
      } catch (error) {
        logger.warn('Set preferred layers rejected', {
          userId: data.userId,
          error: error instanceof Error ? error.message : 'unknown',
        });
        callback?.({ error: 'Failed to set preferred layers' });
      }
    });

    // ── pause-consumer ──────────────────────────────────────────────
    withRateLimit(socket, 'pause-consumer', async ({ consumerId }, callback?) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          if (callback) return callback({ error: 'Not in a room' });
          return;
        }

        await roomManager.pauseConsumer(roomId, data.userId, consumerId);
        if (callback) callback({ success: true });
      } catch (error) {
        logger.error('Error pausing consumer', { error, userId: data.userId });
        if (callback) callback({ error: 'Failed to pause consumer' });
      }
    });

    // ── close-consumer ──────────────────────────────────────────────
    // Client-initiated consumer close (e.g. tune-out of screen share).
    // Frees SFU resources and stops RTP forwarding for this consumer.
    withRateLimit(socket, 'close-consumer', ({ consumerId }, callback?) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          if (callback) return callback({ error: 'Not in a room' });
          return;
        }
        if (!consumerId) {
          if (callback) return callback({ error: 'consumerId is required' });
          return;
        }

        const closed = roomManager.closeConsumer(roomId, data.userId, consumerId);
        if (callback) {
          if (closed) {
            callback({ success: true });
          } else {
            callback({ error: 'Consumer not found' });
          }
        }
      } catch (error) {
        logger.error('Error closing consumer', { error, userId: data.userId });
        if (callback) callback({ error: 'Failed to close consumer' });
      }
    });

    // ── close-recv-transport ────────────────────────────────────────
    // PiP and other secondary receivers explicitly release only their own
    // receive transport. Unknown/non-owned IDs are acknowledged identically.
    withRateLimit(socket, 'close-recv-transport', (payload: unknown, callback?: unknown) => {
      let result: ReturnType<typeof handleCloseRecvTransport>;
      try {
        result = handleCloseRecvTransport(roomManager, data.roomId, data.userId, payload);
      } catch (error) {
        logger.error('Error closing receive transport', {
          error: error instanceof Error ? error.message : 'unknown',
          userId: data.userId,
        });
        result = { error: 'Failed to close receive transport' };
      }
      acknowledgeCloseRecvTransport(callback, result);
    });

    // ── pause-producer ───────────────────────────────────────────────
    withRateLimit(socket, 'pause-producer', async ({ producerId }, callback?) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          if (callback) return callback({ error: 'Not in a room' });
          return;
        }

        await roomManager.pauseProducer(roomId, data.userId, producerId);

        // Notify others
        socket.to(roomId).emit('producer-paused', {
          producerId,
          userId: data.userId,
        });

        if (callback) callback({ success: true });
      } catch (error) {
        logger.error('Error pausing producer', { error, userId: data.userId });
        if (callback) callback({ error: 'Failed to pause producer' });
      }
    });

    // ── resume-producer ──────────────────────────────────────────────
    withRateLimit(socket, 'resume-producer', async ({ producerId }, callback?) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          if (callback) return callback({ error: 'Not in a room' });
          return;
        }

        // Block resume if server-muted and this is an audio producer
        const participant = roomManager.getParticipant(roomId, data.userId);
        if (participant?.serverMuted) {
          const producerEntry = participant.producers.get(producerId);
          if (producerEntry?.kind === 'audio') {
            if (callback)
              return callback({ error: 'server_muted', message: 'Server-muted by moderator' });
            return;
          }
        }

        await roomManager.resumeProducer(roomId, data.userId, producerId);

        // Notify others
        socket.to(roomId).emit('producer-resumed', {
          producerId,
          userId: data.userId,
        });

        if (callback) callback({ success: true });
      } catch (error) {
        logger.error('Error resuming producer', { error, userId: data.userId });
        if (callback) callback({ error: 'Failed to resume producer' });
      }
    });

    // ── set-deafen (#685) ─────────────────────────────────────────────
    // Self-deafen is a client-side choice (the client pauses its own incoming
    // audio consumers locally). Records the authoritative flag on the room
    // participant and notifies the rest of the room so their sidebar reflects it —
    // mirroring the self-mute `pause-producer` → `producer-paused` flow. Distinct
    // from the moderator `voice.enforce.deafen` NATS path. Logic lives in
    // lib/setDeafen.ts (unit-tested; mirrors the forceDisconnect extraction).
    withRateLimit(socket, 'set-deafen', ({ isDeafened }: { isDeafened?: unknown }, callback?) => {
      const result = handleSetDeafen(roomManager, socket, data.roomId, data.userId, isDeafened);
      if (callback) callback(result);
    });

    withRateLimit(socket, 'update-test-status', (payload: unknown, callback?) => {
      const result = handleSetTestingStatus(roomManager, socket, data.roomId, data.userId, payload);
      if (callback) callback(result);
    });

    // ── close-producer ───────────────────────────────────────────────
    withRateLimit(socket, 'close-producer', async ({ producerId }, callback?) => {
      try {
        const roomId = data.roomId;
        if (!roomId) {
          if (callback) return callback({ error: 'Not in a room' });
          return;
        }

        const source = await roomManager.closeProducer(roomId, data.userId, producerId);

        // Notify others
        if (source) {
          socket.to(roomId).emit('producer-closed', {
            producerId,
            userId: data.userId,
            source,
          });
        }

        if (callback) callback({ success: true });
      } catch (error) {
        logger.error('Error closing producer', { error, userId: data.userId });
        if (callback) callback({ error: 'Failed to close producer' });
      }
    });

    // ── leave-room ───────────────────────────────────────────────────
    withRateLimit(socket, 'leave-room', async (_, callback?) => {
      try {
        await handleLeaveRoom(socket);
        if (callback) callback({ success: true });
      } catch (error) {
        logger.error('Error leaving room', { error, userId: data.userId });
        if (callback) callback({ error: 'Failed to leave room' });
      }
    });

    // ── disconnect ───────────────────────────────────────────────────
    socket.on('disconnect', async (reason) => {
      logger.info('Client disconnected', {
        socketId: socket.id,
        userId: data.userId,
        reason,
      });

      try {
        await handleLeaveRoom(socket);
      } catch (error) {
        logger.error('Error during disconnect cleanup', {
          error,
          userId: data.userId,
        });
      }
    });
  });

  /** Shared cleanup for leave-room and disconnect */
  async function handleLeaveRoom(socket: Socket) {
    const socketData = socket.data as AuthenticatedSocketData;
    const roomId = socketData.roomId;
    const userId = socketData.userId;

    if (!roomId || !userId) return;

    // Guard: only clean up if this socket still owns the participant.
    // Race condition: a newer socket may have already re-joined the room
    // (e.g. old socket disconnect fires AFTER new socket's join-room).
    // Without this check, the old socket's cleanup would remove the
    // participant that the new socket just added.
    const participant = roomManager.getParticipant(roomId, userId);
    if (participant && participant.socketId !== socket.id) {
      logger.info('Skipping cleanup — participant reconnected on a newer socket', {
        roomId,
        userId,
        oldSocketId: socket.id,
        newSocketId: participant.socketId,
      });
      socketData.roomId = undefined;
      socket.leave(roomId);
      return;
    }

    // Clean up via RoomManager (closes transports, producers, consumers)
    await roomManager.leaveRoom(roomId, userId);

    // Recompute and broadcast codec floor if room still exists
    if (roomManager.getRoom(roomId)) {
      const codecFloor = roomManager.computeCodecFloor(roomId);
      io.to(roomId).emit('room-codec-floor', { codecFloor });
    }

    // Leave Socket.IO room
    socket.leave(roomId);
    socketData.roomId = undefined;
  }

  // Wire up active speaker events to broadcast to room
  roomManager.onEvent((event) => {
    if (event.type === 'active-speaker') {
      // Empty userId signals silence (no one is speaking)
      io.to(event.roomId).emit('active-speaker', {
        userId: event.userId || null,
        volume: event.volume,
      });
    }

    // Broadcast producer-closed from server-side cleanup (e.g. transport close)
    if (event.type === 'producer-removed') {
      io.to(event.roomId).emit('producer-closed', {
        producerId: event.producerId,
        userId: event.userId,
        kind: event.kind,
        source: event.source,
      });
    }

    if (event.type === 'user-left') {
      io.to(event.roomId).except(event.socketId).emit('user-left', {
        userId: event.userId,
        e2eeEpoch: event.e2eeEpoch,
      });
    }

    if (event.type === 'camera-layering-gate') {
      io.to(event.roomId).emit('camera-layering-gate', { enabled: event.enabled });
    }

    if (event.type === 'screen-layering-gate') {
      // Targeted to the SHARER's socket (#1924 fix "B") — the gate governs that
      // sharer's own publish, so it is never a room broadcast.
      io.to(event.targetSocketId).emit('screen-layering-gate', { enabled: event.enabled });
    }
  });

  // Broadcast epoch-sync to every room every 10s. Under E2EE-everywhere
  // (#201) every room is encrypted by construction, so this loop has no
  // per-room encryption gate — the `if (room)` below is a null guard
  // against race-with-deletion. Short interval keeps voice latency-tolerant.
  const epochSyncInterval = setInterval(() => {
    for (const roomId of roomManager.getActiveRoomIds()) {
      const room = roomManager.getRoom(roomId);
      if (room) {
        io.to(roomId).emit('epoch-sync', { epoch: room.e2eeEpoch });
      }
    }
  }, 10_000);

  // Publish room heartbeat every 30s for control plane reconciliation.
  // Each message contains the authoritative list of users in each room,
  // allowing the control plane to delete stale voice_participants rows.
  let metricsSampling = false; // re-entrancy guard for the async metrics block below
  const roomHeartbeatInterval = setInterval(async () => {
    for (const roomId of roomManager.getActiveRoomIds()) {
      const room = roomManager.getRoom(roomId);
      if (!room) continue;
      natsService.publish('voice.heartbeat', {
        channelId: roomId,
        userIds: Array.from(room.participants.keys()),
        callId: room.callId,
        ringId: room.callRingId,
        callerUserId: room.callCallerUserId,
        timestamp: natsService.nextVoiceLifecycleTimestamp(),
      });
    }
    // #1553 measurement: sample here only when the ops publisher is disabled. When it is
    // enabled, that publisher owns sampling so participant-hours and egress are not counted
    // twice. The heartbeat still emits the latest aggregate-only ops log.
    // The try/catch keeps the async heartbeat from ever throwing into the timer (degrade,
    // don't crash); the metricsSampling guard skips a tick if the prior sampling is still
    // running so two ticks never interleave ingest() on shared state (#1553 review).
    if (config.opsMetrics.enabled) {
      logger.info('media-metrics', mediaMetrics.getSnapshot());
    } else if (!metricsSampling) {
      metricsSampling = true;
      try {
        const sample = await roomManager.collectMetricsSample();
        mediaMetrics.ingest(sample, 30);
        logger.info('media-metrics', mediaMetrics.getSnapshot());
      } catch (err) {
        logger.error('media-metrics sampling failed', { error: err });
      } finally {
        metricsSampling = false;
      }
    }
  }, 30_000);

  // Start server
  const port = config.port;
  httpServer.listen(port, () => {
    logger.info('Media Plane server started', {
      port,
      environment: config.environment,
    });
  });

  // Graceful shutdown
  const shutdown = async () => {
    logger.info('Shutting down gracefully');
    clearInterval(epochSyncInterval);
    clearInterval(roomHeartbeatInterval);
    await opsMetricsPublisher.stop();

    // Close all rooms first (notifies participants, publishes NATS events)
    await roomManager.closeAll();

    // Close inter-service connections
    await natsService.close();
    await redisService.close();

    // Close mediasoup workers
    await mediasoupService.close();

    httpServer.close(() => {
      logger.info('Server closed');
      process.exit(0);
    });

    // Force exit after 10 seconds
    setTimeout(() => {
      logger.warn('Forced shutdown after timeout');
      process.exit(1);
    }, 10_000);
  };

  process.on('SIGTERM', shutdown);
  process.on('SIGINT', shutdown);
}

// Start the application
try {
  await main();
} catch (error) {
  logger.error('Fatal error during startup', { error });
  process.exit(1);
}
