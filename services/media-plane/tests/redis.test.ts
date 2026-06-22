import { describe, it, expect, vi, beforeEach } from 'vitest';
import './mocks/logger.js';

// Mock config
vi.mock('@/config/index.js', () => ({
  config: {
    redisUrl: 'redis://localhost:6379',
  },
}));

// Build mock Redis client
const mockMulti = {
  sAdd: vi.fn().mockReturnThis(),
  expire: vi.fn().mockReturnThis(),
  hSet: vi.fn().mockReturnThis(),
  sRem: vi.fn().mockReturnThis(),
  del: vi.fn().mockReturnThis(),
  exec: vi.fn().mockResolvedValue([]),
};

const mockClient = {
  on: vi.fn(),
  connect: vi.fn().mockResolvedValue(undefined),
  close: vi.fn().mockResolvedValue(undefined),
  multi: vi.fn(() => mockMulti),
  sMembers: vi.fn().mockResolvedValue([]),
  hGet: vi.fn().mockResolvedValue(null),
  del: vi.fn().mockResolvedValue(0),
};

vi.mock('redis', () => ({
  createClient: vi.fn(() => mockClient),
}));

import { RedisService } from '../src/lib/redis.js';

describe('RedisService', () => {
  let service: RedisService;

  beforeEach(() => {
    service = new RedisService();
    // Reset multi chain mocks
    mockMulti.sAdd.mockReturnThis();
    mockMulti.expire.mockReturnThis();
    mockMulti.hSet.mockReturnThis();
    mockMulti.sRem.mockReturnThis();
    mockMulti.del.mockReturnThis();
    mockMulti.exec.mockResolvedValue([]);
  });

  describe('connect', () => {
    it('creates a client and connects', async () => {
      await service.connect();
      expect(mockClient.connect).toHaveBeenCalled();
    });

    it('registers error and reconnecting event handlers', async () => {
      await service.connect();
      const eventNames = mockClient.on.mock.calls.map((c: any[]) => c[0]);
      expect(eventNames).toContain('error');
      expect(eventNames).toContain('reconnecting');
    });

    it('throws on connection failure', async () => {
      mockClient.connect.mockRejectedValueOnce(new Error('ECONNREFUSED'));
      await expect(service.connect()).rejects.toThrow('ECONNREFUSED');
    });
  });

  describe('addParticipant', () => {
    it('is a no-op when client is null (not connected)', async () => {
      // Don't call connect — client stays null
      await service.addParticipant('channel-1', 'user-1');
      expect(mockClient.multi).not.toHaveBeenCalled();
    });

    it('executes multi pipeline with sAdd + expire + hSet + expire', async () => {
      await service.connect();
      await service.addParticipant('channel-1', 'user-1');

      expect(mockClient.multi).toHaveBeenCalled();
      expect(mockMulti.sAdd).toHaveBeenCalledWith('voice:room:channel-1', 'user-1');
      expect(mockMulti.expire).toHaveBeenCalledWith('voice:room:channel-1', 120);
      expect(mockMulti.hSet).toHaveBeenCalledWith(
        'voice:user:user-1',
        expect.objectContaining({ channelId: 'channel-1' })
      );
      expect(mockMulti.exec).toHaveBeenCalled();
    });

    it('catches and logs errors without throwing', async () => {
      await service.connect();
      mockMulti.exec.mockRejectedValueOnce(new Error('Redis error'));

      // Should not throw
      await expect(service.addParticipant('ch', 'u')).resolves.toBeUndefined();
    });
  });

  describe('removeParticipant', () => {
    it('executes multi with sRem + del', async () => {
      await service.connect();
      await service.removeParticipant('channel-1', 'user-1');

      expect(mockMulti.sRem).toHaveBeenCalledWith('voice:room:channel-1', 'user-1');
      expect(mockMulti.del).toHaveBeenCalledWith('voice:user:user-1');
      expect(mockMulti.exec).toHaveBeenCalled();
    });
  });

  describe('clearRoom', () => {
    it('deletes room key and all user keys when members exist', async () => {
      await service.connect();
      mockClient.sMembers.mockResolvedValueOnce(['user-1', 'user-2']);

      await service.clearRoom('channel-1');

      expect(mockClient.sMembers).toHaveBeenCalledWith('voice:room:channel-1');
      expect(mockClient.del).toHaveBeenCalledWith([
        'voice:room:channel-1',
        'voice:user:user-1',
        'voice:user:user-2',
      ]);
    });

    it('deletes only room key when no members', async () => {
      await service.connect();
      mockClient.sMembers.mockResolvedValueOnce([]);

      await service.clearRoom('channel-1');

      expect(mockClient.del).toHaveBeenCalledWith('voice:room:channel-1');
    });
  });

  describe('getRoomParticipants', () => {
    it('returns sMembers result', async () => {
      await service.connect();
      mockClient.sMembers.mockResolvedValueOnce(['user-1', 'user-2']);

      const result = await service.getRoomParticipants('channel-1');
      expect(result).toEqual(['user-1', 'user-2']);
    });

    it('returns empty array when client is null', async () => {
      const result = await service.getRoomParticipants('channel-1');
      expect(result).toEqual([]);
    });
  });

  describe('getUserRoom', () => {
    it('returns channelId from hash', async () => {
      await service.connect();
      mockClient.hGet.mockResolvedValueOnce('channel-1');

      const result = await service.getUserRoom('user-1');
      expect(result).toBe('channel-1');
    });

    it('returns null when not found', async () => {
      await service.connect();
      mockClient.hGet.mockResolvedValueOnce(undefined);

      const result = await service.getUserRoom('user-1');
      expect(result).toBeNull();
    });

    it('returns null when client is null', async () => {
      const result = await service.getUserRoom('user-1');
      expect(result).toBeNull();
    });
  });

  describe('createRoomEventHandler', () => {
    it('routes user-joined to addParticipant', async () => {
      await service.connect();
      const handler = service.createRoomEventHandler();

      handler({
        type: 'user-joined',
        roomId: 'ch-1',
        userId: 'u-1',
        username: 'alice',
        e2eeEpoch: 1,
      });

      // addParticipant is async but the handler is sync — it fires-and-forgets
      expect(mockClient.multi).toHaveBeenCalled();
    });

    it('routes user-left to removeParticipant', async () => {
      await service.connect();
      const handler = service.createRoomEventHandler();

      handler({ type: 'user-left', roomId: 'ch-1', userId: 'u-1' });

      expect(mockMulti.sRem).toHaveBeenCalled();
    });

    it('routes room-empty to clearRoom', async () => {
      await service.connect();
      const handler = service.createRoomEventHandler();

      handler({ type: 'room-empty', roomId: 'ch-1' });

      expect(mockClient.sMembers).toHaveBeenCalled();
    });
  });

  describe('close', () => {
    it('closes the client', async () => {
      await service.connect();
      await service.close();
      expect(mockClient.close).toHaveBeenCalled();
    });
  });
});
