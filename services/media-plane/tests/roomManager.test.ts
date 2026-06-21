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
} from '../src/lib/roomManager.js';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function createMockMediasoupService(router = createMockRouter()) {
  return {
    getOrCreateRouter: vi.fn().mockResolvedValue(router),
    removeRouter: vi.fn(),
  };
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
      const result = await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

      expect(result.rtpCapabilities).toBeDefined();
      expect(result.existingProducers).toEqual([]);
      expect(result.participants).toHaveLength(1);
      expect(result.participants[0].userId).toBe('u-1');
    });

    it('reuses existing room on second join', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const result = await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');

      expect(mockMediasoup.getOrCreateRouter).toHaveBeenCalledTimes(1);
      expect(result.participants).toHaveLength(2);
    });

    it('discards stale room when router is closed', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      mockRouter.closed = true;

      // Second join should detect closed router and recreate
      const newRouter = createMockRouter();
      mockMediasoup.getOrCreateRouter.mockResolvedValue(newRouter);

      const result = await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');
      expect(result.rtpCapabilities).toBe(newRouter.rtpCapabilities);
    });

    it('returns existing producers from other participants', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

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
      const result = await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');
      expect(result.existingProducers).toHaveLength(1);
      expect(result.existingProducers[0].userId).toBe('u-1');
    });
  });

  describe('Self-deafen (#685)', () => {
    it('setParticipantDeafen sets the isDeafened flag', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      manager.setParticipantDeafen('room-1', 'u-1', true);
      expect(manager.getParticipant('room-1', 'u-1')?.isDeafened).toBe(true);
    });

    it('setParticipantDeafen clears the isDeafened flag', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      manager.setParticipantDeafen('room-1', 'u-1', true);
      manager.setParticipantDeafen('room-1', 'u-1', false);
      expect(manager.getParticipant('room-1', 'u-1')?.isDeafened).toBe(false);
    });

    it('setParticipantDeafen is a no-op for an unknown participant', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      expect(() => manager.setParticipantDeafen('room-1', 'ghost', true)).not.toThrow();
      expect(manager.getParticipant('room-1', 'ghost')).toBeUndefined();
    });

    it('joinRoom snapshot defaults isDeafened to false', async () => {
      const result = await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      expect(result.participants[0].isDeafened).toBe(false);
    });

    it('joinRoom snapshot carries an existing self-deafen (late joiner sees it)', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      manager.setParticipantDeafen('room-1', 'u-1', true);

      const result = await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');
      const alice = result.participants.find((p) => p.userId === 'u-1');
      expect(alice?.isDeafened).toBe(true);
    });
  });

  describe('joinRoom — reconnection', () => {
    it('cleans up old session when same userId rejoins', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-old', 'alice');
      const result = await manager.joinRoom('room-1', 'u-1', 'sock-new', 'alice');

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

      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');
      await manager.leaveRoom('room-1', 'u-1');

      const room = manager.getRoom('room-1');
      expect(room?.participants.size).toBe(1);

      const leftEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'user-left');
      expect(leftEvent).toBeDefined();
      expect(leftEvent![0].userId).toBe('u-1');
    });

    it('closes room and emits room-empty when last participant leaves', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.leaveRoom('room-1', 'u-1');

      expect(manager.getRoom('room-1')).toBeUndefined();
      const emptyEvent = handler.mock.calls.find((c: any[]) => c[0].type === 'room-empty');
      expect(emptyEvent).toBeDefined();
    });

    it('cleans up transports, producers, and consumers', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

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

    it('is a no-op for non-existent room or participant', async () => {
      await manager.leaveRoom('nonexistent', 'u-1');
      expect(manager.getRoom('nonexistent')).toBeUndefined();
    });
  });

  // ── Transport management ────────────────────────────────────────────

  describe('createTransport', () => {
    beforeEach(async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
  });

  describe('connectTransport', () => {
    it('calls transport.connect with dtlsParameters', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const transport = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(transport);
      await manager.createTransport('room-1', 'u-1', 'send');

      const dtls = { fingerprints: [], role: 'auto' as const };
      await manager.connectTransport('room-1', 'u-1', transport.id, dtls as any);

      expect(transport.connect).toHaveBeenCalledWith({ dtlsParameters: dtls });
    });

    it('throws if transport not found', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await expect(manager.connectTransport('room-1', 'u-1', 'bad-id', {} as any)).rejects.toThrow(
        'Transport not found'
      );
    });
  });

  // ── Producer management ─────────────────────────────────────────────

  describe('produce', () => {
    let transport: ReturnType<typeof createMockTransport>;

    beforeEach(async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
        await manager.joinRoom('room-1', uid, `sock-${i + 100}`, `user${i}`);
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
        await manager.joinRoom('room-1', uid, `sock-${i + 200}`, `user${i}`);
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

  describe('pauseProducer / resumeProducer', () => {
    it('pauses and resumes a producer', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await expect(manager.pauseProducer('room-1', 'u-1', 'bad-id')).rejects.toThrow(
        'Producer not found'
      );
    });
  });

  describe('closeProducer', () => {
    it('closes producer and emits producer-removed event', async () => {
      const handler = vi.fn();
      manager.onEvent(handler);

      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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

    beforeEach(async () => {
      // u-1 joins and produces
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
      await manager.joinRoom(
        'room-1',
        'u-2',
        'sock-2',
        'bob',
        undefined,
        undefined,
        createRtpCapabilities() as any
      );
      const recvTransport = createMockTransport();
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
      await manager.joinRoom(
        'room-1',
        'u-3',
        'sock-3',
        'charlie',
        undefined,
        undefined,
        createRtpCapabilities() as any
      );

      await expect(manager.consume('room-1', 'u-3', producerInfo.producerId)).rejects.toThrow(
        'No receive transport'
      );
    });

    it('throws when rtpCapabilities not set', async () => {
      // u-4 joins WITHOUT rtpCapabilities
      await manager.joinRoom('room-1', 'u-4', 'sock-4', 'dave');
      const recvT = createMockTransport();
      mockRouter.createWebRtcTransport.mockResolvedValueOnce(recvT);
      await manager.createTransport('room-1', 'u-4', 'recv');

      await expect(manager.consume('room-1', 'u-4', producerInfo.producerId)).rejects.toThrow(
        'RTP capabilities not set'
      );
    });
  });

  describe('resumeConsumer / pauseConsumer / closeConsumer', () => {
    it('resumes a consumer', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const participant = manager.getParticipant('room-1', 'u-1')!;
      const consumer = createMockConsumer();
      participant.consumers.set(consumer.id, consumer as any);

      await manager.pauseConsumer('room-1', 'u-1', consumer.id);
      expect(consumer.pause).toHaveBeenCalled();
    });

    it('closes a consumer and removes from map', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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

  // ── Codec floor ─────────────────────────────────────────────────────

  describe('computeCodecFloor', () => {
    it('returns null for non-existent room', () => {
      expect(manager.computeCodecFloor('nonexistent')).toBeNull();
    });

    it('returns null when fewer than 2 participants have capabilities', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      expect(manager.computeCodecFloor('room-1')).toBeNull();
    });

    it('returns intersection of video codecs', async () => {
      await manager.joinRoom(
        'room-1',
        'u-1',
        'sock-1',
        'alice',
        undefined,
        undefined,
        createRtpCapabilities(['video/VP8', 'video/VP9', 'video/H264']) as any
      );
      await manager.joinRoom(
        'room-1',
        'u-2',
        'sock-2',
        'bob',
        undefined,
        undefined,
        createRtpCapabilities(['video/VP8', 'video/H264']) as any
      );

      const floor = manager.computeCodecFloor('room-1');

      expect(floor).toContain('video/vp8');
      expect(floor).toContain('video/h264');
      expect(floor).not.toContain('video/vp9');
    });

    it('normalizes mimeType to lowercase', async () => {
      await manager.joinRoom(
        'room-1',
        'u-1',
        'sock-1',
        'alice',
        undefined,
        undefined,
        createRtpCapabilities(['video/VP8']) as any
      );
      await manager.joinRoom(
        'room-1',
        'u-2',
        'sock-2',
        'bob',
        undefined,
        undefined,
        createRtpCapabilities(['video/VP8']) as any
      );

      const floor = manager.computeCodecFloor('room-1');
      expect(floor).toEqual(['video/vp8']);
    });

    it('skips participants with null rtpCapabilities', async () => {
      await manager.joinRoom(
        'room-1',
        'u-1',
        'sock-1',
        'alice',
        undefined,
        undefined,
        createRtpCapabilities(['video/VP8']) as any
      );
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob'); // no capabilities
      await manager.joinRoom(
        'room-1',
        'u-3',
        'sock-3',
        'charlie',
        undefined,
        undefined,
        createRtpCapabilities(['video/VP8', 'video/VP9']) as any
      );

      const floor = manager.computeCodecFloor('room-1');
      expect(floor).toEqual(['video/vp8']);
    });
  });

  // ── E2EE epoch ──────────────────────────────────────────────────────

  describe('E2EE epoch', () => {
    it('increments epoch on join', async () => {
      const result1 = await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      expect(result1.e2eeEpoch).toBe(1);

      const result2 = await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');
      expect(result2.e2eeEpoch).toBe(2);
    });

    it('increments epoch on leave for forward secrecy', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');

      await manager.leaveRoom('room-1', 'u-1');

      const room = manager.getRoom('room-1');
      expect(room?.e2eeEpoch).toBe(3); // 1 (join u-1) + 1 (join u-2) + 1 (leave u-1)
    });
  });

  // ── Query methods ───────────────────────────────────────────────────

  describe('query methods', () => {
    it('findRoomBySocketId returns correct roomId and userId', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      const found = manager.findRoomBySocketId('sock-1');
      expect(found).toEqual({ roomId: 'room-1', userId: 'u-1' });
    });

    it('findRoomBySocketId returns null for unknown socket', () => {
      expect(manager.findRoomBySocketId('unknown')).toBeNull();
    });

    it('getRoomSocketIds returns all sockets in room', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-1', 'u-2', 'sock-2', 'bob');

      const ids = manager.getRoomSocketIds('room-1');
      expect(ids).toContain('sock-1');
      expect(ids).toContain('sock-2');
    });

    it('getStats returns correct aggregate counts', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-2', 'u-2', 'sock-2', 'bob');

      const stats = manager.getStats();
      expect(stats.activeRooms).toBe(2);
      expect(stats.totalParticipants).toBe(2);
      expect(stats.totalProducers).toBe(0);
      expect(stats.totalConsumers).toBe(0);
    });

    it('collectMetricsSample counts publishers by source and gathers recv-transport egress (#1553)', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-2', 'u-2', 'sock-2', 'bob');

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

      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

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

      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

      expect(badHandler).toHaveBeenCalled();
      expect(goodHandler).toHaveBeenCalled();
    });
  });

  // ── updateRtpCapabilities ───────────────────────────────────────────

  describe('updateRtpCapabilities', () => {
    it('updates participant capabilities', async () => {
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
      await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
      await manager.joinRoom('room-2', 'u-2', 'sock-2', 'bob');

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
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
        const { producer } = await setupProducer(manager, mockRouter, 'room-1', 'u-1');

        await manager.serverMuteUser('room-1', 'u-1');

        const participant = manager.getParticipant('room-1', 'u-1');
        expect(participant?.serverMuted).toBe(true);
        expect(producer.pause).toHaveBeenCalled();
      });

      it('should not pause video producers', async () => {
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
        const { producer, info: producerInfo } = await setupProducer(
          manager,
          mockRouter,
          'room-1',
          'u-1'
        );

        // u-2 joins, produces audio, and consumes u-1's audio
        await manager.joinRoom(
          'room-1',
          'u-2',
          'sock-2',
          'bob',
          undefined,
          undefined,
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
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
        const { info: producerInfo } = await setupProducer(
          manager,
          mockRouter,
          'room-1',
          'u-1',
          'video',
          'camera'
        );

        // u-2 joins and consumes u-1's video
        await manager.joinRoom(
          'room-1',
          'u-2',
          'sock-2',
          'bob',
          undefined,
          undefined,
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
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
        const { info: producerInfo } = await setupProducer(manager, mockRouter, 'room-1', 'u-1');

        // u-2 joins, produces audio, and consumes u-1's audio
        await manager.joinRoom(
          'room-1',
          'u-2',
          'sock-2',
          'bob',
          undefined,
          undefined,
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
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
        const { producer } = await setupProducer(manager, mockRouter, 'room-1', 'u-1');

        await manager.userMuteParticipant('room-1', 'u-1');

        const participant = manager.getParticipant('room-1', 'u-1');
        expect(participant?.serverMuted).toBe(false);
        expect(producer.pause).toHaveBeenCalled();
      });

      it('should not pause video producers', async () => {
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
        const { info: producerInfo } = await setupProducer(manager, mockRouter, 'room-1', 'u-1');

        // u-2 joins, produces audio, and consumes u-1's audio
        await manager.joinRoom(
          'room-1',
          'u-2',
          'sock-2',
          'bob',
          undefined,
          undefined,
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
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

        const participant = manager.getParticipant('room-1', 'u-1');
        expect(participant?.serverMuted).toBe(false);
        expect(participant?.serverDeafened).toBe(false);
      });

      it('should persist flags after being set', async () => {
        await manager.joinRoom('room-1', 'u-1', 'sock-1', 'alice');

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
    await rm.joinRoom('room-1', 'u-1', 'sock-1', 'alice');
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
    await rm.joinRoom(
      'room-1',
      'u-2',
      'sock-2',
      'bob',
      undefined,
      undefined,
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

  it('never pauses the PRODUCER across an admit -> evict cycle (only consumers)', async () => {
    vi.useFakeTimers();
    try {
      vi.setSystemTime(0);
      const { room, consumer } = await setupRoomWithAudioConsumer();
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

  it('does NOT resume a SERVER-DEAFENED subscriber when its speaker enters top-N (no moderation bypass)', async () => {
    const { rm, room, userId, consumer } = await setupRoomWithAudioConsumer();
    // Server-deafen u-2 (a moderation control) → all their audio consumers paused.
    await rm.serverDeafenUser(room.id, userId);
    consumer.resume.mockClear();
    // p-mic enters the top-N — last-N would normally resume u-2's consumer.
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
