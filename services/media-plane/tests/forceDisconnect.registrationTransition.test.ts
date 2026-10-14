import { createHmac } from 'node:crypto';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';

import {
  createCredentialEpochEjectionAckHandler,
  handleForceDisconnect,
  type ForceDisconnectIO,
  type ForceDisconnectRoomManager,
} from '../src/lib/forceDisconnect.js';

const roomId = '44444444-4444-4444-8444-444444444444';
const userId = '55555555-5555-4555-8555-555555555555';
const socketId = 'socket-a1-pending';
const credentialEpoch = 'b'.repeat(32);
const supersededCredentialEpoch = 'a'.repeat(32);
const generation = '11111111-1111-4111-8111-111111111111';
const bootId = '22222222-2222-4222-8222-222222222222';
const signingValue = 'registration-transition-test-key';

function registeredSession(sessionGeneration = generation) {
  return {
    sessionGeneration,
    nodeBootId: bootId,
    roomId,
    roomKind: 'channel' as const,
    userId,
    credentialEpoch: supersededCredentialEpoch,
    socketId,
  };
}

function credentialEpochCommand() {
  const timestamp = Math.floor(Date.now() / 1000).toString();
  const nonce = 'c'.repeat(64);
  const fields = [userId, credentialEpoch, supersededCredentialEpoch, 'disconnect', nonce];
  const key = createHmac('sha256', signingValue)
    .update('concord/credential-epoch-voice-ejection/request/v1')
    .digest();
  return {
    version: 1,
    userId,
    credentialEpoch,
    supersededCredentialEpoch,
    action: 'disconnect',
    timestamp,
    nonce,
    proof: createHmac('sha256', key).update(['v1', timestamp, ...fields].join('\n')).digest('hex'),
  };
}

function socketFixture(roomClaim?: string) {
  const socket = {
    emit: vi.fn(),
    disconnect: vi.fn(),
    data: {
      userId,
      credentialEpoch: supersededCredentialEpoch,
      ...(roomClaim === undefined ? {} : { roomId: roomClaim }),
    } as Record<string, unknown>,
  };
  const io = { sockets: { sockets: new Map([[socketId, socket]]) } };
  return { socket, io: io as unknown as ForceDisconnectIO };
}

