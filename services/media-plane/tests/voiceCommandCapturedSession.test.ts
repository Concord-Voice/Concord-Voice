import { describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import { VoiceCommandDispatcher } from '../src/lib/voiceCommandDispatcher.js';
import { RoomManager, SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION } from '../src/lib/roomManager.js';
import type { MediasoupService } from '../src/lib/mediasoup.js';
import {
  createMockConsumer,
  createMockRouter,
  createMockProducer,
  createMockTransport,
} from './mocks/mediasoup.js';
import type { Server } from 'socket.io';

const generation = '11111111-1111-4111-8111-111111111111';
const roomId = '22222222-2222-4222-8222-222222222222';
const userId = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const sessionGeneration = '44444444-4444-4444-8444-444444444444';
const successorGeneration = '77777777-7777-4777-8777-777777777777';
const nodeBootId = '55555555-5555-4555-8555-555555555555';

function deferred<T = void>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}

function command(
  kind: 'user_moderation' | 'disconnect' = 'user_moderation',
  moderationAction: 'mute' | 'deafen' = 'mute'
) {
  return {
    schema: 'concord.voice-enforcement' as const,
    version: 1 as const,
    streamGeneration: generation,
    commandId: '66666666-6666-4666-8666-666666666666',
    sequence: '1',
    scope: 'channel' as const,
    kind,
    action: kind === 'disconnect' ? ('disconnect' as const) : moderationAction,
    channelId: roomId,
    userId,
    target: { sessionGeneration, nodeBootId, socketId: 'captured-socket', credentialEpoch: '' },
  };
}

async function setup(
  options: {
    withSocket?: boolean;
    ensureAuthority?: () => Promise<void>;
    nativeTermination?: () => Promise<void>;
  } = {}
) {
  const router = createMockRouter();
  const nativeTermination = vi.fn(options.nativeTermination ?? (async () => undefined));
  const manager = new RoomManager({
    getWebRtcServer: () => undefined,
    getOrCreateRouter: async () => router,
    removeRouter: vi.fn(),
    captureNativeTermination: () => ({ worker: {}, router, routerId: router.id, transportIds: [] }),
    confirmNativeTermination: nativeTermination,
    confirmAllRoutersClosed: async () => undefined,
  } as unknown as MediasoupService);
  await manager.joinRoom(roomId, userId, 'captured-socket', { username: 'fixture' }, undefined, {
    credentialEpoch: '',
    voiceEnforcementSessionGeneration: sessionGeneration,
    mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
  });
  const participant = manager.getParticipant(roomId, userId)!;
  const io = {
    to: () => ({ emit: vi.fn() }),
    sockets: {
      sockets: new Map(
        options.withSocket === false
          ? []
          : [
              [
                'captured-socket',
                {
                  emit: vi.fn(),
                  disconnect: vi.fn(),
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
                      socketId: 'captured-socket',
                      credentialEpoch: '',
                    },
                  },
                },
              ],
            ]
      ),
    },
  };
  const dispatcher = new VoiceCommandDispatcher({
    roomManager: manager,
    io: io as unknown as Server,
    nodeBootId,
    streamGeneration: generation,
    ensureAuthority: options.ensureAuthority ?? (async () => undefined),
    handleLegacy: async () => undefined,
  });
  return { manager, participant, dispatcher, nativeTermination };
}

