import { createHmac, timingSafeEqual } from 'node:crypto';
import type { RoomManager } from './roomManager.js';
import { logger } from './logger.js';
import type { EmitSecurityEvent } from './securityEvent.js';
import {
  releaseVoiceEnforcementSession,
  type VoiceEnforcementSession,
} from './voiceEnforcementSession.js';

const DM_BLOCK_DISCONNECT_VERSION = 2;
const DM_BLOCK_DISCONNECT_PROOF_VERSION = 'v2';
const DM_BLOCK_DISCONNECT_REQUEST_PROOF = 'concord/dm-block-voice-ejection/request/v2';
const DM_BLOCK_DISCONNECT_ACK_PROOF = 'concord/dm-block-voice-ejection/ack/v2';
const DM_BLOCK_DISCONNECT_MAX_AGE_MS = 6_000;
const CREDENTIAL_EPOCH_EJECT_VERSION = 1;
const CREDENTIAL_EPOCH_EJECT_REQUEST_PROOF = 'concord/credential-epoch-voice-ejection/request/v1';
const CREDENTIAL_EPOCH_EJECT_ACK_PROOF = 'concord/credential-epoch-voice-ejection/ack/v1';
const VOICE_ENFORCEMENT_SESSION_EJECT_VERSION = 1;
const VOICE_ENFORCEMENT_SESSION_EJECT_REQUEST_PROOF =
  'concord/voice-enforcement-session/eject/request/v1';
const VOICE_ENFORCEMENT_SESSION_EJECT_ACK_PROOF = 'concord/voice-enforcement-session/eject/ack/v1';
const VOICE_ENFORCEMENT_HEALTH_REQUEST_PROOF =
  'concord/voice-enforcement-session/health/request/v1';
const VOICE_ENFORCEMENT_HEALTH_ACK_PROOF = 'concord/voice-enforcement-session/health/ack/v1';

function proofKey(sharedSecret: string, context: string): Buffer | undefined {
  if (sharedSecret.length === 0) return undefined;
  return createHmac('sha256', sharedSecret).update(context).digest();
}

function signProof(
  key: Buffer,
  timestamp: string,
  fields: string[],
  version: string
): string | undefined {
  const payload = [version, timestamp, ...fields];
  if (payload.some((field) => field.includes('\n'))) return undefined;
  return createHmac('sha256', key).update(payload.join('\n')).digest('hex');
}

function verifiesProof(
  key: Buffer | undefined,
  proof: unknown,
  timestamp: string,
  fields: string[],
  version: string
): boolean {
  if (
    !key ||
    typeof proof !== 'string' ||
    !/^[0-9a-f]{64}$/.test(proof) ||
    !/^(0|[1-9]\d*)$/.test(timestamp)
  ) {
    return false;
  }
  const seconds = Number(timestamp);
  if (
    !Number.isSafeInteger(seconds) ||
    Math.abs(Date.now() - seconds * 1_000) > DM_BLOCK_DISCONNECT_MAX_AGE_MS
  ) {
    return false;
  }
  const expected = signProof(key, timestamp, fields, version);
  return (
    expected !== undefined &&
    timingSafeEqual(Buffer.from(proof, 'hex'), Buffer.from(expected, 'hex'))
  );
}

/**
 * Minimal RoomManager surface needed to force-disconnect a peer. Declared as an
 * interface (rather than the full RoomManager) so unit tests can inject a fake.
 */
export interface ForceDisconnectRoomManager {
  getParticipant: RoomManager['getParticipant'];
  getProvisionalParticipantSocketId: RoomManager['getProvisionalParticipantSocketId'];
  leaveRoomIfSocketOwned: RoomManager['leaveRoomIfSocketOwned'];
  removeProvisionalParticipantForEnforcement: RoomManager['removeProvisionalParticipantForEnforcement'];
}

export interface CredentialEpochEvictionRoomManager {
  getSupersededCredentialEpochSessions: RoomManager['getSupersededCredentialEpochSessions'];
  leaveRoomIfSocketOwned: RoomManager['leaveRoomIfSocketOwned'];
  removeProvisionalParticipantForEnforcement: RoomManager['removeProvisionalParticipantForEnforcement'];
}

