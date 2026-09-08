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
function makeRoomManager(
  participant: { socketId: string } | undefined,
  provisionalSocketId?: string
) {
  const leaveRoom = vi.fn().mockResolvedValue(undefined);
  const leaveRoomIfSocketOwned = vi.fn().mockResolvedValue(Boolean(participant));
  const getParticipant = vi.fn().mockReturnValue(participant);
  const getProvisionalParticipantSocketId = vi.fn().mockReturnValue(provisionalSocketId);
  const removeProvisionalParticipantIfSocketOwned = vi.fn().mockResolvedValue(true);
  return {
    rm: {
      getParticipant,
      getProvisionalParticipantSocketId,
      leaveRoomIfSocketOwned,
      removeProvisionalParticipantIfSocketOwned,
      // The enforcement-specific seam intentionally shares this spy so these
      // tests remain focused on exact removal regardless of which seam calls it.
      removeProvisionalParticipantForEnforcement: removeProvisionalParticipantIfSocketOwned,
    } as unknown as ForceDisconnectRoomManager,
    getParticipant,
    getProvisionalParticipantSocketId,
    leaveRoom,
    leaveRoomIfSocketOwned,
    removeProvisionalParticipantIfSocketOwned,
  };
}

/** Builds a fake Socket.IO server exposing one socket by id. */
function makeIO(...socketIds: string[]) {
  const emit = vi.fn();
  const disconnect = vi.fn();
  const sockets = new Map<string, { emit: typeof emit; disconnect: typeof disconnect }>();
  for (const socketId of socketIds) {
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
    const { rm, getParticipant, leaveRoomIfSocketOwned } = makeRoomManager({ socketId: SOCKET_ID });
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
    expect(leaveRoomIfSocketOwned).toHaveBeenCalledWith(CHANNEL_ID, USER_ID, SOCKET_ID);
  });

  it('is a no-op when the user is not in the room (idempotent)', async () => {
    const { rm, leaveRoomIfSocketOwned } = makeRoomManager(undefined);
    const { io, emit, disconnect } = makeIO();

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID);

    expect(leaveRoomIfSocketOwned).not.toHaveBeenCalled();
    expect(emit).not.toHaveBeenCalled();
    expect(disconnect).not.toHaveBeenCalled();
  });

  it('still calls leaveRoom when the socket is already gone but the participant remains', async () => {
    // Participant tracked in the room, but its socket entry is missing from io
    // (e.g. transport-level disconnect already removed the socket). Teardown must
    // still proceed so the room state is cleaned.
    const { rm, leaveRoomIfSocketOwned } = makeRoomManager({ socketId: SOCKET_ID });
    const { io } = makeIO(/* no socket registered */);

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID);

    expect(leaveRoomIfSocketOwned).toHaveBeenCalledWith(CHANNEL_ID, USER_ID, SOCKET_ID);
  });

  it('disconnects and silently removes an in-flight DM candidate (#2407)', async () => {
    const pendingSocketId = 'socket-pending';
    const {
      rm,
      leaveRoom,
      getProvisionalParticipantSocketId,
      removeProvisionalParticipantIfSocketOwned,
    } = makeRoomManager(undefined, pendingSocketId);
    const { io, emit, disconnect } = makeIO(pendingSocketId);

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID);

    expect(getProvisionalParticipantSocketId).toHaveBeenCalledWith(CHANNEL_ID, USER_ID);
    expect(emit).toHaveBeenCalledWith('force-disconnect', {
      channelId: CHANNEL_ID,
      reason: 'access_revoked',
    });
    expect(disconnect).toHaveBeenCalledWith(true);
    expect(removeProvisionalParticipantIfSocketOwned).toHaveBeenCalledWith(
      CHANNEL_ID,
      USER_ID,
      pendingSocketId
    );
    expect(leaveRoom).not.toHaveBeenCalled();
  });

  it('evicts both admitted and pending same-user reconnect sessions (#2407)', async () => {
    const pendingSocketId = 'socket-pending';
    const { rm, leaveRoomIfSocketOwned, removeProvisionalParticipantIfSocketOwned } =
      makeRoomManager({ socketId: SOCKET_ID }, pendingSocketId);
    const { io, emit, disconnect } = makeIO(SOCKET_ID, pendingSocketId);

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID);

    expect(emit).toHaveBeenCalledTimes(2);
    expect(disconnect).toHaveBeenCalledTimes(2);
    expect(removeProvisionalParticipantIfSocketOwned).toHaveBeenCalledWith(
      CHANNEL_ID,
      USER_ID,
      pendingSocketId
    );
    expect(leaveRoomIfSocketOwned).toHaveBeenCalledWith(CHANNEL_ID, USER_ID, SOCKET_ID);
  });

  it('tears down exact admitted state before synchronous socket disconnect cleanup (#2407)', async () => {
    const pendingSocketId = 'socket-pending';
    let admitted = true;
    let provisional = true;
    let terminalized = false;

    // Model RoomManager's empty-room rule: an admitted disconnect cannot close
    // a history-bearing room while a provisional reconnect is still present.
    const leaveRoom = vi.fn(async () => {
      if (!admitted) return;
      admitted = false;
      if (!provisional) terminalized = true;
    });
    const removeProvisionalParticipantIfSocketOwned = vi.fn(async () => {
      provisional = false;
      return true;
    });
    const rm = {
      getParticipant: vi.fn(() => (admitted ? { socketId: SOCKET_ID } : undefined)),
      getProvisionalParticipantSocketId: vi.fn(() => (provisional ? pendingSocketId : undefined)),
      leaveRoom,
      leaveRoomIfSocketOwned: vi.fn(async (_roomId: string, _userId: string, socketId: string) => {
        if (socketId !== SOCKET_ID || !admitted) return false;
        await leaveRoom(CHANNEL_ID, USER_ID);
        return true;
      }),
      removeProvisionalParticipantIfSocketOwned,
      removeProvisionalParticipantForEnforcement: removeProvisionalParticipantIfSocketOwned,
    } as unknown as ForceDisconnectRoomManager;

    const oldDisconnect = vi.fn(() => {
      // If force-disconnect still relied on this callback for teardown, exact
      // ownership would already be gone and the revocation verdict suppressed.
      admitted = false;
    });
    const pendingDisconnect = vi.fn();
    const io = {
      sockets: {
        sockets: new Map([
          [SOCKET_ID, { emit: vi.fn(), disconnect: oldDisconnect }],
          [pendingSocketId, { emit: vi.fn(), disconnect: pendingDisconnect }],
        ]),
      },
    } as unknown as ForceDisconnectIO;

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID);

    expect(removeProvisionalParticipantIfSocketOwned).toHaveBeenCalledWith(
      CHANNEL_ID,
      USER_ID,
      pendingSocketId
    );
    expect(oldDisconnect).toHaveBeenCalledWith(true);
    expect(pendingDisconnect).toHaveBeenCalledWith(true);
    expect(terminalized).toBe(true);
  });

  it('emits one revocation verdict only after an actual teardown', async () => {
    const { rm } = makeRoomManager({ socketId: SOCKET_ID });
    const { io } = makeIO(SOCKET_ID);
    const emit = vi.fn();
    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, emit);
    expect(emit).toHaveBeenCalledOnce();
    expect(emit).toHaveBeenCalledWith(
      expect.objectContaining({ reasonCode: 'revocation_enforced' })
    );
  });

  it('emits one revocation verdict when admitted ownership would vanish on disconnect', async () => {
    let admitted = true;
    const leaveRoomIfSocketOwned = vi.fn(async () => {
      if (!admitted) return false;
      admitted = false;
      return true;
    });
    const rm = {
      getParticipant: vi.fn(() => (admitted ? { socketId: SOCKET_ID } : undefined)),
      getProvisionalParticipantSocketId: vi.fn(() => undefined),
      leaveRoomIfSocketOwned,
      removeProvisionalParticipantForEnforcement: vi.fn(),
    } as unknown as ForceDisconnectRoomManager;
    const disconnect = vi.fn(() => {
      admitted = false;
    });
    const io = {
      sockets: { sockets: new Map([[SOCKET_ID, { emit: vi.fn(), disconnect }]]) },
    } as unknown as ForceDisconnectIO;
    const emit = vi.fn();

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, emit);

    expect(leaveRoomIfSocketOwned).toHaveBeenCalledWith(CHANNEL_ID, USER_ID, SOCKET_ID);
    expect(disconnect).toHaveBeenCalledWith(true);
    expect(emit).toHaveBeenCalledTimes(1);
    expect(emit).toHaveBeenCalledWith(
      expect.objectContaining({ reasonCode: 'revocation_enforced' })
    );
  });

  it('does not emit when the admitted session races out before exact teardown', async () => {
    const { rm, leaveRoomIfSocketOwned } = makeRoomManager({ socketId: SOCKET_ID });
    leaveRoomIfSocketOwned.mockResolvedValueOnce(false);
    const { io } = makeIO(SOCKET_ID);
    const emit = vi.fn();
    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, emit);
    expect(emit).not.toHaveBeenCalled();
  });

  it('continues the teardown when the optional observer throws', async () => {
    const { rm, leaveRoomIfSocketOwned } = makeRoomManager({ socketId: SOCKET_ID });
    const { io } = makeIO(SOCKET_ID);
    await expect(
      handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, () => {
        throw new Error('observer failure');
      })
    ).resolves.toBeUndefined();
    expect(leaveRoomIfSocketOwned).toHaveBeenCalledWith(CHANNEL_ID, USER_ID, SOCKET_ID);
  });
});
