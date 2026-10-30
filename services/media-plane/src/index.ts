import express from 'express';
import { Server as SocketIOServer, Socket } from 'socket.io';
import { createServer } from 'node:http';
import { randomBytes, randomUUID } from 'node:crypto';
import { networkInterfaces } from 'node:os';
import { announcedIpAdvisory, config } from './config/index.js';
import { logger } from './lib/logger.js';
import { MediasoupService } from './lib/mediasoup.js';
import {
  CryptoVersionMismatchError,
  MediaPolicyCooldownError,
  RoomCreationRefusedError,
  VideoPolicyCooldownError,
  parseMediaFrameCryptoVersion,
  RoomManager,
} from './lib/roomManager.js';
import type {
  ChannelParticipantPromotion,
  DMParticipantPromotion,
  JoinRoomResult,
  MediaSource,
} from './lib/roomManager.js';
import { emitCameraLayeringGate } from './lib/layeringGateBroadcast.js';
import { MediaMetrics } from './lib/mediaMetrics.js';
import { MediaPolicer, MediaPolicyLedger } from './lib/mediaPolicer.js';
import { createMediaPolicerTick } from './lib/mediaPolicerTick.js';
import {
  requireCompatibleVideoPublishProfile,
  VideoClientUpdateRequiredError,
} from './lib/videoPublishProfile.js';
import {
  MEDIA_POLICY_PAUSED_RESUME_ACK,
  mediaPolicyCooldownAck,
  videoPolicyCooldownAck,
} from './lib/mediaPolicyWire.js';
import {
  createAuthMiddleware,
  createVoiceAuthConnectionMiddleware,
  getVoiceAuthConnection,
  createSocketTokenRenewal,
  parsePermissionBitfield,
  releaseDMVoiceAuthorization,
  validateChannelAccess,
  resolveParticipantIdentity,
} from './middleware/auth.js';
import type { AuthenticatedSocketData } from './middleware/auth.js';
import { NatsService } from './lib/nats.js';
import { EnforcementAvailability } from './lib/enforcementAvailability.js';
import { VoiceCommandDispatcher } from './lib/voiceCommandDispatcher.js';
import { createChannelAdmissionActivationHandler } from './lib/channelAdmissionActivation.js';
import { OpsMetricsPublisher } from './lib/opsMetricsPublisher.js';
import { RedisService } from './lib/redis.js';
import { createExpressErrorHandler } from './lib/expressErrorHandler.js';
import { createOriginGate } from './lib/originGate.js';
import { createAdmissionGate } from './lib/admissionGate.js';
import { SecurityEventWriter } from './lib/securityEvent.js';
import type { EmitSecurityEvent } from './lib/securityEvent.js';
import {
  createCredentialEpochEjectionAckHandler,
  createDMBlockDisconnectAckHandler,
  createVoiceEnforcementSessionEjectionHandler,
  handleForceDisconnect,
} from './lib/forceDisconnect.js';
import {
  registerVoiceEnforcementSession,
  setVoiceAuthConnections,
  renewVoiceEnforcementSession,
  pollVoiceEnforcementHealth,
} from './lib/voiceEnforcementSession.js';
import { readVoiceAuthResponse, VoiceAuthConnections } from './lib/voiceAuthConnection.js';
import { newRequestContext, signHttpRequest, verifyHttpResponse } from './lib/voiceAuthProtocol.js';
import type { VoiceEnforcementSession } from './lib/voiceEnforcementSession.js';
import { voiceEnforcementNodeBootId } from './lib/voiceEnforcementIdentity.js';
import {
  releaseVoiceEnforcementSessionsAfterClose,
  snapshotVoiceEnforcementSessions,
  VoiceEnforcementSessionReleaseQueue,
} from './lib/voiceEnforcementShutdown.js';
import { FailureStateLogger, VoiceEnforcementExpiryFence, VoiceEnforcementHealth, VoiceEnforcementLease } from './lib/voiceEnforcementLease.js';
import { handleEnforcePermissionsMessage } from './lib/enforcePermissions.js';
import {
  handleNatsEnforcementCommand,
  isCanonicalEnforcementUUID,
} from './lib/enforcementCommand.js';
import { getIdentityAuthorityReasonCode } from './lib/identityAuthority.js';
import { enqueueServerEnforcement } from './lib/serverEnforcement.js';
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
  reauthorizeAdmission,
  rollbackSocketRoomJoin,
  SocketRoomClaim,
  runSocketBoundJoin,
  withKeyedJoinFence,
  roomCreationRefusalAck,
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

// Complete moderation snapshots are a versioned control-plane contract. Legacy
// action-only enforcement remains intentionally unversioned for rollout compatibility.
const SERVER_ENFORCEMENT_SNAPSHOT_VERSION = 1;

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

/**
 * The join-room failure ack: a structured crypto_version_mismatch (so a
 * lower-version client can prompt "update required"), a media-policy cooldown
 * with its retry-after (#2153), or the generic failure.
 */
function joinErrorAck(error: unknown): object {
  const placementRefusal = roomCreationRefusalAck(error);
  if (placementRefusal) return placementRefusal;
  if (error instanceof CryptoVersionMismatchError) {
    return {
      error: error.message,
      code: error.code,
      roomVersion: error.roomVersion,
      joinVersion: error.joinVersion,
    };
  }
  if (error instanceof MediaPolicyCooldownError) return mediaPolicyCooldownAck(error);
  return { error: 'Failed to join room' };
}

// #1878: extracted from the join-room handler's catch block so that handler
// stays under the S3776 cognitive-complexity limit. Logs the failure and sends
// the ack joinErrorAck selects.
function emitJoinError(
  socket: Socket,
  roomId: string,
  userId: string,
  error: unknown,
  callback?: (payload: unknown) => void
): void {
  // #2153: a cooldown refusal is an expected outcome, already logged (warn) where
  // RoomManager decided it. Logging it again here as an error with a stack would turn
  // every retry of a user in cooldown into a crash-shaped log line.
  if (
    !(error instanceof MediaPolicyCooldownError) &&
    !(error instanceof RoomCreationRefusedError)
  ) {
    logger.error('Error joining room', {
      error: error instanceof Error ? error.message : error,
      stack: error instanceof Error ? error.stack : undefined,
      roomId,
      userId,
    });
  }
  const errPayload = joinErrorAck(error);
  if (callback) {
    callback(errPayload);
    return;
  }
  socket.emit('error', errPayload);
}

type ServerEnforcementErrorContext = {
  roomManager: RoomManager;
  io: SocketIOServer;
  subject: string;
  channelId: string;
  userId: string;
  action: string;
  applyAction: string;
  hasSnapshot: boolean;
  serverMuted: unknown;
  serverDeafened: unknown;
  error: unknown;
};

async function handleServerEnforcementError({
  roomManager,
  io,
  subject,
  channelId,
  userId,
  action,
  applyAction,
  hasSnapshot,
  serverMuted,
  serverDeafened,
  error,
}: ServerEnforcementErrorContext): Promise<void> {
  logger.error(`Failed to handle ${subject}`, { error, channelId, userId, action });
  if ((hasSnapshot && (serverMuted || serverDeafened)) || action === applyAction) {
    try {
      await handleForceDisconnect(roomManager, io, channelId, userId, undefined, {
        reason: 'access_revoked',
      });
    } catch (disconnectErr) {
      logger.error('Failed to fail closed after server enforcement error', {
        error: disconnectErr,
        channelId,
        userId,
      });
    }
  }
}

type ServerEnforcementCommandContext = {
  serverEnforcementChains: Map<string, Promise<void>>;
  natsData: Record<string, unknown>;
  roomManager: RoomManager;
  io: SocketIOServer;
  subject: string;
  channelId: string;
  userId: string;
  action: string | undefined;
  applyAction: string;
  removeAction: string;
  applyFn: (roomId: string, userId: string) => void | Promise<void>;
  removeFn: (roomId: string, userId: string) => boolean | Promise<boolean>;
  eventName: string;
  eventField: string;
};

