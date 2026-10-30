import { createHmac, timingSafeEqual } from 'node:crypto';
import type { RoomManager } from './roomManager.js';
import { logger } from './logger.js';
import type { EmitSecurityEvent, SecurityEventInput } from './securityEvent.js';
import {
  releaseVoiceEnforcementSession,
  type VoiceEnforcementSession,
} from './voiceEnforcementSession.js';
import { createVoiceAuthReceiver } from './voiceAuthReceiver.js';
import type { RawRequestReceiver, VoiceAuthReceiverAdapters } from './voiceAuthReceiver.js';

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
  getRoom: RoomManager['getRoom'];
  getParticipant: RoomManager['getParticipant'];
  getProvisionalParticipant: RoomManager['getProvisionalParticipant'];
  leaveRoomIfSocketOwned: RoomManager['leaveRoomIfSocketOwned'];
  removeProvisionalParticipantForEnforcement: RoomManager['removeProvisionalParticipantForEnforcement'];
  awaitNativeTermination: RoomManager['awaitNativeTermination'];
}

export interface CredentialEpochEvictionRoomManager {
  getSupersededCredentialEpochSessions: RoomManager['getSupersededCredentialEpochSessions'];
  leaveRoomIfSocketOwned: RoomManager['leaveRoomIfSocketOwned'];
  removeProvisionalParticipantForEnforcement: RoomManager['removeProvisionalParticipantForEnforcement'];
  awaitNativeTermination: RoomManager['awaitNativeTermination'];
  awaitNativeTerminationForCredentialEpoch: RoomManager['awaitNativeTerminationForCredentialEpoch'];
}

