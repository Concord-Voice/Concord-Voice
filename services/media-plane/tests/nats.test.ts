import { describe, it, expect, vi, beforeEach } from 'vitest';
import './mocks/logger.js';

// Mock config
vi.mock('@/config/index.js', () => ({
  config: {
    natsUrl: 'nats://localhost:4222',
  },
}));

// Build mock NATS connection — vi.hoisted ensures these exist before vi.mock runs
const { mockNc, mockConnect, mockEncode, mockDecode } = vi.hoisted(() => {
  const mockNc = {
    publish: vi.fn(),
    drain: vi.fn().mockResolvedValue(undefined),
    status: vi.fn(() => ({
      async *[Symbol.asyncIterator]() {
        yield { type: 'update', data: {} };
      },
    })),
    subscribe: vi.fn(),
  };
  const mockConnect = vi.fn().mockResolvedValue(mockNc);
  const mockEncode = vi.fn((data: unknown) => JSON.stringify(data));
  const mockDecode = vi.fn((data: unknown) => JSON.parse(data as string));
  return { mockNc, mockConnect, mockEncode, mockDecode };
});

vi.mock('nats', () => ({
  connect: (...args: any[]) => mockConnect(...args),
  JSONCodec: () => ({ encode: mockEncode, decode: mockDecode }),
}));

import { NatsService } from '../src/lib/nats.js';

