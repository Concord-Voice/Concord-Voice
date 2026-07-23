import { describe, it, expect, vi, beforeEach } from 'vitest';
import './mocks/logger.js';

import {
  handlePermissionsUpdate,
  handleEnforcePermissionsMessage,
  type EnforcePermissionsRoomManager,
  type EnforcePermissionsIO,
} from '../src/lib/enforcePermissions.js';

const CHANNEL_ID = 'ch-1';
const USER_ID = 'u-1';
const SOCKET_ID = 'socket-abc';
const PERM_VIEW_VOICE = 1n << 9n;
const PERM_JOIN_VOICE = 1n << 16n;
// Voice-channel access (CV-CAN-006 dual-bit gate). A push that still clears this
// gate keeps the peer in the room and only audits producers; dropping either bit
// force-disconnects them.
const PERM_VOICE_ACCESS = PERM_VIEW_VOICE | PERM_JOIN_VOICE;
const PERM_SPEAK = 1n << 17n;
const PERM_ADMINISTRATOR = 1n << 62n;

/** Builds a fake RoomManager surface. */
function makeRoomManager(opts: {
  participant?: { socketId: string };
  updateResult?: boolean;
  closedSources?: string[];
}) {
  const getParticipant = vi.fn().mockReturnValue(opts.participant);
  const updateParticipantPermissions = vi.fn().mockReturnValue(opts.updateResult ?? true);
  const closeForbiddenProducers = vi.fn().mockResolvedValue(opts.closedSources ?? []);
  const leaveRoom = vi.fn().mockResolvedValue(undefined);
  const getProvisionalParticipantSocketId = vi.fn().mockReturnValue(undefined);
  const removeProvisionalParticipantIfSocketOwned = vi.fn().mockResolvedValue(false);
  return {
    rm: {
      getParticipant,
      getProvisionalParticipantSocketId,
      updateParticipantPermissions,
      closeForbiddenProducers,
      leaveRoom,
      removeProvisionalParticipantIfSocketOwned,
    } as unknown as EnforcePermissionsRoomManager,
    getParticipant,
    updateParticipantPermissions,
    closeForbiddenProducers,
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
  return { io: { sockets: { sockets } } as unknown as EnforcePermissionsIO, emit, disconnect };
}

describe('handlePermissionsUpdate (CV-CAN-007 P1 mid-session enforcement)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('replaces the snapshot, audits producers, and notifies the peer', async () => {
    const { rm, updateParticipantPermissions, closeForbiddenProducers, leaveRoom } =
      makeRoomManager({
        participant: { socketId: SOCKET_ID },
        closedSources: ['camera', 'screen'],
      });
    const { io, emit } = makeIO(SOCKET_ID);

    // Voice access retained (only a publish bit like Video/ScreenShare was
    // revoked), so the peer stays in the room and its producers are audited.
    const perms = PERM_VOICE_ACCESS | PERM_SPEAK;
    await handlePermissionsUpdate(rm, io, CHANNEL_ID, USER_ID, perms);

    expect(updateParticipantPermissions).toHaveBeenCalledWith(CHANNEL_ID, USER_ID, perms);
    expect(closeForbiddenProducers).toHaveBeenCalledWith(CHANNEL_ID, USER_ID);
    expect(leaveRoom).not.toHaveBeenCalled();
    // The peer learns its new bitfield (decimal-string wire format) and which
    // of its sources were closed, so it can stop local capture.
    expect(emit).toHaveBeenCalledWith('permissions-changed', {
      channelId: CHANNEL_ID,
      permissions: perms.toString(),
      closedSources: ['camera', 'screen'],
    });
  });

  it('force-disconnects the peer when ViewVoiceChannels is revoked', async () => {
    const { rm, closeForbiddenProducers, leaveRoom } = makeRoomManager({
      participant: { socketId: SOCKET_ID },
    });
    const { io, emit, disconnect } = makeIO(SOCKET_ID);

    // Only JoinVoice remains — the dual-bit join gate no longer clears, so a
    // fresh AuthorizeJoin would reject; closing producers is insufficient.
    await handlePermissionsUpdate(rm, io, CHANNEL_ID, USER_ID, PERM_JOIN_VOICE | PERM_SPEAK);

    expect(leaveRoom).toHaveBeenCalledWith(CHANNEL_ID, USER_ID);
    expect(emit).toHaveBeenCalledWith('force-disconnect', {
      channelId: CHANNEL_ID,
      reason: 'access_revoked',
    });
    expect(disconnect).toHaveBeenCalledWith(true);
    // No producer audit / permissions-changed on the disconnect path.
    expect(closeForbiddenProducers).not.toHaveBeenCalled();
    expect(emit).not.toHaveBeenCalledWith('permissions-changed', expect.anything());
  });

  it('force-disconnects the peer when JoinVoice is revoked', async () => {
    const { rm, closeForbiddenProducers, leaveRoom } = makeRoomManager({
      participant: { socketId: SOCKET_ID },
    });
    const { io, disconnect } = makeIO(SOCKET_ID);

    // Only ViewVoiceChannels remains.
    await handlePermissionsUpdate(rm, io, CHANNEL_ID, USER_ID, PERM_VIEW_VOICE | PERM_SPEAK);

    expect(leaveRoom).toHaveBeenCalledWith(CHANNEL_ID, USER_ID);
    expect(disconnect).toHaveBeenCalledWith(true);
    expect(closeForbiddenProducers).not.toHaveBeenCalled();
  });

  it('does NOT disconnect an administrator lacking explicit voice-access bits', async () => {
    const { rm, closeForbiddenProducers, leaveRoom } = makeRoomManager({
      participant: { socketId: SOCKET_ID },
      closedSources: [],
    });
    const { io } = makeIO(SOCKET_ID);

    // Administrator short-circuits every permission check (rbac.Permission.Has),
    // so it implicitly holds voice access even without the concrete bits set.
    await handlePermissionsUpdate(rm, io, CHANNEL_ID, USER_ID, PERM_ADMINISTRATOR);

    expect(leaveRoom).not.toHaveBeenCalled();
    expect(closeForbiddenProducers).toHaveBeenCalledWith(CHANNEL_ID, USER_ID);
  });

  it('is an idempotent no-op when the participant already left', async () => {
    const { rm, updateParticipantPermissions, closeForbiddenProducers } = makeRoomManager({
      participant: undefined,
    });
    const { io, emit } = makeIO(SOCKET_ID);

    await handlePermissionsUpdate(rm, io, CHANNEL_ID, USER_ID, PERM_SPEAK);

    expect(updateParticipantPermissions).not.toHaveBeenCalled();
    expect(closeForbiddenProducers).not.toHaveBeenCalled();
    expect(emit).not.toHaveBeenCalled();
  });

  it('does not audit or notify when the room has no permission model (DM)', async () => {
    const { rm, closeForbiddenProducers } = makeRoomManager({
      participant: { socketId: SOCKET_ID },
      updateResult: false,
    });
    const { io, emit } = makeIO(SOCKET_ID);

    await handlePermissionsUpdate(rm, io, CHANNEL_ID, USER_ID, PERM_SPEAK);

    expect(closeForbiddenProducers).not.toHaveBeenCalled();
    expect(emit).not.toHaveBeenCalled();
  });

  it('still audits producers when the peer socket is already gone', async () => {
    const { rm, closeForbiddenProducers } = makeRoomManager({
      participant: { socketId: SOCKET_ID },
      closedSources: ['mic'],
    });
    // IO with no socket registered under SOCKET_ID.
    const { io, emit } = makeIO(undefined);

    // Voice access retained (only a publish bit revoked) so this stays on the
    // audit path, not the disconnect path.
    await handlePermissionsUpdate(rm, io, CHANNEL_ID, USER_ID, PERM_VOICE_ACCESS);

    expect(closeForbiddenProducers).toHaveBeenCalledWith(CHANNEL_ID, USER_ID);
    expect(emit).not.toHaveBeenCalled();
  });
});

