import { describe, it, expect, vi, beforeEach } from 'vitest';
import { createHmac } from 'node:crypto';
import './mocks/logger.js';

vi.mock('../src/lib/voiceEnforcementSession.js', () => ({
  releaseVoiceEnforcementSession: vi.fn().mockResolvedValue(undefined),
}));

import {
  handleForceDisconnect,
  createCredentialEpochEjectionAckHandler,
  createVoiceEnforcementSessionEjectionHandler,
  type ForceDisconnectIO,
  type ForceDisconnectOptions,
  type ForceDisconnectRoomManager,
} from '../src/lib/forceDisconnect.js';
import { logger } from '../src/lib/logger.js';
import { releaseVoiceEnforcementSession } from '../src/lib/voiceEnforcementSession.js';
import {
  VoiceEnforcementExpiryFence,
  VoiceEnforcementLease,
} from '../src/lib/voiceEnforcementLease.js';

const CHANNEL_ID = 'ch-1';
const USER_ID = 'u-1';
const SOCKET_ID = 'socket-abc';
const REVOKED: ForceDisconnectOptions = { reason: 'access_revoked' };
const POLICED: ForceDisconnectOptions = { reason: 'media_policy', retryAfterSec: 900 };

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

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, REVOKED);

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

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, REVOKED);

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

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, REVOKED);

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

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, REVOKED);

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

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, REVOKED);

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

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, REVOKED);

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
    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, emit, REVOKED);
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

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, emit, REVOKED);

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
    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, emit, REVOKED);
    expect(emit).not.toHaveBeenCalled();
  });

  it('continues the teardown when the optional observer throws', async () => {
    const { rm, leaveRoomIfSocketOwned } = makeRoomManager({ socketId: SOCKET_ID });
    const { io } = makeIO(SOCKET_ID);
    await expect(
      handleForceDisconnect(
        rm,
        io,
        CHANNEL_ID,
        USER_ID,
        () => {
          throw new Error('observer failure');
        },
        REVOKED
      )
    ).resolves.toBeUndefined();
    expect(leaveRoomIfSocketOwned).toHaveBeenCalledWith(CHANNEL_ID, USER_ID, SOCKET_ID);
  });
});