export interface VoiceEnforcementSessionRoomManager {
  getParticipant: RoomManager['getParticipant'];
  getProvisionalParticipant: RoomManager['getProvisionalParticipant'];
  leaveRoomIfSocketOwned: RoomManager['leaveRoomIfSocketOwned'];
  removeProvisionalParticipantForEnforcement: RoomManager['removeProvisionalParticipantForEnforcement'];
}

/**
 * Minimal Socket.IO surface needed to evict a peer's live socket.
 */
export interface ForceDisconnectIO {
  sockets: {
    sockets: Map<
      string,
      {
        emit: (event: string, ...args: unknown[]) => void;
        disconnect: (close?: boolean) => void;
        data?: {
          userId?: unknown;
          credentialEpoch?: unknown;
          roomId?: unknown;
          voiceEnforcementSession?: VoiceEnforcementSession;
        };
      }
    >;
  };
}

function isUUID(value: unknown): value is string {
  return (
    typeof value === 'string' &&
    /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value)
  );
}

function isCredentialEpoch(value: unknown): value is string {
  return value === '' || (typeof value === 'string' && /^[0-9a-f]{32}$/.test(value));
}

interface VoiceEnforcementSessionCommand extends Record<string, unknown> {
  parentGeneration: string;
  sessionGeneration: string;
  nodeBootId: string;
  roomId: string;
  roomKind: 'channel' | 'dm';
  userId: string;
  credentialEpoch: string;
  socketId: string;
  timestamp: string;
  nonce: string;
}

function isVoiceEnforcementSessionCommandShape(
  natsData: Record<string, unknown>,
  localNodeBootID: string
): natsData is VoiceEnforcementSessionCommand {
  const {
    parentGeneration,
    sessionGeneration,
    nodeBootId,
    roomId,
    roomKind,
    userId,
    credentialEpoch,
    socketId,
    timestamp,
    nonce,
  } = natsData;
  if (!isUUID(parentGeneration)) return false;
  if (!isUUID(sessionGeneration)) return false;
  if (nodeBootId !== localNodeBootID || !isUUID(nodeBootId)) return false;
  if (!isUUID(roomId)) return false;
  if (roomKind !== 'channel' && roomKind !== 'dm') return false;
  if (!isUUID(userId)) return false;
  if (!isCredentialEpoch(credentialEpoch)) return false;
  if (
    typeof socketId !== 'string' ||
    socketId.length === 0 ||
    Buffer.byteLength(socketId, 'utf8') > 255
  ) {
    return false;
  }
  if (typeof timestamp !== 'string') return false;
  if (typeof nonce !== 'string' || !/^[0-9a-f]{64}$/.test(nonce)) return false;
  return true;
}

function hasValidVoiceEnforcementSessionProof(
  command: VoiceEnforcementSessionCommand,
  sharedSecret: string
): boolean {
  return verifiesProof(
    proofKey(sharedSecret, VOICE_ENFORCEMENT_SESSION_EJECT_REQUEST_PROOF),
    command.proof,
    command.timestamp,
    [
      command.parentGeneration,
      command.sessionGeneration,
      command.nodeBootId,
      command.roomId,
      command.roomKind,
      command.userId,
      command.credentialEpoch,
      command.socketId,
      command.nonce,
    ],
    'v1'
  );
}

function sameVoiceEnforcementSession(
  candidate: VoiceEnforcementSession | undefined,
  session: VoiceEnforcementSession
): boolean {
  return (
    candidate?.sessionGeneration === session.sessionGeneration &&
    candidate.nodeBootId === session.nodeBootId &&
    candidate.roomId === session.roomId &&
    candidate.roomKind === session.roomKind &&
    candidate.userId === session.userId &&
    candidate.credentialEpoch === session.credentialEpoch &&
    candidate.socketId === session.socketId
  );
}

