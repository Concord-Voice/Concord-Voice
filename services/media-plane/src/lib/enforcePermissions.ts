import type { RoomManager } from './roomManager.js';
import { hasVoiceAccess } from './roomManager.js';
import { isCanonicalEnforcementUUID } from './enforcementCommand.js';
import { logger } from './logger.js';
import { parsePermissionBitfield } from '../middleware/auth.js';
import type { EmitSecurityEvent } from './securityEvent.js';

/**
 * Minimal RoomManager surface needed to apply a mid-session permission push.
 * Declared as an interface (rather than the full RoomManager) so unit tests can
 * inject a fake — mirrors ForceDisconnectRoomManager. `leaveRoom` is included so
 * a voice-access revocation can reuse the authoritative force-disconnect
 * teardown (this interface structurally satisfies ForceDisconnectRoomManager).
 */
export interface EnforcePermissionsRoomManager {
  getParticipant: RoomManager['getParticipant'];
  getProvisionalParticipantSocketId: RoomManager['getProvisionalParticipantSocketId'];
  updateProvisionalParticipantPermissions: RoomManager['updateProvisionalParticipantPermissions'];
  updateParticipantPermissions: RoomManager['updateParticipantPermissions'];
  closeForbiddenProducers: RoomManager['closeForbiddenProducers'];
  leaveRoom: RoomManager['leaveRoom'];
  leaveRoomIfSocketOwned: RoomManager['leaveRoomIfSocketOwned'];
  removeProvisionalParticipantForEnforcement: RoomManager['removeProvisionalParticipantForEnforcement'];
}

/**
 * Minimal Socket.IO surface needed to notify the affected peer and, on a
 * voice-access revocation, force its socket closed (structurally satisfies
 * ForceDisconnectIO).
 */
export interface EnforcePermissionsIO {
  sockets: {
    sockets: Map<
      string,
      {
        emit: (event: string, ...args: unknown[]) => void;
        disconnect: (close?: boolean) => void;
        data?: { userId?: string; roomId?: string };
      }
    >;
  };
}

function safeObserveSecurityEvent(
  emit: EmitSecurityEvent | undefined,
  event: Parameters<EmitSecurityEvent>[0]
): void {
  try {
    emit?.(event);
  } catch {
    // Permission enforcement remains authoritative over its observer.
  }
}

function updateParticipantPermissionSnapshot(
  roomManager: EnforcePermissionsRoomManager,
  channelId: string,
  userId: string,
  permissions: bigint,
  authorizationRevision: bigint | undefined
): boolean {
  return authorizationRevision === undefined
    ? roomManager.updateParticipantPermissions(channelId, userId, permissions)
    : roomManager.updateParticipantPermissions(
        channelId,
        userId,
        permissions,
        undefined,
        authorizationRevision
      );
}

function updateProvisionalPermissionSnapshot(
  roomManager: EnforcePermissionsRoomManager,
  channelId: string,
  userId: string,
  permissions: bigint,
  authorizationRevision: bigint | undefined
): boolean {
  return authorizationRevision === undefined
    ? roomManager.updateProvisionalParticipantPermissions(channelId, userId, permissions)
    : roomManager.updateProvisionalParticipantPermissions(
        channelId,
        userId,
        permissions,
        authorizationRevision
      );
}

function resolvePermissionUpdateArguments(
  emitOrAuthorizationRevision: EmitSecurityEvent | bigint | undefined,
  authorizationRevisionOrEmit: bigint | EmitSecurityEvent | undefined
): { emit: EmitSecurityEvent | undefined; authorizationRevision: bigint | undefined } {
  let emit: EmitSecurityEvent | undefined;
  if (typeof emitOrAuthorizationRevision === 'function') {
    emit = emitOrAuthorizationRevision;
  } else if (typeof authorizationRevisionOrEmit === 'function') {
    emit = authorizationRevisionOrEmit;
  }
  let authorizationRevision: bigint | undefined;
  if (typeof emitOrAuthorizationRevision === 'bigint') {
    authorizationRevision = emitOrAuthorizationRevision;
  } else if (typeof authorizationRevisionOrEmit === 'bigint') {
    authorizationRevision = authorizationRevisionOrEmit;
  }
  return { emit, authorizationRevision };
}

