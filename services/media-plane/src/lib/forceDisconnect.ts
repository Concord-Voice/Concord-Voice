import type { RoomManager } from './roomManager.js';
import { logger } from './logger.js';

/**
 * Minimal RoomManager surface needed to force-disconnect a peer. Declared as an
 * interface (rather than the full RoomManager) so unit tests can inject a fake.
 */
export interface ForceDisconnectRoomManager {
  getParticipant: RoomManager['getParticipant'];
  leaveRoom: RoomManager['leaveRoom'];
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
 *   1. Notifies the peer's live socket (so the client tears down its WebRTC state)
 *      and force-disconnects that socket.
 *   2. Delegates room teardown to RoomManager.leaveRoom — which closes the peer's
 *      transports/producers/consumers and emits `user-left`. The NATS room-event
 *      bridge turns that into the normal `voice.left` published back to the control
 *      plane, so `voice_participants` is cleaned and `voice_state_update` broadcasts.
 *
 * Per [internal]rules/media-plane.md it reuses RoomManager.leaveRoom rather than
 * recreating transport-cleanup logic. Idempotent: a no-op if the peer is not in the
 * room (already left).
 */
export async function handleForceDisconnect(
  roomManager: ForceDisconnectRoomManager,
  io: ForceDisconnectIO,
  channelId: string,
  userId: string
): Promise<void> {
  const participant = roomManager.getParticipant(channelId, userId);
  if (!participant) {
    // Already gone — nothing to evict. Idempotent.
    return;
  }

  // Tell the peer's live socket to leave, then force the socket closed so the
  // client's WebRTC transports tear down even if it ignores the event.
  const socket = io.sockets.sockets.get(participant.socketId);
  if (socket) {
    socket.emit('force-disconnect', { channelId, reason: 'access_revoked' });
    socket.disconnect(true);
  }

  // Authoritative SFU-side teardown (closes transports/producers/consumers,
  // removes the participant, emits user-left -> voice.left via the NATS bridge).
  await roomManager.leaveRoom(channelId, userId);

  logger.info('Force-disconnected participant via voice.enforce.disconnect', {
    channelId,
    userId,
  });
}
