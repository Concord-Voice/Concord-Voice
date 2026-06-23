import type { RoomManager } from './roomManager.js';

export interface SetTestingStatusRoomManager {
  setParticipantTestingStatus: RoomManager['setParticipantTestingStatus'];
  getParticipant: RoomManager['getParticipant'];
}

export interface SetTestingStatusSocket {
  id: string;
  to: (room: string) => { emit: (event: string, ...args: unknown[]) => void };
}

export type SetTestingStatusResult = { success: true } | { error: string };

export function handleSetTestingStatus(
  roomManager: SetTestingStatusRoomManager,
  socket: SetTestingStatusSocket,
  roomId: string | undefined,
  userId: string,
  payload: unknown
): SetTestingStatusResult {
  if (!roomId) return { error: 'Not in a room' };
  const isTesting =
    payload && typeof payload === 'object'
      ? (payload as { isTesting?: unknown }).isTesting
      : undefined;
  if (typeof isTesting !== 'boolean') return { error: 'invalid_payload' };

  const participant = roomManager.getParticipant(roomId, userId);
  if (participant?.socketId !== socket.id) return { error: 'Not in a room' };
  if (participant.isTesting === isTesting) return { success: true };

  roomManager.setParticipantTestingStatus(roomId, userId, isTesting);
  socket.to(roomId).emit('participant-testing-changed', { userId, isTesting });
  return { success: true };
}