describe('NatsService', () => {
  let service: NatsService;

  beforeEach(() => {
    service = new NatsService();
  });

  describe('connect', () => {
    it('connects with correct URL and reconnect options', async () => {
      await service.connect();

      expect(mockConnect).toHaveBeenCalledWith(
        expect.objectContaining({
          servers: 'nats://localhost:4222',
          name: 'media-plane',
          reconnect: true,
          maxReconnectAttempts: -1,
          reconnectTimeWait: 2000,
        })
      );
    });

    it('throws on connection failure', async () => {
      mockConnect.mockRejectedValueOnce(new Error('ECONNREFUSED'));
      await expect(service.connect()).rejects.toThrow('ECONNREFUSED');
    });
  });

  describe('publish', () => {
    it('encodes data and publishes to subject', async () => {
      await service.connect();

      service.publish('voice.joined', { channelId: 'ch-1', userId: 'u-1' });

      expect(mockEncode).toHaveBeenCalledWith({ channelId: 'ch-1', userId: 'u-1' });
      expect(mockNc.publish).toHaveBeenCalledWith('voice.joined', expect.anything());
    });

    it('drops message and logs warning when not connected', () => {
      // Don't call connect — nc stays null
      service.publish('voice.joined', { channelId: 'ch-1' });

      expect(mockNc.publish).not.toHaveBeenCalled();
    });

    it('catches publish errors without throwing', async () => {
      await service.connect();
      mockNc.publish.mockImplementationOnce(() => {
        throw new Error('publish error');
      });

      // Should not throw
      expect(() => service.publish('voice.joined', {})).not.toThrow();
    });
  });

  describe('createRoomEventHandler', () => {
    beforeEach(async () => {
      await service.connect();
    });

    it('maps user-joined to voice.joined with correct payload', () => {
      const handler = service.createRoomEventHandler();

      handler({
        type: 'user-joined',
        roomId: 'ch-1',
        userId: 'u-1',
        username: 'alice',
        displayName: 'Alice',
      });

      expect(mockNc.publish).toHaveBeenCalled();
      const encodedData = mockEncode.mock.calls.at(-1)?.[0];
      expect(encodedData).toMatchObject({
        channelId: 'ch-1',
        userId: 'u-1',
        username: 'alice',
        displayName: 'Alice',
      });
      expect(encodedData).toHaveProperty('timestamp');
    });

    it('maps user-left to voice.left', () => {
      const handler = service.createRoomEventHandler();

      handler({ type: 'user-left', roomId: 'ch-1', userId: 'u-1' });

      const subject = mockNc.publish.mock.calls.at(-1)?.[0];
      expect(subject).toBe('voice.left');
    });

    it('maps room-empty to voice.room_empty', () => {
      const handler = service.createRoomEventHandler();

      handler({ type: 'room-empty', roomId: 'ch-1' });

      const subject = mockNc.publish.mock.calls.at(-1)?.[0];
      expect(subject).toBe('voice.room_empty');
    });

    it('maps producer-added to voice.producer_added', () => {
      const handler = service.createRoomEventHandler();

      handler({
        type: 'producer-added',
        roomId: 'ch-1',
        userId: 'u-1',
        producerId: 'p-1',
        kind: 'audio',
        source: 'mic',
      });

      const subject = mockNc.publish.mock.calls.at(-1)?.[0];
      expect(subject).toBe('voice.producer_added');
      const payload = mockEncode.mock.calls.at(-1)?.[0];
      expect(payload).toMatchObject({
        producerId: 'p-1',
        kind: 'audio',
        source: 'mic',
      });
    });

    it('maps producer-removed to voice.producer_removed', () => {
      const handler = service.createRoomEventHandler();

      handler({
        type: 'producer-removed',
        roomId: 'ch-1',
        userId: 'u-1',
        producerId: 'p-1',
        kind: 'video',
        source: 'camera',
      });

      const subject = mockNc.publish.mock.calls.at(-1)?.[0];
      expect(subject).toBe('voice.producer_removed');
    });

    it('does NOT publish active-speaker events to NATS', () => {
      const handler = service.createRoomEventHandler();

      handler({
        type: 'active-speaker',
        roomId: 'ch-1',
        userId: 'u-1',
        volume: -30,
      });

      // active-speaker is handled locally via Socket.IO, not NATS
      expect(mockNc.publish).not.toHaveBeenCalled();
    });
  });

  describe('subscribe', () => {
    it('calls nc.subscribe and processes decoded messages', async () => {
      await service.connect();

      const messages = [
        { data: JSON.stringify({ userId: 'u-1', action: 'mute' }) },
        { data: JSON.stringify({ userId: 'u-2', action: 'deafen' }) },
      ];

      // Create an async iterable that yields the messages then completes
      mockNc.subscribe.mockReturnValue({
        async *[Symbol.asyncIterator]() {
          for (const msg of messages) {
            yield msg;
          }
        },
      });

      const handler = vi.fn();
      service.subscribe('voice.enforcement', handler);

      // Wait for the async iteration to process
      await vi.waitFor(() => {
        expect(handler).toHaveBeenCalledTimes(2);
      });

      expect(mockNc.subscribe).toHaveBeenCalledWith('voice.enforcement');
      expect(mockDecode).toHaveBeenCalledTimes(2);
      expect(handler).toHaveBeenCalledWith({ userId: 'u-1', action: 'mute' });
      expect(handler).toHaveBeenCalledWith({ userId: 'u-2', action: 'deafen' });
    });

    it('handles decode errors gracefully without crashing', async () => {
      await service.connect();

      mockNc.subscribe.mockReturnValue({
        async *[Symbol.asyncIterator]() {
          yield { data: 'invalid-json' };
        },
      });

      mockDecode.mockImplementationOnce(() => {
        throw new Error('Unexpected token');
      });

      const handler = vi.fn();

      // Should not throw
      service.subscribe('voice.enforcement', handler);

      // Wait for async iteration to complete
      await vi.waitFor(() => {
        expect(mockDecode).toHaveBeenCalled();
      });

      expect(handler).not.toHaveBeenCalled();
    });

    it('is a no-op when not connected (nc is null)', () => {
      // Don't call connect — nc stays null
      const handler = vi.fn();
      service.subscribe('voice.enforcement', handler);

      expect(mockNc.subscribe).not.toHaveBeenCalled();
      expect(handler).not.toHaveBeenCalled();
    });
  });

  describe('close', () => {
    it('calls drain on the NATS connection', async () => {
      await service.connect();
      await service.close();
      expect(mockNc.drain).toHaveBeenCalled();
    });
  });
});
