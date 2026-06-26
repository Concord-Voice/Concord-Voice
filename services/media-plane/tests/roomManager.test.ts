import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
  createMockRouter,
  createMockTransport,
  createMockProducer,
  createMockConsumer,
  createRtpCapabilities,
  createRtpParameters,
} from './mocks/mediasoup.js';
import './mocks/logger.js';

// Mock mediasoup native module
vi.mock('mediasoup', () => ({
  createWorker: vi.fn(),
}));

// Mock config
vi.mock('@/config/index.js', () => ({
  config: {
    freeVideoPublisherCap: 8,
    freeAudioLastN: 8,
    audioLastNHoldMs: 2500,
    mediasoup: {
      webRtcTransport: {
        listenIps: [{ ip: '0.0.0.0', announcedIp: '127.0.0.1' }],
        enableUdp: true,
        enableTcp: true,
        preferUdp: true,
        initialAvailableOutgoingBitrate: 1_000_000,
        maxIncomingBitrate: 50_000_000,
      },
    },
    audioLevelObserver: {
      maxEntries: 1,
      threshold: -60,
      interval: 300,
    },
  },
}));

import {
  RoomManager,
  resolveVideoPublisherCap,
  ABSOLUTE_VIDEO_PUBLISHER_CEILING,
  resolveAudioLastN,
  ABSOLUTE_AUDIO_LAST_N_CEILING,
  SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
  parseMediaFrameCryptoVersion,
  CryptoVersionMismatchError,
} from '../src/lib/roomManager.js';
// `./mocks/logger.js` (imported above) replaces @/lib/logger with vi.fn() spies;
// importing it here gives us the SAME mocked object to assert on.
import { logger } from '../src/lib/logger.js';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function createMockMediasoupService(router = createMockRouter()) {
  return {
    getOrCreateRouter: vi.fn().mockResolvedValue(router),
    removeRouter: vi.fn(),
  };
}

