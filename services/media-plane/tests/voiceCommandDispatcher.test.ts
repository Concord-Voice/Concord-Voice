import { describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import { VoiceCommandDispatcher } from '../src/lib/voiceCommandDispatcher.js';
import { decodeVoiceCommand, type VoiceCommand } from '../src/lib/voiceCommand.js';
import {
  RoomManager,
  SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
  FREE_MEDIA_ENTITLEMENT,
} from '../src/lib/roomManager.js';
import type { MediasoupService } from '../src/lib/mediasoup.js';
import { createMockRouter, createMockProducer, createMockConsumer } from './mocks/mediasoup.js';
import type { Server } from 'socket.io';

const generation = '11111111-1111-4111-8111-111111111111';
const roomId = '22222222-2222-4222-8222-222222222222';
const userId = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const sessionGeneration = '44444444-4444-4444-8444-444444444444';
const nodeBootId = '55555555-5555-4555-8555-555555555555';
const successorGeneration = '77777777-7777-4777-8777-777777777777';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}

function command(overrides: Partial<VoiceCommand> = {}): VoiceCommand {
  return {
    schema: 'concord.voice-enforcement',
    version: 1,
    streamGeneration: generation,
    commandId: '66666666-6666-4666-8666-666666666666',
    sequence: '1',
    scope: 'channel',
    kind: 'user_moderation',
    action: 'mute',
    channelId: roomId,
    userId,
    target: { sessionGeneration, nodeBootId, socketId: 'socket-fixture', credentialEpoch: '' },
    ...overrides,
  } as VoiceCommand;
}

function fixture() {
  const order: string[] = [];
  const participant = {
    userId,
    socketId: 'socket-fixture',
    credentialEpoch: '',
    voiceEnforcementSessionGeneration: sessionGeneration,
    producers: new Map([
      ['audio-one', { kind: 'audio', source: 'mic', producer: { paused: false } }],
    ]),
    consumers: new Map(),
    serverMuted: false,
    serverDeafened: false,
    serverEnforcementRevision: 0n,
  };
  let admitted: typeof participant | undefined = participant;
  let provisional: typeof participant | undefined;
  const room = {
    roomKind: 'channel',
    participants: new Map([[userId, participant]]),
    pendingDMParticipants: new Map(),
    pendingChannelParticipants: new Map(),
  };
  const mutate = vi.fn(async () => {
    const captured = admitted;
    if (!captured) return;
    order.push('mutation');
    for (const entry of captured.producers.values()) entry.producer.paused = true;
  });
  const snapshot = vi.fn(
    async (
      _room: string,
      _user: string,
      target: NonNullable<VoiceCommand['target']>,
      muted: boolean,
      deafened: boolean,
      revision: bigint
    ) => {
      const captured = admitted ?? provisional;
      if (
        !captured ||
        captured.voiceEnforcementSessionGeneration !== target.sessionGeneration ||
        captured.socketId !== target.socketId
      )
        return false;
      captured.serverMuted = muted;
      captured.serverDeafened = deafened;
      captured.serverEnforcementRevision = revision;
      for (const entry of captured.producers.values()) if (muted) entry.producer.paused = true;
      return true;
    }
  );
  const remove = vi.fn(
    async (_room: string, _user: string, socketId: string, generation?: string) => {
      if (
        provisional?.socketId !== socketId ||
        (generation !== undefined && provisional.voiceEnforcementSessionGeneration !== generation)
      )
        return false;
      order.push('remove-provisional');
      provisional = undefined;
      return true;
    }
  );
  const leave = vi.fn(
    async (_room: string, _user: string, socketId: string, generation?: string) => {
      if (
        admitted?.socketId !== socketId ||
        (generation !== undefined && admitted.voiceEnforcementSessionGeneration !== generation)
      )
        return false;
      order.push('leave-admitted');
      admitted = undefined;
      return true;
    }
  );
  const awaitNativeTermination = vi.fn(async () => undefined);
  const manager = {
    awaitNativeTermination,
    getRoom: () => room,
    getParticipant: () => admitted,
    getProvisionalParticipant: () => provisional,
    userMuteParticipant: mutate,
    userDeafenParticipant: mutate,
    applyExactServerEnforcementSnapshot: snapshot,
    removeProvisionalParticipantForEnforcement: remove,
    leaveRoomIfSocketOwned: leave,
  };
  const roomEmit = vi.fn();
  const socketEmit = vi.fn();
  const disconnect = vi.fn(() => order.push('disconnect'));
  const sockets = new Map([
    [
      'socket-fixture',
      {
        emit: socketEmit,
        disconnect,
        data: {
          userId,
          roomId,
          credentialEpoch: '',
          voiceEnforcementSession: {
            sessionGeneration,
            nodeBootId,
            roomId,
            roomKind: 'channel',
            userId,
            socketId: 'socket-fixture',
            credentialEpoch: '',
          },
        },
      },
    ],
  ]);
  const io = { to: vi.fn(() => ({ emit: roomEmit })), sockets: { sockets } };
  const legacy = vi.fn(async (_data: Record<string, unknown>) => undefined);
  const ensureAuthority = vi.fn();
  const dispatcher = new VoiceCommandDispatcher({
    roomManager: manager as unknown as RoomManager,
    io: io as unknown as Server,
    nodeBootId,
    streamGeneration: generation,
    ensureAuthority,
    handleLegacy: legacy,
  });
  return {
    dispatcher,
    manager,
    awaitNativeTermination,
    participant,
    mutate,
    snapshot,
    remove,
    leave,
    roomEmit,
    socketEmit,
    disconnect,
    sockets,
    order,
    legacy,
    ensureAuthority,
    setAdmitted: (value: typeof participant | undefined) => {
      admitted = value;
    },
    setProvisional: (value: typeof participant | undefined) => {
      provisional = value;
    },
  };
}