async function disconnectRevokedVoiceAccess({
  roomManager,
  io,
  channelId,
  userId,
  participant,
  participantPermissionsUpdated,
  provisionalSocketId,
  provisionalPermissionsUpdated,
}: {
  roomManager: EnforcePermissionsRoomManager;
  io: EnforcePermissionsIO;
  channelId: string;
  userId: string;
  participant: ReturnType<RoomManager['getParticipant']>;
  participantPermissionsUpdated: boolean;
  provisionalSocketId: string | undefined;
  provisionalPermissionsUpdated: boolean;
}): Promise<void> {
  const activeSocketIDs = new Set<string>();
  if (participantPermissionsUpdated && participant) {
    activeSocketIDs.add(participant.socketId);
    for (const [socketId, socket] of io.sockets.sockets) {
      if (socket.data?.userId === userId && socket.data.roomId === channelId) {
        activeSocketIDs.add(socketId);
      }
    }
  }
  if (provisionalPermissionsUpdated && provisionalSocketId) {
    await roomManager.removeProvisionalParticipantForEnforcement(
      channelId,
      userId,
      provisionalSocketId
    );
    io.sockets.sockets.get(provisionalSocketId)?.disconnect(true);
  }
  if (participantPermissionsUpdated && participant) {
    await roomManager.leaveRoomIfSocketOwned(channelId, userId, participant.socketId);
    for (const socketId of activeSocketIDs) io.sockets.sockets.get(socketId)?.disconnect(true);
  }
}

/**
 * Handles a `voice.enforce.permissions` command from the control plane
 * (CV-CAN-007 review P1 — mid-session revocation).
 *
 * The produce() gate keys on the permission snapshot captured at join, so an
 * RBAC mutation (role edit/unassign, channel override) would otherwise not
 * bind on a connected peer until rejoin. The control plane re-resolves the
 * effective bitfield after each mutation and pushes it here; the handler:
 *   1. Replaces the participant's snapshot (updateParticipantPermissions —
 *      a no-op for DM rooms, which carry no server permission model).
 *   2. If the new bitfield no longer clears the voice-access gate
 *      (ViewVoiceChannels | JoinVoice), force-disconnects the peer — a fresh
 *      AuthorizeJoin would reject them, so closing producers alone is not
 *      enough (they could keep consuming and receive future new-producer
 *      events). Reuses the voice.enforce.disconnect teardown and returns.
 *   3. Otherwise audits live producers and closes any whose required publish
 *      bit was revoked (closeForbiddenProducers). Each close rides the normal
 *      producer-removed path, so `producer-closed` fans out to the room and
 *      forwarding stops server-side regardless of client cooperation.
 *   4. Emits `permissions-changed` to the affected peer's socket so its UI can
 *      stop local capture (the camera/mic light) without waiting for it to
 *      notice the producer close.
 *
 * Grants propagate too: the snapshot is replaced, not intersected, so a newly
 * granted member can publish without rejoining. Idempotent: a no-op if the
 * peer is not in the room.
 */
export async function handlePermissionsUpdate(
  roomManager: EnforcePermissionsRoomManager,
  io: EnforcePermissionsIO,
  channelId: string,
  userId: string,
  permissions: bigint,
  emitOrAuthorizationRevision?: EmitSecurityEvent | bigint,
  authorizationRevisionOrEmit?: bigint | EmitSecurityEvent
): Promise<void> {
  const { emit, authorizationRevision } = resolvePermissionUpdateArguments(
    emitOrAuthorizationRevision,
    authorizationRevisionOrEmit
  );
  const participant = roomManager.getParticipant(channelId, userId);
  const provisionalSocketId = roomManager.getProvisionalParticipantSocketId(channelId, userId);
  if (!participant && !provisionalSocketId) return;

  // This method is deliberately a no-op for DMs. Check that condition before
  // interpreting a server-RBAC bitfield as a reason to disconnect the peer.
  let participantPermissionsUpdated = false;
  if (participant) {
    participantPermissionsUpdated = updateParticipantPermissionSnapshot(
      roomManager,
      channelId,
      userId,
      permissions,
      authorizationRevision
    );
    if (!participantPermissionsUpdated && !provisionalSocketId) return;
  }

  // A voice-access revoke applies to both an established session and a staged
  // reconnect for the same user. The shared teardown removes the exact staged
  // socket and leaves the admitted session, if any.
  let provisionalPermissionsUpdated = false;
  if (provisionalSocketId) {
    // Preserve a post-A2 enforcement snapshot for promotion. The staged
    // candidate is not yet a participant, so producer auditing is inapplicable.
    provisionalPermissionsUpdated = updateProvisionalPermissionSnapshot(
      roomManager,
      channelId,
      userId,
      permissions,
      authorizationRevision
    );
  }

  if (!hasVoiceAccess(permissions)) {
    // Tear down only records that accepted this snapshot; never broad-scan a
    // legacy message into a newer versioned successor.
    await disconnectRevokedVoiceAccess({
      roomManager,
      io,
      channelId,
      userId,
      participant,
      participantPermissionsUpdated,
      provisionalSocketId,
      provisionalPermissionsUpdated,
    });
    safeObserveSecurityEvent(emit, {
      eventType: 'media_authorization',
      outcome: 'success',
      severity: 'high',
      reasonCode: 'revocation_enforced',
      routeTemplate: 'socket.permissions_update',
    });
    logger.info('Force-disconnected peer on mid-session voice-access revocation', {
      channelId,
      userId,
    });
    return;
  }

  if (!participant || !participantPermissionsUpdated) return;

  const closedSources = await roomManager.closeForbiddenProducers(channelId, userId);

  const socket = io.sockets.sockets.get(participant.socketId);
  if (socket) {
    socket.emit('permissions-changed', {
      channelId,
      permissions: permissions.toString(),
      closedSources,
    });
  }

  if (closedSources.length > 0) {
    safeObserveSecurityEvent(emit, {
      eventType: 'media_authorization',
      outcome: 'success',
      severity: 'high',
      reasonCode: 'revocation_enforced',
      routeTemplate: 'socket.permissions_update',
    });
    logger.info('Closed producers on mid-session permission revocation', {
      channelId,
      userId,
      closedSources,
    });
  }
}

