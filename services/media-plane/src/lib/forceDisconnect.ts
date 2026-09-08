import type { RoomManager } from './roomManager.js';
import { logger } from './logger.js';
import type { EmitSecurityEvent } from './securityEvent.js';

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

/**
 * Minimal Socket.IO surface needed to evict a peer's live socket.
 */
export interface ForceDisconnectIO {
  sockets: {
    sockets: Map<
      string,
      { emit: (event: string, ...args: unknown[]) => void; disconnect: (close?: boolean) => void }
    >;
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