async function removeExactVoiceEnforcementSession(
  roomManager: VoiceEnforcementSessionRoomManager,
  io: ForceDisconnectIO,
  session: VoiceEnforcementSession
): Promise<boolean> {
  const { roomId, userId, socketId, credentialEpoch, sessionGeneration } = session;
  const provisional = roomManager.getProvisionalParticipant(roomId, userId);
  const provisionalExact =
    provisional?.socketId === socketId &&
    provisional.credentialEpoch === credentialEpoch &&
    provisional.voiceEnforcementSessionGeneration === sessionGeneration;
  const socket = io.sockets.sockets.get(socketId);
  const locallyOwned = socket?.data?.voiceEnforcementSession;
  if (socket && !sameVoiceEnforcementSession(locallyOwned, session) && !provisionalExact) {
    return false;
  }

  const admitted = roomManager.getParticipant(roomId, userId);
  const admittedExact =
    admitted?.socketId === socketId &&
    admitted.credentialEpoch === credentialEpoch &&
    admitted.voiceEnforcementSessionGeneration === sessionGeneration;
  // A missing socket is only proof of absence when no local participant still
  // names it. Otherwise retaining the row is safer than acknowledging a
  // session whose ownership the process cannot establish.
  if (!socket && (provisional?.socketId === socketId || admitted?.socketId === socketId)) {
    return false;
  }

  let terminallyAbsent = false;
  if (provisionalExact) {
    terminallyAbsent = await roomManager.removeProvisionalParticipantForEnforcement(
      roomId,
      userId,
      socketId
    );
  } else if (admittedExact) {
    terminallyAbsent = await roomManager.leaveRoomIfSocketOwned(roomId, userId, socketId);
  } else if (
    sameVoiceEnforcementSession(locallyOwned, session) &&
    provisional?.socketId !== socketId &&
    admitted?.socketId !== socketId
  ) {
    // RoomManager has already replaced this exact socket with a successor;
    // its old transports were closed by replacement. Disconnect only the
    // stale socket whose durable identity still matches this command.
    terminallyAbsent = true;
  } else if (!socket && provisional?.socketId !== socketId && admitted?.socketId !== socketId) {
    // No local state names the exact socket. A different successor may be
    // present under this user, but it is not the durable row being released.
    terminallyAbsent = true;
  }

  if (!terminallyAbsent) return false;
  socket?.emit('force-disconnect', { channelId: roomId, reason: 'authority_revoked' });
  socket?.disconnect(true);
  return true;
}

function createVoiceEnforcementSessionAcknowledgement(
  command: VoiceEnforcementSessionCommand,
  sharedSecret: string
): Record<string, unknown> | undefined {
  const acknowledgementKey = proofKey(sharedSecret, VOICE_ENFORCEMENT_SESSION_EJECT_ACK_PROOF);
  if (!acknowledgementKey) return undefined;
  const proof = signProof(
    acknowledgementKey,
    command.timestamp,
    [
      command.parentGeneration,
      command.sessionGeneration,
      command.nodeBootId,
      command.roomId,
      command.roomKind,
      command.userId,
      command.credentialEpoch,
      command.socketId,
      command.nonce,
      'true',
    ],
    'v1'
  );
  if (!proof) return undefined;
  return {
    version: VOICE_ENFORCEMENT_SESSION_EJECT_VERSION,
    parentGeneration: command.parentGeneration,
    sessionGeneration: command.sessionGeneration,
    nodeBootId: command.nodeBootId,
    roomId: command.roomId,
    roomKind: command.roomKind,
    userId: command.userId,
    credentialEpoch: command.credentialEpoch,
    socketId: command.socketId,
    timestamp: command.timestamp,
    nonce: command.nonce,
    ok: true,
    proof,
  };
}

async function handleVoiceEnforcementSessionEjection(
  roomManager: VoiceEnforcementSessionRoomManager,
  io: ForceDisconnectIO,
  command: VoiceEnforcementSessionCommand,
  sharedSecret: string
): Promise<Record<string, unknown> | undefined> {
  const session: VoiceEnforcementSession = {
    sessionGeneration: command.sessionGeneration,
    nodeBootId: command.nodeBootId,
    roomId: command.roomId,
    roomKind: command.roomKind,
    userId: command.userId,
    credentialEpoch: command.credentialEpoch,
    socketId: command.socketId,
  };
  if (!(await removeExactVoiceEnforcementSession(roomManager, io, session))) return undefined;

  // The exact session has been removed before the transport is closed, or no
  // local state named it. Only then may this node release the durable row.
  await releaseVoiceEnforcementSession(session);
  return createVoiceEnforcementSessionAcknowledgement(command, sharedSecret);
}