describe('handleForceDisconnect reasons (#2153)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('sends a media-policy eviction its retryAfterSec', async () => {
    const { rm, leaveRoomIfSocketOwned } = makeRoomManager({ socketId: SOCKET_ID });
    const { io, emit, disconnect } = makeIO(SOCKET_ID);

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, POLICED);

    expect(emit.mock.calls).toStrictEqual([
      ['force-disconnect', { channelId: CHANNEL_ID, reason: 'media_policy', retryAfterSec: 900 }],
    ]);
    expect(disconnect).toHaveBeenCalledWith(true);
    expect(leaveRoomIfSocketOwned).toHaveBeenCalledWith(CHANNEL_ID, USER_ID, SOCKET_ID);
  });

  it('keeps the access-revoked payload free of retryAfterSec', async () => {
    const { rm } = makeRoomManager({ socketId: SOCKET_ID });
    const { io, emit } = makeIO(SOCKET_ID);

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, REVOKED);

    expect(emit.mock.calls).toStrictEqual([
      ['force-disconnect', { channelId: CHANNEL_ID, reason: 'access_revoked' }],
    ]);
  });

  it('reports a policer eviction as a media-admission denial, never as a revocation', async () => {
    const { rm } = makeRoomManager({ socketId: SOCKET_ID });
    const { io } = makeIO(SOCKET_ID);
    const securityEmit = vi.fn();

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, securityEmit, POLICED);

    expect(securityEmit.mock.calls).toStrictEqual([
      [
        {
          eventType: 'media_admission',
          outcome: 'denied',
          severity: 'high',
          reasonCode: 'structural_limit_exceeded',
          routeTemplate: 'socket.force_disconnect',
        },
      ],
    ]);
  });

  it('reports an access revocation with the unchanged revocation event', async () => {
    const { rm } = makeRoomManager({ socketId: SOCKET_ID });
    const { io } = makeIO(SOCKET_ID);
    const securityEmit = vi.fn();

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, securityEmit, REVOKED);

    expect(securityEmit.mock.calls).toStrictEqual([
      [
        {
          eventType: 'media_authorization',
          outcome: 'success',
          severity: 'high',
          reasonCode: 'revocation_enforced',
          routeTemplate: 'socket.force_disconnect',
        },
      ],
    ]);
  });

  it('emits no policer event when the session races out before teardown', async () => {
    const { rm, leaveRoomIfSocketOwned } = makeRoomManager({ socketId: SOCKET_ID });
    leaveRoomIfSocketOwned.mockResolvedValueOnce(false);
    const { io } = makeIO(SOCKET_ID);
    const securityEmit = vi.fn();

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, securityEmit, POLICED);

    expect(securityEmit).not.toHaveBeenCalled();
  });

  it('logs each reason under its own grep string', async () => {
    const { rm } = makeRoomManager({ socketId: SOCKET_ID });
    const { io } = makeIO(SOCKET_ID);

    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, POLICED);
    await handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, REVOKED);

    expect(vi.mocked(logger.info).mock.calls).toStrictEqual([
      [
        'Media policer evicted participant',
        { channelId: CHANNEL_ID, userId: USER_ID, retryAfterSec: 900 },
      ],
      [
        'Force-disconnected participant via voice.enforce.disconnect',
        { channelId: CHANNEL_ID, userId: USER_ID },
      ],
    ]);
  });

  it('refuses an unknown reason before touching any session', async () => {
    const {
      rm,
      getParticipant,
      leaveRoomIfSocketOwned,
      removeProvisionalParticipantIfSocketOwned,
    } = makeRoomManager({ socketId: SOCKET_ID }, 'socket-pending');
    const { io, emit, disconnect } = makeIO(SOCKET_ID, 'socket-pending');

    await expect(
      handleForceDisconnect(rm, io, CHANNEL_ID, USER_ID, undefined, {
        reason: 'bogus',
      } as unknown as ForceDisconnectOptions)
    ).rejects.toThrow('Unhandled force-disconnect reason');

    expect(getParticipant).not.toHaveBeenCalled();
    expect(removeProvisionalParticipantIfSocketOwned).not.toHaveBeenCalled();
    expect(leaveRoomIfSocketOwned).not.toHaveBeenCalled();
    expect(emit).not.toHaveBeenCalled();
    expect(disconnect).not.toHaveBeenCalled();
  });
});

