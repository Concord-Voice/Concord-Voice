import type { RoomManager } from './roomManager.js';

/** Minimal RoomManager surface needed by the close-recv-transport socket handler. */
export interface CloseRecvTransportRoomManager {
  closeRecvTransport: RoomManager['closeRecvTransport'];
}

export type CloseRecvTransportResult = { success: true } | { error: string };

/** Send a Socket.IO acknowledgement only when the client supplied a callback. */
export function acknowledgeCloseRecvTransport(
  callback: unknown,
  result: CloseRecvTransportResult
): void {
  if (typeof callback !== 'function') return;
  (callback as (value: CloseRecvTransportResult) => void)(result);
}

function getTransportId(payload: unknown): string | undefined {
  if (!payload || typeof payload !== 'object' || !('transportId' in payload)) {
    return undefined;
  }

  const transportId = (payload as { transportId?: unknown }).transportId;
  if (typeof transportId !== 'string') return undefined;

  const trimmed = transportId.trim();
  return trimmed === '' ? undefined : trimmed;
}

/**
 * Validate and apply an idempotent receive-transport close request. Room and
 * user identity come from authenticated socket state; payload identity fields
 * are intentionally ignored. Unknown and non-owned IDs receive the same success
 * acknowledgement so callers cannot enumerate another participant's resources.
 */
export function handleCloseRecvTransport(
  roomManager: CloseRecvTransportRoomManager,
  roomId: string | undefined,
  userId: string,
  payload: unknown
): CloseRecvTransportResult {
  if (!roomId) return { error: 'Not in a room' };

  const transportId = getTransportId(payload);
  if (!transportId) return { error: 'transportId is required' };

  roomManager.closeRecvTransport(roomId, userId, transportId);
  return { success: true };
}
