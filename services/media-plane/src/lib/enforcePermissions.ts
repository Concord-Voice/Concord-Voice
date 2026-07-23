import type { RoomManager } from './roomManager.js';
import { hasVoiceAccess } from './roomManager.js';
import { handleForceDisconnect } from './forceDisconnect.js';
import { logger } from './logger.js';
import { parsePermissionBitfield } from '../middleware/auth.js';

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
  updateParticipantPermissions: RoomManager['updateParticipantPermissions'];
  closeForbiddenProducers: RoomManager['closeForbiddenProducers'];
  leaveRoom: RoomManager['leaveRoom'];
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
      { emit: (event: string, ...args: unknown[]) => void; disconnect: (close?: boolean) => void }
    >;
  };
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
  permissions: bigint
): Promise<void> {
  const participant = roomManager.getParticipant(channelId, userId);
  if (!participant) {
    // Already gone — nothing to enforce. Idempotent.
    return;
  }

  if (!roomManager.updateParticipantPermissions(channelId, userId, permissions)) {
    // DM room (no server permission model) or participant raced out — do not
    // bolt a permission model onto a room that never had one.
    return;
  }

  // Losing either voice-access bit (ViewVoiceChannels | JoinVoice) means a fresh
  // AuthorizeJoin would now reject this user, so they have no right to remain in
  // the room at all. Closing their producers is not enough: the socket keeps its
  // recv transports/consumers and would still receive future new-producer events
  // (consume does not re-check voice access). Force-disconnect them via the same
  // authoritative teardown the control-plane's voice.enforce.disconnect uses
  // (leaveRoom closes transports/producers/consumers and emits user-left).
  // Administrator bypasses (mirrors publishPermitted / rbac.Permission.Has).
  if (!hasVoiceAccess(permissions)) {
    await handleForceDisconnect(roomManager, io, channelId, userId);
    logger.info('Force-disconnected peer on mid-session voice-access revocation', {
      channelId,
      userId,
    });
    return;
  }

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
 * validates the raw NATS payload (string channelId/userId, strict fail-closed
 * decimal bitfield via parsePermissionBitfield) before dispatching to
 * handlePermissionsUpdate. A malformed payload is IGNORED — enforcement never
 * fails open, and a bad message never strips a legitimate peer. Well-formed
 * pushes are serialized per participant so the last published bitfield wins even
 * when the NATS handler fires them concurrently.
 */
export async function handleEnforcePermissionsMessage(
  roomManager: EnforcePermissionsRoomManager,
  io: EnforcePermissionsIO,
  natsData: Record<string, unknown>
): Promise<void> {
  const channelId = natsData.channelId;
  const userId = natsData.userId;
  if (typeof channelId !== 'string' || typeof userId !== 'string') return;
  const permissions = parsePermissionBitfield(natsData.permissions);
  if (permissions === undefined) {
    logger.warn('Ignoring malformed voice.enforce.permissions payload', { channelId, userId });
    return;
  }

  // Serialize per participant so back-to-back pushes apply in publish order. A
  // prior update that rejected must not block or reject the next one, so isolate
  // it with .catch before chaining this update onto the tail.
  const key = `${channelId}:${userId}`;
  const prior = permissionUpdateChains.get(key) ?? Promise.resolve();
  const next = prior
    .catch(() => undefined)
    .then(() => handlePermissionsUpdate(roomManager, io, channelId, userId, permissions));
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