describe('durable exact-session ejection', () => {
  const ids = {
    parent: '11111111-1111-4111-8111-111111111111',
    session: '22222222-2222-4222-8222-222222222222',
    boot: '33333333-3333-4333-8333-333333333333',
    room: '44444444-4444-4444-8444-444444444444',
    user: '55555555-5555-4555-8555-555555555555',
  };
  const signingKey = 'fixture';

  function command(sessionGeneration = ids.session) {
    const timestamp = Math.floor(Date.now() / 1000).toString();
    const nonce = 'a'.repeat(64);
    const fields = [
      ids.parent,
      sessionGeneration,
      ids.boot,
      ids.room,
      'dm',
      ids.user,
      '',
      SOCKET_ID,
      nonce,
    ];
    const key = createHmac('sha256', signingKey)
      .update('concord/voice-enforcement-session/eject/request/v1')
      .digest();
    return {
      version: 1,
      parentGeneration: ids.parent,
      sessionGeneration,
      nodeBootId: ids.boot,
      roomId: ids.room,
      roomKind: 'dm',
      userId: ids.user,
      credentialEpoch: '',
      socketId: SOCKET_ID,
      timestamp,
      nonce,
      proof: createHmac('sha256', key)
        .update(['v1', timestamp, ...fields].join('\n'))
        .digest('hex'),
    };
  }

  function healthCommand(challenge = 'b'.repeat(64)) {
    const timestamp = Math.floor(Date.now() / 1000).toString();
    const key = createHmac('sha256', signingKey)
      .update('concord/voice-enforcement-session/health/request/v1')
      .digest();
    return {
      version: 2,
      kind: 'health',
      nodeBootId: ids.boot,
      challenge,
      timestamp,
      proof: createHmac('sha256', key)
        .update(['v1', timestamp, ids.boot, challenge, 'health'].join('\n'))
        .digest('hex'),
    };
  }

  it('tears down a provisional session before disconnect when registration races its response', async () => {
    const remove = vi.fn().mockResolvedValue(true);
    const roomManager = {
      getProvisionalParticipant: vi.fn(() => ({
        socketId: SOCKET_ID,
        credentialEpoch: '',
        voiceEnforcementSessionGeneration: ids.session,
      })),
      getParticipant: vi.fn(() => undefined),
      removeProvisionalParticipantForEnforcement: remove,
      leaveRoomIfSocketOwned: vi.fn(),
    };
    const disconnect = vi.fn();
    const io = {
      sockets: { sockets: new Map([[SOCKET_ID, { emit: vi.fn(), disconnect, data: {} }]]) },
    };
    const handler = createVoiceEnforcementSessionEjectionHandler(
      roomManager,
      io,
      signingKey,
      ids.boot
    );

    await expect(handler(command())).resolves.toMatchObject({
      ok: true,
      sessionGeneration: ids.session,
    });
    expect(remove).toHaveBeenCalledWith(ids.room, ids.user, SOCKET_ID);
    expect(disconnect).toHaveBeenCalledWith(true);
    expect(releaseVoiceEnforcementSession).toHaveBeenCalledOnce();
  });

  it('retains an ambiguous reused socket rather than acknowledging a stale generation', async () => {
    const remove = vi.fn();
    const roomManager = {
      getProvisionalParticipant: vi.fn(() => ({
        socketId: SOCKET_ID,
        credentialEpoch: '',
        voiceEnforcementSessionGeneration: ids.parent,
      })),
      getParticipant: vi.fn(() => undefined),
      removeProvisionalParticipantForEnforcement: remove,
      leaveRoomIfSocketOwned: vi.fn(),
    };
    const io = {
      sockets: {
        sockets: new Map([[SOCKET_ID, { emit: vi.fn(), disconnect: vi.fn(), data: {} }]]),
      },
    };
    const handler = createVoiceEnforcementSessionEjectionHandler(
      roomManager,
      io,
      signingKey,
      ids.boot
    );

    await expect(handler(command())).resolves.toBeUndefined();
    expect(remove).not.toHaveBeenCalled();
    expect(releaseVoiceEnforcementSession).not.toHaveBeenCalled();
  });

  it('does not tear down an admitted reused socket without the exact generation', async () => {
    const leave = vi.fn();
    const roomManager = {
      getProvisionalParticipant: vi.fn(() => undefined),
      getParticipant: vi.fn(() => ({
        socketId: SOCKET_ID,
        credentialEpoch: '',
        voiceEnforcementSessionGeneration: ids.parent,
      })),
      removeProvisionalParticipantForEnforcement: vi.fn(),
      leaveRoomIfSocketOwned: leave,
    };
    const exactSocketSession = {
      sessionGeneration: ids.session,
      nodeBootId: ids.boot,
      roomId: ids.room,
      roomKind: 'dm' as const,
      userId: ids.user,
      credentialEpoch: '',
      socketId: SOCKET_ID,
    };
    const io = {
      sockets: {
        sockets: new Map([
          [
            SOCKET_ID,
            {
              emit: vi.fn(),
              disconnect: vi.fn(),
              data: { voiceEnforcementSession: exactSocketSession },
            },
          ],
        ]),
      },
    };
    const handler = createVoiceEnforcementSessionEjectionHandler(
      roomManager,
      io,
      signingKey,
      ids.boot
    );

    await expect(handler(command())).resolves.toBeUndefined();
    expect(leave).not.toHaveBeenCalled();
    expect(releaseVoiceEnforcementSession).not.toHaveBeenCalled();
  });

  it('releases a stale replaced socket while preserving its successor', async () => {
    const successorSocketID = 'socket-successor';
    const successorGeneration = '66666666-6666-4666-8666-666666666666';
    const roomManager = {
      getProvisionalParticipant: vi.fn(() => undefined),
      getParticipant: vi.fn(() => ({
        socketId: successorSocketID,
        credentialEpoch: '',
        voiceEnforcementSessionGeneration: successorGeneration,
      })),
      removeProvisionalParticipantForEnforcement: vi.fn(),
      leaveRoomIfSocketOwned: vi.fn(),
    };
    const oldDisconnect = vi.fn();
    const successorDisconnect = vi.fn();
    const exactOldSession = {
      sessionGeneration: ids.session,
      nodeBootId: ids.boot,
      roomId: ids.room,
      roomKind: 'dm' as const,
      userId: ids.user,
      credentialEpoch: '',
      socketId: SOCKET_ID,
    };
    const io = {
      sockets: {
        sockets: new Map([
          [
            SOCKET_ID,
            {
              emit: vi.fn(),
              disconnect: oldDisconnect,
              data: { voiceEnforcementSession: exactOldSession },
            },
          ],
          [successorSocketID, { emit: vi.fn(), disconnect: successorDisconnect, data: {} }],
        ]),
      },
    };
    const handler = createVoiceEnforcementSessionEjectionHandler(
      roomManager,
      io,
      signingKey,
      ids.boot
    );

    await expect(handler(command())).resolves.toMatchObject({ ok: true });
    expect(oldDisconnect).toHaveBeenCalledWith(true);
    expect(successorDisconnect).not.toHaveBeenCalled();
    expect(releaseVoiceEnforcementSession).toHaveBeenCalledWith(exactOldSession);
  });

  it('fences expired media before a late valid health command can renew admission', async () => {
    let now = 0;
    const lease = new VoiceEnforcementLease(() => now, 30_000);
    lease.renew();
    now = 30_001;
    const order: string[] = [];
    const roomManager = {
      getProvisionalParticipant: vi.fn(),
      getParticipant: vi.fn(),
      removeProvisionalParticipantForEnforcement: vi.fn(),
      leaveRoomIfSocketOwned: vi.fn(),
    };
    const handler = createVoiceEnforcementSessionEjectionHandler(
      roomManager,
      { sockets: { sockets: new Map() } },
      signingKey,
      ids.boot,
      async () => {
        if (!lease.valid()) {
          order.push('fence');
        }
        lease.renew();
        order.push('renew');
      }
    );
    await expect(handler(healthCommand())).resolves.toMatchObject({ kind: 'health', ok: true });
    expect(order).toEqual(['fence', 'renew']);
    expect(releaseVoiceEnforcementSession).not.toHaveBeenCalled();
    expect(lease.valid()).toBe(true);
  });

  it('renews a healthy lease without teardown', async () => {
    const lease = new VoiceEnforcementLease(() => 0, 30_000);
    lease.renew();
    const fence = vi.fn();
    const roomManager = {
      getProvisionalParticipant: vi.fn(),
      getParticipant: vi.fn(),
      removeProvisionalParticipantForEnforcement: vi.fn(),
      leaveRoomIfSocketOwned: vi.fn(),
    };
    const handler = createVoiceEnforcementSessionEjectionHandler(
      roomManager,
      { sockets: { sockets: new Map() } },
      signingKey,
      ids.boot,
      () => {
        if (!lease.valid()) fence();
        lease.renew();
      }
    );
    await expect(handler(healthCommand())).resolves.toMatchObject({ kind: 'health', ok: true });
    expect(fence).not.toHaveBeenCalled();
    expect(releaseVoiceEnforcementSession).not.toHaveBeenCalled();
  });

  it('waits for an in-flight expiry teardown before a late health command reopens admission', async () => {
    const now = 30_001;
    const lease = new VoiceEnforcementLease(() => now, 30_000);
    let releaseCloseAll!: () => void;
    const closeAll = new Promise<void>((resolve) => {
      releaseCloseAll = resolve;
    });
    const order: string[] = [];
    const fenceExpired = new VoiceEnforcementExpiryFence(lease, async () => {
      order.push('close-start');
      await closeAll;
      order.push('close-end');
    });
    // Start the watchdog's closeAll before the same target subscriber accepts a
    // valid health command. This is the production overlap that must not reopen
    // A1/A2 while closeAll still owns its room snapshot.
    const watchdog = fenceExpired.enforce();
    const roomManager = {
      getProvisionalParticipant: vi.fn(),
      getParticipant: vi.fn(),
      removeProvisionalParticipantForEnforcement: vi.fn(),
      leaveRoomIfSocketOwned: vi.fn(),
    };
    const handler = createVoiceEnforcementSessionEjectionHandler(
      roomManager,
      { sockets: { sockets: new Map() } },
      signingKey,
      ids.boot,
      async () => {
        if (!lease.valid()) await fenceExpired.enforce();
        lease.renew();
        order.push('renew');
      }
    );

    let healthSettled = false;
    const health = handler(healthCommand()).then(() => {
      healthSettled = true;
    });
    await Promise.resolve();
    expect(order).toEqual(['close-start']);
    expect(healthSettled).toBe(false);
    expect(lease.valid()).toBe(false);
    expect(releaseVoiceEnforcementSession).not.toHaveBeenCalled();

    releaseCloseAll();
    await Promise.all([watchdog, health]);
    expect(order).toEqual(['close-start', 'close-end', 'renew']);
    expect(lease.valid()).toBe(true);
    expect(releaseVoiceEnforcementSession).not.toHaveBeenCalled();
  });
});