function joinRoomWithSupportedCrypto(
  manager: RoomManager,
  roomId: string,
  userId: string,
  socketId: string,
  identity: { username: string; displayName?: string; avatarUrl?: string },
  rtpCapabilities?: any,
  entitlement?: any
) {
  return manager.joinRoom(
    roomId,
    userId,
    socketId,
    identity,
    rtpCapabilities,
    entitlement,
    SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION
  );
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('RoomManager', () => {
  let manager: RoomManager;
  let mockRouter: ReturnType<typeof createMockRouter>;
  let mockMediasoup: ReturnType<typeof createMockMediasoupService>;

  beforeEach(() => {
    mockRouter = createMockRouter();
    mockMediasoup = createMockMediasoupService(mockRouter);
    manager = new RoomManager(mockMediasoup as any);
  });

  // ── Room lifecycle ──────────────────────────────────────────────────

  describe('joinRoom', () => {
    it('creates a new room and returns rtpCapabilities', async () => {
      const result = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
        username: 'alice',
      });

      expect(result.rtpCapabilities).toBeDefined();
      expect(result.existingProducers).toEqual([]);
      expect(result.participants).toHaveLength(1);
      expect(result.participants[0].userId).toBe('u-1');
    });

    it('reuses existing room on second join', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const result = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', {
        username: 'bob',
      });

      expect(mockMediasoup.getOrCreateRouter).toHaveBeenCalledTimes(1);
      expect(result.participants).toHaveLength(2);
    });

    it('discards stale room when router is closed', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      mockRouter.closed = true;

      // Second join should detect closed router and recreate
      const newRouter = createMockRouter();
      mockMediasoup.getOrCreateRouter.mockResolvedValue(newRouter);

      const result = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', {
        username: 'bob',
      });
      expect(result.rtpCapabilities).toBe(newRouter.rtpCapabilities);
    });

    it('returns existing producers from other participants', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });

      // Add a producer for u-1
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const producer = createMockProducer({ kind: 'audio' });
      transport.produce.mockResolvedValueOnce(producer);
      await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'audio',
        createRtpParameters() as any,
        'mic'
      );

      // u-2 joins and should see u-1's producer
      const result = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', {
        username: 'bob',
      });
      expect(result.existingProducers).toHaveLength(1);
      expect(result.existingProducers[0].userId).toBe('u-1');
    });

    it('seeds the room media-frame crypto version from the first participant', async () => {
      const result = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
        username: 'alice',
      });

      expect(result.mediaFrameCryptoVersion).toBe(SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION);
      expect(manager.getRoom('room-1')?.mediaFrameCryptoVersion).toBe(
        SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION
      );
      expect(manager.getParticipant('room-1', 'u-1')?.mediaFrameCryptoVersion).toBe(
        SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION
      );
    });

    it('allows a second participant with the matching media-frame crypto version', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });

      const result = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', {
        username: 'bob',
      });

      expect(result.participants).toHaveLength(2);
      expect(result.mediaFrameCryptoVersion).toBe(SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION);
    });

    it('rejects legacy media-frame crypto versions before storing the participant', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const room = manager.getRoom('room-1')!;
      const epochBefore = room.e2eeEpoch;

      await expect(
        manager.joinRoom(
          'room-1',
          'u-legacy',
          'sock-legacy',
          { username: 'legacy' },
          undefined,
          undefined,
          1
        )
      ).rejects.toThrow('Unsupported media frame crypto version 1');

      expect(room.participants.has('u-legacy')).toBe(false);
      expect(room.e2eeEpoch).toBe(epochBefore);
    });

    it('rejects omitted media-frame crypto version before storing the participant', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const room = manager.getRoom('room-1')!;
      const epochBefore = room.e2eeEpoch;

      await expect(
        manager.joinRoom(
          'room-1',
          'u-missing',
          'sock-missing',
          { username: 'missing' },
          undefined,
          undefined,
          undefined
        )
      ).rejects.toThrow('Unsupported media frame crypto version missing');

      expect(room.participants.has('u-missing')).toBe(false);
      expect(room.e2eeEpoch).toBe(epochBefore);
    });

    it('rejects legacy reconnect attempts before replacing the existing participant', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-original', {
        username: 'alice',
      });
      const room = manager.getRoom('room-1')!;
      const epochBefore = room.e2eeEpoch;

      await expect(
        manager.joinRoom(
          'room-1',
          'u-1',
          'sock-legacy',
          { username: 'legacy' },
          undefined,
          undefined,
          1
        )
      ).rejects.toThrow('Unsupported media frame crypto version 1');

      expect(room.participants.get('u-1')?.socketId).toBe('sock-original');
      expect(room.e2eeEpoch).toBe(epochBefore);
    });

    it('rejects missing or legacy media-frame crypto declarations at the socket boundary', () => {
      expect(parseMediaFrameCryptoVersion(SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION)).toBe(
        SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION
      );
      expect(() => parseMediaFrameCryptoVersion(undefined)).toThrow(
        'Unsupported media frame crypto version missing'
      );
      expect(() => parseMediaFrameCryptoVersion(1)).toThrow(
        'Unsupported media frame crypto version 1'
      );
      expect(() => parseMediaFrameCryptoVersion(null)).toThrow(
        'Unsupported media frame crypto version null'
      );
      expect(() => parseMediaFrameCryptoVersion('2')).toThrow(
        'Unsupported media frame crypto version 2'
      );
      expect(() => parseMediaFrameCryptoVersion(false)).toThrow(
        'Unsupported media frame crypto version false'
      );
      expect(() => parseMediaFrameCryptoVersion([])).toThrow(
        'Unsupported media frame crypto version array'
      );
      expect(() => parseMediaFrameCryptoVersion({ version: 2 })).toThrow(
        'Unsupported media frame crypto version object'
      );
      expect(() => parseMediaFrameCryptoVersion('4')).toThrow(
        'Unsupported media frame crypto version 4'
      );
    });

    it('accepts v3 and v4 during the v3→v4 rollout window', () => {
      expect(parseMediaFrameCryptoVersion(3)).toBe(3);
      expect(parseMediaFrameCryptoVersion(4)).toBe(4);
    });

    it('rejects v2 now that the window has moved to v3→v4', () => {
      expect(() => parseMediaFrameCryptoVersion(2)).toThrow(
        'Unsupported media frame crypto version 2'
      );
    });

    it('raises the room from v3 to v4 when a v4 participant joins (higher-version-wins)', async () => {
      await manager.joinRoom('r', 'u1', 's1', { username: 'a' }, undefined, undefined, 3);
      const res = await manager.joinRoom(
        'r',
        'u2',
        's2',
        { username: 'b' },
        undefined,
        undefined,
        4
      );

      expect(res.mediaFrameCryptoVersion).toBe(4);
      expect(manager.getRoom('r')?.mediaFrameCryptoVersion).toBe(4);
    });

    it('keeps the room version when an equal-version participant joins', async () => {
      await manager.joinRoom('r', 'u1', 's1', { username: 'a' }, undefined, undefined, 4);
      const res = await manager.joinRoom(
        'r',
        'u2',
        's2',
        { username: 'b' },
        undefined,
        undefined,
        4
      );

      expect(res.mediaFrameCryptoVersion).toBe(4);
      expect(manager.getRoom('r')?.mediaFrameCryptoVersion).toBe(4);
    });

    it('rejects a v3 joiner into a v4 room with a typed mismatch', async () => {
      await manager.joinRoom('r', 'u1', 's1', { username: 'a' }, undefined, undefined, 4);
      const room = manager.getRoom('r')!;
      const epochBefore = room.e2eeEpoch;

      const promise = manager.joinRoom('r', 'u2', 's2', { username: 'b' }, undefined, undefined, 3);
      await expect(promise).rejects.toMatchObject({
        code: 'crypto_version_mismatch',
        roomVersion: 4,
        joinVersion: 3,
      });
      await expect(promise).rejects.toBeInstanceOf(CryptoVersionMismatchError);

      // Gate must fire BEFORE participant storage / epoch mutation.
      expect(room.participants.has('u2')).toBe(false);
      expect(room.e2eeEpoch).toBe(epochBefore);
    });
  });

  describe('Self-deafen (#685)', () => {
    it('setParticipantDeafen sets the isDeafened flag', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      manager.setParticipantDeafen('room-1', 'u-1', true);
      expect(manager.getParticipant('room-1', 'u-1')?.isDeafened).toBe(true);
    });

    it('setParticipantDeafen clears the isDeafened flag', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      manager.setParticipantDeafen('room-1', 'u-1', true);
      manager.setParticipantDeafen('room-1', 'u-1', false);
      expect(manager.getParticipant('room-1', 'u-1')?.isDeafened).toBe(false);
    });

    it('setParticipantDeafen is a no-op for an unknown participant', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      expect(() => manager.setParticipantDeafen('room-1', 'ghost', true)).not.toThrow();
      expect(manager.getParticipant('room-1', 'ghost')).toBeUndefined();
    });

    it('joinRoom snapshot defaults isDeafened to false', async () => {
      const result = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
        username: 'alice',
      });
      expect(result.participants[0].isDeafened).toBe(false);
    });

    it('joinRoom snapshot carries an existing self-deafen (late joiner sees it)', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      manager.setParticipantDeafen('room-1', 'u-1', true);

      const result = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', {
        username: 'bob',
      });
      const alice = result.participants.find((p) => p.userId === 'u-1');
      expect(alice?.isDeafened).toBe(true);
    });
  });

  describe('Audio testing status (#1163)', () => {
    it('setParticipantTestingStatus sets and clears the isTesting flag', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      manager.setParticipantTestingStatus('room-1', 'u-1', true);
      expect(manager.getParticipant('room-1', 'u-1')?.isTesting).toBe(true);

      manager.setParticipantTestingStatus('room-1', 'u-1', false);
      expect(manager.getParticipant('room-1', 'u-1')?.isTesting).toBe(false);
    });

    it('joinRoom snapshot carries existing testing status', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      manager.setParticipantTestingStatus('room-1', 'u-1', true);

      const result = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', {
        username: 'bob',
      });
      const alice = result.participants.find((p) => p.userId === 'u-1');
      expect(alice?.isTesting).toBe(true);
    });
  });

  describe('joinRoom — reconnection', () => {
    it('cleans up old session when same userId rejoins', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-old', {
        username: 'alice',
      });
      const result = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-new', {
        username: 'alice',
      });

      // Should have exactly 1 participant (the new session)
      expect(result.participants).toHaveLength(1);
      const participant = manager.getParticipant('room-1', 'u-1');
      expect(participant?.socketId).toBe('sock-new');
    });
  });

  describe('leaveRoom', () => {
    it('removes participant and emits user-left event', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', { username: 'bob' });
      await manager.leaveRoom('room-1', 'u-1');

      const room = manager.getRoom('room-1');
      expect(room?.participants.size).toBe(1);

      const leftEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'user-left');
      expect(leftEvent).toBeDefined();
      expect(leftEvent![0].userId).toBe('u-1');
    });

    it('emits user-left with the authoritative post-leave E2EE epoch', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', { username: 'bob' });

      await manager.leaveRoom('room-1', 'u-1');

      const leftEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'user-left');
      expect(leftEvent?.[0]).toEqual(
        expect.objectContaining({
          type: 'user-left',
          roomId: 'room-1',
          userId: 'u-1',
          e2eeEpoch: 3,
        })
      );
    });

    it('closes room and emits room-empty when last participant leaves', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await manager.leaveRoom('room-1', 'u-1');

      expect(manager.getRoom('room-1')).toBeUndefined();
      const emptyEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'room-empty');
      expect(emptyEvent).toBeDefined();
    });

    it('cleans up transports, producers, and consumers', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });

      // Create send transport + producer
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const producer = createMockProducer();
      transport.produce.mockResolvedValueOnce(producer);
      await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'audio',
        createRtpParameters() as any,
        'mic'
      );

      await manager.leaveRoom('room-1', 'u-1');

      expect(producer.close).toHaveBeenCalled();
      expect(transport.close).toHaveBeenCalled();
    });

    it('drops keyframe cooldowns for departing participants', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', { username: 'bob' });
      const room = manager.getRoom('room-1')!;
      room.keyframeRequestCooldowns.set('u-1', Date.now());

      await manager.leaveRoom('room-1', 'u-1');

      expect(room.keyframeRequestCooldowns.has('u-1')).toBe(false);
    });

    it('is a no-op for non-existent room or participant', async () => {
      await manager.leaveRoom('nonexistent', 'u-1');
      expect(manager.getRoom('nonexistent')).toBeUndefined();
    });
  });

  // ── Transport management ────────────────────────────────────────────

  describe('createTransport', () => {
    beforeEach(async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
    });

    it('creates a send transport', async () => {
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);

      const result = await manager.createTransport('room-1', 'u-1', 'send');

      expect(result.id).toBe(transport.id);
      expect(result.iceParameters).toBeDefined();
      expect(result.dtlsParameters).toBeDefined();
    });

    it('creates a recv transport and adds to recvTransports map', async () => {
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);

      const result = await manager.createTransport('room-1', 'u-1', 'recv');

      expect(result.id).toBe(transport.id);
      const participant = manager.getParticipant('room-1', 'u-1');
      expect(participant?.recvTransports.has(transport.id)).toBe(true);
    });

    it('replaces old send transport if one exists', async () => {
      const oldTransport = createMockTransport();
      const newTransport = createMockTransport();
      mockRouter.createWebRtcTransport
        .mockResolvedValueOnce(oldTransport)
        .mockResolvedValueOnce(newTransport);

      await manager.createTransport('room-1', 'u-1', 'send');
      await manager.createTransport('room-1', 'u-1', 'send');

      expect(oldTransport.close).toHaveBeenCalled();
      const participant = manager.getParticipant('room-1', 'u-1');
      expect(participant?.sendTransport?.id).toBe(newTransport.id);
    });

    it('throws if room not found', async () => {
      await expect(manager.createTransport('nonexistent', 'u-1', 'send')).rejects.toThrow(
        'Room not found'
      );
    });

    it('throws if participant not found', async () => {
      await expect(manager.createTransport('room-1', 'unknown', 'send')).rejects.toThrow(
        'Participant not found'
      );
    });

    // ── Per-user tier bitrate cap (#1300) ─────────────────────────────────

    it('caps the SEND transport with the FREE tier maxManualBitrateBps (5 Mbps)', async () => {
      // u-free joins with the free entitlement (5_000_000 bps).
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-free',
        'sock-free',
        { username: 'free' },
        undefined,
        {
          tier: 'free',
          allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard'],
          minPtimeMs: 20,
          maxManualBitrateBps: 5_000_000,
        }
      );
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);

      await manager.createTransport('room-1', 'u-free', 'send');

      // The producing (send) transport is capped at the user's tier ceiling,
      // NOT the global ~50 Mbps default.
      expect(transport.setMaxIncomingBitrate).toHaveBeenCalledWith(5_000_000);
    });

    it('caps the SEND transport with the PREMIUM tier maxManualBitrateBps (10 Mbps)', async () => {
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-prem',
        'sock-prem',
        { username: 'prem' },
        undefined,
        {
          tier: 'premium',
          allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
          minPtimeMs: 10,
          maxManualBitrateBps: 10_000_000,
        }
      );
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);

      await manager.createTransport('room-1', 'u-prem', 'send');

      expect(transport.setMaxIncomingBitrate).toHaveBeenCalledWith(10_000_000);
    });

    it('does NOT apply the per-user cap to the RECV transport (keeps the global default)', async () => {
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-free',
        'sock-free',
        { username: 'free' },
        undefined,
        {
          tier: 'free',
          allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard'],
          minPtimeMs: 20,
          maxManualBitrateBps: 5_000_000,
        }
      );
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);

      await manager.createTransport('room-1', 'u-free', 'recv');

      // recv carries media TO the peer — the global maxIncomingBitrate (50 Mbps
      // from the config mock), never the per-user send cap.
      expect(transport.setMaxIncomingBitrate).toHaveBeenCalledWith(50_000_000);
      expect(transport.setMaxIncomingBitrate).not.toHaveBeenCalledWith(5_000_000);
    });

    it('defaults to the FREE floor send cap when no entitlement is supplied (pre-#1300 caller)', async () => {
      // The default beforeEach join (u-1) passed no entitlement → free floor.
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);

      await manager.createTransport('room-1', 'u-1', 'send');

      expect(transport.setMaxIncomingBitrate).toHaveBeenCalledWith(5_000_000);
    });

    it('logs a PII-safe warning and PROCEEDS (does not fail the join) when setMaxIncomingBitrate rejects (#1300 fail-open)', async () => {
      // A WebRtcTransport always supports setMaxIncomingBitrate, so a rejection
      // is a transient worker error, not a capability gap. The security control
      // fails OPEN (transport left uncapped) — but observably: a PII-safe warn
      // is logged (ids + direction only) and the join still succeeds.
      const warnSpy = vi.mocked(logger.warn);
      warnSpy.mockClear();
      const transport = createMockTransport();
      transport.setMaxIncomingBitrate.mockRejectedValueOnce(new Error('worker IPC error'));
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);

      const info = await manager.createTransport('room-1', 'u-1', 'send');
      expect(info).toBeDefined(); // join not blocked by the flaky cap call

      const call = warnSpy.mock.calls.find((c) => String(c[0]).includes('fail-open'));
      expect(call).toBeDefined();
      const meta = call![1] as Record<string, unknown>;
      expect(meta.userId).toBe('u-1');
      expect(meta.direction).toBe('send');
      // No identity/display fields leaked.
      expect(JSON.stringify(meta)).not.toMatch(/displayName|avatarUrl|username/);
    });
  });

  describe('connectTransport', () => {
    it('calls transport.connect with dtlsParameters', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const dtls = { fingerprints: [], role: 'auto' as const };
      await manager.connectTransport('room-1', 'u-1', transport.id, dtls as any);

      expect(transport.connect).toHaveBeenCalledWith({ dtlsParameters: dtls });
    });

    it('throws if transport not found', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await expect(manager.connectTransport('room-1', 'u-1', 'bad-id', {} as any)).rejects.toThrow(
        'Transport not found'
      );
    });
  });

  // ── Producer management ─────────────────────────────────────────────

  describe('produce', () => {
    let transport: ReturnType<typeof createMockTransport>;

    beforeEach(async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');
    });

    it('creates producer and emits producer-added event', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      const producer = createMockProducer({ kind: 'audio' });
      transport.produce.mockResolvedValueOnce(producer);

      const info = await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'audio',
        createRtpParameters() as any,
        'mic'
      );

      expect(info.producerId).toBe(producer.id);
      expect(info.source).toBe('mic');
      const addedEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'producer-added');
      expect(addedEvent).toBeDefined();
    });

    it('adds mic audio producer to AudioLevelObserver', async () => {
      const producer = createMockProducer({ kind: 'audio' });
      transport.produce.mockResolvedValueOnce(producer);

      await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'audio',
        createRtpParameters() as any,
        'mic'
      );

      expect(mockRouter._audioLevelObserver.addProducer).toHaveBeenCalledWith({
        producerId: producer.id,
      });
    });

    it('does NOT add screen-audio to AudioLevelObserver', async () => {
      // First add a screen producer (required for screen-audio)
      const screenProducer = createMockProducer({ kind: 'video' });
      transport.produce.mockResolvedValueOnce(screenProducer);
      await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'video',
        createRtpParameters() as any,
        'screen'
      );

      mockRouter._audioLevelObserver.addProducer.mockClear();

      const screenAudioProducer = createMockProducer({ kind: 'audio' });
      transport.produce.mockResolvedValueOnce(screenAudioProducer);
      await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'audio',
        createRtpParameters() as any,
        'screen-audio'
      );

      expect(mockRouter._audioLevelObserver.addProducer).not.toHaveBeenCalled();
    });

    it('enforces camera limit (free cap = 8 from config mock)', async () => {
      // Add 8 camera producers across multiple participants
      for (let i = 0; i < 8; i++) {
        const uid = `u-cam-${i}`;
        await joinRoomWithSupportedCrypto(manager, 'room-1', uid, `sock-${i + 100}`, {
          username: `user${i}`,
        });
        const t = createMockTransport();
        mockRouter.createWebRtcTransport.mockResolvedValueOnce(t);
        await manager.createTransport('room-1', uid, 'send');
        const p = createMockProducer({ kind: 'video' });
        t.produce.mockResolvedValueOnce(p);
        await manager.produce('room-1', uid, t.id, 'video', createRtpParameters() as any, 'camera');
      }

      // 9th should fail
      const p = createMockProducer({ kind: 'video' });
      transport.produce.mockResolvedValueOnce(p);
      await expect(
        manager.produce(
          'room-1',
          'u-1',
          transport.id,
          'video',
          createRtpParameters() as any,
          'camera'
        )
      ).rejects.toThrow('Video participant limit reached (max 8)');
    });

    it('rejects the 9th camera producer (free cap = 8) and keeps the 8 allowed', async () => {
      for (let i = 0; i < 8; i++) {
        transport.produce.mockResolvedValueOnce(
          createMockProducer({ kind: 'video', id: `cam-${i}` })
        );
        await manager.produce(
          'room-1',
          'u-1',
          transport.id,
          'video',
          createRtpParameters() as any,
          'camera'
        );
      }

      await expect(
        manager.produce(
          'room-1',
          'u-1',
          transport.id,
          'video',
          createRtpParameters() as any,
          'camera'
        )
      ).rejects.toThrow('Video participant limit reached (max 8)');
    });

    it('does not apply the camera cap to screen producers', async () => {
      // Seed 8 cameras (at the camera cap) ...
      for (let i = 0; i < 8; i++) {
        transport.produce.mockResolvedValueOnce(
          createMockProducer({ kind: 'video', id: `cam-${i}` })
        );
        await manager.produce(
          'room-1',
          'u-1',
          transport.id,
          'video',
          createRtpParameters() as any,
          'camera'
        );
      }
      // ... a screen producer still succeeds (separate limit of 5).
      transport.produce.mockResolvedValueOnce(createMockProducer({ kind: 'video', id: 'scr-0' }));
      const info = await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'video',
        createRtpParameters() as any,
        'screen'
      );
      expect(info.source).toBe('screen');
    });

    it('enforces the camera cap under a concurrent produce burst (TOCTOU-safe, #1539)', async () => {
      // produce() yields the event loop at the await; without a synchronous slot
      // reservation, N concurrent calls would all pass the count check against a
      // stale (pre-record) count and overrun the cap. Fire cap+1 calls WITHOUT
      // awaiting individually so their synchronous reservation sections interleave.
      // (This test FAILS against the pre-reservation code — it is the regression
      // lock for the Gitar/security-review TOCTOU finding.)
      let pid = 0;
      transport.produce.mockImplementation(async () =>
        createMockProducer({ kind: 'video', id: `burst-cam-${pid++}` })
      );
      const results = await Promise.allSettled(
        Array.from({ length: 9 }, () =>
          manager.produce(
            'room-1',
            'u-1',
            transport.id,
            'video',
            createRtpParameters() as any,
            'camera'
          )
        )
      );
      const fulfilled = results.filter((r) => r.status === 'fulfilled');
      const rejected = results.filter((r): r is PromiseRejectedResult => r.status === 'rejected');
      // Exactly the cap is admitted; the surplus is rejected — the cap holds.
      expect(fulfilled.length).toBe(8);
      expect(rejected.length).toBe(1);
      expect(rejected[0].reason.message).toContain('Video participant limit reached (max 8)');
    });

    it('rejects a video producer with a non-video source (anti-mislabel, #1539)', async () => {
      await expect(
        manager.produce('room-1', 'u-1', transport.id, 'video', createRtpParameters() as any, 'mic')
      ).rejects.toThrow('Invalid media source for producer kind');
    });

    it('rejects an audio producer with a video source', async () => {
      await expect(
        manager.produce(
          'room-1',
          'u-1',
          transport.id,
          'audio',
          createRtpParameters() as any,
          'camera'
        )
      ).rejects.toThrow('Invalid media source for producer kind');
    });

    it('rejects an unknown source string', async () => {
      await expect(
        manager.produce(
          'room-1',
          'u-1',
          transport.id,
          'video',
          createRtpParameters() as any,
          'bogus' as never
        )
      ).rejects.toThrow('Invalid media source for producer kind');
    });

    it('enforces screen share limit (max 5)', async () => {
      for (let i = 0; i < 5; i++) {
        const uid = `u-scr-${i}`;
        await joinRoomWithSupportedCrypto(manager, 'room-1', uid, `sock-${i + 200}`, {
          username: `user${i}`,
        });
        const t = createMockTransport();
        mockRouter.createWebRtcTransport.mockResolvedValueOnce(t);
        await manager.createTransport('room-1', uid, 'send');
        const p = createMockProducer({ kind: 'video' });
        t.produce.mockResolvedValueOnce(p);
        await manager.produce('room-1', uid, t.id, 'video', createRtpParameters() as any, 'screen');
      }

      const p = createMockProducer({ kind: 'video' });
      transport.produce.mockResolvedValueOnce(p);
      await expect(
        manager.produce(
          'room-1',
          'u-1',
          transport.id,
          'video',
          createRtpParameters() as any,
          'screen'
        )
      ).rejects.toThrow('Screen share limit reached (max 5)');
    });

    it('rejects screen-audio when kind is not audio', async () => {
      // Add screen producer first
      const sp = createMockProducer({ kind: 'video' });
      transport.produce.mockResolvedValueOnce(sp);
      await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'video',
        createRtpParameters() as any,
        'screen'
      );

      await expect(
        manager.produce(
          'room-1',
          'u-1',
          transport.id,
          'video',
          createRtpParameters() as any,
          'screen-audio'
        )
      ).rejects.toThrow('screen-audio source must be audio kind');
    });

    it('rejects screen-audio when no screen producer exists', async () => {
      await expect(
        manager.produce(
          'room-1',
          'u-1',
          transport.id,
          'audio',
          createRtpParameters() as any,
          'screen-audio'
        )
      ).rejects.toThrow('screen-audio requires an active screen producer');
    });

    it('rejects duplicate screen-audio', async () => {
      // Screen producer
      const sp = createMockProducer({ kind: 'video' });
      transport.produce.mockResolvedValueOnce(sp);
      await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'video',
        createRtpParameters() as any,
        'screen'
      );

      // First screen-audio
      const sa1 = createMockProducer({ kind: 'audio' });
      transport.produce.mockResolvedValueOnce(sa1);
      await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'audio',
        createRtpParameters() as any,
        'screen-audio'
      );

      // Second screen-audio should fail
      await expect(
        manager.produce(
          'room-1',
          'u-1',
          transport.id,
          'audio',
          createRtpParameters() as any,
          'screen-audio'
        )
      ).rejects.toThrow('Only one active screen-audio producer allowed');
    });

    it('throws if send transport does not match', async () => {
      await expect(
        manager.produce(
          'room-1',
          'u-1',
          'wrong-transport-id',
          'audio',
          createRtpParameters() as any,
          'mic'
        )
      ).rejects.toThrow('Send transport not found or mismatch');
    });
  });

  // ── Per-user audio tier gating (#1300) ────────────────────────────────
  describe('produce — per-user audio tier gating (#1300)', () => {
    // u-1 (free floor: minPtime 20, standard opus ceiling 96 kbps) is joined by
    // the parent produce beforeEach. Add a premium peer with their own transport.
    let freeTransport: ReturnType<typeof createMockTransport>;
    let premiumTransport: ReturnType<typeof createMockTransport>;

    beforeEach(async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      freeTransport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(freeTransport);
      await manager.createTransport('room-1', 'u-1', 'send');

      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-prem',
        'sock-prem',
        { username: 'prem' },
        undefined,
        {
          tier: 'premium',
          allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
          minPtimeMs: 10,
          maxManualBitrateBps: 10_000_000,
        }
      );
      premiumTransport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(premiumTransport);
      await manager.createTransport('room-1', 'u-prem', 'send');
    });

    it('REJECTS a free-tier mic producer with ptime below 20 ms (and never creates the producer)', async () => {
      await expect(
        manager.produce(
          'room-1',
          'u-1',
          freeTransport.id,
          'audio',
          createRtpParameters({ ptime: 10 }) as any,
          'mic'
        )
      ).rejects.toThrow('Audio producer exceeds tier media limits');

      // The producer is rejected BEFORE the await — transport.produce never ran.
      expect(freeTransport.produce).not.toHaveBeenCalled();
      expect(manager.getParticipant('room-1', 'u-1')?.producers.size).toBe(0);
    });

    it('REJECTS a free-tier mic producer whose opus bitrate exceeds the standard ceiling (96 kbps)', async () => {
      await expect(
        manager.produce(
          'room-1',
          'u-1',
          freeTransport.id,
          'audio',
          // 256 kbps = the hifi tier, above the free 'standard' ceiling of 96 kbps.
          createRtpParameters({ maxaveragebitrate: 256_000 }) as any,
          'mic'
        )
      ).rejects.toThrow('Audio producer exceeds tier media limits');

      expect(freeTransport.produce).not.toHaveBeenCalled();
      expect(manager.getParticipant('room-1', 'u-1')?.producers.size).toBe(0);
    });

    it('ADMITS a stock free client that declares only minptime (no actual ptime) — minptime is NOT the effective ptime', async () => {
      // Regression lock (Gitar review, #1300): Chromium/Electron emit
      // `minptime=10` (or lower) by default while actually packetizing at 20 ms.
      // minptime is the LOWER BOUND the encoder may drop to, not the real ptime,
      // so it must NOT trip the ptime gate — otherwise every stock free-tier mic
      // is falsely rejected. An undeclared actual ptime admits (bitrate cap backstops).
      const producer = createMockProducer({ kind: 'audio' });
      freeTransport.produce.mockResolvedValueOnce(producer);
      const info = await manager.produce(
        'room-1',
        'u-1',
        freeTransport.id,
        'audio',
        createRtpParameters({ minptime: 5 }) as any, // only minptime, no ptime
        'mic'
      );
      expect(info).toBeDefined();
      expect(freeTransport.produce).toHaveBeenCalled();
    });

    it('ADMITS a free-tier mic producer at exactly the standard ceiling and 20 ms ptime', async () => {
      const producer = createMockProducer({ kind: 'audio' });
      freeTransport.produce.mockResolvedValueOnce(producer);

      const info = await manager.produce(
        'room-1',
        'u-1',
        freeTransport.id,
        'audio',
        createRtpParameters({ ptime: 20, maxaveragebitrate: 96_000 }) as any,
        'mic'
      );

      expect(info.producerId).toBe(producer.id);
      expect(freeTransport.produce).toHaveBeenCalled();
    });

    it('ADMITS a producer with NO opus fmtp params — DOCUMENTED best-effort residual, NOT benign default', async () => {
      // This is the explicit, accepted #1300 residual (spec §5): the audio gate
      // inspects only client-DECLARED, OPTIONAL opus fmtp. A patched client
      // EVADES the gate simply by omitting `maxaveragebitrate`/`ptime` — and we
      // admit-on-absence BY DESIGN, because the stock client legitimately omits
      // these, so we cannot fail-closed on a missing fmtp without rejecting
      // legitimate free users. The advisory transport bitrate cap is the only
      // remaining backstop here; hard enforcement against a non-cooperative
      // client is future stats-monitoring work (#1542), not this gate. This
      // test asserts the residual is REACHED on purpose, not that an over-tier
      // stream is benign.
      const producer = createMockProducer({ kind: 'audio' });
      freeTransport.produce.mockResolvedValueOnce(producer);

      const info = await manager.produce(
        'room-1',
        'u-1',
        freeTransport.id,
        'audio',
        createRtpParameters() as any, // no parameters → gate admits (residual)
        'mic'
      );

      expect(info.producerId).toBe(producer.id);
    });

    it('ADMITS a PREMIUM mic producer at studio bitrate (510 kbps) and 10 ms ptime', async () => {
      const producer = createMockProducer({ kind: 'audio' });
      premiumTransport.produce.mockResolvedValueOnce(producer);

      const info = await manager.produce(
        'room-1',
        'u-prem',
        premiumTransport.id,
        'audio',
        createRtpParameters({ ptime: 10, maxaveragebitrate: 510_000 }) as any,
        'mic'
      );

      expect(info.producerId).toBe(producer.id);
      expect(premiumTransport.produce).toHaveBeenCalled();
    });

    it('does NOT gate a non-mic audio source (screen-audio is not tier-quality-gated)', async () => {
      // Seed a screen producer (screen-audio requires one) ...
      freeTransport.produce.mockResolvedValueOnce(createMockProducer({ kind: 'video', id: 'scr' }));
      await manager.produce(
        'room-1',
        'u-1',
        freeTransport.id,
        'video',
        createRtpParameters() as any,
        'screen'
      );

      // ... then a screen-audio stream with an "over-tier" opus shape still passes
      // (it is NOT mic, so the audio-tier gate does not apply).
      const screenAudio = createMockProducer({ kind: 'audio', id: 'scr-aud' });
      freeTransport.produce.mockResolvedValueOnce(screenAudio);
      const info = await manager.produce(
        'room-1',
        'u-1',
        freeTransport.id,
        'audio',
        createRtpParameters({ ptime: 5, maxaveragebitrate: 510_000 }) as any,
        'screen-audio'
      );

      expect(info.source).toBe('screen-audio');
    });

    it('the rejection violation log contains NO rtpParameters payload, codec params, or display fields', async () => {
      const { logger } = await import('../src/lib/logger.js');
      const warnSpy = vi.mocked(logger.warn);
      warnSpy.mockClear();

      await expect(
        manager.produce(
          'room-1',
          'u-1',
          freeTransport.id,
          'audio',
          createRtpParameters({ ptime: 5, maxaveragebitrate: 256_000 }) as any,
          'mic'
        )
      ).rejects.toThrow('Audio producer exceeds tier media limits');

      // Exactly the PII-safe shape: { userId, kind, source, reason } — no more.
      const rejectionCall = warnSpy.mock.calls.find((c) =>
        String(c[0]).includes('over-tier media')
      );
      expect(rejectionCall).toBeDefined();
      const meta = rejectionCall![1] as Record<string, unknown>;
      expect(Object.keys(meta).sort()).toEqual(['kind', 'reason', 'source', 'userId']);
      expect(meta.userId).toBe('u-1');
      expect(meta.kind).toBe('audio');
      expect(meta.source).toBe('mic');
      // Nothing media-derived leaked.
      const serialized = JSON.stringify(meta);
      expect(serialized).not.toContain('256000');
      expect(serialized).not.toContain('rtpParameters');
      expect(serialized).not.toContain('maxaveragebitrate');
      expect(serialized).not.toContain('codecs');
    });
  });

  describe('pauseProducer / resumeProducer', () => {
    it('pauses and resumes a producer', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const producer = createMockProducer();
      transport.produce.mockResolvedValueOnce(producer);
      const info = await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'audio',
        createRtpParameters() as any,
        'mic'
      );

      await manager.pauseProducer('room-1', 'u-1', info.producerId);
      expect(producer.pause).toHaveBeenCalled();

      await manager.resumeProducer('room-1', 'u-1', info.producerId);
      expect(producer.resume).toHaveBeenCalled();
    });

    it('throws if producer not found', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await expect(manager.pauseProducer('room-1', 'u-1', 'bad-id')).rejects.toThrow(
        'Producer not found'
      );
    });
  });

  describe('closeProducer', () => {
    it('closes producer and emits producer-removed event', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const producer = createMockProducer();
      transport.produce.mockResolvedValueOnce(producer);
      const info = await manager.produce(
        'room-1',
        'u-1',
        transport.id,
        'audio',
        createRtpParameters() as any,
        'mic'
      );

      const source = await manager.closeProducer('room-1', 'u-1', info.producerId);

      expect(source).toBe('mic');
      expect(producer.close).toHaveBeenCalled();
      const removedEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'producer-removed');
      expect(removedEvent).toBeDefined();
    });

    it('returns null for non-existent room', async () => {
      const result = await manager.closeProducer('nonexistent', 'u-1', 'p-1');
      expect(result).toBeNull();
    });
  });

  // ── Consumer management ─────────────────────────────────────────────

  describe('consume', () => {
    let producerInfo: { producerId: string };
    let recvTransport: ReturnType<typeof createMockTransport>;

    beforeEach(async () => {
      // u-1 joins and produces
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const sendTransport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(sendTransport);
      await manager.createTransport('room-1', 'u-1', 'send');
      const producer = createMockProducer({ kind: 'audio' });
      sendTransport.produce.mockResolvedValueOnce(producer);
      producerInfo = await manager.produce(
        'room-1',
        'u-1',
        sendTransport.id,
        'audio',
        createRtpParameters() as any,
        'mic'
      );

      // u-2 joins with rtpCapabilities and creates recv transport
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-2',
        'sock-2',
        { username: 'bob' },
        createRtpCapabilities() as any
      );
      recvTransport = createMockTransport();
      const consumer = createMockConsumer({ producerId: producerInfo.producerId, kind: 'audio' });
      recvTransport.consume.mockResolvedValue(consumer);
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(recvTransport);
      await manager.createTransport('room-1', 'u-2', 'recv');
    });

    it('creates consumer on recv transport (starts paused)', async () => {
      const result = await manager.consume('room-1', 'u-2', producerInfo.producerId);

      expect(result).not.toBeNull();
      expect(result!.producerUserId).toBe('u-1');
      expect(result!.source).toBe('mic');
    });

    it('returns null when router.canConsume returns false', async () => {
      mockRouter.canConsume.mockReturnValueOnce(false);

      const result = await manager.consume('room-1', 'u-2', producerInfo.producerId);
      expect(result).toBeNull();
    });

    it('throws when no recv transport exists', async () => {
      // u-3 joins without creating recv transport
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-3',
        'sock-3',
        { username: 'charlie' },
        createRtpCapabilities() as any
      );

      await expect(manager.consume('room-1', 'u-3', producerInfo.producerId)).rejects.toThrow(
        'No receive transport'
      );
    });

    it('throws when rtpCapabilities not set', async () => {
      // u-4 joins WITHOUT rtpCapabilities
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-4', 'sock-4', { username: 'dave' });
      const recvT = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(recvT);
      await manager.createTransport('room-1', 'u-4', 'recv');

      await expect(manager.consume('room-1', 'u-4', producerInfo.producerId)).rejects.toThrow(
        'RTP capabilities not set'
      );
    });

    it('passes producer metadata into recvTransport.consume appData', async () => {
      await manager.consume('room-1', 'u-2', producerInfo.producerId);

      expect(recvTransport.consume).toHaveBeenCalledWith(
        expect.objectContaining({
          appData: {
            source: 'mic',
            producerUserId: 'u-1',
            producerId: producerInfo.producerId,
          },
        })
      );
    });
  });

  describe('setPreferredCameraLayers', () => {
    function validLayerDemand(consumerId: string, overrides: Record<string, unknown> = {}) {
      return {
        consumerId,
        spatialLayer: 2,
        temporalLayer: 2,
        visible: true,
        cssWidth: 1280,
        cssHeight: 720,
        devicePixelRatio: 1,
        role: 'focus',
        focusedWindow: true,
        pressureStepDown: false,
        ...overrides,
      };
    }

    async function ensureParticipant(userId: string, videoCodecs?: string[]) {
      if (manager.getParticipant('room-1', userId)) return;
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        userId,
        `sock-${userId}`,
        { username: userId },
        videoCodecs ? (createRtpCapabilities(videoCodecs) as any) : undefined
      );
    }

    async function addCameraConsumer(userId: string, consumerId: string, videoCodecs?: string[]) {
      await ensureParticipant(userId, videoCodecs);
      const participant = manager.getParticipant('room-1', userId)!;
      const consumer = createMockConsumer({
        id: consumerId,
        kind: 'video',
        type: 'svc',
        appData: { source: 'camera' },
      });
      participant.consumers.set(consumerId, consumer as any);
      return { participant, consumer };
    }

    async function addCameraProducer(userId: string, producerId: string, videoCodecs?: string[]) {
      await ensureParticipant(userId, videoCodecs);
      const participant = manager.getParticipant('room-1', userId)!;
      const producer = createMockProducer({ id: producerId, kind: 'video' });
      participant.producers.set(producerId, {
        producer: producer as any,
        source: 'camera',
        kind: 'video',
      });
      return producer;
    }

    function gateEvents(handler: ReturnType<typeof vi.fn>) {
      return handler.mock.calls
        .map((call: any[]) => call[0])
        .filter((event) => event.type === 'camera-layering-gate');
    }

    it('rejects a consumer not owned by the caller', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });

      await expect(
        manager.setPreferredCameraLayers('room-1', 'u-1', validLayerDemand('missing'))
      ).rejects.toThrow('Consumer not found');
    });

    it('clamps and applies preferred layers for an owned camera consumer', async () => {
      const { consumer } = await addCameraConsumer('u-1', 'c1');

      const result = await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c1')
      );

      expect(consumer.setPreferredLayers).toHaveBeenCalledWith({
        spatialLayer: 1,
        temporalLayer: 2,
      });
      expect(result.effectiveLayers).toEqual({ spatialLayer: 1, temporalLayer: 2 });
    });

    it('caps 1080p camera demand to the default free entitlement layer', async () => {
      const { consumer } = await addCameraConsumer('u-1', 'c1');

      const result = await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c1', { cssWidth: 1920, cssHeight: 1080 })
      );

      expect(consumer.setPreferredLayers).toHaveBeenCalledWith({
        spatialLayer: 1,
        temporalLayer: 2,
      });
      expect(result.effectiveLayers).toEqual({ spatialLayer: 1, temporalLayer: 2 });
    });

    it('clamps from physical pixels so HiDPI clients match server policy', async () => {
      const { participant, consumer } = await addCameraConsumer('u-1', 'c1');
      participant.maxManualBitrateBps = 10_000_000;

      const result = await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c1', { cssWidth: 700, cssHeight: 400, devicePixelRatio: 2 })
      );

      expect(consumer.setPreferredLayers).toHaveBeenCalledWith({
        spatialLayer: 2,
        temporalLayer: 2,
      });
      expect(result.effectiveLayers).toEqual({ spatialLayer: 2, temporalLayer: 2 });
    });

    it('rejects a video consumer that is not a camera source', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const participant = manager.getParticipant('room-1', 'u-1')!;
      const consumer = createMockConsumer({
        id: 'screen-c1',
        kind: 'video',
        appData: { source: 'screen' },
      });
      participant.consumers.set('screen-c1', consumer as any);

      await expect(
        manager.setPreferredCameraLayers('room-1', 'u-1', validLayerDemand('screen-c1'))
      ).rejects.toThrow('Consumer is not camera');
    });

    it('records demand and recomputes the gate without calling setPreferredLayers on simple consumers', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);
      const svcCodecs = ['video/VP9', 'video/H264'];
      await addCameraProducer('u-1', 'p1', svcCodecs);
      await addCameraProducer('u-2', 'p2', svcCodecs);
      const { consumer: consumer1 } = await addCameraConsumer('u-1', 'c1');
      const participant = manager.getParticipant('room-1', 'u-1')!;
      const consumer2 = createMockConsumer({
        id: 'c2',
        kind: 'video',
        type: 'simple',
        appData: { source: 'camera' },
        setPreferredLayers: vi.fn(() => Promise.reject(new Error('simple consumer'))),
      });
      (consumer1 as any).type = 'simple';
      consumer1.setPreferredLayers.mockRejectedValue(new Error('simple consumer'));
      participant.consumers.set('c2', consumer2 as any);

      await manager.setPreferredCameraLayers('room-1', 'u-1', validLayerDemand('c1'));
      await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c2', { cssWidth: 1920, cssHeight: 1080 })
      );

      expect(consumer1.setPreferredLayers).not.toHaveBeenCalled();
      expect(consumer2.setPreferredLayers).not.toHaveBeenCalled();
      expect(manager.getRoom('room-1')!.cameraLayeringGateEnabled).toBe(true);
      expect(gateEvents(handler)).toEqual([
        { type: 'camera-layering-gate', roomId: 'room-1', enabled: true },
      ]);
    });

    it('emits camera-layering-gate once when demand makes layering beneficial', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);
      await addCameraConsumer('u-1', 'c1');
      const participant = manager.getParticipant('room-1', 'u-1')!;
      const consumer2 = createMockConsumer({
        id: 'c2',
        kind: 'video',
        appData: { source: 'camera' },
      });
      participant.consumers.set('c2', consumer2 as any);

      await manager.setPreferredCameraLayers('room-1', 'u-1', validLayerDemand('c1'));
      await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c2', { cssWidth: 1920, cssHeight: 1080 })
      );
      await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c2', { cssWidth: 1920, cssHeight: 1080 })
      );

      expect(gateEvents(handler)).toEqual([
        { type: 'camera-layering-gate', roomId: 'room-1', enabled: true },
      ]);
    });

    it('keeps gate on at one useful consumer, then disables when no useful demand remains', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);
      await addCameraConsumer('u-1', 'c1');
      const participant = manager.getParticipant('room-1', 'u-1')!;
      const consumer2 = createMockConsumer({
        id: 'c2',
        kind: 'video',
        appData: { source: 'camera' },
      });
      participant.consumers.set('c2', consumer2 as any);

      await manager.setPreferredCameraLayers('room-1', 'u-1', validLayerDemand('c1'));
      await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c2', { cssWidth: 1920, cssHeight: 1080 })
      );

      expect(manager.closeConsumer('room-1', 'u-1', 'c1')).toBe(true);

      expect(manager.getRoom('room-1')!.cameraLayeringGateEnabled).toBe(true);
      expect(gateEvents(handler)).toEqual([
        { type: 'camera-layering-gate', roomId: 'room-1', enabled: true },
      ]);

      expect(manager.closeConsumer('room-1', 'u-1', 'c2')).toBe(true);

      expect(gateEvents(handler)).toEqual([
        { type: 'camera-layering-gate', roomId: 'room-1', enabled: true },
        { type: 'camera-layering-gate', roomId: 'room-1', enabled: false },
      ]);
    });

    it('keeps fallback simulcast gate disabled with only two camera producers', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);
      const fallbackCodecs = ['video/H264', 'video/VP8'];
      await addCameraProducer('u-1', 'p1', fallbackCodecs);
      await addCameraProducer('u-2', 'p2', fallbackCodecs);
      await addCameraConsumer('u-1', 'c1');
      await addCameraConsumer('u-1', 'c2');

      await manager.setPreferredCameraLayers('room-1', 'u-1', validLayerDemand('c1'));
      await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c2', { cssWidth: 1920, cssHeight: 1080 })
      );

      expect(manager.getRoom('room-1')!.cameraLayeringGateEnabled).toBe(false);
      expect(gateEvents(handler)).toEqual([]);
    });

    it('enables fallback simulcast gate at three camera producers', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);
      const fallbackCodecs = ['video/H264', 'video/VP8'];
      await addCameraProducer('u-1', 'p1', fallbackCodecs);
      await addCameraProducer('u-2', 'p2', fallbackCodecs);
      await addCameraProducer('u-3', 'p3', fallbackCodecs);
      await addCameraConsumer('u-1', 'c1');
      await addCameraConsumer('u-1', 'c2');

      await manager.setPreferredCameraLayers('room-1', 'u-1', validLayerDemand('c1'));
      await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c2', { cssWidth: 1920, cssHeight: 1080 })
      );

      expect(manager.getRoom('room-1')!.cameraLayeringGateEnabled).toBe(true);
      expect(gateEvents(handler)).toEqual([
        { type: 'camera-layering-gate', roomId: 'room-1', enabled: true },
      ]);
    });

    it('keeps fallback simulcast gate on at two producers, then disables below hysteresis floor', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);
      const fallbackCodecs = ['video/H264', 'video/VP8'];
      await addCameraProducer('u-1', 'p1', fallbackCodecs);
      await addCameraProducer('u-2', 'p2', fallbackCodecs);
      await addCameraProducer('u-3', 'p3', fallbackCodecs);
      await addCameraConsumer('u-1', 'c1');
      await addCameraConsumer('u-1', 'c2');
      await manager.setPreferredCameraLayers('room-1', 'u-1', validLayerDemand('c1'));
      await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c2', { cssWidth: 1920, cssHeight: 1080 })
      );

      await manager.closeProducer('room-1', 'u-3', 'p3');

      expect(manager.getRoom('room-1')!.cameraLayeringGateEnabled).toBe(true);
      expect(gateEvents(handler)).toEqual([
        { type: 'camera-layering-gate', roomId: 'room-1', enabled: true },
      ]);

      await manager.closeProducer('room-1', 'u-2', 'p2');

      expect(manager.getRoom('room-1')!.cameraLayeringGateEnabled).toBe(false);
      expect(gateEvents(handler)).toEqual([
        { type: 'camera-layering-gate', roomId: 'room-1', enabled: true },
        { type: 'camera-layering-gate', roomId: 'room-1', enabled: false },
      ]);
    });

    it('enables SVC-capable gate with two camera producers', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);
      const svcCodecs = ['video/VP9', 'video/H264'];
      await addCameraProducer('u-1', 'p1', svcCodecs);
      await addCameraProducer('u-2', 'p2', svcCodecs);
      await addCameraConsumer('u-1', 'c1');
      await addCameraConsumer('u-1', 'c2');

      await manager.setPreferredCameraLayers('room-1', 'u-1', validLayerDemand('c1'));
      await manager.setPreferredCameraLayers(
        'room-1',
        'u-1',
        validLayerDemand('c2', { cssWidth: 1920, cssHeight: 1080 })
      );

      expect(manager.getRoom('room-1')!.cameraLayeringGateEnabled).toBe(true);
      expect(gateEvents(handler)).toEqual([
        { type: 'camera-layering-gate', roomId: 'room-1', enabled: true },
      ]);
    });
  });

  describe('resumeConsumer / pauseConsumer / closeConsumer', () => {
    it('resumes a consumer', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const recvTransport = createMockTransport();
      const consumer = createMockConsumer();
      recvTransport.consume.mockResolvedValue(consumer);
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(recvTransport);
      await manager.createTransport('room-1', 'u-1', 'recv');

      // Manually add the consumer to the participant
      const participant = manager.getParticipant('room-1', 'u-1')!;
      participant.consumers.set(consumer.id, consumer as any);

      await manager.resumeConsumer('room-1', 'u-1', consumer.id);
      expect(consumer.resume).toHaveBeenCalled();
    });

    it('pauses a consumer', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const participant = manager.getParticipant('room-1', 'u-1')!;
      const consumer = createMockConsumer();
      participant.consumers.set(consumer.id, consumer as any);

      await manager.pauseConsumer('room-1', 'u-1', consumer.id);
      expect(consumer.pause).toHaveBeenCalled();
    });

    it('closes a consumer and removes from map', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const participant = manager.getParticipant('room-1', 'u-1')!;
      const consumer = createMockConsumer();
      participant.consumers.set(consumer.id, consumer as any);

      const result = manager.closeConsumer('room-1', 'u-1', consumer.id);
      expect(result).toBe(true);
      expect(consumer.close).toHaveBeenCalled();
      expect(participant.consumers.has(consumer.id)).toBe(false);
    });

    it('returns false for non-existent consumer', () => {
      const result = manager.closeConsumer('room-1', 'u-1', 'bad-id');
      expect(result).toBe(false);
    });
  });

  describe('requestKeyFrame', () => {
    async function setupVideoProducer() {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'sender', 'sock-sender', {
        username: 'sender',
      });
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'viewer',
        'sock-viewer',
        { username: 'viewer' },
        createRtpCapabilities() as any
      );
      const sendTransport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(sendTransport);
      await manager.createTransport('room-1', 'sender', 'send');
      const producer = createMockProducer({
        kind: 'video',
      });
      sendTransport.produce.mockResolvedValueOnce(producer);
      await manager.produce(
        'room-1',
        'sender',
        sendTransport.id,
        'video',
        createRtpParameters() as any,
        'camera'
      );

      const recvTransport = createMockTransport();
      const consumer = createMockConsumer({
        kind: 'video',
        producerId: producer.id,
        requestKeyFrame: vi.fn().mockResolvedValue(undefined),
      });
      recvTransport.consume.mockResolvedValueOnce(consumer);
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(recvTransport);
      await manager.createTransport('room-1', 'viewer', 'recv');
      await manager.consume('room-1', 'viewer', producer.id);
      return consumer;
    }

    it('requests keyframes for the requester video consumers', async () => {
      const consumer = await setupVideoProducer();
      const requestKeyFrame = (manager as any).requestKeyFrame;
      expect(requestKeyFrame).toBeTypeOf('function');

      await requestKeyFrame.call(manager, 'room-1', 'viewer', 'sender');

      expect(consumer.requestKeyFrame).toHaveBeenCalledTimes(1);
    });

    it('rejects an unknown sender', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'viewer', 'sock-viewer', {
        username: 'viewer',
      });
      const requestKeyFrame = (manager as any).requestKeyFrame;
      expect(requestKeyFrame).toBeTypeOf('function');

      await expect(requestKeyFrame.call(manager, 'room-1', 'viewer', 'missing')).rejects.toThrow(
        'Sender not found'
      );
    });

    it('reserves cooldown while the keyframe request is pending', async () => {
      const consumer = await setupVideoProducer();
      let release!: () => void;
      const pending = new Promise<void>((resolve) => {
        release = resolve;
      });
      (consumer.requestKeyFrame as any).mockReturnValueOnce(pending);
      const requestKeyFrame = (manager as any).requestKeyFrame;

      const first = requestKeyFrame.call(manager, 'room-1', 'viewer', 'sender');
      const second = requestKeyFrame.call(manager, 'room-1', 'viewer', 'sender');

      await expect(second).resolves.toBe(0);
      expect(consumer.requestKeyFrame).toHaveBeenCalledTimes(1);

      release();
      await expect(first).resolves.toBe(1);
    });

    it('does not cooldown no-op requests without an active video producer', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'viewer', 'sock-viewer', {
        username: 'viewer',
      });
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'sender', 'sock-sender', {
        username: 'sender',
      });
      const room = manager.getRoom('room-1')!;
      const requestKeyFrame = (manager as any).requestKeyFrame;

      await expect(requestKeyFrame.call(manager, 'room-1', 'viewer', 'sender')).resolves.toBe(0);

      expect(room.keyframeRequestCooldowns.has('sender')).toBe(false);
    });

    it('does not start cooldown when keyframe request fails', async () => {
      const consumer = await setupVideoProducer();
      const requestKeyFrame = (manager as any).requestKeyFrame;
      (consumer.requestKeyFrame as any)
        .mockRejectedValueOnce(new Error('pli failed'))
        .mockResolvedValueOnce(undefined);

      await expect(requestKeyFrame.call(manager, 'room-1', 'viewer', 'sender')).rejects.toThrow(
        'pli failed'
      );
      await requestKeyFrame.call(manager, 'room-1', 'viewer', 'sender');

      expect(consumer.requestKeyFrame).toHaveBeenCalledTimes(2);
    });

    it('suppresses repeated requests during the cooldown window', async () => {
      vi.useFakeTimers();
      try {
        const consumer = await setupVideoProducer();
        const requestKeyFrame = (manager as any).requestKeyFrame;
        expect(requestKeyFrame).toBeTypeOf('function');

        await requestKeyFrame.call(manager, 'room-1', 'viewer', 'sender');
        await requestKeyFrame.call(manager, 'room-1', 'viewer', 'sender');
        expect(consumer.requestKeyFrame).toHaveBeenCalledTimes(1);

        await vi.advanceTimersByTimeAsync(5000);
        await requestKeyFrame.call(manager, 'room-1', 'viewer', 'sender');
        expect(consumer.requestKeyFrame).toHaveBeenCalledTimes(2);
      } finally {
        vi.useRealTimers();
      }
    });
  });

  // ── Codec floor ─────────────────────────────────────────────────────

  describe('computeCodecFloor', () => {
    it('returns null for non-existent room', () => {
      expect(manager.computeCodecFloor('nonexistent')).toBeNull();
    });

    it('returns null when fewer than 2 participants have capabilities', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      expect(manager.computeCodecFloor('room-1')).toBeNull();
    });

    it('returns intersection of video codecs', async () => {
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-1',
        'sock-1',
        { username: 'alice' },
        createRtpCapabilities(['video/VP8', 'video/VP9', 'video/H264']) as any
      );
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-2',
        'sock-2',
        { username: 'bob' },
        createRtpCapabilities(['video/VP8', 'video/H264']) as any
      );

      const floor = manager.computeCodecFloor('room-1');

      expect(floor).toContain('video/vp8');
      expect(floor).toContain('video/h264');
      expect(floor).not.toContain('video/vp9');
    });

    it('normalizes mimeType to lowercase', async () => {
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-1',
        'sock-1',
        { username: 'alice' },
        createRtpCapabilities(['video/VP8']) as any
      );
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-2',
        'sock-2',
        { username: 'bob' },
        createRtpCapabilities(['video/VP8']) as any
      );

      const floor = manager.computeCodecFloor('room-1');
      expect(floor).toEqual(['video/vp8']);
    });

    it('skips participants with null rtpCapabilities', async () => {
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-1',
        'sock-1',
        { username: 'alice' },
        createRtpCapabilities(['video/VP8']) as any
      );
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', { username: 'bob' }); // no capabilities
      await joinRoomWithSupportedCrypto(
        manager,
        'room-1',
        'u-3',
        'sock-3',
        { username: 'charlie' },
        createRtpCapabilities(['video/VP8', 'video/VP9']) as any
      );

      const floor = manager.computeCodecFloor('room-1');
      expect(floor).toEqual(['video/vp8']);
    });
  });

  // ── E2EE epoch ──────────────────────────────────────────────────────

  describe('E2EE epoch', () => {
    it('increments epoch on join', async () => {
      const result1 = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
        username: 'alice',
      });
      expect(result1.e2eeEpoch).toBe(1);

      const result2 = await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', {
        username: 'bob',
      });
      expect(result2.e2eeEpoch).toBe(2);
    });

    it('emits user-joined with the authoritative E2EE epoch', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });

      expect(handler).toHaveBeenCalledWith(
        expect.objectContaining({
          type: 'user-joined',
          roomId: 'room-1',
          userId: 'u-1',
          e2eeEpoch: 1,
        })
      );
    });

    it('increments epoch on leave for forward secrecy', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', { username: 'bob' });

      await manager.leaveRoom('room-1', 'u-1');

      const room = manager.getRoom('room-1');
      expect(room?.e2eeEpoch).toBe(3); // 1 (join u-1) + 1 (join u-2) + 1 (leave u-1)
    });
  });

  // ── Query methods ───────────────────────────────────────────────────

  describe('query methods', () => {
    it('findRoomBySocketId returns correct roomId and userId', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const found = manager.findRoomBySocketId('sock-1');
      expect(found).toEqual({ roomId: 'room-1', userId: 'u-1' });
    });

    it('findRoomBySocketId returns null for unknown socket', () => {
      expect(manager.findRoomBySocketId('unknown')).toBeNull();
    });

    it('getRoomSocketIds returns all sockets in room', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-2', 'sock-2', { username: 'bob' });

      const ids = manager.getRoomSocketIds('room-1');
      expect(ids).toContain('sock-1');
      expect(ids).toContain('sock-2');
    });

    it('getStats returns correct aggregate counts', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await joinRoomWithSupportedCrypto(manager, 'room-2', 'u-2', 'sock-2', { username: 'bob' });

      const stats = manager.getStats();
      expect(stats.activeRooms).toBe(2);
      expect(stats.totalParticipants).toBe(2);
      expect(stats.totalProducers).toBe(0);
      expect(stats.totalConsumers).toBe(0);
    });

    it('collectMetricsSample counts publishers by source and gathers recv-transport egress (#1553)', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const p = manager.getRoom('room-1')!.participants.get('u-1')!;
      p.producers.set('pc', {
        producer: createMockProducer({ kind: 'video' }) as any,
        source: 'camera',
        kind: 'video',
      });
      p.producers.set('ps', {
        producer: createMockProducer({ kind: 'video' }) as any,
        source: 'screen',
        kind: 'video',
      });
      p.producers.set('pm', {
        producer: createMockProducer({ kind: 'audio' }) as any,
        source: 'mic',
        kind: 'audio',
      });
      p.recvTransports.set(
        't-1',
        createMockTransport({ getStats: vi.fn().mockResolvedValue([{ bytesSent: 5000 }]) }) as any
      );

      const sample = await manager.collectMetricsSample();
      expect(sample.publishers).toEqual({ camera: 1, screen: 1 });
      expect(sample.activeByKind).toEqual({ audio: 1, webcam: 1, screenshare: 1 });
      expect(sample.perRoomVideoPublishers).toEqual([2]);
      expect(sample.egressBytesByTransport.get('t-1')).toBe(5000);
      expect(sample.liveTransportIds.has('t-1')).toBe(true);
    });

    it('collectMetricsSample keeps a getStats-failed transport LIVE but omits its bytes (#1553)', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const p = manager.getRoom('room-1')!.participants.get('u-1')!;
      p.recvTransports.set(
        't-bad',
        createMockTransport({ getStats: vi.fn().mockRejectedValue(new Error('closing')) }) as any
      );

      const sample = await manager.collectMetricsSample();
      // omitted from egress bytes (getStats threw) ...
      expect(sample.egressBytesByTransport.has('t-bad')).toBe(false);
      // ... but still reported LIVE, so the accumulator won't prune + re-count it from zero.
      expect(sample.liveTransportIds.has('t-bad')).toBe(true);
      expect(sample.publishers).toEqual({ camera: 0, screen: 0 });
    });

    it('getActiveRoomIds returns all room IDs', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await joinRoomWithSupportedCrypto(manager, 'room-2', 'u-2', 'sock-2', { username: 'bob' });

      expect(manager.getActiveRoomIds()).toEqual(['room-1', 'room-2']);
    });
  });

  // ── Event system ────────────────────────────────────────────────────

  describe('event system', () => {
    it('calls all registered handlers', async () => {
      const handler1 = vi.fn();
      const handler2 = vi.fn();
      manager.onEvent(handler1);
      manager.onEvent(handler2);

      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });

      expect(handler1).toHaveBeenCalled();
      expect(handler2).toHaveBeenCalled();
    });

    it('catches handler errors without breaking other handlers', async () => {
      const badHandler = vi.fn(() => {
        throw new Error('boom');
      });
      const goodHandler = vi.fn();
      manager.onEvent(badHandler);
      manager.onEvent(goodHandler);

      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });

      expect(badHandler).toHaveBeenCalled();
      expect(goodHandler).toHaveBeenCalled();
    });
  });

  // ── updateRtpCapabilities ───────────────────────────────────────────

  describe('updateRtpCapabilities', () => {
    it('updates participant capabilities', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      const caps = createRtpCapabilities();

      manager.updateRtpCapabilities('room-1', 'u-1', caps as any);

      const participant = manager.getParticipant('room-1', 'u-1');
      expect(participant?.rtpCapabilities).toBe(caps);
    });

    it('is a no-op for non-existent room', () => {
      manager.updateRtpCapabilities('nonexistent', 'u-1', {} as any);
      expect(manager.getParticipant('nonexistent', 'u-1')).toBeUndefined();
    });
  });

  // ── closeAll ────────────────────────────────────────────────────────

  describe('closeAll', () => {
    it('closes all rooms', async () => {
      await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
      await joinRoomWithSupportedCrypto(manager, 'room-2', 'u-2', 'sock-2', { username: 'bob' });

      await manager.closeAll();

      expect(manager.getActiveRoomIds()).toEqual([]);
    });
  });

  // ── Server-enforced mute/deafen ──────────────────────────────────────

  describe('Server-enforced mute/deafen', () => {
    // Shared helpers for producing and consuming audio

    async function setupProducer(
      mgr: RoomManager,
      router: ReturnType<typeof createMockRouter>,
      roomId: string,
      userId: string,
      kind: 'audio' | 'video' = 'audio',
      source: 'mic' | 'camera' | 'screen' | 'screen-audio' = 'mic'
    ) {
      const transport = createMockTransport();
      router.createWebRtcTransport.mockResolvedValueOnce(transport);
      await mgr.createTransport(roomId, userId, 'send');

      const producer = createMockProducer({ kind });
      transport.produce.mockResolvedValueOnce(producer);
      const info = await mgr.produce(
        roomId,
        userId,
        transport.id,
        kind,
        createRtpParameters() as any,
        source
      );
      return { transport, producer, info };
    }

    async function setupConsumer(
      mgr: RoomManager,
      router: ReturnType<typeof createMockRouter>,
      roomId: string,
      consumerUserId: string,
      producerId: string,
      kind: 'audio' | 'video' = 'audio'
    ) {
      const recvTransport = createMockTransport();
      const consumer = createMockConsumer({ producerId, kind });
      recvTransport.consume.mockResolvedValue(consumer);
      router.createWebRtcTransport.mockResolvedValueOnce(recvTransport);
      await mgr.createTransport(roomId, consumerUserId, 'recv');

      const result = await mgr.consume(roomId, consumerUserId, producerId);
      return { recvTransport, consumer, result };
    }

    describe('serverMuteUser', () => {
      it('should set serverMuted flag and pause audio producers', async () => {
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });
        const { producer } = await setupProducer(manager, mockRouter, 'room-1', 'u-1');

        await manager.serverMuteUser('room-1', 'u-1');

        const participant = manager.getParticipant('room-1', 'u-1');
        expect(participant?.serverMuted).toBe(true);
        expect(producer.pause).toHaveBeenCalled();
      });

      it('should not pause video producers', async () => {
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });
        const { producer } = await setupProducer(
          manager,
          mockRouter,
          'room-1',
          'u-1',
          'video',
          'camera'
        );

        await manager.serverMuteUser('room-1', 'u-1');

        const participant = manager.getParticipant('room-1', 'u-1');
        expect(participant?.serverMuted).toBe(true);
        expect(producer.pause).not.toHaveBeenCalled();
      });

      it('should be a no-op for non-existent participant', async () => {
        // Should not throw and participant should remain undefined
        await manager.serverMuteUser('room-1', 'unknown');
        expect(manager.getParticipant('room-1', 'unknown')).toBeUndefined();
      });
    });

    describe('serverUnmuteUser', () => {
      it('should clear serverMuted flag without resuming producers', async () => {
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });
        const { producer } = await setupProducer(manager, mockRouter, 'room-1', 'u-1');

        await manager.serverMuteUser('room-1', 'u-1');
        await manager.serverUnmuteUser('room-1', 'u-1');

        const participant = manager.getParticipant('room-1', 'u-1');
        expect(participant?.serverMuted).toBe(false);
        expect(producer.resume).not.toHaveBeenCalled();
      });
    });

    describe('serverDeafenUser', () => {
      it('should set both flags and pause audio producers and consumers', async () => {
        // u-1 produces audio
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });
        const { producer, info: producerInfo } = await setupProducer(
          manager,
          mockRouter,
          'room-1',
          'u-1'
        );

        // u-2 joins, produces audio, and consumes u-1's audio
        await joinRoomWithSupportedCrypto(
          manager,
          'room-1',
          'u-2',
          'sock-2',
          { username: 'bob' },
          createRtpCapabilities() as any
        );
        const { producer: u2Producer } = await setupProducer(manager, mockRouter, 'room-1', 'u-2');
        const { consumer } = await setupConsumer(
          manager,
          mockRouter,
          'room-1',
          'u-2',
          producerInfo.producerId
        );

        await manager.serverDeafenUser('room-1', 'u-2');

        const participant = manager.getParticipant('room-1', 'u-2');
        expect(participant?.serverMuted).toBe(true);
        expect(participant?.serverDeafened).toBe(true);
        expect(u2Producer.pause).toHaveBeenCalled();
        expect(consumer.pause).toHaveBeenCalled();
      });

      it('should not pause video consumers', async () => {
        // u-1 produces video
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });
        const { info: producerInfo } = await setupProducer(
          manager,
          mockRouter,
          'room-1',
          'u-1',
          'video',
          'camera'
        );

        // u-2 joins and consumes u-1's video
        await joinRoomWithSupportedCrypto(
          manager,
          'room-1',
          'u-2',
          'sock-2',
          { username: 'bob' },
          createRtpCapabilities() as any
        );
        const { consumer: videoConsumer } = await setupConsumer(
          manager,
          mockRouter,
          'room-1',
          'u-2',
          producerInfo.producerId,
          'video'
        );

        await manager.serverDeafenUser('room-1', 'u-2');

        // Video consumer should NOT be paused (deafen only affects audio)
        expect(videoConsumer.pause).not.toHaveBeenCalled();
      });
    });

    describe('serverUndeafenUser', () => {
      it('should clear both flags without resuming producers or consumers', async () => {
        // u-1 produces audio
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });
        const { info: producerInfo } = await setupProducer(manager, mockRouter, 'room-1', 'u-1');

        // u-2 joins, produces audio, and consumes u-1's audio
        await joinRoomWithSupportedCrypto(
          manager,
          'room-1',
          'u-2',
          'sock-2',
          { username: 'bob' },
          createRtpCapabilities() as any
        );
        const { producer: u2Producer } = await setupProducer(manager, mockRouter, 'room-1', 'u-2');
        const { consumer } = await setupConsumer(
          manager,
          mockRouter,
          'room-1',
          'u-2',
          producerInfo.producerId
        );

        await manager.serverDeafenUser('room-1', 'u-2');
        // Reset mock call counts to check that undeafen does NOT call resume
        u2Producer.resume.mockClear();
        consumer.resume.mockClear();

        await manager.serverUndeafenUser('room-1', 'u-2');

        const participant = manager.getParticipant('room-1', 'u-2');
        expect(participant?.serverMuted).toBe(false);
        expect(participant?.serverDeafened).toBe(false);
        expect(u2Producer.resume).not.toHaveBeenCalled();
        expect(consumer.resume).not.toHaveBeenCalled();
      });
    });

    describe('userMuteParticipant', () => {
      it('should pause audio producers without setting server flags', async () => {
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });
        const { producer } = await setupProducer(manager, mockRouter, 'room-1', 'u-1');

        await manager.userMuteParticipant('room-1', 'u-1');

        const participant = manager.getParticipant('room-1', 'u-1');
        expect(participant?.serverMuted).toBe(false);
        expect(producer.pause).toHaveBeenCalled();
      });

      it('should not pause video producers', async () => {
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });
        const { producer } = await setupProducer(
          manager,
          mockRouter,
          'room-1',
          'u-1',
          'video',
          'camera'
        );

        await manager.userMuteParticipant('room-1', 'u-1');

        expect(producer.pause).not.toHaveBeenCalled();
      });
    });

    describe('userDeafenParticipant', () => {
      it('should pause audio producers and consumers without setting server flags', async () => {
        // u-1 produces audio
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });
        const { info: producerInfo } = await setupProducer(manager, mockRouter, 'room-1', 'u-1');

        // u-2 joins, produces audio, and consumes u-1's audio
        await joinRoomWithSupportedCrypto(
          manager,
          'room-1',
          'u-2',
          'sock-2',
          { username: 'bob' },
          createRtpCapabilities() as any
        );
        const { producer: u2Producer } = await setupProducer(manager, mockRouter, 'room-1', 'u-2');
        const { consumer } = await setupConsumer(
          manager,
          mockRouter,
          'room-1',
          'u-2',
          producerInfo.producerId
        );

        await manager.userDeafenParticipant('room-1', 'u-2');

        const participant = manager.getParticipant('room-1', 'u-2');
        expect(participant?.serverMuted).toBe(false);
        expect(participant?.serverDeafened).toBe(false);
        expect(u2Producer.pause).toHaveBeenCalled();
        expect(consumer.pause).toHaveBeenCalled();
      });
    });

    describe('enforcement on join', () => {
      it('should initialize serverMuted and serverDeafened as false', async () => {
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });

        const participant = manager.getParticipant('room-1', 'u-1');
        expect(participant?.serverMuted).toBe(false);
        expect(participant?.serverDeafened).toBe(false);
      });

      it('should persist flags after being set', async () => {
        await joinRoomWithSupportedCrypto(manager, 'room-1', 'u-1', 'sock-1', {
          username: 'alice',
        });

        await manager.serverMuteUser('room-1', 'u-1');
        await manager.serverDeafenUser('room-1', 'u-1');

        const participant = manager.getParticipant('room-1', 'u-1');
        expect(participant?.serverMuted).toBe(true);
        expect(participant?.serverDeafened).toBe(true);
      });
    });
  });
});

