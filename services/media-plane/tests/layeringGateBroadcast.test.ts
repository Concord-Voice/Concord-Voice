/**
 * Tests the CONSUMER of the camera-layering-gate event, not the emitter.
 *
 * Codex's #3275 review named the gap exactly: the RoomManager tests assert the
 * event carries `targetSocketId`, so restoring a room-wide broadcast in the
 * consumer would leave all three of them green while late joiners still miss the
 * snapshot. These observe the actual Socket.IO recipient instead.
 */
import { describe, it, expect, vi } from 'vitest';
import {
  cameraGateRecipient,
  emitCameraLayeringGate,
} from '@/lib/layeringGateBroadcast';

function recordingIo() {
  const emit = vi.fn();
  const to = vi.fn(() => ({ emit }));
  return { io: { to }, to, emit };
}

describe('camera-layering-gate broadcast routing', () => {
  it('delivers a joiner snapshot to that socket and NOT to the room', () => {
    const { io, to, emit } = recordingIo();

    emitCameraLayeringGate(io, { roomId: 'room-1', enabled: true, targetSocketId: 'sock-late' });

    expect(to).toHaveBeenCalledTimes(1);
    expect(to).toHaveBeenCalledWith('sock-late');
    // The assertion the RoomManager tests structurally cannot make: the room is
    // never the recipient of a snapshot.
    expect(to).not.toHaveBeenCalledWith('room-1');
    expect(emit).toHaveBeenCalledWith('camera-layering-gate', { enabled: true });
  });

  it('delivers a real transition to the whole room', () => {
    const { io, to, emit } = recordingIo();

    emitCameraLayeringGate(io, { roomId: 'room-1', enabled: false });

    expect(to).toHaveBeenCalledWith('room-1');
    expect(emit).toHaveBeenCalledWith('camera-layering-gate', { enabled: false });
  });

  it('treats a malformed empty socket id as no target rather than a silent broadcast', () => {
    // Cannot arrive today (participant.socketId is always populated), but if it
    // ever did, `??` alone would route a PRIVATE snapshot to the whole room.
    expect(cameraGateRecipient({ roomId: 'room-1', enabled: true, targetSocketId: '' })).toBe(
      'room-1'
    );
    expect(cameraGateRecipient({ roomId: 'room-1', enabled: true })).toBe('room-1');
    expect(cameraGateRecipient({ roomId: 'room-1', enabled: true, targetSocketId: 's' })).toBe('s');
  });
});
