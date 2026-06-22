import express from 'express';
import { Server as SocketIOServer, Socket } from 'socket.io';
import { createServer } from 'node:http';
import { config } from './config/index.js';
import { logger } from './lib/logger.js';
import { MediasoupService } from './lib/mediasoup.js';
import { RoomManager } from './lib/roomManager.js';
import type { MediaSource } from './lib/roomManager.js';
import { MediaMetrics } from './lib/mediaMetrics.js';
import { createAuthMiddleware, validateChannelAccess } from './middleware/auth.js';
import type { AuthenticatedSocketData } from './middleware/auth.js';
import { NatsService } from './lib/nats.js';
import { RedisService } from './lib/redis.js';
import { createExpressErrorHandler } from './lib/expressErrorHandler.js';
import { createOriginGate } from './lib/originGate.js';
import { handleForceDisconnect } from './lib/forceDisconnect.js';
import { handleSetDeafen } from './lib/setDeafen.js';

const expectedKeyframeRequestErrors = new Set([
  'Room not found',
  'Requester not found',
  'Sender not found',
]);

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
  const roomManager = new RoomManager(mediasoupService);
  // #1553 measurement counters — accumulated from heartbeat samples, surfaced on /health.
  const mediaMetrics = new MediaMetrics();

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

  // ─── Socket.IO connection handling ───────────────────────────────────

  io.on('connection', (socket) => {
    const data = socket.data as AuthenticatedSocketData;
    logger.info('Client connected', {
      socketId: socket.id,
      userId: data.userId,
      username: data.username,
    });

    // ── join-room ────────────────────────────────────────────────────
    // Client sends: { roomId, rtpCapabilities }
    // Server responds with: room-joined event containing router caps, existing producers, participants
    socket.on('join-room', async ({ roomId, rtpCapabilities }, callback?) => {
      try {
        // Validate channel access via control plane. Room kind is a
        // routing hint from the renderer's Socket.IO handshake (per
        // #1209 plan C1 + spec §6.5): 'channel' hits the server-channel
        // voice-join endpoint; 'dm' hits the DM authorize endpoint
        // (G7 defense-in-depth). Only the userId in `data` comes from
        // the verified JWT (see createAuthMiddleware); the display
        // fields (username/displayName/avatarUrl) currently flow from
        // socket.handshake.auth and are a known client-supplied gap.
        // room_kind is the same shape: a client-supplied routing hint
        // that doesn't grant any privilege beyond which control-plane
        // endpoint is consulted for authorization.
        const token = socket.handshake.auth.token;
        const roomKind: 'channel' | 'dm' =
          socket.handshake.auth.room_kind === 'dm' ? 'dm' : 'channel';
        const access = await validateChannelAccess(data.userId, roomId, token, roomKind);

        if (!access.allowed) {
          logger.warn('Channel access denied', {
            userId: data.userId,
            roomId,
            error: access.error,
          });
          const errPayload = { error: access.error || 'Access denied' };
          if (callback) return callback(errPayload);
          socket.emit('error', errPayload);
          return;
        }

        logger.debug('Channel access validated', {
          roomId,
          userId: data.userId,
        });

        // Join the room via RoomManager. Thread the server-authoritative per-user
        // media entitlement (#1300) parsed from the join-authorize response into
        // the Participant — it caps THIS user's own send transport + mic produce
        // (never sourced from socket.handshake.auth).
        const result = await roomManager.joinRoom(
          roomId,
          data.userId,
          socket.id,
          {
            username: data.username,
            displayName: data.displayName,
            avatarUrl: data.avatarUrl,
          },
          rtpCapabilities,
          {
            tier: access.userTier,
            allowedAudioTiers: access.allowedAudioTiers,
            minPtimeMs: access.minPtimeMs,
            maxManualBitrateBps: access.maxManualBitrateBps,
          }
        );

        // Apply server enforcement if the joining user has active enforcement
        if (access.serverMuted) {
          await roomManager.serverMuteUser(roomId, data.userId);
          logger.info('Applied server-mute enforcement on join', { roomId, userId: data.userId });
        }
        if (access.serverDeafened) {
          await roomManager.serverDeafenUser(roomId, data.userId);
          logger.info('Applied server-deafen enforcement on join', { roomId, userId: data.userId });
        }

        // Join Socket.IO room for broadcasting
        socket.join(roomId);
        data.roomId = roomId;

        // Notify others in the room
        socket.to(roomId).emit('user-joined', {
          userId: data.userId,
          username: data.username,
          displayName: data.displayName,
          avatarUrl: data.avatarUrl,
          e2eeEpoch: result.e2eeEpoch,
        });

        // Respond to the joining client
        const response = {
          rtpCapabilities: result.rtpCapabilities,
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
        logger.error('Error joining room', {
          error: error instanceof Error ? error.message : error,
          stack: error instanceof Error ? error.stack : undefined,
          roomId,
          userId: data.userId,
        });
        const errPayload = { error: 'Failed to join room' };
        if (callback) return callback(errPayload);
        socket.emit('error', errPayload);
      }
    });

    // ── update-rtp-capabilities ─────────────────────────────────────
    // Client sends this after device.load() to provide its actual RTP capabilities
    socket.on('update-rtp-capabilities', ({ rtpCapabilities }, callback?) => {
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
    socket.on('create-transport', async ({ direction }, callback) => {
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
    socket.on('connect-transport', async ({ transportId, dtlsParameters }, callback) => {
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
    });

    // ── produce ──────────────────────────────────────────────────────
    // Client sends: { transportId, kind, rtpParameters, appData: { source } }
    socket.on('produce', async ({ transportId, kind, rtpParameters, appData }, callback) => {
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
          requiresOptIn: producerInfo.source === 'screen' || producerInfo.source === 'screen-audio',
        });

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
    });

    // ── consume ──────────────────────────────────────────────────────
    socket.on('consume', async ({ producerId, transportId }, callback) => {
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
    socket.on('request-keyframe', async (payload, callback?) => {
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
    socket.on('resume-consumer', async ({ consumerId }, callback?) => {
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

    // ── pause-consumer ──────────────────────────────────────────────
    socket.on('pause-consumer', async ({ consumerId }, callback?) => {
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
    socket.on('close-consumer', ({ consumerId }, callback?) => {
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

    // ── pause-producer ───────────────────────────────────────────────
    socket.on('pause-producer', async ({ producerId }, callback?) => {
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
    socket.on('resume-producer', async ({ producerId }, callback?) => {
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
    socket.on('set-deafen', ({ isDeafened }: { isDeafened?: unknown }, callback?) => {
      const result = handleSetDeafen(roomManager, socket, data.roomId, data.userId, isDeafened);
      if (callback) callback(result);
    });

    // ── close-producer ───────────────────────────────────────────────
    socket.on('close-producer', async ({ producerId }, callback?) => {
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
    socket.on('leave-room', async (_, callback?) => {
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

    // Notify others before cleanup
    socket.to(roomId).emit('user-left', { userId });

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
        timestamp: new Date().toISOString(),
      });
    }
    // #1553 measurement: sample live rooms, accumulate, emit aggregate-only ops log
    // (structural metadata only — no userIds/PII/keys, per the logging-discipline rule).
    // The try/catch keeps the async heartbeat from ever throwing into the timer (degrade,
    // don't crash); the metricsSampling guard skips a tick if the prior sampling is still
    // running so two ticks never interleave ingest() on shared state (#1553 review).
    if (!metricsSampling) {
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