describe('VoiceCommandDispatcher exact durable effects', () => {
  it('waits for fresh authority continuity before mutation and keeps a successor behind that boundary', async () => {
    const f = fixture();
    const continuity = deferred<void>();
    f.ensureAuthority.mockImplementation(() => continuity.promise);
    const first = f.dispatcher.applyDurable(command());

    expect(f.ensureAuthority).toHaveBeenCalled();
    expect(f.mutate, 'no media mutation may cross unresolved fresh continuity').not.toHaveBeenCalled();
    expect(f.roomEmit, 'no fanout may cross unresolved fresh continuity').not.toHaveBeenCalled();
    await expect(
      f.dispatcher.applyDurable(command({ sequence: '2', commandId: successorGeneration }))
    ).rejects.toThrow('Voice command predecessor incomplete');
    expect(f.mutate, 'a successor must not overtake unresolved continuity').not.toHaveBeenCalled();

    continuity.resolve();
    await expect(first).resolves.toBe('applied');
    expect(f.mutate).toHaveBeenCalledTimes(1);
    expect(f.roomEmit).toHaveBeenCalledTimes(1);
    await expect(
      f.dispatcher.applyDurable(command({ sequence: '2', commandId: successorGeneration }))
    ).resolves.toBe('applied');
    expect(f.mutate).toHaveBeenCalledTimes(2);
  });

  it('refuses an unresolved durable command when fresh continuity rejects before its first effect', async () => {
    const f = fixture();
    const continuity = deferred<void>();
    // Keep the rejection observed on current code too, which calls the boundary without awaiting it.
    void continuity.promise.catch(() => undefined);
    f.ensureAuthority.mockImplementation(() => continuity.promise);
    const pending = f.dispatcher.applyDurable(command());
    const unavailable = new Error('durable consumer continuity unavailable');
    continuity.reject(unavailable);

    expect(f.mutate, 'refused fresh continuity must stop media mutation').not.toHaveBeenCalled();
    expect(f.roomEmit, 'refused fresh continuity must stop fanout').not.toHaveBeenCalled();
    expect(f.disconnect).not.toHaveBeenCalled();
    await expect(pending).rejects.toThrow('durable consumer continuity unavailable');
  });

  it('waits for fresh continuity after mutation and before fanout, then retries fanout without replaying mutation', async () => {
    const f = fixture();
    const continuity = deferred<void>();
    const fanoutGateReached = deferred<void>();
    void continuity.promise.catch(() => undefined);
    f.ensureAuthority.mockImplementation(() => {
      if (
        f.participant.producers.get('audio-one')!.producer.paused &&
        !f.roomEmit.mock.calls.some(([event]) => event === 'producer-paused')
      ) {
        fanoutGateReached.resolve();
        return continuity.promise;
      }
      return undefined;
    });
    const pending = f.dispatcher.applyDurable(command());

    await vi.waitFor(() => expect(f.mutate).toHaveBeenCalledTimes(1));
    await fanoutGateReached.promise;
    expect(f.roomEmit, 'fanout must wait for fresh continuity after mutation').not.toHaveBeenCalled();
    const unavailable = new Error('continuity lost before fanout');
    continuity.reject(unavailable);
    await expect(pending).rejects.toThrow('continuity lost before fanout');
    expect(f.roomEmit).not.toHaveBeenCalled();
    expect(f.mutate).toHaveBeenCalledTimes(1);

    f.ensureAuthority.mockImplementation(() => undefined);
    await expect(f.dispatcher.applyDurable(command())).resolves.toBe('applied');
    expect(f.mutate, 'fanout retry must not replay completed mutation').toHaveBeenCalledTimes(1);
    expect(f.roomEmit).toHaveBeenCalledTimes(1);
  });

  it('waits for fresh continuity before captured disconnect removal or socket effects', async () => {
    const f = fixture();
    const continuity = deferred<void>();
    f.ensureAuthority.mockImplementation(() => continuity.promise);
    const pending = f.dispatcher.applyDurable(
      command({ kind: 'disconnect', action: 'disconnect', reason: 'moderator_request' })
    );

    expect(f.ensureAuthority).toHaveBeenCalled();
    expect(f.leave, 'disconnect removal must wait for fresh continuity').not.toHaveBeenCalled();
    expect(f.remove).not.toHaveBeenCalled();
    expect(f.awaitNativeTermination).not.toHaveBeenCalled();
    expect(f.socketEmit).not.toHaveBeenCalled();
    expect(f.disconnect).not.toHaveBeenCalled();
    continuity.resolve();
    await expect(pending).resolves.toBe('applied');
    expect(f.leave).toHaveBeenCalledTimes(1);
    expect(f.awaitNativeTermination).toHaveBeenCalledTimes(1);
    expect(f.disconnect).toHaveBeenCalledTimes(1);
  });

  it.each([
    { action: 'mute', serverMuted: true, serverDeafened: false, event: 'server-mute-changed' },
    { action: 'unmute', serverMuted: false, serverDeafened: false, event: 'server-mute-changed' },
    { action: 'deafen', serverMuted: true, serverDeafened: true, event: 'server-deafen-changed' },
    { action: 'undeafen', serverMuted: true, serverDeafened: false, event: 'server-deafen-changed' },
  ] as const)(
    'fans out the complete authoritative snapshot for $action, including cross-flag states, and retries without reapplying it',
    async ({ action, serverMuted, serverDeafened, event }) => {
      const f = fixture();
      const snapshot = command({
        kind: 'server_moderation',
        serverId: '33333333-3333-4333-8333-333333333333',
        authorizationRevision: '42',
        action,
        serverMuted,
        serverDeafened,
      });
      f.roomEmit.mockImplementationOnce(() => {
        throw new Error('fanout unavailable');
      });
      await expect(f.dispatcher.applyDurable(snapshot)).rejects.toThrow('fanout unavailable');
      expect(f.snapshot).toHaveBeenCalledTimes(1);
      await expect(f.dispatcher.applyDurable(snapshot)).resolves.toBe('applied');
      expect(f.snapshot).toHaveBeenCalledTimes(1);
      expect(f.roomEmit).toHaveBeenCalledTimes(2);
      expect(f.roomEmit).toHaveBeenLastCalledWith(event, {
        userId,
        serverMuted,
        serverDeafened,
      });
      expect(f.participant.serverMuted).toBe(serverMuted);
      expect(f.participant.serverDeafened).toBe(serverDeafened);
      expect(f.participant.serverEnforcementRevision).toBe(42n);
    }
  );

  it('awaits authoritative mutation before fanout and completion', async () => {
    const f = fixture();
    const gate = deferred<void>();
    f.mutate.mockImplementationOnce(async () => {
      await gate.promise;
      f.participant.producers.get('audio-one')!.producer.paused = true;
    });
    const pending = f.dispatcher.applyDurable(command());
    await vi.waitFor(() => expect(f.mutate).toHaveBeenCalledTimes(1));
    expect(f.roomEmit).not.toHaveBeenCalled();
    gate.resolve();
    await expect(pending).resolves.toBe('applied');
    expect(f.participant.producers.get('audio-one')!.producer.paused).toBe(true);
    expect(f.roomEmit).toHaveBeenCalledWith(
      'producer-paused',
      expect.objectContaining({ producerId: 'audio-one', userId })
    );
  });

  it('retries failed fanout without muting again after the user resumes', async () => {
    const f = fixture();
    f.roomEmit.mockImplementationOnce(() => {
      throw new Error('fanout unavailable');
    });
    await expect(f.dispatcher.applyDurable(command())).rejects.toThrow();
    expect(f.participant.producers.get('audio-one')!.producer.paused).toBe(true);
    f.participant.producers.get('audio-one')!.producer.paused = false;
    await expect(f.dispatcher.applyDurable(command())).resolves.toBe('applied');
    expect(f.mutate).toHaveBeenCalledTimes(1);
    expect(f.roomEmit).toHaveBeenCalledTimes(2);
    expect(f.participant.producers.get('audio-one')!.producer.paused).toBe(false);
    await expect(f.dispatcher.applyDurable(command())).resolves.toBe('already_complete');
    expect(f.mutate).toHaveBeenCalledTimes(1);
  });

  it('skips fanout retry when the captured participant has been replaced while its old socket remains live', async () => {
    const f = fixture();
    f.roomEmit.mockImplementationOnce(() => {
      throw new Error('fanout unavailable');
    });
    await expect(f.dispatcher.applyDurable(command())).rejects.toThrow();
    const successor = {
      ...f.participant,
      socketId: 'successor-socket',
      voiceEnforcementSessionGeneration: successorGeneration,
      producers: new Map([
        ['successor-audio', { kind: 'audio', source: 'mic', producer: { paused: false } }],
      ]),
    };
    f.setAdmitted(successor);
    await expect(f.dispatcher.applyDurable(command())).resolves.toBe('applied');
    expect(f.mutate).toHaveBeenCalledTimes(1);
    expect(f.roomEmit).toHaveBeenCalledTimes(1);
    expect(successor.producers.get('successor-audio')!.producer.paused).toBe(false);
  });

  it('skips fanout retry when the exact Socket.IO object has been replaced', async () => {
    const f = fixture();
    f.roomEmit.mockImplementationOnce(() => {
      throw new Error('fanout unavailable');
    });
    await expect(f.dispatcher.applyDurable(command())).rejects.toThrow();
    f.sockets.set('socket-fixture', {
      ...f.sockets.get('socket-fixture')!,
      emit: vi.fn(),
      disconnect: vi.fn(),
    });
    await expect(f.dispatcher.applyDurable(command())).resolves.toBe('applied');
    expect(f.mutate).toHaveBeenCalledTimes(1);
    expect(f.roomEmit).toHaveBeenCalledTimes(1);
  });

  it('skips obsolete snapshot fanout after a newer desired revision lands before retry', async () => {
    const f = fixture();
    const snapshot = command({
      kind: 'server_moderation',
      serverId: '33333333-3333-4333-8333-333333333333',
      authorizationRevision: '42',
      serverMuted: true,
      serverDeafened: false,
    });
    f.roomEmit.mockImplementationOnce(() => {
      throw new Error('fanout unavailable');
    });
    await expect(f.dispatcher.applyDurable(snapshot)).rejects.toThrow();
    expect(f.participant.serverEnforcementRevision).toBe(42n);
    f.participant.serverEnforcementRevision = 43n;
    f.participant.serverMuted = false;
    await expect(f.dispatcher.applyDurable(snapshot)).resolves.toBe('applied');
    expect(f.snapshot).toHaveBeenCalledTimes(1);
    expect(f.roomEmit).toHaveBeenCalledTimes(1);
    expect(f.participant.serverMuted).toBe(false);
  });

  it('retries failed mutation before fanout rather than claiming completion', async () => {
    const f = fixture();
    f.mutate.mockRejectedValueOnce(new Error('pause failed'));
    await expect(f.dispatcher.applyDurable(command())).rejects.toThrow();
    expect(f.roomEmit).not.toHaveBeenCalled();
    await expect(f.dispatcher.applyDurable(command())).resolves.toBe('applied');
    expect(f.participant.producers.get('audio-one')!.producer.paused).toBe(true);
    expect(f.mutate).toHaveBeenCalledTimes(2);
  });

  it.each(['generation', 'boot', 'epoch', 'socket'] as const)(
    'does not mutate a mismatched exact %s',
    async (field) => {
      const f = fixture();
      const target = { ...command().target! };
      if (field === 'generation') target.sessionGeneration = successorGeneration;
      if (field === 'boot') target.nodeBootId = successorGeneration;
      if (field === 'epoch') target.credentialEpoch = '11111111111111111111111111111111';
      if (field === 'socket') target.socketId = 'successor-socket';
      await expect(f.dispatcher.applyDurable(command({ target }))).resolves.toBe('target_absent');
      expect(f.mutate).not.toHaveBeenCalled();
      expect(f.participant.producers.get('audio-one')!.producer.paused).toBe(false);
    }
  );

  it('does not infer target-absent completion while retained exact native termination is unresolved', async () => {
    const f = fixture();
    f.setAdmitted(undefined);
    f.setProvisional(undefined);
    const retained = deferred<void>();
    f.awaitNativeTermination.mockImplementationOnce(() => retained.promise);
    const pending = f.dispatcher.applyDurable(command());
    let complete = false;
    void pending.then(() => {
      complete = true;
    });
    await vi.waitFor(() =>
      expect(f.awaitNativeTermination).toHaveBeenCalledWith(
        roomId,
        userId,
        'socket-fixture',
        sessionGeneration
      )
    );
    expect(complete).toBe(false);
    expect(f.roomEmit).not.toHaveBeenCalled();
    retained.resolve();
    await expect(pending).resolves.toBe('target_absent');
  });

  it('acknowledgeable target-null markers never resolve a live same-user successor', async () => {
    const f = fixture();
    await expect(
      f.dispatcher.applyDurable(
        command({
          kind: 'disconnect',
          action: 'disconnect',
          scope: 'dm',
          reason: 'membership_removed',
          target: null,
        })
      )
    ).resolves.toBe('target_absent');
    expect(f.leave).not.toHaveBeenCalled();
    expect(f.disconnect).not.toHaveBeenCalled();
  });

  it('targets a registered provisional candidate while leaving the admitted predecessor intact', async () => {
    const f = fixture();
    const candidate = {
      ...f.participant,
      socketId: 'pending-socket',
      voiceEnforcementSessionGeneration: successorGeneration,
    };
    f.setProvisional(candidate);
    const candidateDisconnect = vi.fn();
    f.sockets.set('pending-socket', {
      emit: vi.fn(),
      disconnect: candidateDisconnect,
      data: {
        userId,
        roomId,
        credentialEpoch: '',
        voiceEnforcementSession: {
          ...f.sockets.get('socket-fixture')!.data.voiceEnforcementSession,
          socketId: 'pending-socket',
          sessionGeneration: successorGeneration,
        },
      },
    });
    await f.dispatcher.applyDurable(
      command({
        kind: 'disconnect',
        action: 'disconnect',
        reason: 'moderator_request',
        target: {
          ...command().target!,
          socketId: 'pending-socket',
          sessionGeneration: successorGeneration,
        },
      })
    );
    expect(f.remove).toHaveBeenCalledWith(roomId, userId, 'pending-socket', successorGeneration);
    expect(candidateDisconnect).toHaveBeenCalledWith(true);
    expect(f.leave).not.toHaveBeenCalled();
    expect(f.disconnect).not.toHaveBeenCalled();
  });

  it('supports exact legacy admitted targets without an admissionId and never removes another provisional session', async () => {
    const f = fixture();
    f.setProvisional({
      ...f.participant,
      socketId: 'new-pending',
      voiceEnforcementSessionGeneration: successorGeneration,
    });
    await f.dispatcher.applyDurable(
      command({ kind: 'disconnect', action: 'disconnect', reason: 'moderator_request' })
    );
    expect(f.leave).toHaveBeenCalledWith(roomId, userId, 'socket-fixture', sessionGeneration);
    expect(f.remove).not.toHaveBeenCalled();
    expect(f.order).toEqual(['leave-admitted', 'disconnect']);
  });

  it('captures the old target across teardown awaits and leaves a reused socket successor intact', async () => {
    const f = fixture();
    const gate = deferred<boolean>();
    f.leave.mockImplementationOnce(async () => {
      await gate.promise;
      return true;
    });
    const pending = f.dispatcher.applyDurable(
      command({ kind: 'disconnect', action: 'disconnect', reason: 'moderator_request' })
    );
    await vi.waitFor(() => expect(f.leave).toHaveBeenCalledTimes(1));
    f.setAdmitted({ ...f.participant, voiceEnforcementSessionGeneration: successorGeneration });
    const successorDisconnect = vi.fn();
    f.sockets.set('socket-fixture', {
      ...f.sockets.get('socket-fixture')!,
      disconnect: successorDisconnect,
      data: {
        ...f.sockets.get('socket-fixture')!.data,
        voiceEnforcementSession: {
          ...f.sockets.get('socket-fixture')!.data.voiceEnforcementSession,
          sessionGeneration: successorGeneration,
        },
      },
    });
    gate.resolve(true);
    await pending;
    expect(successorDisconnect).not.toHaveBeenCalled();
  });

  it.each(['admitted', 'provisional'] as const)(
    'retries the original %s socket effects after native-proof failure following local removal (#2624)',
    async (phase) => {
      const f = fixture();
      if (phase === 'provisional') {
        f.setAdmitted(undefined);
        f.setProvisional(f.participant);
      }
      const disconnectCommand = command({
        kind: 'disconnect',
        action: 'disconnect',
        reason: 'moderator_request',
      });
      expect(
        decodeVoiceCommand(new TextEncoder().encode(JSON.stringify(disconnectCommand)), generation)
      ).toEqual(disconnectCommand);
      const nativeFailure = new Error('fixture native proof temporarily unavailable');
      const firstProofStarted = deferred<void>();
      const firstProof = deferred<void>();
      f.awaitNativeTermination.mockImplementationOnce(() => {
        firstProofStarted.resolve();
        return firstProof.promise;
      });
      const firstAttempt = f.dispatcher.applyDurable(disconnectCommand);
      const rejectedAttempt = expect(firstAttempt).rejects.toBe(nativeFailure);
      await firstProofStarted.promise;
      expect(phase === 'admitted' ? f.leave : f.remove).toHaveBeenCalledWith(
        roomId,
        userId,
        'socket-fixture',
        sessionGeneration
      );
      expect(f.manager.getParticipant()).toBeUndefined();
      expect(f.manager.getProvisionalParticipant()).toBeUndefined();
      expect(f.socketEmit).not.toHaveBeenCalled();
      expect(f.disconnect).not.toHaveBeenCalled();
      firstProof.reject(nativeFailure);
      await rejectedAttempt;

      const successor = {
        ...f.participant,
        socketId: 'successor-socket',
        voiceEnforcementSessionGeneration: successorGeneration,
        producers: new Map([
          ['successor-audio', { kind: 'audio', source: 'mic', producer: { paused: false } }],
        ]),
      };
      const candidate = {
        ...successor,
        socketId: 'successor-candidate',
        voiceEnforcementSessionGeneration: '99999999-9999-4999-8999-999999999999',
      };
      f.setAdmitted(successor);
      f.setProvisional(candidate);
      const originalSocket = f.sockets.get('socket-fixture')!;
      const successorSockets = [successor, candidate].map((participant) => {
        const socket = {
          emit: vi.fn(),
          disconnect: vi.fn(),
          data: {
            ...originalSocket.data,
            voiceEnforcementSession: {
              ...originalSocket.data.voiceEnforcementSession,
              socketId: participant.socketId,
              sessionGeneration: participant.voiceEnforcementSessionGeneration,
            },
          },
        };
        f.sockets.set(participant.socketId, socket);
        return socket;
      });
      const retryProofStarted = deferred<void>();
      const retryProof = deferred<void>();
      f.awaitNativeTermination.mockImplementationOnce(() => {
        retryProofStarted.resolve();
        return retryProof.promise;
      });
      const retry = f.dispatcher.applyDurable(disconnectCommand);
      let retrySettled = false;
      void retry.then(
        () => {
          retrySettled = true;
        },
        () => {
          retrySettled = true;
        }
      );
      await retryProofStarted.promise;
      expect(retrySettled, 'durable retry must wait for the original native proof').toBe(false);
      expect(f.socketEmit).not.toHaveBeenCalled();
      expect(f.disconnect).not.toHaveBeenCalled();
      retryProof.resolve();
      const result = await retry;
      expect
        .soft(result, 'durable retry must complete its originally captured disconnect')
        .toBe('applied');
      expect
        .soft(f.socketEmit, 'durable retry must emit to the original socket exactly once')
        .toHaveBeenCalledExactlyOnceWith('force-disconnect', {
          channelId: roomId,
          reason: 'access_revoked',
        });
      expect
        .soft(f.disconnect, 'durable retry must close the original socket exactly once')
        .toHaveBeenCalledExactlyOnceWith(true);
      expect(f.manager.getParticipant()).toBe(successor);
      expect(f.manager.getProvisionalParticipant()).toBe(candidate);
      expect(successor.producers.get('successor-audio')!.producer.paused).toBe(false);
      for (const socket of successorSockets) {
        expect(socket.emit).not.toHaveBeenCalled();
        expect(socket.disconnect).not.toHaveBeenCalled();
      }
      expect(f.awaitNativeTermination).toHaveBeenCalledTimes(2);
      for (const call of f.awaitNativeTermination.mock.calls) {
        expect(call).toEqual([roomId, userId, 'socket-fixture', sessionGeneration]);
      }
      await expect(f.dispatcher.applyDurable(disconnectCommand)).resolves.toBe('already_complete');
      expect
        .soft(f.socketEmit, 'completed replay must retain exactly one original fanout')
        .toHaveBeenCalledTimes(1);
      expect
        .soft(f.disconnect, 'completed replay must retain exactly one original disconnect')
        .toHaveBeenCalledTimes(1);
    }
  );

  it.each(['admitted', 'provisional'] as const)(
    'positive control: native proof completes %s socket fanout and disconnect once (#2624)',
    async (phase) => {
      const f = fixture();
      if (phase === 'provisional') {
        f.setAdmitted(undefined);
        f.setProvisional(f.participant);
      }
      const nativeProofStarted = deferred<void>();
      const nativeProof = deferred<void>();
      f.awaitNativeTermination.mockImplementationOnce(() => {
        nativeProofStarted.resolve();
        return nativeProof.promise;
      });
      const disconnectCommand = command({
        kind: 'disconnect',
        action: 'disconnect',
        reason: 'moderator_request',
      });
      const pending = f.dispatcher.applyDurable(disconnectCommand);
      await nativeProofStarted.promise;
      expect(f.manager.getParticipant()).toBeUndefined();
      expect(f.manager.getProvisionalParticipant()).toBeUndefined();
      expect(f.socketEmit).not.toHaveBeenCalled();
      expect(f.disconnect).not.toHaveBeenCalled();
      nativeProof.resolve();
      await expect(pending).resolves.toBe('applied');
      expect(f.awaitNativeTermination).toHaveBeenCalledExactlyOnceWith(
        roomId,
        userId,
        'socket-fixture',
        sessionGeneration
      );
      expect(f.socketEmit).toHaveBeenCalledExactlyOnceWith('force-disconnect', {
        channelId: roomId,
        reason: 'access_revoked',
      });
      expect(f.disconnect).toHaveBeenCalledExactlyOnceWith(true);
      await expect(f.dispatcher.applyDurable(disconnectCommand)).resolves.toBe('already_complete');
      expect(f.socketEmit).toHaveBeenCalledTimes(1);
      expect(f.disconnect).toHaveBeenCalledTimes(1);
    }
  );

  it('passes the frozen exact snapshot identity and preserves inverse complete flags', async () => {
    const f = fixture();
    const muted = command({
      kind: 'server_moderation',
      serverId: '33333333-3333-4333-8333-333333333333',
      authorizationRevision: '9007199254740993',
      serverMuted: true,
      serverDeafened: true,
      action: 'deafen',
    });
    await f.dispatcher.applyDurable(muted);
    expect(f.snapshot).toHaveBeenCalledWith(
      roomId,
      userId,
      muted.target,
      true,
      true,
      9007199254740993n
    );
    const inverse = {
      ...muted,
      sequence: '2',
      commandId: successorGeneration,
      action: 'undeafen',
      authorizationRevision: '9007199254740994',
      serverMuted: true,
      serverDeafened: false,
    } as VoiceCommand;
    await f.dispatcher.applyDurable(inverse);
    expect(f.participant.serverMuted).toBe(true);
    expect(f.participant.serverDeafened).toBe(false);
  });

  it('refuses an effect boundary when authority has become uncertain', async () => {
    const f = fixture();
    f.ensureAuthority.mockImplementation(() => {
      throw new Error('owner invalid');
    });
    await expect(f.dispatcher.applyDurable(command())).rejects.toThrow();
    expect(f.mutate).not.toHaveBeenCalled();
    expect(f.roomEmit).not.toHaveBeenCalled();
  });
});

