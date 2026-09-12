/**
 * Socket.IO routing for the two layering-gate events.
 *
 * Extracted from index.ts's `roomManager.onEvent` closure because that file is
 * excluded from coverage by config (`vitest.config.ts` → `coverage.exclude`),
 * so any decision made inside it is structurally untestable. The decision here
 * is small but load-bearing: a camera gate SNAPSHOT must reach one arriving
 * socket, while a real gate TRANSITION must reach the whole room.
 *
 * Codex's #3275 review is what forced this. The RoomManager tests assert that
 * the event carries `targetSocketId`, which is the emitter's half — revert the
 * consumer to `io.to(event.roomId)` and all three stay green while late joiners
 * still miss the snapshot. Testing the handshake is not testing the consumer.
 */

/** The narrow slice of Socket.IO this module needs — a recording fake satisfies
 *  it in tests, and the real `Server` satisfies it structurally. */
export interface GateBroadcastTarget {
  to(room: string): { emit(event: string, payload: unknown): void };
}

export interface CameraGateEvent {
  readonly roomId: string;
  readonly enabled: boolean;
  /** Present only for a per-socket state snapshot (a joiner). */
  readonly targetSocketId?: string;
}

/**
 * Room-wide for a transition; one socket for a snapshot.
 *
 * The `??` is deliberate rather than a truthiness check: an empty-string socket
 * id is a malformed target, and falling back to the room would silently turn a
 * private snapshot into a broadcast. It cannot arrive today — `participant.socketId`
 * is always populated — but the failure mode if it ever did is the wrong one.
 */
export function cameraGateRecipient(event: CameraGateEvent): string {
  const target = event.targetSocketId;
  return target !== undefined && target !== '' ? target : event.roomId;
}

export function emitCameraLayeringGate(io: GateBroadcastTarget, event: CameraGateEvent): void {
  io.to(cameraGateRecipient(event)).emit('camera-layering-gate', { enabled: event.enabled });
}
