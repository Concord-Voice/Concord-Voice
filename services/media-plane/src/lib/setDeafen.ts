import type { RoomManager } from './roomManager.js';

/** Minimal RoomManager surface needed to record a self-deafen state change. */
export interface SetDeafenRoomManager {
  setParticipantDeafen: RoomManager['setParticipantDeafen'];
}

/** Minimal Socket.IO surface needed to broadcast the change to the rest of the room. */
export interface SetDeafenSocket {
  to: (room: string) => { emit: (event: string, ...args: unknown[]) => void };
}

/** Outcome of a set-deafen request, returned to the client via the socket ack. */
export type SetDeafenResult = { success: true } | { error: string };

/**
 * Apply a client's self-deafen choice (#685): record the authoritative flag on
 * the room participant and broadcast `participant-deafen-changed` to the rest of
 * the room so their sidebars reflect it — mirroring the self-mute
 * `pause-producer` → `producer-paused` flow. Kept free of socket-ack plumbing
 * (it returns a result the caller acks) so it is unit-testable without a live
 * Socket.IO server, following the `handleForceDisconnect` pattern.
 *
 * Idempotent and validates the wire payload: a non-boolean `isDeafened` or a
 * socket not yet in a room is rejected without mutating state.
 */
export function handleSetDeafen(
  roomManager: SetDeafenRoomManager,
  socket: SetDeafenSocket,
  roomId: string | undefined,
  userId: string,
  isDeafened: unknown
): SetDeafenResult {
  if (!roomId) return { error: 'Not in a room' };
  if (typeof isDeafened !== 'boolean') return { error: 'invalid_payload' };

  roomManager.setParticipantDeafen(roomId, userId, isDeafened);
  socket.to(roomId).emit('participant-deafen-changed', { userId, isDeafened });
  return { success: true };
}
