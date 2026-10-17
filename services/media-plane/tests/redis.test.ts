import { describe, it, expect, vi, beforeEach } from 'vitest';
import './mocks/logger.js';

// Mock config
vi.mock('@/config/index.js', () => ({
  config: {
    redisUrl: 'redis://:redis-secret@redis:6379',
  },
}));

// Build mock Redis client
const mockMulti = {
  set: vi.fn().mockReturnThis(),
  eval: vi.fn().mockReturnThis(),
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
  destroy: vi.fn(),
  multi: vi.fn(() => mockMulti),
  sMembers: vi.fn().mockResolvedValue([]),
  hGet: vi.fn().mockResolvedValue(null),
  del: vi.fn().mockResolvedValue(0),
  mGet: vi.fn().mockResolvedValue([null, null, null]),
  get: vi.fn().mockResolvedValue(null),
  set: vi.fn().mockResolvedValue('OK'),
  incr: vi.fn().mockResolvedValue(42),
  eval: vi.fn().mockResolvedValue(1),
};

vi.mock('redis', () => ({
  createClient: vi.fn(() => mockClient),
}));

import { RedisService, redisLogTarget } from '../src/lib/redis.js';
import { logger } from '../src/lib/logger.js';
import { createClient } from 'redis';
import { config } from '@/config/index.js';