async function applyServerEnforcementCommand({
  serverEnforcementChains,
  natsData,
  roomManager,
  io,
  subject,
  channelId,
  userId,
  action,
  applyAction,
  removeAction,
  applyFn,
  removeFn,
  eventName,
  eventField,
}: ServerEnforcementCommandContext): Promise<void> {
  await enqueueServerEnforcement(serverEnforcementChains, channelId, userId, async () => {
    const authorizationRevision = parsePermissionBitfield(natsData.authorizationRevision);
    const serverMuted = natsData.serverMuted;
    const serverDeafened = natsData.serverDeafened;
    const carriesSnapshot =
      serverMuted !== undefined ||
      serverDeafened !== undefined ||
      natsData.authorizationRevision !== undefined ||
      natsData.version !== undefined;
    const hasSnapshot =
      typeof serverMuted === 'boolean' &&
      typeof serverDeafened === 'boolean' &&
      authorizationRevision !== undefined &&
      authorizationRevision > 0n &&
      natsData.version === SERVER_ENFORCEMENT_SNAPSHOT_VERSION;
    if (carriesSnapshot && !hasSnapshot) {
      logger.warn('Rejecting malformed server enforcement snapshot', { channelId, userId });
      try {
        await handleForceDisconnect(roomManager, io, channelId, userId, undefined, {
          reason: 'access_revoked',
        });
      } catch (disconnectErr) {
        logger.error('Failed to fail closed after malformed server enforcement snapshot', {
          error: disconnectErr,
          channelId,
          userId,
        });
      }
      return;
    }

    try {
      if (hasSnapshot) {
        const applied = await roomManager.applyServerEnforcementSnapshot(
          channelId,
          userId,
          serverMuted,
          serverDeafened,
          authorizationRevision
        );
        if (!applied) return;
        io.to(channelId).emit(eventName, { userId, [eventField]: natsData[eventField] });
        return;
      }
      if (action === applyAction) {
        await applyFn(channelId, userId);
        io.to(channelId).emit(eventName, { userId, [eventField]: true });
      } else if (action === removeAction) {
        const applied = await removeFn(channelId, userId);
        if (!applied) return;
        io.to(channelId).emit(eventName, { userId, [eventField]: false });
      }
    } catch (error) {
      await handleServerEnforcementError({
        roomManager,
        io,
        subject,
        channelId,
        userId,
        action: action ?? '',
        applyAction,
        hasSnapshot,
        serverMuted,
        serverDeafened,
        error,
      });
    }
  });
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

type RoomJoinValue = {
  authorizedCallId: string | undefined;
  identity: ReturnType<typeof resolveParticipantIdentity>;
  promotion: ChannelParticipantPromotion | DMParticipantPromotion | undefined;
  result: JoinRoomResult;
};

function rollbackSocketMembership(
  socket: Socket,
  data: AuthenticatedSocketData,
  roomId: string
): void {
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
}

function commitChannelAdmission(
  roomManager: RoomManager,
  socket: Socket,
  data: AuthenticatedSocketData,
  roomId: string,
  value: RoomJoinValue,
  commitSocketMembership: () => void
): RoomJoinValue {
  if (!value.promotion) {
    throw new Error('Room admission is missing its A2 promotion state');
  }
  if (!('permissions' in value.promotion)) {
    throw new Error('Channel admission is missing its A2 promotion state');
  }
  try {
    const displacedSocketId = roomManager.getParticipant(roomId, data.userId)?.socketId;
    value.result = roomManager.promoteChannelParticipant(
      roomId,
      data.userId,
      socket.id,
      value.promotion,
      commitSocketMembership
    );
    if (displacedSocketId && displacedSocketId !== socket.id) {
      socket.nsp.sockets.get(displacedSocketId)?.disconnect(true);
    }
  } catch (error) {
    // A custom/failed adapter must not strand Socket.IO membership or socket.data
    // when its synchronous join boundary throws.
    rollbackSocketMembership(socket, data, roomId);
    throw error;
  }
  return value;
}

function commitDMAdmission(
  roomManager: RoomManager,
  socket: Socket,
  data: AuthenticatedSocketData,
  roomId: string,
  value: RoomJoinValue,
  commitSocketMembership: () => void
): RoomJoinValue {
  if (!value.promotion) {
    throw new Error('Room admission is missing its A2 promotion state');
  }
  if (!value.authorizedCallId || !('callId' in value.promotion)) {
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
    // A custom/failed adapter must not strand Socket.IO membership or socket.data
    // when its synchronous join boundary throws.
    rollbackSocketMembership(socket, data, roomId);
    throw error;
  }
  return value;
}

async function rollbackRegisteredRoomJoin({
  roomManager,
  cleanupDMJoin,
  roomId,
  userId,
  socketId,
  sessionGeneration,
  roomKind,
  callId,
}: {
  roomManager: RoomManager;
  cleanupDMJoin: (authorizedCallId?: string) => Promise<void>;
  roomId: string;
  userId: string;
  socketId: string;
  sessionGeneration: string;
  roomKind: 'channel' | 'dm';
  callId?: string;
}): Promise<void> {
  await rollbackSocketRoomJoin({
    cleanupDMJoin: () => cleanupDMJoin(callId),
    leaveChannelIfOwned: () => roomManager.leaveRoomIfSocketOwned(roomId, userId, socketId, sessionGeneration),
    removeDMParticipant: () =>
      roomManager.removeProvisionalParticipantIfSocketOwned(roomId, userId, socketId, sessionGeneration),
    roomKind,
  });
}

function parseJoinMediaFrameCryptoVersion(
  value: unknown,
  observe: (event: Parameters<EmitSecurityEvent>[0]) => void
): number {
  try {
    return parseMediaFrameCryptoVersion(value);
  } catch (error) {
    observe({
      eventType: 'media_integrity',
      outcome: 'denied',
      severity: 'medium',
      reasonCode: 'crypto_version_invalid',
      routeTemplate: 'socket.join',
    });
    throw error;
  }
}

function registerJoinRoomHandler(
  socket: Socket,
  data: AuthenticatedSocketData,
  {
    roomManager,
    dmJoinFence,
    channelAdmissionFence,
    voiceAuthConnections,
    emitSecurityEvent,
    // Required: the per-connection wrapper carries this socket's security-event
    // reporter. A module-level default existed and was unreachable -- the sole
    // call site always passes one -- so it only hid a miswiring (PR #3157).
    withRateLimit,
    isShuttingDown,
  }: {
    roomManager: RoomManager;
    dmJoinFence: KeyedJoinFence;
    channelAdmissionFence: KeyedJoinFence;
    voiceAuthConnections: VoiceAuthConnections;
    emitSecurityEvent: EmitSecurityEvent | undefined;
    withRateLimit: <H extends SocketListener>(
      socket: RateLimitedSocket,
      event: RoomEventName,
      handler: H
    ) => void;
    isShuttingDown: () => boolean;
  }
): void {
  const observe = (event: Parameters<EmitSecurityEvent>[0]) => {
    try {
      emitSecurityEvent?.(event);
    } catch {
      // The join result/ack is authoritative over best-effort telemetry.
    }
  };
  const socketRoomClaim = new SocketRoomClaim();
  const refuseJoin = (message: string, callback?: (payload: unknown) => void) => {
    const error = { error: message };
    if (callback) callback(error);
    else socket.emit('error', error);
  };

  // Client sends: { roomId, rtpCapabilities, mediaFrameCryptoVersion, callId? }
  // Server responds with: room-joined event containing router caps, existing producers, participants
  withRateLimit(
    socket,
    'join-room',
    async ({ roomId, rtpCapabilities, mediaFrameCryptoVersion, callId }, callback?) => {
      if (isShuttingDown()) {
        refuseJoin('Voice service is shutting down', callback);
        return;
      }
      if (!isCanonicalEnforcementUUID(roomId)) {
        refuseJoin('Invalid room ID: canonical UUID required', callback);
        return;
      }
      // Claim before the first await: socket.data can name only one room, so a
      // concurrent or subsequent admission must not create an untracked ghost.
      const releaseSocketRoomClaim = socketRoomClaim.claim(data.roomId, roomId);
      if (!releaseSocketRoomClaim) {
        observe({
          eventType: 'media_integrity',
          outcome: 'denied',
          severity: 'medium',
          reasonCode: 'room_claim_conflict',
          routeTemplate: 'socket.join',
        });
        refuseJoin('Socket is already joining or joined to a voice room', callback);
        return;
      }
      const connection = getVoiceAuthConnection(socket);
      if (!connection) {
        releaseSocketRoomClaim();
        refuseJoin('Voice connection unavailable', callback);
        return;
      }
      const commitSocketMembership = () => {
        voiceAuthConnections.assertProductive(connection);
        socket.join(roomId);
        data.roomId = roomId;
        // Deliberately INSIDE this function, not after its call sites. The
        // camera gate is room-wide state announced only on a transition, so a
        // joiner needs the current value — and it must be read after the socket
        // is in the room, or a transition landing in between reaches everyone
        // except the joiner. Both the channel and DM paths commit through here,
        // so one line covers both and the ordering cannot drift.
        //
        // `socket.id`, not `data.userId`: the room-membership map is not
        // populated yet on the DM promotion path, so resolving the target
        // through it silently missed exactly the participants this exists for.
        roomManager.emitCameraGateSnapshotFor(roomId, socket.id);
      };
      const roomKind: 'channel' | 'dm' =
        socket.handshake.auth.room_kind === 'dm' ? 'dm' : 'channel';
      const token = socket.handshake.auth.token;
      let enforcementSession: VoiceEnforcementSession | undefined;
      let admissionCommitted = false;
      let placementRefused = false;
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
      // One admission identity covers A1 registration and A2 activation. It is
      // deliberately created by the media service, never accepted from a peer.
      const admissionId = roomKind === 'channel' ? randomUUID() : undefined;
      const authorizeRoom = async (requestedCallId?: string, activate = false) => {
        const exactSession = enforcementSession;
        if (!exactSession) throw new Error('Voice room reservation required');
        // A1 can create durable admission state. Retain its in-flight work just
        // like native creation, so a concurrent terminal command cannot release
        // this generation while authorization is still unresolved.
        return voiceAuthConnections.trackRoomOperation(exactSession, async () => {
          const access = await validateChannelAccess(
            data.userId,
            roomId,
            token,
            roomKind,
            requestedCallId,
            admissionId ? { admissionId, socketId: socket.id, activate } : undefined
          );
          if (roomKind === 'dm') dmCallId.observe(access.callId);
          return access;
        });
      };
      const isAuthorizedAccess = (
        access: Awaited<ReturnType<typeof authorizeRoom>>,
        authorizedAccess?: Awaited<ReturnType<typeof authorizeRoom>>
      ) =>
        access.allowed &&
        (roomKind === 'channel' ||
          (typeof authorizedAccess?.callId === 'string' &&
            authorizedAccess.callId !== '' &&
            access.callId === authorizedAccess.callId));
      const executeJoin = async () => {
        try {
          if (!socket.connected || Date.now() >= data.tokenExpiresAt || isShuttingDown()) {
            await cleanupDMJoin(dmCallId.current());
            return;
          }
          const parsedMediaFrameCryptoVersion = parseJoinMediaFrameCryptoVersion(
            mediaFrameCryptoVersion,
            observe
          );
          // room_kind is only a routing hint: every endpoint independently
          // authorizes the JWT-derived user before any room mutation.

          const outcome = await runSocketBoundJoin({
            authorize: () => authorizeRoom(inputCallId, false),
            isAllowed: isAuthorizedAccess,
            // executeJoin checks availability after the queue wait and before
            // A1; repeat it at the promotion boundaries after authorization.
            isConnected: () =>
              socket.connected && Date.now() < data.tokenExpiresAt && !isShuttingDown() &&
              voiceAuthConnections.snapshot(connection).state === 'registered',
            join: async (access) => {
              logger.debug('Channel access validated', { roomId, userId: data.userId });
              // Prefer the server-authoritative identity returned by the
              // control plane over client-supplied display fields (CV-CAN-017).
              const identity = resolveParticipantIdentity(access, {
                username: data.username,
                displayName: data.displayName,
                avatarUrl: data.avatarUrl,
              });
              const exactSession = enforcementSession;
              if (!exactSession) throw new Error('Voice room reservation required');
              // Thread the server-authoritative entitlement and permissions
              // into the participant; neither comes from the handshake.
              let result: JoinRoomResult;
              try {
                const joinOptions: Parameters<typeof roomManager.joinRoom>[5] = {
                    voiceEnforcementSessionGeneration: exactSession.sessionGeneration,
                    entitlement: {
                      tier: access.userTier,
                      allowedAudioTiers: access.allowedAudioTiers,
                      minPtimeMs: access.minPtimeMs,
                      maxManualBitrateBps: access.maxManualBitrateBps,
                      cameraMaxBitrateBps: access.cameraMaxBitrateBps,
                      screenMaxBitrateBps: access.screenMaxBitrateBps,
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
                    // A missing channel field fails closed to an empty bitfield.
                    permissions: roomKind === 'channel' ? (access.permissions ?? 0n) : undefined,
                    admissionId,
                    authorizationRevision: access.authorizationRevision,
                    credentialEpoch: data.credentialEpoch,
                    deferChannelPromotion: roomKind === 'channel',
                };
                // bind, not a closure: keeps the handler within the nesting limit.
                result = await voiceAuthConnections.trackRoomOperation(exactSession, roomManager.joinRoom.bind(
                  roomManager, roomId, data.userId, socket.id, identity, rtpCapabilities, joinOptions,
                ));
              } catch (error) {
                if (error instanceof RoomCreationRefusedError) placementRefused = true;
                await voiceAuthConnections.releaseRoom(exactSession);
                throw error;
              }

              // A2 must see an enforcement-addressable exact socket before it
              // can promote it. Failure is an admission failure: runSocketBoundJoin
              // rolls this provisional candidate back without mutating the room.
              try {
                await registerVoiceEnforcementSession(token, exactSession);
              } catch (error) {
                await voiceAuthConnections.releaseRoom(exactSession);
                throw error;
              }
              data.voiceEnforcementSession = exactSession;

              return {
                authorizedCallId: access.callId,
                identity,
                promotion: undefined as
                  ChannelParticipantPromotion | DMParticipantPromotion | undefined,
                result,
              };
            },
            // Reauthorize only after the exact session is visible to enforcement.
            reauthorize: (access) =>
              reauthorizeAdmission(roomKind, access, inputCallId, authorizeRoom),
            finalize: async (access, value) => {
              // The second authorization is authoritative for both channel and
              // Private Call moderator state. Apply it before socket.join/ack.
              const identity = resolveParticipantIdentity(access, {
                username: data.username,
                displayName: data.displayName,
                avatarUrl: data.avatarUrl,
              });
              value.identity = identity;
              if (roomKind === 'dm') {
                value.promotion = {
                  callId: access.callId,
                  authorizationRevision: access.authorizationRevision,
                  identity,
                  entitlement: {
                    tier: access.userTier,
                    allowedAudioTiers: access.allowedAudioTiers,
                    minPtimeMs: access.minPtimeMs,
                    maxManualBitrateBps: access.maxManualBitrateBps,
                    cameraMaxBitrateBps: access.cameraMaxBitrateBps,
                    screenMaxBitrateBps: access.screenMaxBitrateBps,
                  },
                  serverMuted: access.serverMuted,
                  serverDeafened: access.serverDeafened,
                };
                return;
              }
              value.promotion = {
                admissionId,
                authorizationRevision: access.authorizationRevision,
                identity,
                entitlement: {
                  tier: access.userTier,
                  allowedAudioTiers: access.allowedAudioTiers,
                  minPtimeMs: access.minPtimeMs,
                  maxManualBitrateBps: access.maxManualBitrateBps,
                  cameraMaxBitrateBps: access.cameraMaxBitrateBps,
                  screenMaxBitrateBps: access.screenMaxBitrateBps,
                },
                ownerTier: access.roomOwnerTier ?? 'free',
                permissions: access.permissions ?? 0n,
                serverMuted: access.serverMuted,
                serverDeafened: access.serverDeafened,
              };
            },
            commit: (_access, value) => {
              const committed = roomKind === 'channel'
                ? commitChannelAdmission(roomManager, socket, data, roomId, value, commitSocketMembership)
                : commitDMAdmission(roomManager, socket, data, roomId, value, commitSocketMembership);
              // Later broadcast/ack failures cannot roll back a successful
              // synchronous promotion or free its still-live identity slot.
              admissionCommitted = true;
              return committed;
            },
            rollback: async (access) => {
              const exactSession = enforcementSession;
              if (placementRefused || !exactSession) return;
              await rollbackRegisteredRoomJoin({
                roomManager,
                cleanupDMJoin,
                roomId,
                userId: data.userId,
                socketId: socket.id,
                sessionGeneration: exactSession.sessionGeneration,
                roomKind,
                callId: access.callId,
              });
              if (enforcementSession) {
                try {
                  await voiceAuthConnections.releaseRoom(enforcementSession);
                } finally {
                  if (data.voiceEnforcementSession === enforcementSession) {
                    data.voiceEnforcementSession = undefined;
                  }
                }
              }
            },
          });

          if (outcome.status === 'denied' || outcome.status === 'revoked') {
            observe({
              eventType: 'media_authorization',
              outcome: 'denied',
              severity: 'high',
              reasonCode:
                outcome.status === 'revoked' ? 'authorization_revoked' : 'authorization_denied',
              routeTemplate: 'socket.join',
            });
            if (outcome.status === 'denied') {
              await cleanupDMJoinSafely('Failed to clean up a queued DM room join');
            }
            logger.warn('Channel access denied', {
              userId: data.userId,
              roomId,
              error: outcome.access.error,
            });
            const error = { error: outcome.access.error || 'Access denied' };
            if (!callback) {
              socket.emit('error', error);
              return;
            }
            callback(error);
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
          const identityReasonCode = getIdentityAuthorityReasonCode(
            access.authIdentityPresent,
            identity,
            data
          );
          if (identityReasonCode) {
            observe({
              eventType: 'media_authorization',
              outcome: 'denied',
              severity: 'medium',
              reasonCode: identityReasonCode,
              routeTemplate: 'socket.join',
            });
          }
          const joinedParticipant = result.participants.find(
            (participant) => participant.userId === data.userId
          );
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
            cameraSpatialCap: result.cameraSpatialCap,
            cameraMaxBitrateBps: result.cameraMaxBitrateBps,
            screenMaxBitrateBps: result.screenMaxBitrateBps,
            allowedAudioTiers: result.allowedAudioTiers,
            minPtimeMs: result.minPtimeMs,
            // This is the A2 channel standard that granted the admitted audio
            // caps above. Null explicitly clears a standard seen by an earlier
            // desktop REST join when the channel changed before promotion.
            channelAudioUpliftTier: access.channelAudioUpliftTier ?? null,
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

          if (!callback) {
            socket.emit('room-joined', response);
            return;
          }
          callback(response);
        } catch (error) {
          if (!placementRefused) {
            await cleanupDMJoinSafely('Failed to clean up an errored DM room join');
          }
          emitJoinError(socket, roomId, data.userId, error, callback);
        }
      };

      try {
        const exactSession: VoiceEnforcementSession = {
          connectionGeneration: voiceAuthConnections.snapshot(connection).target.connectionGeneration,
          sessionGeneration: randomUUID(),
          nodeBootId: voiceEnforcementNodeBootId,
          roomId,
          roomKind,
          userId: data.userId,
          credentialEpoch: data.credentialEpoch,
          socketId: socket.id,
        };
        // Reserve atomically before even waiting for the admission fence. A1
        // has durable effects, so a count check or post-A1 reservation is late.
        roomManager.reserveVoiceAuthRoom(connection, exactSession);
        // Only a successfully reserved owner is eligible for rollback/release.
        enforcementSession = exactSession;
        if (roomKind === 'dm') await withKeyedJoinFence(dmJoinFence, roomId, executeJoin);
        else {
          // Serialize A1 through rollback for one channel user across sockets.
          // A second session must not replace or roll back the first candidate
          // while its A2 authorization is still in flight.
          await withKeyedJoinFence(
            channelAdmissionFence,
            JSON.stringify([roomId, data.userId]),
            executeJoin
          );
        }
      } catch (error) {
        emitJoinError(socket, roomId, data.userId, error, callback);
      } finally {
        try {
          if (enforcementSession && !admissionCommitted) {
            await voiceAuthConnections.releaseRoom(enforcementSession);
            if (data.voiceEnforcementSession === enforcementSession) data.voiceEnforcementSession = undefined;
          }
        } finally {
          releaseSocketRoomClaim();
        }
      }
    }
  );
}

async function main() {
  // Create Express app
  const app = express();
  const httpServer = createServer(app);
  const securityEvents = new SecurityEventWriter({ enabled: config.environment === 'production' });

  // Middleware
  app.use(express.json());

  // Initialize mediasoup
  const mediasoupService = new MediasoupService();
  mediasoupService.setSecurityEventEmitter(
    (event) => securityEvents.emit(event),
    () => securityEvents.flush()
  );
  await mediasoupService.init();

  logger.info('Mediasoup initialized', {
    workers: mediasoupService.getWorkerCount(),
  });

  // Media dies silently when loopback is advertised on a multi-interface host
  // (see announcedIpAdvisory). Nothing downstream can detect it, so say it here.
  const ipAdvisory = announcedIpAdvisory(
    process.env.ANNOUNCED_IP,
    Object.values(networkInterfaces())
      .flat()
      .filter((i) => i !== undefined)
  );
  if (ipAdvisory) logger.warn(ipAdvisory);

  // Initialize NATS (inter-service messaging)
  const natsService = new NatsService();
  const voiceEnforcementLease = new VoiceEnforcementLease();
  let voiceEnforcementSubscriptionReady = false;
  let shuttingDown = false;
  const voiceEnforcementSubject = `voice.enforce.session.${voiceEnforcementNodeBootId}`;
  natsService.setSecurityEventEmitter((event) => securityEvents.emit(event));
  await natsService.connect();

  // Initialize Redis (room state)
  const redisService = new RedisService();
  redisService.setSecurityEventEmitter((event) => securityEvents.emit(event));
  try {
    await redisService.connect();
  } catch {
    if (config.redisCredentialMode === 'managed') {
      throw new Error('managed_redis_cold_start_failed');
    }
    logger.warn('Redis connection failed — running without persistent room state');
  }
  const mediaInstanceIdentity = config.mediaInstanceColor
    ? await redisService.initializeMediaInstance(config.mediaInstanceColor)
    : undefined;
  if (mediaInstanceIdentity) natsService.setMediaInstanceIdentity(mediaInstanceIdentity);

  // Initialize RoomManager
  // #1553 measurement counters — accumulated from heartbeat samples, surfaced on /health.
  // Constructed BEFORE RoomManager so the #3104 ICE counters can be injected into it.
  const mediaMetrics = new MediaMetrics();
  const roomManager = new RoomManager(mediasoupService, {
    onIceSelected: (protocol) => mediaMetrics.incrementIceSelected(protocol),
    onIceTerminalWithoutConnect: () => mediaMetrics.incrementIceTerminalWithoutConnect(),
    onCameraLayeringGateFlip: () => mediaMetrics.incrementCameraLayeringGateFlip(),
    onCameraPressureDemand: () => mediaMetrics.incrementCameraPressureDemand(),
  });
  roomManager.setSecurityEventEmitter((event) => securityEvents.emit(event));
  if (config.roomPlacementEnabled) {
    roomManager.setRoomCreationGuard((roomId) => redisService.guardRoomCreation(roomId));
  }

  // Worker placement reads live consumer counts from RoomManager (#3149).
  // Installed here rather than injected: mediasoupService is constructed
  // above, before roomManager exists. The arrow defers the reference.
  mediasoupService.setRoomConsumerCounts(() => roomManager.getRoomConsumerCounts());
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
    // A tier-violation carries no room, by design (#1308).
    logger.debug('Room event', {
      type: event.type,
      roomId: 'roomId' in event ? event.roomId : undefined,
    });
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
    const placement = redisService.healthState();
    res.json({
      status: 'healthy',
      service: 'media-plane',
      colour: placement.color,
      accepting_new_rooms: placement.acceptingNewRooms,
      rooms: roomManager.getRoomIds().length,
      // Reported on every profile: the legacy-conversion actuator needs it
      // where /readyz omits the placement body.
      creates_in_flight: roomManager.getCreatesInFlightCount(),
      owner_refusals_total: placement.ownerRefusalsTotal,
      workers: mediasoupService.getWorkerCount(),
      activeRooms: stats.activeRooms,
      totalParticipants: stats.totalParticipants,
      totalProducers: stats.totalProducers,
      totalConsumers: stats.totalConsumers,
      // #1553 measurement counters (aggregate-only; no PII/IDs/keys).
      metrics: mediaMetrics.getSnapshot(),
    });
  });
  app.get('/readyz', async (_req, res) => {
    const acceptingNewRooms = await redisService.refreshAcceptingNewRooms();
    const placement = redisService.healthState();
    const rooms = roomManager.getRoomIds().length;
    const createsInFlight = roomManager.getCreatesInFlightCount();
    const placementBody = config.roomPlacementEnabled
      ? {
          colour: placement.color,
          generation: placement.generation,
          accepting_new_rooms: acceptingNewRooms,
          rooms,
          creates_in_flight: createsInFlight,
        }
      : {};
    const placementReady =
      !config.roomPlacementEnabled ||
      (acceptingNewRooms &&
        placement.generation !== null &&
        mediasoupService.getWorkerCount() >= config.mediasoup.initialWorkers &&
        !(rooms === 0 && !placement.redisReachable));
    const managedRedisReady = config.redisCredentialMode !== 'managed' || placement.redisReachable;
    if (
      shuttingDown ||
      !managedRedisReady ||
      !placementReady ||
      !mediasoupService.isReady() ||
      voiceEnforcementReleaseQueue.saturated() ||
      !enforcementAvailable() ||
      !voiceEnforcementLease.valid() ||
      !natsService.voiceCommandReady() ||
      !voiceEnforcementSubscriptionReady ||
      !natsService.isRequestSubscriptionHealthy(voiceEnforcementSubject)
    ) {
      res.status(503).json({
        ready: false,
        ...voiceEnforcementHealth.status(),
        ...placementBody,
      });
      return;
    }
    res.json({
      ready: true,
      ...voiceEnforcementHealth.status(),
      ...placementBody,
    });
  });

  // Express error handler — surface uncaught route errors via Winston with a
  // canonical 500 response. Per-event Socket.IO errors (post-upgrade) are
  // handled in each socket-event try/catch and logged via logger.error.
  app.use(createExpressErrorHandler(logger));

  const socketAdmissionGate = createAdmissionGate({
    trustedProxies: config.trustedProxies,
    onReject: () => {
      mediaMetrics.incrementAdmissionRejected();
      securityEvents.emit({
        eventType: 'media_admission',
        outcome: 'denied',
        severity: 'medium',
        reasonCode: 'admission_rejected',
        routeTemplate: 'socket.join',
      });
    },
    // Startup-only configuration warnings (empty/invalid TRUSTED_PROXIES).
    warn: (message) => {
      logger.warn(message);
      if (message.startsWith('Admission gate INACTIVE:')) {
        securityEvents.emit({
          eventType: 'security_control',
          outcome: 'degraded',
          severity: 'high',
          reasonCode: 'admission_gate_inactive',
        });
      }
    },
  });
  // Initialize Socket.IO. Availability composes with the existing rate gate.
  const io = new SocketIOServer(httpServer, {
    cors: {
      origin: createOriginGate(config.allowedOrigins, (event) => securityEvents.emit(event)),
      credentials: true,
    },
    allowRequest: (request, callback) => {
      if (!enforcementAvailable()) {
        callback('Voice enforcement unavailable', false);
        return;
      }
      socketAdmissionGate(request, callback);
    },
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
  const voiceAuthConnections = new VoiceAuthConnections({
    nodeBootId: voiceEnforcementNodeBootId, secret: config.jwtSecret,
    controlPlaneUrl: config.controlPlaneUrl,
  });
  roomManager.setVoiceAuthConnections(voiceAuthConnections);
  setVoiceAuthConnections(voiceAuthConnections);
  const voiceEnforcementReleaseQueue = new VoiceEnforcementSessionReleaseQueue(
    (session) =>
      roomManager.awaitNativeTermination(
        session.roomId,
        session.userId,
        session.socketId,
        session.sessionGeneration
      ),
    (capacity, overflowedCount) => {
      logger.error('Voice enforcement release queue saturated; operator recovery required', {
        capacity,
        overflowedCount,
      });
    },
    voiceAuthConnections
  );
  const voiceEnforcementExpiryFence = new VoiceEnforcementExpiryFence(
    voiceEnforcementLease,
    async () => {
      // Capture only legacy liabilities before disconnect callbacks remove public
      // references. Every family room/idle/provisional connection stays private.
      const legacy = snapshotVoiceEnforcementSessions(io.sockets.sockets.values(), voiceAuthConnections);
      voiceEnforcementReleaseQueue.holdAll(legacy);
      const owned = voiceAuthConnections.terminateAll();
      for (const socket of io.sockets.sockets.values()) {
        socket.data.voiceEnforcementSession = undefined;
        socket.disconnect(true);
      }
      await Promise.all([owned, roomManager.closeAll()]);
    },
    () => voiceAuthConnections.reservationGeneration()
  );

  let availabilityTeardownEpisode: object | undefined;
  const enforcementAvailability = new EnforcementAvailability(async () => {
    if (!availabilityTeardownEpisode) {
      availabilityTeardownEpisode = Object.freeze({});
      voiceEnforcementExpiryFence.invalidate();
    }
    const episode = availabilityTeardownEpisode;
    voiceEnforcementLease.expire();
    await voiceEnforcementExpiryFence.enforce();
    if (availabilityTeardownEpisode === episode) availabilityTeardownEpisode = undefined;
  });
  const stopObservingRequiredSubscriptions = natsService.observeRequiredSubscriptions((ready) => {
    enforcementAvailability.setAvailable(ready);
  });
  function enforcementAvailable(): boolean {
    return (
      mediasoupService.isReady() &&
      natsService.requiredSubscriptionsReady() &&
      enforcementAvailability.isReady() &&
      voiceEnforcementLease.valid() &&
      natsService.voiceCommandReady() &&
      voiceEnforcementExpiryFence.ready() &&
      !shuttingDown
    );
  }

  const enforceVoiceCommandQuarantine = async (): Promise<void> => {
    voiceEnforcementLease.expire();
    await voiceEnforcementExpiryFence.enforce();
  };
  let lastCommandQuarantineEpisode: object | undefined;
  const quarantineVoiceCommands = async (_reason: string, episode: object): Promise<void> => {
    if (lastCommandQuarantineEpisode !== episode) {
      lastCommandQuarantineEpisode = episode;
      voiceEnforcementExpiryFence.invalidate();
    }
    await enforceVoiceCommandQuarantine();
  };
  const voiceCommandOwner = await natsService.createVoiceCommandOwner({
    nodeBootId: voiceEnforcementNodeBootId,
    onInvalidated: () => {
      voiceEnforcementExpiryFence.invalidate();
      voiceEnforcementLease.expire();
      if (voiceEnforcementExpiryFence.pending()) return;
      void voiceEnforcementExpiryFence.enforce().catch(() => {
        logger.error('Voice command authority quarantine failed', {
          error: 'voice_command_quarantine_failed',
        });
      });
    },
  });
  roomManager.setNativeTerminationFailureHandler(() => {
    voiceEnforcementExpiryFence.invalidate();
    voiceEnforcementLease.expire();
    voiceCommandOwner.invalidate();
  });
  const legacyVoiceCommands = new WeakMap<Record<string, unknown>, () => Promise<void>>();
  const voiceCommandDispatcher = new VoiceCommandDispatcher({
    roomManager,
    io,
    nodeBootId: voiceEnforcementNodeBootId,
    streamGeneration: voiceCommandOwner.streamGeneration,
    ensureAuthority: async () => {
      natsService.requireVoiceCommandAuthority();
      const ownerEpoch = voiceCommandOwner.authorityEpoch();
      try {
        await natsService.confirmVoiceCommandContinuity();
        natsService.requireVoiceCommandAuthority();
        if (voiceCommandOwner.authorityEpoch() !== ownerEpoch)
          throw new Error('Voice command authority unavailable');
      } catch {
        voiceCommandOwner.invalidate();
        await enforceVoiceCommandQuarantine();
        throw new Error('Voice command authority unavailable');
      }
    },
    handleLegacy: async (data) => {
      const handler = legacyVoiceCommands.get(data);
      if (!handler) throw new Error('Voice command compatibility handler unavailable');
      await handler();
    },
    emitSecurityEvent: (event) => securityEvents.emit(event),
  });
  roomManager.setEnforcementAuthorityGate(() => natsService.requireVoiceCommandAuthority());
  const subscribeVoiceCompatibility = (
    subject: string,
    handler: (data: Record<string, unknown>) => Promise<void>
  ): void => {
    natsService.subscribe(
      subject,
      async (data) => {
        legacyVoiceCommands.set(data, () => handler(data));
        try {
          await voiceCommandDispatcher.handleCompatibility(data);
        } finally {
          legacyVoiceCommands.delete(data);
        }
      },
      { required: true }
    );
  };

  // Socket.IO JWT authentication middleware (A2)
  const requireEnforcementAvailability = (_socket: Socket, next: (error?: Error) => void): void => {
    next(enforcementAvailable() ? undefined : new Error('Voice enforcement unavailable'));
  };
  io.use(requireEnforcementAvailability);
  io.use(createAuthMiddleware((event) => securityEvents.emit(event)));
  // Registration installs its private owner before await and joins failed middleware cleanup.
  io.use(createVoiceAuthConnectionMiddleware(voiceAuthConnections, enforcementAvailable));

  // ── Media-rate policer (#2153) ────────────────────────────────────────
  // Installed before the first socket can connect, so no join or produce is
  // ever admitted without the cooldown gate. One monotonic clock feeds the
  // RoomManager stamps, the gate and the tick. Every decision lives in
  // lib/mediaPolicer.ts, lib/mediaPolicerTick.ts and RoomManager.
  const mediaPolicyClock = (): number => performance.now();
  const mediaPolicer = new MediaPolicer();
  const mediaPolicyLedger = new MediaPolicyLedger();
  roomManager.setMonotonicClock(mediaPolicyClock);
  roomManager.setMediaPolicyGate(mediaPolicyLedger);
  roomManager.setIngressFinalizer((sample) => mediaPolicer.recordFinal(sample));
  const mediaPolicerTick = createMediaPolicerTick({
    roomManager,
    io,
    policer: mediaPolicer,
    ledger: mediaPolicyLedger,
    emit: (event) => securityEvents.emit(event),
    now: mediaPolicyClock,
  });
  mediaPolicerTick.start();

  // ── NATS subscriptions for enforcement commands from control plane ────

  // NATS dispatches every subscription independently. Keep mute and deafen
  // updates for one participant ordered without blocking other participants.
  const serverEnforcementChains = new Map<string, Promise<void>>();

  /** Creates a NATS handler for server-level toggle enforcement (mute/deafen). */
  function createServerEnforcementHandler(
    subject: string,
    applyAction: string,
    removeAction: string,
    applyFn: (roomId: string, userId: string) => void | Promise<void>,
    removeFn: (roomId: string, userId: string) => boolean | Promise<boolean>,
    eventName: string,
    eventField: string
  ) {
    subscribeVoiceCompatibility(subject, async (natsData) => {
      let cmd: { channelId?: string; userId?: string; action?: string } = {};
      try {
        await handleNatsEnforcementCommand(
          natsData,
          [applyAction, removeAction],
          async ({ channelId, userId, action }) => {
            cmd = { channelId, userId, action };
            await applyServerEnforcementCommand({
              serverEnforcementChains,
              natsData,
              roomManager,
              io,
              subject,
              channelId,
              userId,
              action,
              applyAction,
              removeAction,
              applyFn,
              removeFn,
              eventName,
              eventField,
            });
          },
          (event) => securityEvents.emit(event)
        );
      } catch (err) {
        logger.error(`Failed to handle ${subject}`, { error: err, ...cmd });
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
    subscribeVoiceCompatibility(subject, async (natsData) => {
      let cmd: { channelId?: string; userId?: string } = {};
      try {
        await handleNatsEnforcementCommand(
          natsData,
          undefined,
          async ({ channelId, userId }) => {
            cmd = { channelId, userId };
            await applyFn(channelId, userId);
            const participant = roomManager.getParticipant(channelId, userId);
            if (participant) {
              for (const [producerId, entry] of participant.producers) {
                if (entry.kind === 'audio') {
                  io.to(channelId).emit('producer-paused', {
                    producerId,
                    userId,
                    kind: entry.kind,
                    source: entry.source,
                  });
                }
              }
            }
          },
          (event) => securityEvents.emit(event)
        );
      } catch (err) {
        logger.error(`Failed to handle ${subject}`, { error: err, ...cmd });
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
  subscribeVoiceCompatibility('voice.enforce.disconnect', async (natsData) => {
    let cmd: { channelId?: string; userId?: string } = {};
    try {
      await handleNatsEnforcementCommand(
        natsData,
        undefined,
        ({ channelId, userId, socketId, admissionId, callId }) => {
          cmd = { channelId, userId };
          return handleForceDisconnect(
            roomManager,
            io,
            channelId,
            userId,
            (event) => securityEvents.emit(event),
            { reason: 'access_revoked', socketId, admissionId, callId }
          );
        },
        (event) => securityEvents.emit(event)
      );
    } catch (err) {
      logger.error('Failed to handle voice.enforce.disconnect', { error: err, ...cmd });
    }
  });

  // Durable authority outboxes use request/reply: a signed acknowledgement is
  // issued only after the existing RoomManager teardown has completed.
  natsService.subscribeRequest(
    'voice.enforce.disconnect.ack',
    createDMBlockDisconnectAckHandler(roomManager, io, config.jwtSecret),
    { required: true }
  );
  natsService.subscribeRequest(
    'voice.enforce.credential_epoch.ack',
    createCredentialEpochEjectionAckHandler(roomManager, io, config.jwtSecret),
    { required: true }
  );
  voiceEnforcementSubscriptionReady = natsService.subscribeRequest(
    voiceEnforcementSubject,
    createVoiceEnforcementSessionEjectionHandler(
      roomManager,
      io,
      config.jwtSecret,
      voiceEnforcementNodeBootId,
      async () => {
        // Legacy proof may help drain legacy obligations, never renew protocol4.
        voiceEnforcementReleaseQueue.promoteHeld();
        void voiceEnforcementReleaseQueue.retryPending();
      },
      {
        fenceAndJoin: (command) => voiceAuthConnections.fenceAndJoin(command),
        healthSnapshot: (command) => voiceEnforcementHealth.healthSnapshot(command),
      }
    ),
    { required: true }
  );
  voiceEnforcementSubscriptionReady =
    voiceEnforcementSubscriptionReady &&
    natsService.isRequestSubscriptionHealthy(voiceEnforcementSubject);

  const enforceVoiceEnforcementLease = (): Promise<void> => {
    if (shuttingDown) return Promise.resolve();
    return voiceEnforcementExpiryFence.enforce();
  };
  const leaseEnforcementFailures = new FailureStateLogger('Voice enforcement lease teardown failed; will retry');
  const enforceLeaseObserved = (): void => {
    enforceVoiceEnforcementLease().then(
      () => leaseEnforcementFailures.succeeded(),
      () => leaseEnforcementFailures.failed()
    );
  };
  const voiceEnforcementHealth = new VoiceEnforcementHealth({
    lease: voiceEnforcementLease,
    nodeBootId: voiceEnforcementNodeBootId,
    transportReady: () => natsService.exactVoiceTransportReady(voiceEnforcementSubject),
    localReady: () => !shuttingDown && mediasoupService.isReady() &&
      enforcementAvailability.isReady() && voiceEnforcementExpiryFence.ready() &&
      !voiceAuthConnections.hasStalledTermination() && natsService.voiceCommandReady(),
    poll: () => pollVoiceEnforcementHealth(voiceEnforcementNodeBootId),
    recover: async () => {
      if (!shuttingDown) await natsService.confirmRequiredSubscriptions();
      await enforcementAvailability.awaitTeardown();
      await enforceVoiceEnforcementLease();
      if (!shuttingDown && !natsService.voiceCommandReady()) {
        await natsService.recoverVoiceCommandConsumerAfterTeardown();
      }
      await natsService.confirmVoiceCommandContinuity();
    },
    onTransport: () => {
      voiceEnforcementReleaseQueue.promoteHeld();
      void voiceEnforcementReleaseQueue.retryPending();
    },
  });
  const healthPoll = async () => {
    await voiceEnforcementHealth.poll();
    // Never join a hung teardown into the poll lane; the fence retains ONE task.
    if (!voiceEnforcementExpiryFence.pending()) enforceLeaseObserved();
  };
  void healthPoll();
  const voiceEnforcementHealthInterval = setInterval(() => {
    void healthPoll();
  }, 10_000);
  const voiceEnforcementLeaseInterval = setInterval(() => {
    // The fence keeps ownership; avoid adding one reaction per tick while hung.
    if (!voiceEnforcementExpiryFence.pending()) enforceLeaseObserved();
  }, 1_000);

  natsService.subscribeRequest(
    'voice.admission.activate',
    createChannelAdmissionActivationHandler(
      roomManager,
      config.jwtSecret,
      (socketId) => io.sockets.sockets.get(socketId)?.connected === true
    ),
    { required: true }
  );

  // ── Mid-session permission push (CV-CAN-007 review P1) ────────────────
  // Control plane publishes voice.enforce.permissions {channelId, userId,
  // permissions} after an RBAC mutation touching a voice-connected member. The
  // bitfield rides the same decimal-string wire format as join-authorize and
  // goes through the same strict fail-closed parser; a malformed payload is
  // ignored (the join-time snapshot stays — enforcement never fails open, and
  // a bad message never strips a legitimate peer).
  natsService.subscribe(
    'voice.enforce.permissions',
    async (natsData) => {
      try {
        await handleEnforcePermissionsMessage(roomManager, io, natsData, (event) =>
          securityEvents.emit(event)
        );
      } catch (err) {
        logger.error('Failed to handle voice.enforce.permissions', { error: err });
      }
    },
    { required: true }
  );

  // ─── Socket.IO connection handling ───────────────────────────────────

  // ponytail: process-local serialization matches single-node room ownership;
  // use a distributed admission ID before one DM room can span media nodes.
  const dmJoinFence = new KeyedJoinFence();
  const channelAdmissionFence = new KeyedJoinFence();

  io.on('connection', (socket) => {
    const data = socket.data as AuthenticatedSocketData;
    let correlationRef = 'correlation_ref_missing';
    try {
      correlationRef = randomUUID().replaceAll('-', '');
    } catch {
      // The best-effort observer must not perturb an accepted voice socket.
    }
    const emitSocketSecurityEvent: EmitSecurityEvent = (event) => {
      try {
        return securityEvents.emit({ ...event, correlationRef });
      } catch {
        return false;
      }
    };
    const socketRateLimitDeps: RateLimitDeps = {
      onReject: (event, userId) => {
        emitSocketSecurityEvent({
          eventType: 'media_admission',
          outcome: 'denied',
          severity: 'medium',
          reasonCode: 'socket_rate_limited',
        });
        logRateLimitRejection(event, userId);
      },
      onHandlerError: (event, userId) => {
        emitSocketSecurityEvent({
          eventType: 'media_integrity',
          outcome: 'failure',
          severity: 'medium',
          reasonCode: 'socket_handler_failed',
        });
        logSocketHandlerFailure(event, userId);
      },
    };
    const withRateLimit = <H extends SocketListener>(
      limitedSocket: RateLimitedSocket,
      event: RoomEventName,
      handler: H
    ): void => registerRateLimited(limitedSocket, event, handler, socketRateLimitDeps);
    withRateLimit(
      socket,
      'refresh-auth',
      createSocketTokenRenewal(socket, async (token) => {
        const session = data.voiceEnforcementSession;
        if (!session || !data.roomId) throw new Error('No admitted session');
        // Registration is idempotent and reuses live bearer, epoch and DM gates.
        const sessionIsCurrent = () => {
          const participant = roomManager.getParticipant(session.roomId, session.userId);
          return (
            socket.connected &&
            data.voiceEnforcementSession === session &&
            data.roomId === session.roomId &&
            participant?.socketId === socket.id &&
            participant.voiceEnforcementSessionGeneration === session.sessionGeneration
          );
        };
        await renewVoiceEnforcementSession(token, session, sessionIsCurrent, (staleSession) =>
          voiceEnforcementReleaseQueue.releaseAfterTerminalClose(staleSession)
        );
      })
    );
    logger.info('Client connected', {
      socketId: socket.id,
      userId: data.userId,
      username: data.username,
    });

    // ── join-room ────────────────────────────────────────────────────
    registerJoinRoomHandler(socket, data, {
      roomManager,
      dmJoinFence,
      channelAdmissionFence,
      voiceAuthConnections,
      emitSecurityEvent: emitSocketSecurityEvent,
      withRateLimit,
      isShuttingDown: () =>
        shuttingDown ||
        !voiceEnforcementLease.valid() ||
        !natsService.voiceCommandReady() ||
        !enforcementAvailable(),
    });

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
    // Client sends: { transportId, kind, rtpParameters,
    //   appData: { source, videoBitrateProfileVersion? } }
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
          // Older desktop video simulcast encodings can exceed today's aggregate
          // cap. Refuse those clients before a producer exists; audio and receiving
          // remain available. This marker is only a rollout compatibility check.
          requireCompatibleVideoPublishProfile(kind, appData);
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
          if (error instanceof VideoClientUpdateRequiredError) {
            return callback({ error: error.message, code: error.code });
          }
          const message = error instanceof Error ? error.message : 'Failed to produce';
          // #2153: a cooldown refusal is already logged (warn) where RoomManager decided it.
          if (!(
            error instanceof MediaPolicyCooldownError || error instanceof VideoPolicyCooldownError
          )) {
            logger.error('Error producing', {
              error: message,
              userId: data.userId,
            });
          }
          // Surface limit errors to client (e.g. "Video participant limit reached (max 25)"),
          // and a media-policy cooldown with its retry-after (#2153).
          if (error instanceof MediaPolicyCooldownError) callback(mediaPolicyCooldownAck(error));
          else if (error instanceof VideoPolicyCooldownError)
            callback(videoPolicyCooldownAck(error));
          else callback({ error: message });
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

        const { kind, source } = await roomManager.pauseProducer(roomId, data.userId, producerId);

        // Notify others
        socket.to(roomId).emit('producer-paused', {
          producerId,
          userId: data.userId,
          kind,
          source,
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
        const producerEntry = participant?.producers.get(producerId);
        if (participant?.serverMuted && producerEntry?.kind === 'audio') {
          if (callback)
            return callback({ error: 'server_muted', message: 'Server-muted by moderator' });
          return;
        }

        // #2153: a policer-latched producer stays paused. RoomManager refuses
        // before its await and re-checks after it, so no producer-resumed follows.
        const outcome = await roomManager.resumeProducer(roomId, data.userId, producerId);
        if (outcome === 'media_policy_paused') {
          if (callback) return callback(MEDIA_POLICY_PAUSED_RESUME_ACK);
          return;
        }

        // Notify others. resumeProducer throws for an unknown producer, so the
        // entry read above is the one it just resumed.
        if (producerEntry) {
          socket.to(roomId).emit('producer-resumed', {
            producerId,
            userId: data.userId,
            kind: producerEntry.kind,
            source: producerEntry.source,
          });
        }

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

        // #2153: the client-close path takes the final ingress sample first.
        const source = await roomManager.closeProducerFromClient(roomId, data.userId, producerId);

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
    const enforcementSession = socketData.voiceEnforcementSession;

    if (!roomId || !userId) return;
    // Start the retained family-aware task once, fencing before any cleanup
    // await. Its native callback owns exact RoomManager teardown.
    const familyRelease = enforcementSession?.connectionGeneration
      ? voiceAuthConnections.releaseRoom(enforcementSession) : undefined;

    const releaseEnforcementSession = async () => {
      const session = enforcementSession;
      if (!session) return;
      try {
        if (familyRelease) await familyRelease;
        else await voiceEnforcementReleaseQueue.releaseAfterTerminalClose(session);
      } finally {
        // A failed release keeps the DB row for durable retry, but it must not
        // strand this locally departed socket in a pseudo-admitted state.
        if (socketData.voiceEnforcementSession === session) {
          socketData.voiceEnforcementSession = undefined;
        }
      }
    };

    // Guard: only clean up if this socket still owns the participant.
    // Race condition: a newer socket may have already re-joined the room
    // (e.g. old socket disconnect fires AFTER new socket's join-room).
    // Without this check, the old socket's cleanup would remove the
    // participant that the new socket just added.
    const participant = roomManager.getParticipant(roomId, userId);
    if (
      participant &&
      (participant.socketId !== socket.id ||
        (enforcementSession &&
          participant.voiceEnforcementSessionGeneration !== enforcementSession.sessionGeneration))
    ) {
      logger.info('Skipping cleanup — participant reconnected on a newer socket', {
        roomId,
        userId,
        oldSocketId: socket.id,
        newSocketId: participant.socketId,
      });
      socketData.roomId = undefined;
      socket.leave(roomId);
      // The old exact socket is no longer in the authoritative participant
      // map, so it cannot forward media. Release only after that fact is known.
      await releaseEnforcementSession();
      return;
    }

    if (familyRelease) await familyRelease;
    else {
      await roomManager.leaveRoomIfSocketOwned(
        roomId, userId, socket.id, enforcementSession?.sessionGeneration
      );
    }
    try {
      await releaseEnforcementSession();
    } finally {
      // Recompute and broadcast codec floor if room still exists
      if (roomManager.getRoom(roomId)) {
        const codecFloor = roomManager.computeCodecFloor(roomId);
        io.to(roomId).emit('room-codec-floor', { codecFloor });
      }

      // Always leave local state even when the durable release is retryable.
      socket.leave(roomId);
      socketData.roomId = undefined;
    }
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
      // Routing lives in lib/layeringGateBroadcast so it is reachable by a test:
      // this file is excluded from coverage, and the RoomManager tests can only
      // see the emitter's half of the contract.
      emitCameraLayeringGate(io, event);
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
    const acceptingNewRooms = await redisService.refreshAcceptingNewRooms();
    if (config.roomPlacementEnabled && !acceptingNewRooms) {
      await mediasoupService.demoteIdleWorkersToInitial();
    }
    const roomIds = roomManager.getRoomIds();
    await redisService.heartbeatRoomOccupancy(roomIds);
    for (const roomId of roomIds) {
      const room = roomManager.getRoom(roomId);
      if (!room) continue;
      natsService.publishRoomHeartbeat({
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

  // Confirm every core/request enforcement subscriber before acquiring the
  // durable owner and binding HTTP. Partial installation is a startup failure.
  await natsService.confirmRequiredSubscriptions();
  // Ownership and the required durable consumer are established before listen.
  await natsService.startVoiceCommandConsumer({
    owner: voiceCommandOwner,
    dispatcher: voiceCommandDispatcher,
    quarantine: quarantineVoiceCommands,
  });

  // Start server
  const port = config.port;
  httpServer.listen(port, () => {
    logger.info('Media Plane server started', {
      port,
      environment: config.environment,
    });

    // #3105 / ADR-0040. An env gate cannot see a firewall, so this converts an
    // invisible misconfiguration into a visible one and does nothing more.
    if (config.mediasoup.webRtcTransport.enableTcp) {
      logger.warn(
        'MEDIASOUP_ENABLE_TCP is set — mediasoup will bind ICE-TCP listeners ' +
          'and advertise TCP candidates. This process ' +
          'CANNOT verify that those ports are reachable. Publish the matching media ports ' +
          'for TCP in compose AND allow it on both firewall surfaces (ufw and ' +
          'the DOCKER-USER chain), or every client will burn ICE connectivity ' +
          'checks on a black-holed candidate. See ADR-0040.',
        {
          rtcMinPort: config.mediasoup.worker.rtcMinPort,
          rtcMaxPort: config.mediasoup.worker.rtcMaxPort,
        }
      );
    }
  });

  // Compose grants the process 45 seconds to stop. Reserve the existing final
  // 10 seconds for HTTP close, so unreleased exact rows fail visibly before a
  // supervisor kill can discard the only live node_boot_id target.
  const shutdownReleaseDrainTimeoutMs = 35_000;

  const failShutdown = (): void => {
    logger.error('Voice command shutdown retained permanent ownership', {
      error: 'voice_command_shutdown_failed',
    });
    process.exit(1);
  };
  let shutdownTerminalDeadline: ReturnType<typeof setTimeout> | undefined;
  let shutdownNativeDeadline: number | undefined;
  let shutdownTerminalWork: Promise<void> | undefined;
  // Only this shutdown continuation can invoke retirement. The signed request
  // authenticates its completed native/in-flight witness, never an empty count.
  const retireVoiceEnforcementBoot = async (deadline: number): Promise<void> => {
    const remaining = Math.floor(deadline - performance.now());
    if (remaining <= 0) throw new Error('Boot retirement deadline exhausted');
    const timestamp = Math.floor(Date.now() / 1000).toString();
    const nonce = randomBytes(32).toString('hex');
    const secret = Buffer.from(config.jwtSecret, 'utf8');
    const body = Buffer.from(JSON.stringify({
      schema: 'concord.auth-boot-retirement', version: 1, nodeBootId: voiceEnforcementNodeBootId,
    }));
    const { wire, request } = signHttpRequest('boot_retire', body, '', { timestamp, nonce }, secret);
    const context = newRequestContext(request, {});
    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    for (const header of wire.headers) headers[header.name] = header.value;
    const response = await fetch(`${config.controlPlaneUrl}${wire.path}`, {
      method: 'POST', headers, body, signal: AbortSignal.timeout(Math.min(5000, remaining)),
    });
    verifyHttpResponse('boot_retire', await readVoiceAuthResponse(response),
      context, secret, BigInt(Math.floor(Date.now() / 1000)));
    if (performance.now() >= deadline) throw new Error('Boot retirement deadline exhausted');
  };
  const completeShutdown = async () => {
    if (shutdownNativeDeadline === undefined)
      throw new Error('Native shutdown deadline unavailable');
    // Stop scheduling only after releases drain. Join the final poll and the
    // retained terminal owner it may have retried; retryPending only schedules.
    const finalPoll = voiceEnforcementHealth.stopPollingAndJoin();
    clearInterval(voiceEnforcementHealthInterval);
    await finalPoll;
    if (!shutdownTerminalWork) throw new Error('Shutdown terminal owner unavailable');
    await shutdownTerminalWork;
    await voiceAuthConnections.terminateAll();
    if (voiceEnforcementReleaseQueue.outstandingCount() !== 0)
      throw new Error('Shutdown retains voice identities');
    // Keep exact subscribers alive through registry release and actual child exit.
    await mediasoupService.close(shutdownNativeDeadline);
    await natsService.stopVoiceCommandRenewal();
    if (performance.now() + 2000 >= shutdownNativeDeadline) {
      throw new Error('Native shutdown deadline exhausted');
    }
    await voiceCommandOwner.release();
    await retireVoiceEnforcementBoot(shutdownNativeDeadline);
    if (shutdownTerminalDeadline) clearTimeout(shutdownTerminalDeadline);

    // Close inter-service connections
    await natsService.close();
    await redisService.close();

    // Close last so preceding shutdown work remains observable.
    if (!(await securityEvents.close())) {
      logger.error('Security event sink retained records during shutdown');
    }

    httpServer.close(() => {
      logger.info('Server closed');
      process.exit(0);
    });

    // Force exit after 10 seconds.
    setTimeout(() => {
      logger.warn('Forced shutdown after timeout');
      process.exit(1);
    }, 10_000);
  };

  // Graceful shutdown
  let shutdownPromise: Promise<void> | undefined;
  // Lives in main (not module scope) so the shutdown continuation stays one unit.
  // Polls each second until outstanding() is zero, then settles with onDrained().
  function afterQueueDrains(outstanding: () => number, onDrained: () => Promise<void>): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const timer = setInterval(() => {
        if (outstanding() !== 0) return;
        clearInterval(timer);
        void onDrained().then(resolve, reject);
      }, 1_000);
    });
  }

  const shutdown = (): Promise<void> => {
    if (shutdownPromise) return shutdownPromise;
    shutdownPromise = (async () => {
      const shutdownStartedAt = performance.now();
      shutdownNativeDeadline = shutdownStartedAt + shutdownReleaseDrainTimeoutMs;
      // Reject A1/A2 work before closeAll so no registry row can race teardown.
      shuttingDown = true;
      const shutdownRecovery = voiceEnforcementHealth.fenceRecoveryAndJoin();
      const shutdownLegacySessions = snapshotVoiceEnforcementSessions(io.sockets.sockets.values(), voiceAuthConnections);
      voiceEnforcementReleaseQueue.enqueueAll(shutdownLegacySessions);
      shutdownTerminalWork = voiceAuthConnections.terminateAll();
      enforcementAvailability.stop();
      stopObservingRequiredSubscriptions();
      natsService.stopRequiredSubscriptionRecovery();
      shutdownTerminalDeadline = setTimeout(() => {
        logger.error('Voice command shutdown retained permanent ownership', {
          error: 'voice_command_shutdown_deadline',
        });
        process.exit(1);
      }, shutdownReleaseDrainTimeoutMs);
      logger.info('Shutting down gracefully');
      clearInterval(epochSyncInterval);
      clearInterval(roomHeartbeatInterval);
      mediaPolicerTick.stop();
      clearInterval(voiceEnforcementLeaseInterval);
      await opsMetricsPublisher.stop();
      await shutdownRecovery;
      await natsService.stopVoiceCommandConsumer();

      // Close all rooms first (notifies participants, publishes NATS events)
      await roomManager.closeAll(shutdownNativeDeadline);
      const retainedReleases = await releaseVoiceEnforcementSessionsAfterClose(
        [],
        voiceEnforcementReleaseQueue,
        voiceAuthConnections
      );
      const remainingReleaseDrainMs =
        shutdownReleaseDrainTimeoutMs - (performance.now() - shutdownStartedAt);
      if (remainingReleaseDrainMs <= 0) {
        // RoomManager teardown has completed, so failing here cannot leave media
        // forwarding. Exit visibly instead of consuming the final HTTP-close
        // reserve and letting Docker SIGKILL this exact NATS target.
        logger.error('Voice enforcement shutdown terminal teardown exceeded deadline', {
          failureCount: retainedReleases,
        });
        process.exit(1);
        return;
      }
      if (retainedReleases > 0) {
        logger.warn('Voice enforcement releases retained during shutdown', {
          failureCount: retainedReleases,
        });
        // Keep the exact NATS target and health poll alive. The signed health
        // callback is the only retry trigger; neither elapsed time nor shutdown
        // infers a release. The bounded failure below leaves durable rows for
        // operator recovery instead of allowing an external SIGKILL to hide it.
        const shutdownReleaseDeadline = setTimeout(() => {
          const failureCount = voiceEnforcementReleaseQueue.outstandingCount();
          if (failureCount === 0) return;
          logger.error('Voice enforcement releases exceeded shutdown deadline', { failureCount });
          process.exit(1);
        }, remainingReleaseDrainMs);
        await afterQueueDrains(() => voiceEnforcementReleaseQueue.outstandingCount(), () => {
          clearTimeout(shutdownReleaseDeadline);
          return completeShutdown();
        });
        return;
      }

      await completeShutdown();
    })().catch(failShutdown);
    return shutdownPromise;
  };

  process.on('SIGTERM', shutdown);
  process.on('SIGINT', shutdown);
}

// Start the application
try {
  await main();
} catch {
  logger.error('Fatal error during startup', { error: 'media_startup_failed' });
  process.exit(1);
}