async function handleVoiceEnforcementHealthCommand(
  natsData: Record<string, unknown>,
  localNodeBootID: string,
  sharedSecret: string,
  renewHealth?: () => void | Promise<void>
): Promise<Record<string, unknown> | undefined> {
  const timestamp = natsData.timestamp;
  const challenge = natsData.challenge;
  if (
    natsData.version !== 2 ||
    natsData.nodeBootId !== localNodeBootID ||
    typeof timestamp !== 'string' ||
    typeof challenge !== 'string' ||
    !/^[0-9a-f]{64}$/.test(challenge) ||
    !verifiesProof(
      proofKey(sharedSecret, VOICE_ENFORCEMENT_HEALTH_REQUEST_PROOF),
      natsData.proof,
      timestamp,
      [localNodeBootID, challenge, 'health'],
      'v1'
    )
  ) {
    logger.warn('Rejected voice enforcement health command', {
      reason: 'invalid_shape_or_proof',
    });
    return undefined;
  }
  const key = proofKey(sharedSecret, VOICE_ENFORCEMENT_HEALTH_ACK_PROOF);
  if (!key) return undefined;
  const proof = signProof(key, timestamp, [localNodeBootID, challenge, 'health', 'true'], 'v1');
  if (!proof) return undefined;
  await renewHealth?.();
  return {
    version: 2,
    kind: 'health',
    nodeBootId: localNodeBootID,
    challenge,
    timestamp,
    ok: true,
    proof,
  };
}

/**
 * Handles one exact, boot-targeted durable authority command. A reply means
 * the target has terminally removed that exact local session (or proved it is
 * already absent) and the control-plane release completed; it never means a
 * generic broadcast subscriber happened to receive a request.
 */
export function createVoiceEnforcementSessionEjectionHandler(
  roomManager: VoiceEnforcementSessionRoomManager,
  io: ForceDisconnectIO,
  sharedSecret: string,
  localNodeBootID: string,
  renewHealth?: () => void | Promise<void>
): (natsData: Record<string, unknown>) => Promise<Record<string, unknown> | undefined> {
  return async (natsData) => {
    if (natsData.kind === 'health') {
      return handleVoiceEnforcementHealthCommand(
        natsData,
        localNodeBootID,
        sharedSecret,
        renewHealth
      );
    }
    if (natsData.version !== VOICE_ENFORCEMENT_SESSION_EJECT_VERSION) {
      logger.warn('Rejected voice enforcement session command', {
        reason: 'unsupported_version',
      });
      return undefined;
    }
    if (
      !isVoiceEnforcementSessionCommandShape(natsData, localNodeBootID) ||
      !hasValidVoiceEnforcementSessionProof(natsData, sharedSecret)
    ) {
      // Do not log any wire values: every rejected field is NATS input and can
      // contain identifiers, proof material, or an attacker-controlled value.
      logger.warn('Rejected voice enforcement session command', {
        reason: 'invalid_shape_or_proof',
      });
      return undefined;
    }
    return handleVoiceEnforcementSessionEjection(roomManager, io, natsData, sharedSecret);
  };
}

/**
 * Handles a `voice.enforce.disconnect` command from the control plane (#487 P3).
 *
 * Revoking a user's VIEW/CONNECT permission does NOT eject an already-connected
 * peer, so the control plane publishes this command to authoritatively remove the
 * peer from the SFU. The handler:
 *   1. Silently removes the exact provisional candidate before disconnecting
 *      either socket, so synchronous admitted cleanup can terminalize the room.
 *   2. Delegates exact admitted teardown to RoomManager before force-closing that
 *      socket, so synchronous disconnect cleanup cannot erase its ownership.
 *   3. Notifies every admitted or provisional live socket for the user (so the
 *      client tears down its WebRTC state) and force-disconnects each socket.
 *      RoomManager teardown closes the peer's
 *      transports/producers/consumers and emits `user-left`. The NATS room-event
 *      bridge turns that into the normal `voice.left` published back to the control
 *      plane, so `voice_participants` is cleaned and `voice_state_update` broadcasts.
 *
 * Per [internal]rules/media-plane.md it reuses RoomManager's admitted and exact
 * provisional cleanup seams rather than recreating lifecycle logic. Idempotent:
 * a no-op if the user has neither session in the room (already left).
 */
