import type { RoomManager } from './roomManager.js';

/**
 * Sliding-window rate limit for testing-status broadcasts (#2030 — the
 * interim mitigation promised by #1163 spec §7.5). The media-plane has no
 * general per-socket rate limiter (known gap), and every STATE-CHANGING
 * call here costs a room-wide broadcast. Only state changes count — the
 * same-state idempotence guard already absorbs duplicates — so a
 * legitimate start/stop test cycle (2 changes) never trips the budget.
 */
export const TESTING_STATUS_WINDOW_MS = 3000;
export const TESTING_STATUS_MAX_CHANGES = 6;

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

  // Sliding-window rate limit (#2030): reject a state-change flood before
  // it mutates state or broadcasts. Timestamps live on the participant so
  // the limiter state is freed with the participant (no module-level map).
  const now = Date.now();
  const recentChanges = (participant.testingStatusChangeTimes ?? []).filter(
    (t) => now - t < TESTING_STATUS_WINDOW_MS
  );
  if (recentChanges.length >= TESTING_STATUS_MAX_CHANGES) {
    participant.testingStatusChangeTimes = recentChanges;
    return { error: 'rate_limited' };
  }
  recentChanges.push(now);
  participant.testingStatusChangeTimes = recentChanges;

  roomManager.setParticipantTestingStatus(roomId, userId, isTesting);
  socket.to(roomId).emit('participant-testing-changed', { userId, isTesting });
  return { success: true };
}