describe('handleEnforcePermissionsMessage (NATS payload validation)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it.each([
    ['non-string channelId', { channelId: 5, userId: USER_ID, permissions: '131072' }],
    ['missing userId', { channelId: CHANNEL_ID, permissions: '131072' }],
    ['hex bitfield', { channelId: CHANNEL_ID, userId: USER_ID, permissions: '0x20000' }],
    ['negative bitfield', { channelId: CHANNEL_ID, userId: USER_ID, permissions: '-1' }],
    ['numeric bitfield', { channelId: CHANNEL_ID, userId: USER_ID, permissions: 131072 }],
    ['array bitfield', { channelId: CHANNEL_ID, userId: USER_ID, permissions: ['131072'] }],
    ['out-of-range decimal', { channelId: CHANNEL_ID, userId: USER_ID, permissions: '18446744073709551615' }],
    ['missing bitfield', { channelId: CHANNEL_ID, userId: USER_ID }],
  ])('ignores a malformed payload: %s', async (_label, payload) => {
    const { rm, getParticipant, updateParticipantPermissions, closeForbiddenProducers } =
      makeRoomManager({ participant: { socketId: SOCKET_ID } });
    const { io, emit } = makeIO(SOCKET_ID);

    await handleEnforcePermissionsMessage(rm, io, payload as Record<string, unknown>);

    // The snapshot is untouched — a bad message never strips a peer.
    expect(getParticipant).not.toHaveBeenCalled();
    expect(updateParticipantPermissions).not.toHaveBeenCalled();
    expect(closeForbiddenProducers).not.toHaveBeenCalled();
    expect(emit).not.toHaveBeenCalled();
  });

  it('dispatches a well-formed payload to handlePermissionsUpdate', async () => {
    const { rm, updateParticipantPermissions, closeForbiddenProducers } = makeRoomManager({
      participant: { socketId: SOCKET_ID },
      closedSources: [],
    });
    const { io } = makeIO(SOCKET_ID);

    // Voice access retained so dispatch reaches the producer-audit path.
    const perms = PERM_VOICE_ACCESS | PERM_SPEAK;
    await handleEnforcePermissionsMessage(rm, io, {
      channelId: CHANNEL_ID,
      userId: USER_ID,
      permissions: perms.toString(),
    });

    expect(updateParticipantPermissions).toHaveBeenCalledWith(CHANNEL_ID, USER_ID, perms);
    expect(closeForbiddenProducers).toHaveBeenCalledWith(CHANNEL_ID, USER_ID);
  });

  it('serializes concurrent pushes for the same participant so the last bitfield wins', async () => {
    // closeForbiddenProducers on the FIRST push blocks on a manual deferred so
    // the two updates are forced to overlap the way the un-awaited NATS handler
    // would. Without serialization the second update would apply its snapshot
    // while the first is still mid-flight.
    let releaseFirstClose: () => void = () => {};
    const firstClose = new Promise<string[]>((resolve) => {
      releaseFirstClose = () => resolve([]);
    });
    const closeForbiddenProducers = vi
      .fn()
      .mockReturnValueOnce(firstClose)
      .mockResolvedValue([]);
    const updateParticipantPermissions = vi.fn().mockReturnValue(true);
    const rm = {
      getParticipant: vi.fn().mockReturnValue({ socketId: SOCKET_ID }),
      updateParticipantPermissions,
      closeForbiddenProducers,
      leaveRoom: vi.fn().mockResolvedValue(undefined),
    } as unknown as EnforcePermissionsRoomManager;
    const { io } = makeIO(SOCKET_ID);

    // Both pushes retain voice access, so each reaches the audit path.
    const revoke = (PERM_VOICE_ACCESS).toString();
    const grant = (PERM_VOICE_ACCESS | PERM_SPEAK).toString();

    const first = handleEnforcePermissionsMessage(rm, io, {
      channelId: CHANNEL_ID,
      userId: USER_ID,
      permissions: revoke,
    });
    const second = handleEnforcePermissionsMessage(rm, io, {
      channelId: CHANNEL_ID,
      userId: USER_ID,
      permissions: grant,
    });

    // Drain the microtask queue to a macrotask boundary so both chains advance
    // as far as they can. The second update must still be queued behind the
    // first, which is parked on firstClose.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(updateParticipantPermissions).toHaveBeenCalledTimes(1);
    expect(updateParticipantPermissions).toHaveBeenLastCalledWith(
      CHANNEL_ID,
      USER_ID,
      PERM_VOICE_ACCESS
    );

    releaseFirstClose();
    await Promise.all([first, second]);

    // The grant applied strictly after the revoke, so the final snapshot wins.
    expect(updateParticipantPermissions).toHaveBeenCalledTimes(2);
    expect(updateParticipantPermissions).toHaveBeenLastCalledWith(
      CHANNEL_ID,
      USER_ID,
      PERM_VOICE_ACCESS | PERM_SPEAK
    );
  });
});