export async function handleForceDisconnect(
  roomManager: ForceDisconnectRoomManager,
  io: ForceDisconnectIO,
  channelId: string,
  userId: string,
  emit?: EmitSecurityEvent
): Promise<void> {
  const participant = roomManager.getParticipant(channelId, userId);
  const provisionalSocketId = roomManager.getProvisionalParticipantSocketId(channelId, userId);
  if (!participant && !provisionalSocketId) {
    // Already gone — nothing to evict. Idempotent.
    return;
  }

  // Cancel the captured A1 state before disconnecting either socket. Socket.IO
  // fires the admitted socket's disconnect handler synchronously, so this keeps
  // its terminal decision correct. The enforcement-specific seam also finalizes
  // a room that was already pending-only: silently without admitted history, or
  // through normal terminal lifecycle with history. Exact ownership prevents
  // this command from deleting a successor session.
  let changed = false;
  if (provisionalSocketId) {
    changed = await roomManager.removeProvisionalParticipantForEnforcement(
      channelId,
      userId,
      provisionalSocketId
    );
  }

  // Tear down the captured admitted session before force-closing its socket.
  // Socket.IO dispatches disconnect cleanup synchronously, which would otherwise
  // erase exact ownership before this authoritative RoomManager call can run.
  if (participant) {
    changed =
      (await roomManager.leaveRoomIfSocketOwned(channelId, userId, participant.socketId)) ||
      changed;
  }

  // Tell every admitted/provisional session to leave, then force each socket
  // closed. A Set handles the defensive case where both registries name one ID.
  const socketIds = new Set<string>();
  if (participant) socketIds.add(participant.socketId);
  if (provisionalSocketId) socketIds.add(provisionalSocketId);
  for (const socketId of socketIds) {
    const socket = io.sockets.sockets.get(socketId);
    if (!socket) continue;
    socket.emit('force-disconnect', { channelId, reason: 'access_revoked' });
    socket.disconnect(true);
  }

  if (changed) {
    try {
      emit?.({
        eventType: 'media_authorization',
        outcome: 'success',
        severity: 'high',
        reasonCode: 'revocation_enforced',
        routeTemplate: 'socket.force_disconnect',
      });
    } catch {
      // An audit observer cannot alter an authoritative teardown.
    }
  }

  logger.info('Force-disconnected participant via voice.enforce.disconnect', {
    channelId,
    userId,
  });
}

// createDMBlockDisconnectAckHandler acknowledges only after the existing,
// authoritative RoomManager teardown has completed. Invalid or unauthenticated
// NATS requests receive no success response, keeping the SQL outbox pending.
export function createDMBlockDisconnectAckHandler(
  roomManager: ForceDisconnectRoomManager,
  io: ForceDisconnectIO,
  sharedSecret: string
): (natsData: Record<string, unknown>) => Promise<Record<string, unknown> | undefined> {
  return async (natsData) => {
    const { channelId, userId, timestamp, nonce } = natsData;
    if (
      natsData.version !== DM_BLOCK_DISCONNECT_VERSION ||
      typeof channelId !== 'string' ||
      typeof userId !== 'string' ||
      natsData.action !== 'disconnect' ||
      typeof timestamp !== 'string' ||
      typeof nonce !== 'string' ||
      !/^[0-9a-f]{64}$/.test(nonce) ||
      !verifiesProof(
        proofKey(sharedSecret, DM_BLOCK_DISCONNECT_REQUEST_PROOF),
        natsData.proof,
        timestamp,
        [channelId, userId, 'disconnect', nonce],
        DM_BLOCK_DISCONNECT_PROOF_VERSION
      )
    ) {
      return undefined;
    }

    await handleForceDisconnect(roomManager, io, channelId, userId);
    const acknowledgementKey = proofKey(sharedSecret, DM_BLOCK_DISCONNECT_ACK_PROOF);
    if (!acknowledgementKey) return undefined;
    const proof = signProof(
      acknowledgementKey,
      timestamp,
      [channelId, userId, 'disconnect', nonce, 'true'],
      DM_BLOCK_DISCONNECT_PROOF_VERSION
    );
    if (!proof) return undefined;
    return {
      version: DM_BLOCK_DISCONNECT_VERSION,
      channelId,
      userId,
      action: 'disconnect',
      timestamp,
      nonce,
      ok: true,
      proof,
    };
  };
}

