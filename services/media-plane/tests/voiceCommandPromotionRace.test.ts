import { describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import { VoiceCommandDispatcher } from '../src/lib/voiceCommandDispatcher.js';
import {
  FREE_MEDIA_ENTITLEMENT,
  RoomManager,
  SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
} from '../src/lib/roomManager.js';
import type { MediasoupService } from '../src/lib/mediasoup.js';
import { createMockRouter, createMockTransport } from './mocks/mediasoup.js';
import type { Server } from 'socket.io';

const roomId = '22222222-2222-4222-8222-222222222222';
const userId = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const roomAnchorUserId = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const sessionGeneration = '44444444-4444-4444-8444-444444444444';
const successorGeneration = '77777777-7777-4777-8777-777777777777';
const nodeBootId = '55555555-5555-4555-8555-555555555555';
const streamGeneration = '11111111-1111-4111-8111-111111111111';

function deferred<T = void>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}

describe('durable disconnect during channel promotion', () => {
  it.each([
    { promotionBoundary: 0, label: 'already admitted positive control' },
    { promotionBoundary: 1, label: 'initial authority await' },
    { promotionBoundary: 2, label: 'removal authority await' },
    { promotionBoundary: 1, label: 'last participant replacement', lastParticipant: true },
  ])('$label: waits for exact native proof before completion', async ({ promotionBoundary, lastParticipant }) => {
    const authorityReached = deferred<void>();
    const authorityGate = deferred<void>();
    const nativeProofReached = deferred<void>();
    const nativeProofGate = deferred<void>();
    const nativeProof = vi.fn(async () => {
      nativeProofReached.resolve();
      await nativeProofGate.promise;
    });
    const router = createMockRouter();
    const manager = new RoomManager({
      getOrCreateRouter: async () => router,
      removeRouter: vi.fn(),
      captureNativeTermination: (_router: unknown, transportIds: string[]) => ({
        worker: {},
        router,
        routerId: router.id,
        transportIds,
      }),
      confirmNativeTermination: nativeProof,
      confirmAllRoutersClosed: async () => undefined,
    } as unknown as MediasoupService);
    await manager.joinRoom(roomId, userId, 'captured-socket', { username: 'fixture' }, undefined, {
      credentialEpoch: '',
      voiceEnforcementSessionGeneration: sessionGeneration,
      mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
      deferChannelPromotion: promotionBoundary !== 0,
      admissionId: '88888888-8888-4888-8888-888888888888',
    });
    if (!lastParticipant) {
      await manager.joinRoom(
        roomId,
        roomAnchorUserId,
        'room-anchor-socket',
        { username: 'room anchor' },
        undefined,
        {
          credentialEpoch: '',
          voiceEnforcementSessionGeneration: '99999999-9999-4999-8999-999999999999',
          mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
        }
      );
    }
    const capturedRoom = manager.getRoom(roomId);
    expect(capturedRoom).toBeDefined();
    const capturedCandidate =
      promotionBoundary === 0
        ? manager.getParticipant(roomId, userId)
        : manager.getRoom(roomId)!.pendingChannelParticipants.get(userId);
    expect(
      capturedCandidate,
      'the fixture must capture an actual participant object'
    ).toBeDefined();
    if (promotionBoundary === 0) {
      manager.getParticipant(roomId, userId)!.sendTransport = createMockTransport({
        id: 'captured-transport',
      });
    }
    const socket = {
      emit: vi.fn(),
      disconnect: vi.fn(),
      data: {
        userId,
        credentialEpoch: '',
        roomId: (promotionBoundary === 0 ? roomId : undefined) as string | undefined,
        voiceEnforcementSession: {
          sessionGeneration,
          nodeBootId,
          roomId,
          roomKind: 'channel' as const,
          userId,
          socketId: 'captured-socket',
          credentialEpoch: '',
        },
      },
    };
    const roomEmit = vi.fn();
    const io = {
      to: () => ({ emit: roomEmit }),
      sockets: { sockets: new Map([['captured-socket', socket]]) },
    };
    let authorityCalls = 0;
    const dispatcher = new VoiceCommandDispatcher({
      roomManager: manager,
      io: io as unknown as Server,
      nodeBootId,
      streamGeneration,
      ensureAuthority: async () => {
        authorityCalls += 1;
        if (authorityCalls !== promotionBoundary) return;
        authorityReached.resolve();
        await authorityGate.promise;
        manager.promoteChannelParticipant(
          roomId,
          userId,
          'captured-socket',
          {
            admissionId: '88888888-8888-4888-8888-888888888888',
            authorizationRevision: 42n,
            identity: { username: 'fixture' },
            entitlement: { ...FREE_MEDIA_ENTITLEMENT },
            ownerTier: 'free',
            permissions: 0n,
            serverMuted: false,
            serverDeafened: false,
          },
          () => {
            socket.data.roomId = roomId;
          }
        );
        expect(
          manager.getParticipant(roomId, userId),
          'promotion must install the captured object'
        ).toBe(capturedCandidate);
        manager.getParticipant(roomId, userId)!.sendTransport = createMockTransport({
          id: 'captured-transport',
        });
      },
      handleLegacy: async () => undefined,
    });
    const command = {
      schema: 'concord.voice-enforcement' as const,
      version: 1 as const,
      streamGeneration,
      commandId: '66666666-6666-4666-8666-666666666666',
      sequence: '1',
      scope: 'channel' as const,
      kind: 'disconnect' as const,
      action: 'disconnect' as const,
      reason: 'moderator_request',
      channelId: roomId,
      userId,
      target: { sessionGeneration, nodeBootId, socketId: 'captured-socket', credentialEpoch: '' },
    };

    let operation: Promise<unknown> | undefined;
    let commandCompleted = false;
    try {
      operation = dispatcher.applyDurable(command);
      void operation.then(
        () => {
          commandCompleted = true;
        },
        () => {
          commandCompleted = true;
        }
      );
      if (promotionBoundary !== 0) await authorityReached.promise;
      expect(command.target).toEqual({
        sessionGeneration,
        nodeBootId,
        socketId: 'captured-socket',
        credentialEpoch: '',
      });
      if (promotionBoundary !== 0) {
        expect(manager.getProvisionalParticipant(roomId, userId)?.socketId).toBe('captured-socket');
        authorityGate.resolve();
      } else {
        expect(manager.getParticipant(roomId, userId)).toBe(capturedCandidate);
      }

      const firstTeardownEvent = await Promise.race([
        nativeProofReached.promise.then(() => 'native-proof' as const),
        operation.then(
          () => 'completed-before-proof' as const,
          () => 'rejected-before-proof' as const
        ),
      ]);
      expect(
        firstTeardownEvent,
        'the captured admitted participant must reach native proof before command completion'
      ).toBe('native-proof');
      expect(
        commandCompleted,
        'durable completion must remain pending until exact native proof'
      ).toBe(false);
      expect(
        manager.getParticipant(roomId, userId),
        'the captured participant must be absent while its native teardown is unresolved'
      ).toBeUndefined();
      expect(
        nativeProof,
        'native teardown proof must be reached for the promoted transport'
      ).toHaveBeenCalledOnce();
      expect(socket.emit, 'socket fanout must wait for native proof').not.toHaveBeenCalled();
      expect(
        socket.disconnect,
        'socket teardown must wait for native proof'
      ).not.toHaveBeenCalled();
      expect(roomEmit, 'room fanout must wait for native proof').not.toHaveBeenCalled();

      const replacementUserId = lastParticipant ? roomAnchorUserId : userId;
      await manager.joinRoom(
        roomId,
        replacementUserId,
        lastParticipant ? 'replacement-room-socket' : 'successor-socket',
        { username: lastParticipant ? 'replacement room user' : 'successor' },
        undefined,
        {
          credentialEpoch: '',
          voiceEnforcementSessionGeneration: lastParticipant
            ? '99999999-9999-4999-8999-999999999999'
            : successorGeneration,
          mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
        }
      );
      const replacement = manager.getRoom(roomId);
      const successor = manager.getParticipant(roomId, replacementUserId);
      if (lastParticipant) {
        expect(replacement).toBeDefined();
        expect(successor).toBeDefined();
        expect(replacement).not.toBe(capturedRoom);
      }

      nativeProofGate.resolve();
      await expect(operation).resolves.toBe('applied');
      expect(
        manager.getParticipant(roomId, lastParticipant ? roomAnchorUserId : userId),
        'captured teardown must preserve its replacement'
      ).toBe(successor);
      expect(socket.disconnect, 'the captured socket is closed after proof').toHaveBeenCalledOnce();
    } finally {
      authorityGate.resolve();
      nativeProofGate.resolve();
      await operation?.catch(() => undefined);
      await manager.closeAll();
    }
  });
});