/**
 * Per-(channel,user) serialization for permission pushes. NatsService.subscribe
 * launches the enforce handler without awaiting the returned promise, so two
 * pushes for the same participant can overlap. Because handlePermissionsUpdate
 * replaces the snapshot before awaiting producer closure, an interleaved
 * revoke -> grant could let the stale revoke resume and close producers the
 * final bitfield actually allows. Chaining updates per participant guarantees
 * the last published bitfield wins. Keys are pruned once their chain drains, so
 * the map only ever holds in-flight participants.
 */
const permissionUpdateChains = new Map<string, Promise<void>>();

/**
 * Message-level entry point for the `voice.enforce.permissions` subscription:
 * validates the raw NATS payload (canonical UUID channelId/userId, strict
 * fail-closed decimal bitfield via parsePermissionBitfield) before dispatching to
 * handlePermissionsUpdate. A malformed payload is IGNORED — enforcement never
 * fails open, and a bad message never strips a legitimate peer. Well-formed
 * pushes are serialized per participant so the last published bitfield wins even
 * when the NATS handler fires them concurrently.
 */
export async function handleEnforcePermissionsMessage(
  roomManager: EnforcePermissionsRoomManager,
  io: EnforcePermissionsIO,
  natsData: Record<string, unknown>,
  emit?: EmitSecurityEvent
): Promise<void> {
  const channelId = natsData.channelId;
  const userId = natsData.userId;
  if (!isCanonicalEnforcementUUID(channelId) || !isCanonicalEnforcementUUID(userId)) {
    safeObserveSecurityEvent(emit, {
      eventType: 'media_integrity',
      outcome: 'denied',
      severity: 'medium',
      reasonCode: 'media_schema_rejected',
      routeTemplate: 'socket.permissions_update',
    });
    return;
  }
  const permissions = parsePermissionBitfield(natsData.permissions);
  if (permissions === undefined) {
    safeObserveSecurityEvent(emit, {
      eventType: 'media_integrity',
      outcome: 'denied',
      severity: 'medium',
      reasonCode: 'media_schema_rejected',
      routeTemplate: 'socket.permissions_update',
    });
    logger.warn('Ignoring malformed voice.enforce.permissions payload', { channelId, userId });
    return;
  }
  const authorizationRevision = parsePermissionBitfield(natsData.authorizationRevision);
  if (natsData.authorizationRevision !== undefined && authorizationRevision === undefined) return;

  // Serialize per participant so back-to-back pushes apply in publish order. A
  // prior update that rejected must not block or reject the next one, so isolate
  // it with .catch before chaining this update onto the tail.
  const key = `${channelId}:${userId}`;
  const prior = permissionUpdateChains.get(key) ?? Promise.resolve();
  const next = prior
    .catch(() => undefined)
    .then(() =>
      handlePermissionsUpdate(
        roomManager,
        io,
        channelId,
        userId,
        permissions,
        emit,
        authorizationRevision
      )
    );
  permissionUpdateChains.set(key, next);
  try {
    await next;
  } finally {
    // Drop the key only when no newer push has chained onto it.
    if (permissionUpdateChains.get(key) === next) {
      permissionUpdateChains.delete(key);
    }
  }
}
