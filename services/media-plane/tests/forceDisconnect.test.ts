import { describe, it, expect, vi, beforeEach } from 'vitest';
import './mocks/logger.js';

import {
  handleForceDisconnect,
  type ForceDisconnectRoomManager,
  type ForceDisconnectIO,
} from '../src/lib/forceDisconnect.js';

const CHANNEL_ID = 'ch-1';
const USER_ID = 'u-1';
const SOCKET_ID = 'socket-abc';

/** Builds a fake RoomManager surface. */
function makeRoomManager(participant: { socketId: string } | undefined) {
  const leaveRoom = vi.fn().mockResolvedValue(undefined);
  const getParticipant = vi.fn().mockReturnValue(participant);
  return {
    rm: { getParticipant, leaveRoom } as unknown as ForceDisconnectRoomManager,
    getParticipant,
    leaveRoom,
  };
}

/** Builds a fake Socket.IO server exposing one socket by id. */
function makeIO(socketId?: string) {
  const emit = vi.fn();
  const disconnect = vi.fn();
  const sockets = new Map<string, { emit: typeof emit; disconnect: typeof disconnect }>();
  if (socketId) {
    sockets.set(socketId, { emit, disconnect });
  }
  return {
    io: { sockets: { sockets } } as unknown as ForceDisconnectIO,
    emit,
    disconnect,
  };
}

describe('handleForceDisconnect (#487 P3)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('evicts the live peer: disconnects the socket and calls leaveRoom', async () => {
    const { rm, getParticipant, leaveRoom } = makeRoomManager({ socketId: SOCKET_ID });
    const { io, emit, disconnect } = makeIO(SOCKET_ID);

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID);

    expect(getParticipant).toHaveBeenCalledWith(CHANNEL_ID, USER_ID);
    // Notifies the client then force-closes the socket.
    expect(emit).toHaveBeenCalledWith('force-disconnect', {
      channelId: CHANNEL_ID,
      reason: 'access_revoked',
    });
    expect(disconnect).toHaveBeenCalledWith(true);
    // Reuses RoomManager.leaveRoom for authoritative teardown (emits user-left -> voice.left).
    expect(leaveRoom).toHaveBeenCalledWith(CHANNEL_ID, USER_ID);
  });

  it('is a no-op when the user is not in the room (idempotent)', async () => {
    const { rm, leaveRoom } = makeRoomManager(undefined);
    const { io, emit, disconnect } = makeIO();

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID);

    expect(leaveRoom).not.toHaveBeenCalled();
    expect(emit).not.toHaveBeenCalled();
    expect(disconnect).not.toHaveBeenCalled();
  });

  it('still calls leaveRoom when the socket is already gone but the participant remains', async () => {
    // Participant tracked in the room, but its socket entry is missing from io
    // (e.g. transport-level disconnect already removed the socket). Teardown must
    // still proceed so the room state is cleaned.
    const { rm, leaveRoom } = makeRoomManager({ socketId: SOCKET_ID });
    const { io } = makeIO(/* no socket registered */);

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID);

    expect(leaveRoom).toHaveBeenCalledWith(CHANNEL_ID, USER_ID);
  });
});