describe('VoiceCommandDispatcher compatibility ordering', () => {
  it('joins the active durable command without duplicating an asynchronous mutation', async () => {
    const f = fixture();
    const gate = deferred<void>();
    f.mutate.mockImplementationOnce(() => gate.promise);
    const durable = f.dispatcher.applyDurable(command());
    await vi.waitFor(() => expect(f.mutate).toHaveBeenCalledTimes(1));
    const copy = f.dispatcher.handleCompatibility(command() as unknown as Record<string, unknown>);
    gate.resolve();
    await Promise.all([durable, copy]);
    expect(f.mutate).toHaveBeenCalledTimes(1);
    expect(f.roomEmit).toHaveBeenCalledTimes(1);
  });

  it('keeps a future core copy queued until its durable turn', async () => {
    const f = fixture();
    const future = command({ sequence: '2', commandId: successorGeneration });
    await f.dispatcher.handleCompatibility(future as unknown as Record<string, unknown>);
    expect(f.mutate).not.toHaveBeenCalled();
    expect(f.legacy).not.toHaveBeenCalled();
    await f.dispatcher.applyDurable(command());
    await f.dispatcher.applyDurable(future);
    expect(f.mutate).toHaveBeenCalledTimes(2);
  });

  it('drops completed compatibility copies after a user resumes', async () => {
    const f = fixture();
    await f.dispatcher.applyDurable(command());
    f.participant.producers.get('audio-one')!.producer.paused = false;
    await f.dispatcher.handleCompatibility(command() as unknown as Record<string, unknown>);
    expect(f.mutate).toHaveBeenCalledTimes(1);
    expect(f.participant.producers.get('audio-one')!.producer.paused).toBe(false);
  });

  it('continues routing existing version-1 server snapshots through legacy decoding', async () => {
    const f = fixture();
    const legacy = {
      version: 1,
      channelId: roomId,
      userId,
      action: 'mute',
      authorizationRevision: '42',
      serverMuted: true,
      serverDeafened: false,
    };
    await f.dispatcher.handleCompatibility(legacy);
    expect(f.legacy).toHaveBeenCalledWith(legacy);
  });

  it.each([{ schema: 'wrong' }, { commandId: 'invalid' }, { streamGeneration: 'invalid' }])(
    'never falls back to legacy after reserved fields fail validation: %j',
    async (reserved) => {
      const f = fixture();
      await expect(
        f.dispatcher.handleCompatibility({ channelId: roomId, userId, action: 'mute', ...reserved })
      ).resolves.toBeUndefined();
      expect(f.legacy).not.toHaveBeenCalled();
      expect(f.mutate).not.toHaveBeenCalled();
    }
  );
});