describe('A1 registration transition with no socket room claim', () => {
  beforeEach(() => vi.clearAllMocks());

  it('force-disconnects the exact A1 admitted socket after matching registration completes during native termination', async () => {
    let completeTermination!: () => void;
    const termination = new Promise<void>((resolve) => { completeTermination = resolve; });
    const participant = {
      socketId,
      credentialEpoch: supersededCredentialEpoch,
      voiceEnforcementSessionGeneration: generation,
    };
    const getParticipant = vi.fn().mockReturnValueOnce(participant).mockReturnValue(undefined);
    const { socket, io } = socketFixture();
    const roomManager = {
      getParticipant,
      getProvisionalParticipant: vi.fn(),
      getRoom: vi.fn(),
      leaveRoomIfSocketOwned: vi.fn().mockResolvedValue(true),
      removeProvisionalParticipantForEnforcement: vi.fn(),
      awaitNativeTermination: vi.fn(async () => {
        expect(socket.data.roomId).toBeUndefined();
        socket.data.voiceEnforcementSession = registeredSession();
        await termination;
      }),
    };
    const operation = handleForceDisconnect(
      roomManager as unknown as ForceDisconnectRoomManager,
      io,
      roomId,
      userId,
      undefined,
      { reason: 'access_revoked' }
    );

    try {
      await new Promise<void>((resolve) => setImmediate(resolve));
      expect(roomManager.awaitNativeTermination).toHaveBeenCalledWith(roomId, userId, socketId, generation);
      expect(socket.data.roomId).toBeUndefined();
      completeTermination();
      await operation;
      expect(socket.emit, 'the exact A1 socket must be disconnected using its frozen RoomManager room and generation even though socket.data.roomId is unset during registration').toHaveBeenCalledWith(
        'force-disconnect',
        { channelId: roomId, reason: 'access_revoked' }
      );
      expect(socket.disconnect).toHaveBeenCalledWith(true);
    } finally {
      completeTermination();
      await operation.catch(() => undefined);
    }
  });

  it('credential-epoch ejection uses the exact RoomManager room after matching registration completes during proof', async () => {
    let completeProof!: () => void;
    const proof = new Promise<void>((resolve) => { completeProof = resolve; });
    const { socket, io } = socketFixture();
    const target = { roomId, socketId, provisional: true, sessionGeneration: generation };
    const getSessions = vi.fn().mockReturnValueOnce([target]).mockReturnValueOnce([]);
    const roomManager = {
      getSupersededCredentialEpochSessions: getSessions,
      removeProvisionalParticipantForEnforcement: vi.fn().mockResolvedValue(true),
      leaveRoomIfSocketOwned: vi.fn(),
      awaitNativeTermination: vi.fn().mockResolvedValue(undefined),
      awaitNativeTerminationForCredentialEpoch: vi.fn(async () => {
        expect(socket.data.roomId).toBeUndefined();
        socket.data.voiceEnforcementSession = registeredSession();
        await proof;
      }),
    };
    const handler = createCredentialEpochEjectionAckHandler(roomManager, io, signingValue);
    const acknowledgement = handler(credentialEpochCommand());

    try {
      await new Promise<void>((resolve) => setImmediate(resolve));
      expect(roomManager.awaitNativeTermination).toHaveBeenCalledWith(roomId, userId, socketId, generation);
      expect(socket.data.roomId).toBeUndefined();
      completeProof();
      await expect(acknowledgement).resolves.toMatchObject({
        ok: true,
        userId,
        supersededCredentialEpoch,
      });
      expect(socket.emit, 'credential-epoch ejection must use the exact RoomManager room and frozen generation when socket.data.roomId is unset during A1 registration').toHaveBeenCalledWith(
        'force-disconnect',
        { channelId: roomId, reason: 'credential_rotated' }
      );
      expect(socket.disconnect).toHaveBeenCalledWith(true);
    } finally {
      completeProof();
      await acknowledgement.catch(() => undefined);
    }
  });

  it('preserves the same socket when cleanup clears its room claim before exact registration completes', async () => {
    const participant = {
      socketId,
      credentialEpoch: supersededCredentialEpoch,
      voiceEnforcementSessionGeneration: generation,
    };
    const getParticipant = vi.fn().mockReturnValueOnce(participant).mockReturnValue(undefined);
    const { socket, io } = socketFixture(roomId);
    const roomManager = {
      getParticipant,
      getProvisionalParticipant: vi.fn(),
      getRoom: vi.fn(),
      leaveRoomIfSocketOwned: vi.fn(async () => {
        delete socket.data.roomId;
        return true;
      }),
      removeProvisionalParticipantForEnforcement: vi.fn(),
      awaitNativeTermination: vi.fn(async () => {
        socket.data.voiceEnforcementSession = registeredSession();
      }),
    };

    await handleForceDisconnect(
      roomManager as unknown as ForceDisconnectRoomManager,
      io,
      roomId,
      userId,
      undefined,
      { reason: 'access_revoked' }
    );

    expect(socket.data.roomId).toBeUndefined();
    expect(socket.emit).toHaveBeenCalledWith('force-disconnect', {
      channelId: roomId,
      reason: 'access_revoked',
    });
    expect(socket.disconnect).toHaveBeenCalledWith(true);
  });

  it('preserves an A1 socket whose pending provisional target advances to a different generation during proof', async () => {
    const successorGeneration = '33333333-3333-4333-8333-333333333333';
    const { socket, io } = socketFixture();
    const oldTarget = { roomId, socketId, provisional: true, sessionGeneration: generation };
    const successorTarget = { ...oldTarget, sessionGeneration: successorGeneration };
    const getSessions = vi.fn().mockReturnValueOnce([oldTarget]).mockReturnValueOnce([successorTarget]);
    const handler = createCredentialEpochEjectionAckHandler(
      {
        getSupersededCredentialEpochSessions: getSessions,
        removeProvisionalParticipantForEnforcement: vi.fn().mockResolvedValue(true),
        leaveRoomIfSocketOwned: vi.fn(),
        awaitNativeTermination: vi.fn().mockResolvedValue(undefined),
        awaitNativeTerminationForCredentialEpoch: vi.fn(async () => {
          socket.data.voiceEnforcementSession = registeredSession(successorGeneration);
        }),
      },
      io,
      signingValue
    );

    await expect(handler(credentialEpochCommand())).resolves.toMatchObject({ ok: true });
    expect(socket.data.roomId).toBeUndefined();
    expect(socket.emit).not.toHaveBeenCalled();
    expect(socket.disconnect).not.toHaveBeenCalled();
  });
});