describe('RedisService', () => {
  let service: RedisService;

  beforeEach(() => {
    vi.clearAllMocks();
    service = new RedisService();
    // Reset multi chain mocks
    mockMulti.sAdd.mockReturnThis();
    mockMulti.set.mockReturnThis();
    mockMulti.eval.mockReturnThis();
    mockMulti.expire.mockReturnThis();
    mockMulti.hSet.mockReturnThis();
    mockMulti.sRem.mockReturnThis();
    mockMulti.del.mockReturnThis();
    mockMulti.exec.mockResolvedValue([]);
  });

  describe('redisLogTarget', () => {
    it('returns only host and port for credentialed URLs', () => {
      expect(redisLogTarget('redis://:redis-secret@redis:6379')).toEqual({
        endpoint: 'redis:6379',
      });
    });

    it('falls back without echoing malformed Redis URLs', () => {
      expect(redisLogTarget('not-a-url redis-secret')).toEqual({ endpoint: 'configured' });
    });
  });

  describe('connect', () => {
    it('creates a client and connects', async () => {
      await service.connect();
      expect(mockClient.connect).toHaveBeenCalled();
    });

    it('does not log Redis credentials', async () => {
      await service.connect();

      expect(createClient).toHaveBeenCalledWith({ url: 'redis://:redis-secret@redis:6379' });

      const infoCalls = vi.mocked(logger.info).mock.calls.map((call) => JSON.stringify(call));
      const logged = infoCalls.join('\n');
      expect(logged).toContain('redis:6379');
      expect(logged).not.toContain('redis-secret');
      expect(logged).not.toContain('redis://');
    });

    it('bounds the command queue only when placement is enabled', async () => {
      (config as { roomPlacementEnabled?: boolean }).roomPlacementEnabled = true;
      try {
        await service.connect();
        expect(createClient).toHaveBeenCalledWith({
          url: 'redis://:redis-secret@redis:6379',
          disableOfflineQueue: true,
          commandsQueueMaxLength: 256,
        });
      } finally {
        (config as { roomPlacementEnabled?: boolean }).roomPlacementEnabled = false;
      }
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

  it('does not reuse an instance generation after a same-second counter reset', async () => {
    const clock = vi.spyOn(Date, 'now').mockReturnValue(1_700_000_000_000);
    try {
      await service.connect();
      const beforeFlush = await service.initializeMediaInstance('blue');
      const restarted = new RedisService();
      await restarted.connect();
      // Redis has lost its counter; both allocations see the same seed and sequence.
      const afterFlush = await restarted.initializeMediaInstance('blue');

      expect(beforeFlush.generation).toMatch(/^42-[0-9a-f-]{36}$/);
      expect(afterFlush.generation).not.toBe(beforeFlush.generation);
      expect(afterFlush.instanceId).not.toBe(beforeFlush.instanceId);
      expect(mockClient.incr).toHaveBeenCalledTimes(2);
    } finally {
      clock.mockRestore();
    }
  });

  describe('createRoomEventHandler', () => {
    it('marks provisional room creation live, including a ring-only DM room', async () => {
      await service.connect();
      const identity = await service.initializeMediaInstance('blue');
      const handler = service.createRoomEventHandler();

      handler({ type: 'room-created', roomId: 'dm-ring-only' });

      await vi.waitFor(() =>
        expect(mockClient.eval).toHaveBeenCalledWith(
          expect.stringContaining('if live and live ~= ARGV[2] then return 0 end'),
          {
            keys: ['voice:room_owner:dm-ring-only', 'voice:room_live:dm-ring-only'],
            arguments: ['blue', identity.instanceId, '90'],
          }
        )
      );
    });

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

      handler({
        type: 'user-left',
        roomId: 'ch-1',
        userId: 'u-1',
        socketId: 'sock-1',
        e2eeEpoch: 3,
      });

      expect(mockMulti.sRem).toHaveBeenCalled();
    });

    it('routes room-empty to clearRoom', async () => {
      await service.connect();
      const handler = service.createRoomEventHandler();

      handler({ type: 'room-empty', roomId: 'ch-1' });

      expect(mockClient.sMembers).toHaveBeenCalled();
    });
  });

  describe('room creation guard', () => {
    let instanceId: string;
    beforeEach(async () => {
      await service.connect();
      instanceId = (await service.initializeMediaInstance('blue')).instanceId;
    });

    it('retires a black-holed drain refresh so the standby can recover', async () => {
      vi.useFakeTimers();
      try {
        let disconnect!: (error: Error) => void;
        mockClient.get.mockReturnValueOnce(
          new Promise((_, reject) => {
            disconnect = reject;
          })
        );
        mockClient.destroy.mockImplementationOnce(() => disconnect(new Error('socket destroyed')));
        const refresh = service.refreshAcceptingNewRooms();
        await vi.advanceTimersByTimeAsync(1_000);
        await refresh;
        expect(mockClient.destroy).toHaveBeenCalledOnce();
        expect(createClient).toHaveBeenCalledTimes(2);
      } finally {
        vi.useRealTimers();
      }
    });

    it('allows a matching owner and reads the exact three-key fence', async () => {
      mockClient.mGet.mockResolvedValueOnce(['blue', 'green-41', null]);

      await expect(service.guardRoomCreation('room-1')).resolves.toBeUndefined();

      expect(mockClient.mGet).toHaveBeenCalledWith([
        'voice:room_owner:room-1',
        'voice:room_live:room-1',
        'media:colour_draining:blue',
      ]);
      expect(service.pendingRoomOwnerReadCount()).toBe(0);
    });

    it('publishes a replacement generation when the room owner is this colour', async () => {
      (config as { roomPlacementEnabled?: boolean }).roomPlacementEnabled = true;
      try {
        mockClient.mGet.mockResolvedValueOnce(['blue', 'blue-old-generation', null]);
        await expect(service.guardRoomCreation('room-restart')).resolves.toBeUndefined();
        await expect(
          service.createRoomEventHandler()({ type: 'room-created', roomId: 'room-restart' })
        ).resolves.toBeUndefined();
        expect(mockClient.eval).toHaveBeenCalledWith(
          expect.stringContaining(
            'if live and live ~= ARGV[2] and owner ~= ARGV[1] then return -2 end'
          ),
          {
            keys: [
              'voice:room_owner:room-restart',
              'voice:room_live:room-restart',
              'media:colour_draining:blue',
            ],
            arguments: ['blue', instanceId, '90'],
          }
        );
      } finally {
        (config as { roomPlacementEnabled?: boolean }).roomPlacementEnabled = false;
      }
    });

    it('does not leave a pending owner-read command after a closed socket', async () => {
      mockClient.mGet.mockRejectedValueOnce(new Error('socket closed'));

      await expect(service.guardRoomCreation('room-1')).rejects.toMatchObject({
        code: 'room_owner_unavailable',
      });
      expect(service.pendingRoomOwnerReadCount()).toBe(0);
    });

    it.each([
      [['green', null, null], 'room_owner_mismatch'],
      [[null, 'green-41', null], 'room_owner_mismatch'],
      [[null, null, 'armed'], 'room_draining'],
    ] as const)('refuses conflicting placement: %j', async (values, code) => {
      mockClient.mGet.mockResolvedValueOnce([...values]);

      await expect(service.guardRoomCreation('room-1')).rejects.toMatchObject({ code });
      expect(service.healthState().ownerRefusalsTotal).toBe(1);
    });

    it('fails closed when the bounded Redis read fails', async () => {
      mockClient.mGet.mockRejectedValueOnce(new Error('redis unavailable'));

      await expect(service.guardRoomCreation('room-1')).rejects.toMatchObject({
        code: 'room_owner_unavailable',
      });
    });

    it('refuses required occupancy publication without a media instance identity', async () => {
      (config as { roomPlacementEnabled?: boolean }).roomPlacementEnabled = true;
      try {
        const isolatedService = new RedisService();
        const handler = isolatedService.createRoomEventHandler();
        await expect(
          handler({ type: 'room-created', roomId: 'unidentified' })
        ).rejects.toMatchObject({
          code: 'room_owner_unavailable',
        });
      } finally {
        (config as { roomPlacementEnabled?: boolean }).roomPlacementEnabled = false;
      }
    });

    it('destroys a black-holed owner read at the one-second deadline', async () => {
      vi.useFakeTimers();
      try {
        let disconnect!: (error: Error) => void;
        mockClient.mGet.mockReturnValueOnce(
          new Promise((_, reject) => {
            disconnect = reject;
          })
        );
        mockClient.destroy.mockImplementationOnce(() => disconnect(new Error('socket destroyed')));
        const pending = service.guardRoomCreation('room-1');
        const refusal = expect(pending).rejects.toMatchObject({ code: 'room_owner_unavailable' });
        await vi.advanceTimersByTimeAsync(1_000);
        await refusal;
        expect(mockClient.destroy).toHaveBeenCalledOnce();
        expect(service.pendingRoomOwnerReadCount()).toBe(0);
        expect(createClient).toHaveBeenCalledTimes(2);
      } finally {
        vi.useRealTimers();
      }
    });

    it.each([
      [-1, 'room_draining'],
      [-2, 'room_owner_mismatch'],
    ] as const)(
      'rechecks drain and ownership atomically after the initial guard (%s)',
      async (scriptResult, code) => {
        (config as { roomPlacementEnabled?: boolean }).roomPlacementEnabled = true;
        try {
          mockClient.mGet.mockResolvedValueOnce(['blue', null, null]);
          await service.guardRoomCreation('room-race');

          // Cutover or placement reassignment occurs after MGET but before the
          // room is made visible; the Lua publication fence must refuse it.
          mockClient.eval.mockResolvedValueOnce(scriptResult);
          const handler = service.createRoomEventHandler();
          await expect(
            handler({ type: 'room-created', roomId: 'room-race' })
          ).rejects.toMatchObject({ code });

          expect(mockClient.eval).toHaveBeenCalledWith(
            expect.stringContaining('if draining then return -1 end'),
            {
              keys: [
                'voice:room_owner:room-race',
                'voice:room_live:room-race',
                'media:colour_draining:blue',
              ],
              arguments: ['blue', instanceId, '90'],
            }
          );
          expect(mockClient.set).not.toHaveBeenCalledWith('voice:room_live:room-race', instanceId, {
            EX: 90,
          });
        } finally {
          (config as { roomPlacementEnabled?: boolean }).roomPlacementEnabled = false;
        }
      }
    );

    it('retires a black-holed required publication before it can write after refusal', async () => {
      (config as { roomPlacementEnabled?: boolean }).roomPlacementEnabled = true;
      vi.useFakeTimers();
      try {
        mockClient.eval.mockReturnValueOnce(new Promise(() => {}));
        const handler = service.createRoomEventHandler();
        const refusal = expect(
          handler({ type: 'room-created', roomId: 'room-stalled' })
        ).rejects.toMatchObject({ code: 'room_owner_unavailable' });

        await vi.advanceTimersByTimeAsync(1_000);
        await refusal;
        expect(mockClient.destroy).toHaveBeenCalledOnce();
        expect(createClient).toHaveBeenCalledTimes(2);
      } finally {
        vi.useRealTimers();
        (config as { roomPlacementEnabled?: boolean }).roomPlacementEnabled = false;
      }
    });

    it('writes occupancy for every supplied room, including empty rooms', async () => {
      await service.heartbeatRoomOccupancy(['active', 'ring-only']);

      for (const roomId of ['active', 'ring-only']) {
        expect(mockMulti.eval).toHaveBeenCalledWith(
          expect.stringContaining('if owner and owner ~= ARGV[1] then return 0 end'),
          {
            keys: [`voice:room_owner:${roomId}`, `voice:room_live:${roomId}`],
            arguments: ['blue', instanceId, '90'],
          }
        );
      }
      expect(mockMulti.set).not.toHaveBeenCalled();
    });

    it('bounds a stalled occupancy heartbeat and retires its Redis client', async () => {
      mockMulti.exec.mockReturnValueOnce(new Promise(() => {}));
      vi.useFakeTimers();
      try {
        const heartbeat = service.heartbeatRoomOccupancy(['active']);
        await vi.advanceTimersByTimeAsync(1_000);
        await heartbeat;

        expect(mockClient.destroy).toHaveBeenCalledOnce();
        expect(createClient).toHaveBeenCalledTimes(2);
      } finally {
        vi.useRealTimers();
      }
    });

    it('checks owner and generation on optional user-joined publication', async () => {
      const handler = service.createRoomEventHandler();
      handler({
        type: 'user-joined',
        roomId: 'room-joined',
        userId: 'u-1',
        username: 'alice',
        e2eeEpoch: 1,
      });

      await vi.waitFor(() =>
        expect(mockClient.eval).toHaveBeenCalledWith(
          expect.stringContaining('if live and live ~= ARGV[2] then return 0 end'),
          {
            keys: ['voice:room_owner:room-joined', 'voice:room_live:room-joined'],
            arguments: ['blue', instanceId, '90'],
          }
        )
      );
    });
  });

  describe('close', () => {
    it('closes the client', async () => {
      await service.connect();
      await service.close();
      expect(mockClient.close).toHaveBeenCalled();
    });
  });

  it('emits a fixed dependency event after a Redis write failure', async () => {
    const emit = vi.fn();
    service.setSecurityEventEmitter(emit);
    await service.connect();
    mockMulti.exec.mockRejectedValueOnce(new Error('secret'));
    await service.addParticipant('channel-1', 'user-1');
    expect(emit).toHaveBeenCalledWith({
      eventType: 'dependency',
      outcome: 'degraded',
      severity: 'high',
      reasonCode: 'dependency_unavailable',
    });
  });

  it('deduplicates missing-client failures and restores after a normal write', async () => {
    const emit = vi.fn();
    service.setSecurityEventEmitter(emit);
    await service.addParticipant('private-channel', 'private-user');
    await service.getUserRoom('private-user');
    expect(emit).toHaveBeenCalledTimes(1);
    await service.connect();
    await service.addParticipant('private-channel', 'private-user');
    expect(emit).toHaveBeenLastCalledWith({
      eventType: 'dependency',
      outcome: 'restored',
      severity: 'informational',
      reasonCode: 'dependency_recovered',
    });
  });
});