// ---------------------------------------------------------------------------
// Audio last-N security guard (#1544)
// ---------------------------------------------------------------------------

describe('resumeConsumer last-N guard (#1544)', () => {
  let rm: RoomManager;
  let router: ReturnType<typeof createMockRouter>;

  /**
   * Build a room with a producer (u-1) + a subscriber (u-2) who has one audio
   * consumer with a deterministic id 'c-audio'. Reuses the mediasoup mock
   * factories — same scaffolding the mute/deafen + cap tests use.
   */
  async function setupRoomWithAudioConsumer(): Promise<{
    rm: RoomManager;
    room: import('../src/lib/roomManager.js').Room;
    userId: string;
    consumer: ReturnType<typeof createMockConsumer>;
  }> {
    router = createMockRouter();
    rm = new RoomManager(createMockMediasoupService(router) as any);

    // u-1 produces mic audio.
    await joinRoomWithSupportedCrypto(rm, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
    const sendTransport = createMockTransport();
    router.createWebRtcTransport.mockResolvedValueOnce(sendTransport);
    await rm.createTransport('room-1', 'u-1', 'send');
    const producer = createMockProducer({ kind: 'audio', id: 'p-mic' });
    sendTransport.produce.mockResolvedValueOnce(producer);
    const producerInfo = await rm.produce(
      'room-1',
      'u-1',
      sendTransport.id,
      'audio',
      createRtpParameters() as any,
      'mic'
    );

    // u-2 subscribes to u-1's audio with a deterministic consumer id.
    await joinRoomWithSupportedCrypto(
      rm,
      'room-1',
      'u-2',
      'sock-2',
      { username: 'bob' },
      createRtpCapabilities() as any
    );
    const recvTransport = createMockTransport();
    const consumer = createMockConsumer({
      id: 'c-audio',
      producerId: producerInfo.producerId,
      kind: 'audio',
    });
    recvTransport.consume.mockResolvedValue(consumer);
    router.createWebRtcTransport.mockResolvedValueOnce(recvTransport);
    await rm.createTransport('room-1', 'u-2', 'recv');
    await rm.consume('room-1', 'u-2', producerInfo.producerId);

    const room = rm.getRoom('room-1')!;
    return { rm, room, userId: 'u-2', consumer };
  }

  function getConsumer(
    room: import('../src/lib/roomManager.js').Room,
    userId: string,
    consumerId: string
  ) {
    return room.participants.get(userId)!.consumers.get(consumerId) as any;
  }

  /**
   * #1742: last-N only ENGAGES when the room exceeds the cap. Inflate the
   * server-authoritative mic-publisher count past N (with filler ids, so we
   * don't stand up N real producers) so the evict/admit path genuinely
   * pauses/resumes consumers. In a room at/under N last-N is a no-op — that path
   * has its own dedicated suite (`audio last-N small-room no-op (#1742)`).
   */
  function makeRoomOverCap(room: import('../src/lib/roomManager.js').Room): void {
    while (room.micProducerIds.size <= resolveAudioLastN(room)) {
      room.micProducerIds.add(`filler-${room.micProducerIds.size}`);
    }
  }

  it('refuses to resume a consumer in lastNPausedConsumers (client cannot bypass the cap)', async () => {
    const { rm, room, userId } = await setupRoomWithAudioConsumer();
    room.lastNPausedConsumers.add('c-audio');
    const consumer = getConsumer(room, userId, 'c-audio');
    await rm.resumeConsumer(room.id, userId, 'c-audio');
    expect(consumer.resume).not.toHaveBeenCalled(); // guard held
  });

  it('honors resume for a consumer whose speaker IS in the top-N (not in the last-N set)', async () => {
    const { rm, room, userId, consumer } = await setupRoomWithAudioConsumer();
    // Consume-time init starts a non-top-N mic consumer last-N-paused. Admit the
    // speaker into the top-N via a 'volumes' tick — applyLastNDelta clears its
    // consumer from lastNPausedConsumers (and resumes it). The subsequent client
    // resumeConsumer is now honored (top-N audio is resumable).
    router._audioLevelObserver._emit('volumes', [{ producer: { id: 'p-mic' }, volume: -20 }]);
    await Promise.resolve();
    await Promise.resolve();
    expect(room.lastNPausedConsumers.has('c-audio')).toBe(false); // cleared by admit
    consumer.resume.mockClear(); // ignore the last-N admit resume; test the client path

    await rm.resumeConsumer(room.id, userId, 'c-audio');
    expect(consumer.resume).toHaveBeenCalledOnce();
  });

  it('never pauses the PRODUCER across an admit -> evict cycle, only consumers (over-cap room)', async () => {
    vi.useFakeTimers();
    try {
      vi.setSystemTime(0);
      const { room, consumer } = await setupRoomWithAudioConsumer();
      // #1742: last-N only engages over the cap; push the room over N so the
      // evict path genuinely pauses a consumer (the invariant under test). In a
      // room at/under N this whole cycle is a no-op — see the #1742 suite.
      makeRoomOverCap(room);
      const observer = router._audioLevelObserver;

      // The mic producer's own mock — assert last-N never touches it.
      const producerEntry = room.participants.get('u-1')!.producers.get('p-mic')!;
      const producerMock = producerEntry.producer as any;

      // Admit p-mic into the top-N at t=0 (delta.added → resume its consumer,
      // clear it from the paused set).
      observer._emit('volumes', [{ producer: { id: 'p-mic' }, volume: -20 }]);
      await Promise.resolve();
      await Promise.resolve();
      expect(room.activeSpeakers.current().has('p-mic')).toBe(true);

      // Advance past the hysteresis hold (2500ms) and go silent → evict p-mic
      // (delta.removed → PAUSE its consumer, re-add to the paused set).
      vi.setSystemTime(2500 + 1);
      observer._emit('silence');
      await Promise.resolve();
      await Promise.resolve();

      // The cap acts on the CONSUMER only; the PRODUCER is never paused by last-N.
      expect(producerMock.pause).not.toHaveBeenCalled();
      expect(consumer.pause).toHaveBeenCalled();
      expect(room.lastNPausedConsumers.has('c-audio')).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('does NOT resume a SERVER-DEAFENED subscriber when its speaker enters top-N (no moderation bypass, over-cap room)', async () => {
    const { rm, room, userId, consumer } = await setupRoomWithAudioConsumer();
    // #1742: only meaningful over the cap, where last-N's admit path runs.
    makeRoomOverCap(room);
    // Simulate c-audio already last-N-paused (its speaker was out of the top-N),
    // so the admit below genuinely reaches the resume branch the deafen blocks.
    room.lastNPausedConsumers.add('c-audio');
    // Server-deafen u-2 (a moderation control) → all their audio consumers paused.
    await rm.serverDeafenUser(room.id, userId);
    consumer.resume.mockClear();
    // p-mic enters the top-N — last-N's admit would normally resume u-2's consumer.
    router._audioLevelObserver._emit('volumes', [{ producer: { id: 'p-mic' }, volume: -20 }]);
    await Promise.resolve();
    await Promise.resolve();
    // Server-deafen is server-enforced: last-N must NOT resume it (else the
    // deafened user receives audio — a moderation bypass).
    expect(consumer.resume).not.toHaveBeenCalled();
    // The consumer is still cleared from the last-N set (its producer is top-N);
    // the deafen pause holds, and server-undeafen + client resume restores it.
    expect(room.lastNPausedConsumers.has('c-audio')).toBe(false);
  });

  it('closeConsumer removes the consumer from lastNPausedConsumers (no leak)', async () => {
    const { rm, room, userId } = await setupRoomWithAudioConsumer();
    room.lastNPausedConsumers.add('c-audio');
    rm.closeConsumer(room.id, userId, 'c-audio');
    expect(room.lastNPausedConsumers.has('c-audio')).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Audio last-N small-room no-op regression (#1742)
//
// Regression for the voice-crackle report: in a room whose mic-publisher count
// is at or under the forwarded-speaker cap (N), last-N MUST be a structural
// no-op — no mic consumer is ever paused, and a fresh consumer is never seeded
// last-N-paused. The cap is a DoS guardrail for rooms LARGER than N; applying it
// to a room that already fits under N is pure regression: the evict-on-silence
// pause/resume churn loses the leading frames of each new utterance during the
// resume gap → Opus PLC crackle (the user-reported symptom).
// ---------------------------------------------------------------------------

describe('audio last-N small-room no-op (#1742)', () => {
  /**
   * Build a small room: u-1 publishes mic audio 'p-mic'; u-2 subscribes with a
   * deterministic consumer id 'c-audio'. micProducerIds.size === 1, well under
   * the free cap N=8 — last-N should never engage. Mirrors the #1544 guard
   * suite's scaffolding.
   */
  async function setupSmallRoom(opts: { overCap?: boolean } = {}): Promise<{
    rm: RoomManager;
    room: import('../src/lib/roomManager.js').Room;
    router: ReturnType<typeof createMockRouter>;
    consumer: ReturnType<typeof createMockConsumer>;
  }> {
    const router = createMockRouter();
    const rm = new RoomManager(createMockMediasoupService(router) as any);

    // u-1 publishes mic audio.
    await joinRoomWithSupportedCrypto(rm, 'room-1', 'u-1', 'sock-1', { username: 'alice' });
    const sendTransport = createMockTransport();
    router.createWebRtcTransport.mockResolvedValueOnce(sendTransport);
    await rm.createTransport('room-1', 'u-1', 'send');
    const producer = createMockProducer({ kind: 'audio', id: 'p-mic' });
    sendTransport.produce.mockResolvedValueOnce(producer);
    const producerInfo = await rm.produce(
      'room-1',
      'u-1',
      sendTransport.id,
      'audio',
      createRtpParameters() as any,
      'mic'
    );

    const room = rm.getRoom('room-1')!;
    // #1742: optionally simulate an over-cap room BEFORE the consume so the
    // consume-time last-N seed exercises its over-cap branch.
    if (opts.overCap) {
      while (room.micProducerIds.size <= resolveAudioLastN(room)) {
        room.micProducerIds.add(`filler-${room.micProducerIds.size}`);
      }
    }

    // u-2 subscribes to u-1's mic with a deterministic consumer id.
    await joinRoomWithSupportedCrypto(
      rm,
      'room-1',
      'u-2',
      'sock-2',
      { username: 'bob' },
      createRtpCapabilities() as any
    );
    const recvTransport = createMockTransport();
    const consumer = createMockConsumer({
      id: 'c-audio',
      producerId: producerInfo.producerId,
      kind: 'audio',
    });
    recvTransport.consume.mockResolvedValue(consumer);
    router.createWebRtcTransport.mockResolvedValueOnce(recvTransport);
    await rm.createTransport('room-1', 'u-2', 'recv');
    await rm.consume('room-1', 'u-2', producerInfo.producerId);

    return { rm, room, router, consumer };
  }

  it('does not seed a fresh mic consumer as last-N-paused when the room is at/under the cap', async () => {
    const { room } = await setupSmallRoom();
    // micProducerIds (1) <= resolveAudioLastN (8): everyone fits under the cap,
    // so a fresh joiner's consumer must NOT start last-N-paused (it must hear
    // from the first frame). Pre-fix the consume-time seed pauses it because the
    // producer is not yet in the (empty) top-N.
    expect(room.micProducerIds.size).toBeLessThanOrEqual(resolveAudioLastN(room));
    expect(room.lastNPausedConsumers.has('c-audio')).toBe(false);
  });

  it('never pauses a mic consumer across a speak/silence cycle when the room is at/under the cap', async () => {
    vi.useFakeTimers();
    try {
      vi.setSystemTime(0);
      const { room, router, consumer } = await setupSmallRoom();
      const observer = router._audioLevelObserver;

      // Two full speak → (past-hold) silence → speak cycles, mirroring real
      // back-and-forth conversation in a 2-person call. Each silence advances
      // past the 2500ms hysteresis hold so ActiveSpeakerSet evicts the speaker.
      for (let cycle = 0; cycle < 2; cycle++) {
        observer._emit('volumes', [{ producer: { id: 'p-mic' }, volume: -20 }]);
        await Promise.resolve();
        await Promise.resolve();

        vi.setSystemTime(2500 * (cycle + 1) + (cycle + 1));
        observer._emit('silence');
        await Promise.resolve();
        await Promise.resolve();
      }

      // A room that already fits under N must never pause a consumer. Pre-fix the
      // evict-on-silence path pauses c-audio on every conversational silence
      // boundary → the reported per-utterance crackle.
      expect(consumer.pause).not.toHaveBeenCalled();
      expect(room.lastNPausedConsumers.has('c-audio')).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it('resumes a last-N-paused consumer once the room shrinks back to/under the cap (no stuck-silent participant)', async () => {
    const { room, router, consumer } = await setupSmallRoom();
    const observer = router._audioLevelObserver;

    // Simulate the consumer having been last-N-paused during a prior over-cap
    // window: push the count over N and mark/pause the consumer as the over-cap
    // path would have.
    while (room.micProducerIds.size <= resolveAudioLastN(room)) {
      room.micProducerIds.add(`filler-${room.micProducerIds.size}`);
    }
    room.lastNPausedConsumers.add('c-audio');
    await consumer.pause();
    consumer.resume.mockClear();

    // A second consumer that is NOT last-N-paused (e.g. an already-active mic or
    // a screen-audio consumer): the drain must leave it untouched — it only
    // resumes consumers it itself paused for last-N.
    const otherConsumer = createMockConsumer({
      id: 'c-other',
      producerId: 'p-other',
      kind: 'audio',
    });
    room.participants.get('u-2')!.consumers.set('c-other', otherConsumer as any);

    // The room shrinks back to <= N (publishers left).
    room.micProducerIds.clear();
    room.micProducerIds.add('p-mic');
    expect(room.micProducerIds.size).toBeLessThanOrEqual(resolveAudioLastN(room));

    // Observer-tick drain path (backstop): an AudioLevelObserver tick drives
    // applyLastNDelta → the small-room no-op branch, which drains the paused set
    // and resumes the consumer. (The producer-close path — removeMicProducer —
    // is the primary, event-driven trigger, covered by its own test below.)
    observer._emit('silence');
    await Promise.resolve();
    await Promise.resolve();

    expect(room.lastNPausedConsumers.has('c-audio')).toBe(false);
    expect(consumer.resume).toHaveBeenCalled();
    // The non-paused consumer is not touched by the drain.
    expect(otherConsumer.resume).not.toHaveBeenCalled();
  });

  it('still seeds a fresh mic consumer last-N-paused when the room is over the cap (#1544/#1632 enforcement preserved)', async () => {
    const { room } = await setupSmallRoom({ overCap: true });
    expect(room.micProducerIds.size).toBeGreaterThan(resolveAudioLastN(room));
    // Over the cap + the producer not yet in the (empty) top-N → the consumer is
    // seeded last-N-paused so the client's unconditional resume is refused until
    // the speaker enters the top-N (the egress cap still holds for large rooms).
    expect(room.lastNPausedConsumers.has('c-audio')).toBe(true);
  });

  it('drains on shrink but does NOT resume a SERVER-DEAFENED subscriber (moderation not bypassed)', async () => {
    const { rm, room, router, consumer } = await setupSmallRoom();
    const observer = router._audioLevelObserver;

    // Over-cap window: the consumer is last-N-paused.
    while (room.micProducerIds.size <= resolveAudioLastN(room)) {
      room.micProducerIds.add(`filler-${room.micProducerIds.size}`);
    }
    room.lastNPausedConsumers.add('c-audio');
    // Moderation: server-deafen the subscriber.
    await rm.serverDeafenUser(room.id, 'u-2');
    consumer.resume.mockClear();

    // Room shrinks back to <= N → the drain path runs on the next tick.
    room.micProducerIds.clear();
    room.micProducerIds.add('p-mic');
    observer._emit('silence');
    await Promise.resolve();
    await Promise.resolve();

    // The consumer is cleared from the last-N set (last-N no longer owns it)…
    expect(room.lastNPausedConsumers.has('c-audio')).toBe(false);
    // …but it is NOT resumed — the deafen pause must hold (moderation control).
    expect(consumer.resume).not.toHaveBeenCalled();
  });

  it('resumes a last-N-paused consumer when a publisher leaves and the room drops to <= N (event-driven, no observer tick)', async () => {
    const { rm, room, router, consumer } = await setupSmallRoom();

    // Add a second real publisher u-3 (p-extra) so closing it shrinks the room
    // without tearing down c-audio (which subscribes to u-1's p-mic).
    await joinRoomWithSupportedCrypto(rm, 'room-1', 'u-3', 'sock-3', { username: 'carol' });
    const sendTransport3 = createMockTransport();
    router.createWebRtcTransport.mockResolvedValueOnce(sendTransport3);
    await rm.createTransport('room-1', 'u-3', 'send');
    const producer3 = createMockProducer({ kind: 'audio', id: 'p-extra' });
    sendTransport3.produce.mockResolvedValueOnce(producer3);
    await rm.produce(
      'room-1',
      'u-3',
      sendTransport3.id,
      'audio',
      createRtpParameters() as any,
      'mic'
    );

    // Push the room over the cap and mark/pause c-audio as the over-cap path would.
    while (room.micProducerIds.size <= resolveAudioLastN(room)) {
      room.micProducerIds.add(`filler-${room.micProducerIds.size}`);
    }
    room.lastNPausedConsumers.add('c-audio');
    await consumer.pause();
    consumer.resume.mockClear();

    // A DIFFERENT publisher leaves → closeProducer routes through removeMicProducer,
    // which drops the room to <= N. The drain must fire from THIS leave event with
    // NO AudioLevelObserver tick (the #1742 event-driven shrink-recovery path).
    await rm.closeProducer('room-1', 'u-3', 'p-extra');

    expect(room.micProducerIds.size).toBeLessThanOrEqual(resolveAudioLastN(room));
    expect(room.lastNPausedConsumers.has('c-audio')).toBe(false);
    expect(consumer.resume).toHaveBeenCalled();
  });
});

describe('resolveVideoPublisherCap', () => {
  const room = {} as any; // room is unused until the #1294 tier-seam lands

  it('returns the free cap when below the absolute ceiling', () => {
    expect(resolveVideoPublisherCap(room, 8)).toBe(8);
    expect(resolveVideoPublisherCap(room, 3)).toBe(3);
  });

  it('clamps to the absolute ceiling (25)', () => {
    expect(resolveVideoPublisherCap(room, 100)).toBe(ABSOLUTE_VIDEO_PUBLISHER_CEILING);
    expect(ABSOLUTE_VIDEO_PUBLISHER_CEILING).toBe(25);
  });

  it('defaults the free cap from config (8) when omitted', () => {
    expect(resolveVideoPublisherCap(room)).toBe(8);
  });

  it('clamps a zero/negative free cap up to 1 (never disables the cap — #1294 seam guard)', () => {
    // A future #1294 tier value bypasses config.parsePositiveIntEnv, so the
    // resolver must defend its own lower bound: Math.min(0, 25) = 0 would make
    // `count >= 0` always true and reject every camera produce (a DoS).
    expect(resolveVideoPublisherCap(room, 0)).toBe(1);
    expect(resolveVideoPublisherCap(room, -5)).toBe(1);
  });
});

describe('resolveAudioLastN (#1544)', () => {
  it('returns the free default (8) for Beta', () => {
    expect(resolveAudioLastN(undefined, 8)).toBe(8);
  });
  it('clamps above the absolute ceiling', () => {
    expect(resolveAudioLastN(undefined, 999)).toBe(ABSOLUTE_AUDIO_LAST_N_CEILING);
  });
  it('floors at 1 (a 0/negative tier value cannot disable the cap)', () => {
    expect(resolveAudioLastN(undefined, 0)).toBe(1);
    expect(resolveAudioLastN(undefined, -5)).toBe(1);
  });
});