describe('durable dispatcher with actual RoomManager admission state', () => {
  async function admittedFixture() {
    const router = createMockRouter();
    const manager = new RoomManager({
      getWebRtcServer: () => undefined,
      getOrCreateRouter: async () => router,
      removeRouter: vi.fn(),
      captureNativeTermination: () => ({
        worker: {},
        router,
        routerId: router.id,
        transportIds: [],
      }),
      confirmNativeTermination: async () => undefined,
      confirmAllRoutersClosed: async () => undefined,
    } as unknown as MediasoupService);
    await manager.joinRoom(
      roomId,
      userId,
      'socket-fixture',
      { username: 'fixture-user' },
      undefined,
      {
        credentialEpoch: '',
        voiceEnforcementSessionGeneration: sessionGeneration,
        mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
      }
    );
    const predecessor = manager.getParticipant(roomId, userId)!;
    const disconnect = vi.fn();
    const io = {
      to: () => ({ emit: vi.fn() }),
      sockets: {
        sockets: new Map([
          [
            'socket-fixture',
            {
              emit: vi.fn(),
              disconnect,
              data: {
                userId,
                roomId,
                credentialEpoch: '',
                voiceEnforcementSession: {
                  sessionGeneration,
                  nodeBootId,
                  roomId,
                  roomKind: 'channel',
                  userId,
                  socketId: 'socket-fixture',
                  credentialEpoch: '',
                },
              },
            },
          ],
        ]),
      },
    };
    const dispatcher = new VoiceCommandDispatcher({
      roomManager: manager,
      io: io as unknown as Server,
      nodeBootId,
      streamGeneration: generation,
      ensureAuthority: async () => undefined,
      handleLegacy: async () => undefined,
    });
    return { manager, dispatcher, predecessor, disconnect, sockets: io.sockets.sockets };
  }

  it('applies the exact frozen snapshot only to its matching admitted generation', async () => {
    const f = await admittedFixture();
    try {
      f.manager.setEnforcementAuthorityGate(() => undefined);
      const producer = createMockProducer({ id: 'exact-audio', kind: 'audio', paused: false });
      producer.pause.mockImplementation(async () => {
        producer.paused = true;
      });
      f.predecessor.producers.set(producer.id, {
        producer,
        kind: 'audio',
        source: 'mic',
        policed: false,
        createdAtMs: 0,
      } as never);
      const target = command().target!;
      await expect(
        f.manager.applyExactServerEnforcementSnapshot(
          roomId,
          userId,
          { ...target, sessionGeneration: successorGeneration },
          true,
          false,
          41n
        )
      ).resolves.toBe(false);
      expect(f.predecessor.serverMuted).toBe(false);
      expect(producer.paused).toBe(false);
      await expect(
        f.manager.applyExactServerEnforcementSnapshot(roomId, userId, target, true, false, 42n)
      ).resolves.toBe(true);
      expect(f.predecessor.serverMuted).toBe(true);
      expect(producer.paused).toBe(true);
      expect(f.predecessor.serverEnforcementAppliedRevision).toBe(42n);
    } finally {
      await f.manager.closeAll();
    }
  });

  it.each(['producer', 'consumer'] as const)(
    'checks the owner gate before the next individual audio %s pause',
    async (boundary) => {
      const f = await admittedFixture();
      try {
        let authority = true;
        f.manager.setEnforcementAuthorityGate(() => {
          if (!authority) throw new Error('fixture owner unavailable');
        });
        const first = createMockProducer({ id: 'first-audio', kind: 'audio', paused: false });
        first.pause.mockImplementation(async () => {
          first.paused = true;
        });
        first.pause.mockImplementationOnce(async () => {
          first.paused = true;
          authority = false;
        });
        f.predecessor.producers.set(first.id, {
          producer: first,
          kind: 'audio',
          source: 'mic',
          policed: false,
          createdAtMs: 0,
        } as never);
        const second = createMockProducer({ id: 'second-audio', kind: 'audio', paused: false });
        second.pause.mockImplementation(async () => {
          second.paused = true;
        });
        const consumer = createMockConsumer({ id: 'audio-consumer', kind: 'audio', paused: false });
        consumer.pause.mockImplementation(async () => {
          consumer.paused = true;
        });
        if (boundary === 'producer')
          f.predecessor.producers.set(second.id, {
            producer: second,
            kind: 'audio',
            source: 'mic',
            policed: false,
            createdAtMs: 0,
          } as never);
        else f.predecessor.consumers.set(consumer.id, consumer as never);
        const target = command().target!;
        await expect(
          f.manager.applyExactServerEnforcementSnapshot(
            roomId,
            userId,
            target,
            true,
            boundary === 'consumer',
            42n
          )
        ).rejects.toThrow('fixture owner unavailable');
        expect(first.paused).toBe(true);
        expect(second.pause).not.toHaveBeenCalled();
        expect(consumer.pause).not.toHaveBeenCalled();
        expect(f.predecessor.serverEnforcementAppliedRevision).not.toBe(42n);
        authority = true;
        await expect(
          f.manager.applyExactServerEnforcementSnapshot(
            roomId,
            userId,
            target,
            true,
            boundary === 'consumer',
            42n
          )
        ).resolves.toBe(true);
        expect(boundary === 'producer' ? second.paused : consumer.paused).toBe(true);
      } finally {
        await f.manager.closeAll();
      }
    }
  );

  it.each([
    'registered with room data',
    'registration response pending',
    'registered before A2',
  ] as const)(
    'removes an actual registered provisional candidate before disconnect while preserving the admitted predecessor: %s',
    async (phase) => {
      const f = await admittedFixture();
      try {
        await f.manager.joinRoom(
          roomId,
          userId,
          'pending-candidate',
          { username: 'candidate-user' },
          undefined,
          {
            credentialEpoch: '',
            voiceEnforcementSessionGeneration: successorGeneration,
            mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
            deferChannelPromotion: true,
            admissionId: '88888888-8888-4888-8888-888888888888',
          }
        );
        expect(f.manager.getProvisionalParticipant(roomId, userId)).toMatchObject({
          socketId: 'pending-candidate',
          voiceEnforcementSessionGeneration: successorGeneration,
        });
        expect(
          f.manager.hasExactProvisionalChannelParticipant(
            roomId,
            userId,
            'pending-candidate',
            '88888888-8888-4888-8888-888888888888'
          )
        ).toBe(true);
        const candidateDisconnect = vi.fn(() => {
          expect(f.manager.getProvisionalParticipant(roomId, userId)).toBeUndefined();
        });
        const oldSocket = f.sockets.get('socket-fixture')!;
        f.sockets.set('pending-candidate', {
          emit: vi.fn(),
          disconnect: candidateDisconnect,
          data: {
            ...oldSocket.data,
            roomId: phase === 'registered with room data' ? roomId : undefined,
            voiceEnforcementSession:
              phase === 'registration response pending'
                ? undefined
                : {
                    ...oldSocket.data.voiceEnforcementSession,
                    socketId: 'pending-candidate',
                    sessionGeneration: successorGeneration,
                  },
          },
        } as never);
        await f.dispatcher.applyDurable(
          command({
            kind: 'disconnect',
            action: 'disconnect',
            reason: 'moderator_request',
            target: {
              ...command().target!,
              socketId: 'pending-candidate',
              sessionGeneration: successorGeneration,
            },
          })
        );
        expect(f.manager.getProvisionalParticipant(roomId, userId)).toBeUndefined();
        expect(f.manager.getParticipant(roomId, userId)).toBe(f.predecessor);
        expect(candidateDisconnect).toHaveBeenCalledWith(true);
        expect(f.disconnect).not.toHaveBeenCalled();
        expect(
          f.manager.hasExactProvisionalChannelParticipant(
            roomId,
            userId,
            'pending-candidate',
            '88888888-8888-4888-8888-888888888888'
          )
        ).toBe(false);
        const lateSocketMembership = vi.fn();
        expect(() =>
          f.manager.promoteChannelParticipant(
            roomId,
            userId,
            'pending-candidate',
            {
              admissionId: '88888888-8888-4888-8888-888888888888',
              authorizationRevision: 42n,
              identity: { username: 'candidate-user' },
              entitlement: { ...FREE_MEDIA_ENTITLEMENT },
              ownerTier: 'free',
              permissions: 0n,
              serverMuted: false,
              serverDeafened: false,
            },
            lateSocketMembership
          )
        ).toThrow('Channel provisional participant is not owned by this socket');
        expect(lateSocketMembership).not.toHaveBeenCalled();
        expect(f.manager.getParticipant(roomId, userId)).toBe(f.predecessor);
      } finally {
        await f.manager.closeAll();
      }
    }
  );

  it('tears down the exact legacy admitted generation while preserving its provisional successor', async () => {
    const f = await admittedFixture();
    try {
      expect(f.predecessor.admissionId).toBeUndefined();
      await f.manager.joinRoom(
        roomId,
        userId,
        'pending-successor',
        { username: 'successor-user' },
        undefined,
        {
          credentialEpoch: '',
          voiceEnforcementSessionGeneration: successorGeneration,
          mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
          deferChannelPromotion: true,
          admissionId: '88888888-8888-4888-8888-888888888888',
        }
      );
      await f.dispatcher.applyDurable(
        command({ kind: 'disconnect', action: 'disconnect', reason: 'moderator_request' })
      );
      expect(f.manager.getParticipant(roomId, userId)).toBeUndefined();
      expect(f.manager.getProvisionalParticipant(roomId, userId)).toMatchObject({
        socketId: 'pending-successor',
        voiceEnforcementSessionGeneration: successorGeneration,
      });
      expect(f.disconnect).toHaveBeenCalledWith(true);
    } finally {
      await f.manager.closeAll();
    }
  });

  it('does not evict a replacement generation which reused the captured Socket.IO ID', async () => {
    const f = await admittedFixture();
    try {
      await f.manager.leaveRoom(roomId, userId);
      await f.manager.joinRoom(
        roomId,
        userId,
        'socket-fixture',
        { username: 'successor-user' },
        undefined,
        {
          credentialEpoch: '',
          voiceEnforcementSessionGeneration: successorGeneration,
          mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
        }
      );
      const successor = f.manager.getParticipant(roomId, userId)!;
      expect(successor).not.toBe(f.predecessor);
      await expect(
        f.dispatcher.applyDurable(
          command({ kind: 'disconnect', action: 'disconnect', reason: 'moderator_request' })
        )
      ).resolves.toBe('target_absent');
      expect(f.manager.getParticipant(roomId, userId)).toBe(successor);
      expect(f.disconnect).not.toHaveBeenCalled();
    } finally {
      await f.manager.closeAll();
    }
  });
});