describe('durable effects stay bound to their captured voice session', () => {
  it('treats a genuinely absent exact target as complete without native proof', async () => {
    const f = await setup({ withSocket: false });
    try {
      await f.manager.leaveRoom(roomId, userId);
      f.nativeTermination.mockClear();
      await expect(f.dispatcher.applyDurable(command('disconnect'))).resolves.toBe('target_absent');
      expect(
        f.nativeTermination,
        'an absent exact target has no native close obligation'
      ).not.toHaveBeenCalled();
    } finally {
      await f.manager.closeAll();
    }
  });

  it.each(['mute', 'deafen'] as const)(
    'applies a healthy captured session %s exactly once',
    async (action) => {
      const f = await setup();
      try {
        const producer = createMockProducer({ id: 'healthy-audio', kind: 'audio', paused: false });
        const consumer = createMockConsumer({ id: 'healthy-consumer' });
        f.participant.producers.set(producer.id, {
          producer,
          kind: 'audio',
          source: 'mic',
          policed: false,
          createdAtMs: 0,
        } as never);
        f.participant.consumers.set(consumer.id, consumer as never);
        await expect(f.dispatcher.applyDurable(command('user_moderation', action))).resolves.toBe(
          'applied'
        );
        expect(
          producer.pause,
          `a healthy captured session ${action} must pause its own producer once`
        ).toHaveBeenCalledTimes(1);
        expect(
          consumer.pause,
          `a healthy captured session ${action} consumer effect must match its contract`
        ).toHaveBeenCalledTimes(action === 'deafen' ? 1 : 0);
      } finally {
        await f.manager.closeAll();
      }
    }
  );

  it.each(['mute', 'deafen'] as const)(
    'does not %s a same-user successor admitted during fresh authority continuity',
    async (action) => {
      let authorityCalls = 0;
      const continuity = deferred<void>();
      const f = await setup({
        ensureAuthority: async () => {
          authorityCalls += 1;
          if (authorityCalls === 2) await continuity.promise;
        },
      });
      try {
        const oldProducer = createMockProducer({ id: 'old-audio', kind: 'audio', paused: false });
        f.participant.producers.set(oldProducer.id, {
          producer: oldProducer,
          kind: 'audio',
          source: 'mic',
          policed: false,
          createdAtMs: 0,
        } as never);
        const applying = f.dispatcher.applyDurable(command('user_moderation', action));
        await Promise.resolve();
        await Promise.resolve();
        expect(authorityCalls).toBe(2);
        expect(oldProducer.pause).not.toHaveBeenCalled();

        await f.manager.leaveRoom(roomId, userId, 'captured-socket', sessionGeneration);
        await f.manager.joinRoom(
          roomId,
          userId,
          'successor-socket',
          { username: 'successor' },
          undefined,
          {
            credentialEpoch: '',
            voiceEnforcementSessionGeneration: successorGeneration,
            mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
          }
        );
        const successor = f.manager.getParticipant(roomId, userId)!;
        const successorProducer = createMockProducer({
          id: 'successor-audio',
          kind: 'audio',
          paused: false,
        });
        const successorConsumer = createMockConsumer({ id: 'successor-consumer' });
        successor.producers.set(successorProducer.id, {
          producer: successorProducer,
          kind: 'audio',
          source: 'mic',
          policed: false,
          createdAtMs: 0,
        } as never);
        successor.consumers.set(successorConsumer.id, successorConsumer as never);

        continuity.resolve();
        await expect(applying).resolves.toBe('applied');
        expect(
          successorProducer.pause,
          `a durable user ${action} must never pause a successor producer`
        ).not.toHaveBeenCalled();
        expect(
          successorConsumer.pause,
          `a durable user ${action} must never pause a successor consumer`
        ).not.toHaveBeenCalled();
      } finally {
        await f.manager.closeAll();
      }
    }
  );

  it('tears down an exact RoomManager target without its socket and retries retained native proof', async () => {
    const firstProof = deferred<void>();
    const retryProof = deferred<void>();
    const retryProofStarted = deferred<void>();
    let proofCalls = 0;
    const f = await setup({
      withSocket: false,
      nativeTermination: () => {
        proofCalls += 1;
        if (proofCalls === 1) return firstProof.promise;
        retryProofStarted.resolve();
        return retryProof.promise;
      },
    });
    try {
      const original = f.participant;
      original.sendTransport = createMockTransport() as never;
      const disconnect = command('disconnect');
      const firstAttempt = f.dispatcher.applyDurable(disconnect);
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
      expect(
        f.nativeTermination,
        'a missing socket must still create and prove the captured native teardown'
      ).toHaveBeenCalledTimes(1);
      expect(
        f.manager.getParticipant(roomId, userId),
        'the exact captured participant must be retired before proof completes'
      ).toBeUndefined();
      firstProof.reject(new Error('native target remains unproven'));
      await expect(firstAttempt).rejects.toThrow('Native media termination unavailable');

      await f.manager.joinRoom(
        roomId,
        userId,
        'successor-socket',
        { username: 'successor' },
        undefined,
        {
          credentialEpoch: '',
          voiceEnforcementSessionGeneration: successorGeneration,
          mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
        }
      );
      const successor = f.manager.getParticipant(roomId, userId)!;
      const retry = f.dispatcher.applyDurable(disconnect);
      const retryState = await Promise.race([
        retryProofStarted.promise.then(() => 'proof_started' as const),
        retry.then(
          () => 'completed_without_proof' as const,
          () => 'failed_without_proof' as const
        ),
      ]);
      expect(
        retryState,
        'a replay after failed native proof cannot complete without retrying authoritative proof'
      ).toBe('proof_started');
      expect(
        f.nativeTermination,
        'the failed native proof must remain a durable retry obligation'
      ).toHaveBeenCalledTimes(2);
      retryProof.resolve();
      await expect(retry).resolves.toBe('applied');
      expect(
        f.manager.getParticipant(roomId, userId),
        'retrying captured teardown must preserve a same-user successor'
      ).toBe(successor);
      expect(original.voiceEnforcementSessionGeneration).toBe(sessionGeneration);
    } finally {
      await f.manager.closeAll();
    }
  });
});