export interface VoiceEnforcementSessionRoomManager {
  getParticipant: RoomManager['getParticipant'];
  getProvisionalParticipant: RoomManager['getProvisionalParticipant'];
  leaveRoomIfSocketOwned: RoomManager['leaveRoomIfSocketOwned'];
  removeProvisionalParticipantForEnforcement: RoomManager['removeProvisionalParticipantForEnforcement'];
  awaitNativeTermination: RoomManager['awaitNativeTermination'];
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

function isExactVoiceEnforcementParticipant(
  participant: ReturnType<RoomManager['getProvisionalParticipant']>,
  socketId: string,
  credentialEpoch: string,
  sessionGeneration: string
): boolean {
  return (
    participant?.socketId === socketId &&
    participant.credentialEpoch === credentialEpoch &&
    participant.voiceEnforcementSessionGeneration === sessionGeneration
  );
}

async function removeExactVoiceEnforcementSession(
  roomManager: VoiceEnforcementSessionRoomManager,
  io: ForceDisconnectIO,
  session: VoiceEnforcementSession
): Promise<boolean> {
  const { roomId, userId, socketId, credentialEpoch, sessionGeneration } = session;
  const provisional = roomManager.getProvisionalParticipant(roomId, userId);
  const provisionalExact = isExactVoiceEnforcementParticipant(
    provisional,
    socketId,
    credentialEpoch,
    sessionGeneration
  );
  const socket = io.sockets.sockets.get(socketId);
  const locallyOwned = socket?.data?.voiceEnforcementSession;
  const capturedSocketState = {
    userId: socket?.data?.userId,
    credentialEpoch: socket?.data?.credentialEpoch,
    roomId: socket?.data?.roomId,
  };
  if (socket && !sameVoiceEnforcementSession(locallyOwned, session) && !provisionalExact) {
    return false;
  }

  const admitted = roomManager.getParticipant(roomId, userId);
  const admittedExact = isExactVoiceEnforcementParticipant(
    admitted,
    socketId,
    credentialEpoch,
    sessionGeneration
  );
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
      socketId,
      sessionGeneration
    );
  } else if (admittedExact) {
    terminallyAbsent = await roomManager.leaveRoomIfSocketOwned(
      roomId,
      userId,
      socketId,
      sessionGeneration
    );
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
  await roomManager.awaitNativeTermination(roomId, userId, socketId, sessionGeneration);
  const currentAdmitted = roomManager.getParticipant(roomId, userId);
  const currentProvisional = roomManager.getProvisionalParticipant(roomId, userId);
  const successorOwnsSocket = [currentAdmitted, currentProvisional].some(
    (current) =>
      current?.socketId === socketId &&
      current.voiceEnforcementSessionGeneration !== sessionGeneration
  );
  const stillRegisteringCapturedCandidate =
    provisionalExact &&
    locallyOwned === undefined &&
    socket?.data?.voiceEnforcementSession === undefined &&
    socket?.data?.userId === capturedSocketState.userId &&
    socket?.data?.credentialEpoch === capturedSocketState.credentialEpoch &&
    socket?.data?.roomId === capturedSocketState.roomId;
  if (
    socket &&
    io.sockets.sockets.get(socketId) === socket &&
    !successorOwnsSocket &&
    (sameVoiceEnforcementSession(socket.data?.voiceEnforcementSession, session) ||
      stillRegisteringCapturedCandidate)
  ) {
    socket.emit('force-disconnect', { channelId: roomId, reason: 'authority_revoked' });
    socket.disconnect(true);
  }
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
  await releaseVoiceEnforcementSession(session, (target) =>
    roomManager.awaitNativeTermination(
      target.roomId,
      target.userId,
      target.socketId,
      target.sessionGeneration
    )
  );
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
  renewHealth?: () => void | Promise<void>,
  adapters: VoiceAuthReceiverAdapters = {}
): ((natsData: Record<string, unknown>) => Promise<Record<string, unknown> | undefined>) & {
  receive: RawRequestReceiver;
} {
  const handler = async (natsData: Record<string, unknown>) => {
    // Raw protocol-4 envelopes are handled only by the verified receiver. Never
    // let a mixed/unknown schema reach legacy health or room teardown.
    if (Object.hasOwn(natsData, 'schema')) return undefined;
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
  const terminalKeys = [
    'version', 'parentGeneration', 'sessionGeneration', 'nodeBootId', 'roomId',
    'roomKind', 'userId', 'credentialEpoch', 'socketId', 'timestamp', 'nonce', 'proof',
  ];
  const healthKeys = ['version', 'kind', 'nodeBootId', 'challenge', 'timestamp', 'proof'];
  const receive = createVoiceAuthReceiver(sharedSecret, localNodeBootID, {
    verify(data) {
      const health = data.kind === 'health';
      const keys = health ? healthKeys : terminalKeys;
      if (Object.keys(data).length !== keys.length || keys.some((key) => !Object.hasOwn(data, key)))
        return undefined;
      if (health) {
        return data.version === 2 && data.nodeBootId === localNodeBootID &&
          typeof data.timestamp === 'string' && typeof data.challenge === 'string' &&
          /^[0-9a-f]{64}$/.test(data.challenge) && verifiesProof(
            proofKey(sharedSecret, VOICE_ENFORCEMENT_HEALTH_REQUEST_PROOF),
            data.proof, data.timestamp, [localNodeBootID, data.challenge, 'health'], 'v1'
          ) ? 'health' : undefined;
      }
      return data.version === VOICE_ENFORCEMENT_SESSION_EJECT_VERSION &&
        isVoiceEnforcementSessionCommandShape(data, localNodeBootID) &&
        hasValidVoiceEnforcementSessionProof(data, sharedSecret) ? 'terminal' : undefined;
    },
    terminalKey: (data) => JSON.stringify(terminalKeys.filter(
      (key) => !['timestamp', 'nonce', 'proof', 'parentGeneration'].includes(key)
    ).map((key) => data[key])),
    terminal: async (data) => {
      const response = await handler(data);
      if (response === undefined) throw new Error('Exact room termination unconfirmed');
    },
    acknowledge: (data) => isVoiceEnforcementSessionCommandShape(data, localNodeBootID)
      ? createVoiceEnforcementSessionAcknowledgement(data, sharedSecret) : undefined,
    health: (data) => handler(data),
  }, adapters);
  return Object.assign(handler, { receive });
}

/**
 * Why a peer is being evicted. Required, because the reason selects both the
 * `force-disconnect` payload and the security event: a media-policy eviction
 * (#2153) must never be reported as a permission revocation.
 */
export type ForceDisconnectOptions =
  | {
      readonly reason: 'access_revoked';
      readonly socketId?: string;
      readonly admissionId?: string;
      readonly callId?: string;
    }
  | {
      readonly reason: 'media_policy';
      readonly retryAfterSec: number;
      readonly socketId?: string;
      readonly admissionId?: string;
    }
  | {
      readonly reason: 'stats_unavailable';
      /** Exact failed send transport; a reconnect must never be evicted by an old sample. */
      readonly sendTransportId: string;
      readonly socketId?: never;
      readonly admissionId?: never;
    };

/** The `force-disconnect` payload the evicted socket receives. */
export type ForceDisconnectPayload =
  | { channelId: string; reason: 'access_revoked' }
  | { channelId: string; reason: 'media_policy'; retryAfterSec: number }
  | { channelId: string; reason: 'stats_unavailable' };

interface ResolvedReason {
  payload: ForceDisconnectPayload;
  event: SecurityEventInput;
  logMessage: string;
  logDetail: Record<string, number>;
}

function unhandledReason(_options: never): never {
  throw new Error('Unhandled force-disconnect reason');
}

/** Exhaustive by construction: a new reason is a compile error here, not a mislabelled event. */
function resolveReason(channelId: string, options: ForceDisconnectOptions): ResolvedReason {
  switch (options.reason) {
    case 'access_revoked':
      return {
        payload: { channelId, reason: 'access_revoked' },
        event: {
          eventType: 'media_authorization',
          outcome: 'success',
          severity: 'high',
          reasonCode: 'revocation_enforced',
          routeTemplate: 'socket.force_disconnect',
        },
        logMessage: 'Force-disconnected participant via voice.enforce.disconnect',
        logDetail: {},
      };
    case 'media_policy':
      return {
        payload: { channelId, reason: 'media_policy', retryAfterSec: options.retryAfterSec },
        event: {
          eventType: 'media_admission',
          outcome: 'denied',
          severity: 'high',
          reasonCode: 'structural_limit_exceeded',
          routeTemplate: 'socket.force_disconnect',
        },
        logMessage: 'Media policer evicted participant',
        logDetail: { retryAfterSec: options.retryAfterSec },
      };
    case 'stats_unavailable':
      return {
        payload: { channelId, reason: 'stats_unavailable' },
        event: {
          eventType: 'security_control',
          outcome: 'degraded',
          severity: 'high',
          reasonCode: 'dependency_unavailable',
          routeTemplate: 'socket.force_disconnect',
        },
        logMessage: 'Media policer closed session after persistent stats failures',
        logDetail: {},
      };
    default:
      return unhandledReason(options);
  }
}

function resolveExactSessions(
  participant: ReturnType<RoomManager['getParticipant']>,
  provisional: ReturnType<RoomManager['getProvisionalParticipant']>,
  socketId: string | undefined,
  admissionId: string | undefined
): { participant: typeof participant; provisional: typeof provisional; valid: boolean } {
  if ((socketId === undefined) !== (admissionId === undefined)) {
    return { participant: undefined, provisional: undefined, valid: false };
  }
  if (socketId === undefined) {
    return { participant, provisional, valid: true };
  }
  return {
    participant:
      participant?.socketId === socketId && participant.admissionId === admissionId
        ? participant
        : undefined,
    provisional:
      provisional?.socketId === socketId && provisional.admissionId === admissionId
        ? provisional
        : undefined,
    valid: true,
  };
}

function sessionsForCall(
  roomManager: ForceDisconnectRoomManager,
  channelId: string,
  userId: string,
  options: ForceDisconnectOptions
) {
  const participant = roomManager.getParticipant(channelId, userId);
  const provisional = roomManager.getProvisionalParticipant(channelId, userId);
  if (options.reason === 'stats_unavailable') {
    const hasForwardedMedia = [...(participant?.producers.values() ?? [])].some(
      (entry) => !entry.producer.closed
    );
    return {
      participant:
        participant?.sendTransport?.id === options.sendTransportId && hasForwardedMedia
          ? participant
          : undefined,
      provisional: undefined,
    };
  }
  const callId = options.reason === 'access_revoked' ? options.callId : undefined;
  if (callId === undefined) return { participant, provisional };

  const room = roomManager.getRoom(channelId);
  return {
    participant: room?.callId === callId ? participant : undefined,
    provisional:
      room?.pendingDMParticipants.get(userId)?.callId === callId ? provisional : undefined,
  };
}

type CapturedForceDisconnectSocket = {
  socket: ReturnType<ForceDisconnectIO['sockets']['sockets']['get']>;
  session: VoiceEnforcementSession | undefined;
  userId: unknown;
  credentialEpoch: unknown;
  roomId: unknown;
  sessionGeneration: string | undefined;
  admissionId: string | undefined;
};

function matchesCapturedSocket(
  io: ForceDisconnectIO,
  socketId: string,
  captured: CapturedForceDisconnectSocket | undefined
): boolean {
  const socket = captured?.socket;
  if (
    !socket ||
    io.sockets.sockets.get(socketId) !== socket ||
    socket.data?.userId !== captured.userId ||
    socket.data?.credentialEpoch !== captured.credentialEpoch ||
    (socket.data?.roomId !== undefined && socket.data.roomId !== captured.roomId)
  ) {
    return false;
  }
  const currentSession = socket.data?.voiceEnforcementSession;
  if (currentSession === captured.session) return true;
  // A1 records this generation before the registration response installs the
  // socket session. Only that exact pending registration may complete here.
  return (
    captured.session === undefined &&
    captured.sessionGeneration !== undefined &&
    currentSession?.sessionGeneration === captured.sessionGeneration &&
    currentSession.socketId === socketId &&
    currentSession.userId === captured.userId &&
    currentSession.credentialEpoch === captured.credentialEpoch &&
    currentSession.roomId === captured.roomId
  );
}

function disconnectCapturedSocket(
  roomManager: ForceDisconnectRoomManager,
  io: ForceDisconnectIO,
  socketId: string,
  captured: CapturedForceDisconnectSocket | undefined,
  channelId: string,
  userId: string,
  payload: ForceDisconnectPayload
): void {
  if (!matchesCapturedSocket(io, socketId, captured)) return;
  const successorOwnsSocket = [
    roomManager.getParticipant(channelId, userId),
    roomManager.getProvisionalParticipant(channelId, userId),
  ].some(
    (current) =>
      current?.socketId === socketId &&
      (current.voiceEnforcementSessionGeneration !== captured?.sessionGeneration ||
        current.admissionId !== captured?.admissionId)
  );
  if (successorOwnsSocket) {
    return;
  }
  const socket = captured?.socket;
  if (!socket) return;
  socket.emit('force-disconnect', payload);
  socket.disconnect(true);
}

/**
 * Evicts a peer from one room: `voice.enforce.disconnect` from the control
 * plane (#487 P3, reason `access_revoked`) or the media policer's repeat-offender
 * eviction (#2153, reason `media_policy`).
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
  emit: EmitSecurityEvent | undefined,
  options: ForceDisconnectOptions
): Promise<void> {
  // Resolved before any teardown, so a malformed reason touches no session.
  const resolved = resolveReason(channelId, options);
  const { participant, provisional } = sessionsForCall(roomManager, channelId, userId, options);
  const exact = resolveExactSessions(
    participant,
    provisional,
    options.socketId,
    options.admissionId
  );
  if (!exact.valid) return;
  const { participant: exactParticipant, provisional: exactProvisional } = exact;
  if (!exactParticipant && !exactProvisional) {
    await roomManager.awaitNativeTermination(channelId, userId, options.socketId);
    return;
  }

  // Cancel the captured A1 state before disconnecting either socket. Socket.IO
  // fires the admitted socket's disconnect handler synchronously, so this keeps
  // its terminal decision correct. The enforcement-specific seam also finalizes
  // a room that was already pending-only: silently without admitted history, or
  // through normal terminal lifecycle with history. Exact ownership prevents
  // this command from deleting a successor session.
  const capturedSockets = new Map(
    [exactParticipant, exactProvisional]
      .filter((target) => target !== undefined)
      .map((target) => {
        const socket = io.sockets.sockets.get(target.socketId);
        return [
          target.socketId,
          {
            socket,
            session: socket?.data?.voiceEnforcementSession,
            userId: socket?.data?.userId,
            credentialEpoch: socket?.data?.credentialEpoch,
            roomId: channelId,
            sessionGeneration: target.voiceEnforcementSessionGeneration,
            admissionId: target.admissionId,
          },
        ] as const;
      })
  );
  let changed = false;
  if (exactProvisional) {
    changed = await roomManager.removeProvisionalParticipantForEnforcement(
      channelId,
      userId,
      exactProvisional.socketId,
      exactProvisional.voiceEnforcementSessionGeneration
    );
  }

  // Tear down the captured admitted session before force-closing its socket.
  // Socket.IO dispatches disconnect cleanup synchronously, which would otherwise
  // erase exact ownership before this authoritative RoomManager call can run.
  if (exactParticipant) {
    changed =
      (await roomManager.leaveRoomIfSocketOwned(
        channelId,
        userId,
        exactParticipant.socketId,
        exactParticipant.voiceEnforcementSessionGeneration
      )) || changed;
  }

  for (const target of [exactProvisional, exactParticipant].filter(
    (target): target is NonNullable<typeof target> => Boolean(target)
  )) {
    await roomManager.awaitNativeTermination(
      channelId,
      userId,
      target.socketId,
      target.voiceEnforcementSessionGeneration
    );
  }

  // Tell every admitted/provisional session to leave, then force each socket
  // closed. A Set handles the defensive case where both registries name one ID.
  const socketIds = new Set<string>();
  if (exactParticipant) socketIds.add(exactParticipant.socketId);
  if (exactProvisional) socketIds.add(exactProvisional.socketId);
  for (const socketId of socketIds) {
    disconnectCapturedSocket(
      roomManager,
      io,
      socketId,
      capturedSockets.get(socketId),
      channelId,
      userId,
      resolved.payload
    );
  }

  if (changed) {
    try {
      emit?.(resolved.event);
    } catch {
      // An audit observer cannot alter an authoritative teardown.
    }
  }

  if (options.reason === 'stats_unavailable') {
    // Policer operational logs are aggregate-only. The security event above
    // records the reason without adding room/user/transport identifiers.
    logger.info(resolved.logMessage, { reason: options.reason });
  } else {
    logger.info(resolved.logMessage, { channelId, userId, ...resolved.logDetail });
  }
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

    await handleForceDisconnect(roomManager, io, channelId, userId, undefined, {
      reason: 'access_revoked',
    });
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
  const capturedSockets = [...io.sockets.sockets]
    .filter(
      ([, socket]) =>
        socket.data?.userId === userId && socket.data.credentialEpoch === supersededCredentialEpoch
    )
    .map(([socketId, socket]) => {
      const matchingTargets = sessions.filter((session) => session.socketId === socketId);
      const target = matchingTargets.length === 1 ? matchingTargets[0] : undefined;
      return {
        socketId,
        socket,
        session: socket.data?.voiceEnforcementSession,
        userId: socket.data?.userId,
        credentialEpoch: socket.data?.credentialEpoch,
        roomId:
          target?.roomId ?? socket.data?.voiceEnforcementSession?.roomId ?? socket.data?.roomId,
        sessionGeneration: target?.sessionGeneration,
        admissionId: undefined,
      };
    });
  for (const session of sessions) {
    if (session.provisional) {
      await roomManager.removeProvisionalParticipantForEnforcement(
        session.roomId,
        userId,
        session.socketId,
        session.sessionGeneration
      );
    }
  }
  for (const session of sessions) {
    if (!session.provisional) {
      await roomManager.leaveRoomIfSocketOwned(
        session.roomId,
        userId,
        session.socketId,
        session.sessionGeneration
      );
    }
    await roomManager.awaitNativeTermination(
      session.roomId,
      userId,
      session.socketId,
      session.sessionGeneration
    );
  }
  await roomManager.awaitNativeTerminationForCredentialEpoch(userId, supersededCredentialEpoch);
  const remainingSessions = roomManager.getSupersededCredentialEpochSessions(
    userId,
    supersededCredentialEpoch
  );
  for (const captured of capturedSockets) {
    const { socket, socketId } = captured;
    if (
      !matchesCapturedSocket(io, socketId, captured) ||
      remainingSessions.some(
        (current) =>
          current.socketId === socketId &&
          (current.roomId !== captured.roomId ||
            current.sessionGeneration !== captured.sessionGeneration)
      )
    )
      continue;
    const roomId = typeof captured.roomId === 'string' ? captured.roomId : '';
    socket.emit('force-disconnect', { channelId: roomId, reason: 'credential_rotated' });
    socket.disconnect(true);
  }
}