describe('credential-epoch ejection', () => {
  const userId = 'user-credential-epoch';
  const credentialEpoch = 'b'.repeat(32);
  const supersededCredentialEpoch = 'a'.repeat(32);
  const signingValue = 'credential-epoch-fixture';

  function command(overrides: Record<string, unknown> = {}) {
    const timestamp = Math.floor(Date.now() / 1000).toString();
    const nonce = 'c'.repeat(64);
    const key = createHmac('sha256', signingValue)
      .update('concord/credential-epoch-voice-ejection/request/v1')
      .digest();
    const fields = [userId, credentialEpoch, supersededCredentialEpoch, 'disconnect', nonce];
    return {
      version: 1,
      userId,
      credentialEpoch,
      supersededCredentialEpoch,
      action: 'disconnect',
      timestamp,
      nonce,
      proof: createHmac('sha256', key)
        .update(['v1', timestamp, ...fields].join('\n'))
        .digest('hex'),
      ...overrides,
    };
  }

  it('evicts superseded provisional, admitted, and matching live sessions', async () => {
    const removeProvisional = vi.fn().mockResolvedValue(true);
    const leaveRoom = vi.fn().mockResolvedValue(true);
    const sessions = [
      { roomId: 'room-pending', socketId: 'socket-pending', provisional: true },
      { roomId: 'room-admitted', socketId: 'socket-admitted', provisional: false },
      { roomId: 'room-missing', socketId: 'socket-missing', provisional: false },
    ];
    const sockets = new Map([
      ['socket-pending', { emit: vi.fn(), disconnect: vi.fn(), data: {} }],
      ['socket-admitted', { emit: vi.fn(), disconnect: vi.fn(), data: {} }],
      [
        'socket-live-only',
        {
          emit: vi.fn(),
          disconnect: vi.fn(),
          data: { userId, credentialEpoch: supersededCredentialEpoch },
        },
      ],
      [
        'socket-current',
        {
          emit: vi.fn(),
          disconnect: vi.fn(),
          data: { userId, credentialEpoch },
        },
      ],
    ]);
    const handler = createCredentialEpochEjectionAckHandler(
      {
        getSupersededCredentialEpochSessions: vi.fn(() => sessions),
        removeProvisionalParticipantForEnforcement: removeProvisional,
        leaveRoomIfSocketOwned: leaveRoom,
      },
      { sockets: { sockets } },
      signingValue
    );

    await expect(handler(command())).resolves.toMatchObject({
      ok: true,
      userId,
      supersededCredentialEpoch,
    });
    expect(removeProvisional).toHaveBeenCalledWith('room-pending', userId, 'socket-pending');
    expect(leaveRoom).toHaveBeenCalledWith('room-admitted', userId, 'socket-admitted');
    expect(leaveRoom).toHaveBeenCalledWith('room-missing', userId, 'socket-missing');
    expect(sockets.get('socket-pending')?.emit).toHaveBeenCalledWith('force-disconnect', {
      channelId: 'room-pending',
      reason: 'credential_rotated',
    });
    expect(sockets.get('socket-admitted')?.disconnect).toHaveBeenCalledWith(true);
    expect(sockets.get('socket-live-only')?.disconnect).toHaveBeenCalledWith(true);
    expect(sockets.get('socket-current')?.disconnect).not.toHaveBeenCalled();
  });

  it('rejects malformed or same-epoch commands before touching room state', async () => {
    const getSessions = vi.fn(() => []);
    const handler = createCredentialEpochEjectionAckHandler(
      {
        getSupersededCredentialEpochSessions: getSessions,
        removeProvisionalParticipantForEnforcement: vi.fn(),
        leaveRoomIfSocketOwned: vi.fn(),
      },
      { sockets: { sockets: new Map() } },
      signingValue
    );

    await expect(
      handler(command({ supersededCredentialEpoch: credentialEpoch }))
    ).resolves.toBeUndefined();
    await expect(handler(command({ proof: 'not-a-proof' }))).resolves.toBeUndefined();
    expect(getSessions).not.toHaveBeenCalled();
  });
});