export function createCredentialEpochEjectionAckHandler(
  roomManager: CredentialEpochEvictionRoomManager,
  io: ForceDisconnectIO,
  sharedSecret: string
): (natsData: Record<string, unknown>) => Promise<Record<string, unknown> | undefined> {
  return async (natsData) => {
    const { userId, credentialEpoch, supersededCredentialEpoch, timestamp, nonce } = natsData;
    if (
      natsData.version !== CREDENTIAL_EPOCH_EJECT_VERSION ||
      typeof userId !== 'string' ||
      typeof credentialEpoch !== 'string' ||
      !/^[0-9a-f]{32}$/.test(credentialEpoch) ||
      !(
        supersededCredentialEpoch === '' ||
        (typeof supersededCredentialEpoch === 'string' &&
          /^[0-9a-f]{32}$/.test(supersededCredentialEpoch))
      ) ||
      credentialEpoch === supersededCredentialEpoch ||
      natsData.action !== 'disconnect' ||
      typeof timestamp !== 'string' ||
      typeof nonce !== 'string' ||
      !/^[0-9a-f]{64}$/.test(nonce) ||
      !verifiesProof(
        proofKey(sharedSecret, CREDENTIAL_EPOCH_EJECT_REQUEST_PROOF),
        natsData.proof,
        timestamp,
        [userId, credentialEpoch, supersededCredentialEpoch, 'disconnect', nonce],
        'v1'
      )
    ) {
      return undefined;
    }

    await handleCredentialEpochEviction(roomManager, io, userId, supersededCredentialEpoch);
    const acknowledgementKey = proofKey(sharedSecret, CREDENTIAL_EPOCH_EJECT_ACK_PROOF);
    if (!acknowledgementKey) return undefined;
    const proof = signProof(
      acknowledgementKey,
      timestamp,
      [userId, credentialEpoch, supersededCredentialEpoch, 'disconnect', nonce, 'true'],
      'v1'
    );
    if (!proof) return undefined;
    return {
      version: CREDENTIAL_EPOCH_EJECT_VERSION,
      userId,
      credentialEpoch,
      supersededCredentialEpoch,
      action: 'disconnect',
      timestamp,
      nonce,
      ok: true,
      proof,
    };
  };
}

async function handleCredentialEpochEviction(
  roomManager: CredentialEpochEvictionRoomManager,
  io: ForceDisconnectIO,
  userId: string,
  supersededCredentialEpoch: string
): Promise<void> {
  const sessions = roomManager.getSupersededCredentialEpochSessions(
    userId,
    supersededCredentialEpoch
  );
  for (const session of sessions) {
    if (session.provisional) {
      await roomManager.removeProvisionalParticipantForEnforcement(
        session.roomId,
        userId,
        session.socketId
      );
    }
  }
  const socketRooms = new Map(sessions.map((session) => [session.socketId, session.roomId]));
  for (const [socketId, socket] of io.sockets.sockets) {
    if (
      socket.data?.userId === userId &&
      socket.data.credentialEpoch === supersededCredentialEpoch
    ) {
      socketRooms.set(socketId, typeof socket.data.roomId === 'string' ? socket.data.roomId : '');
    }
  }
  for (const [socketId, roomId] of socketRooms) {
    const socket = io.sockets.sockets.get(socketId);
    socket?.emit('force-disconnect', { channelId: roomId, reason: 'credential_rotated' });
    socket?.disconnect(true);
  }
  for (const session of sessions) {
    if (!session.provisional) {
      await roomManager.leaveRoomIfSocketOwned(session.roomId, userId, session.socketId);
    }
  }
}
