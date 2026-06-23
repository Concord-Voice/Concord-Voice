import { createClient, RedisClientType } from 'redis';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import type { RoomEvent, RoomEventHandler } from './roomManager.js';

// ---------------------------------------------------------------------------
// Redis room state — cross-instance awareness + control plane queries.
//
// Keys:
//   voice:room:{channelId}   → SET of userIds (TTL 120s, refreshed on join)
//   voice:user:{userId}      → HASH { channelId, joinedAt } (TTL 120s)
//
// The TTL acts as a safety net: if the media plane crashes without cleanup,
// stale entries expire within 2 minutes.
// ---------------------------------------------------------------------------

const ROOM_TTL = 120; // seconds
const USER_TTL = 120;

export function redisLogTarget(redisUrl: string): { endpoint: string } {
  try {
    const parsed = new URL(redisUrl);
    const port = parsed.port ? `:${parsed.port}` : '';
    return { endpoint: `${parsed.hostname}${port}` };
  } catch {
    return { endpoint: 'configured' };
  }
}

export class RedisService {
  private client: RedisClientType | null = null;

  async connect(): Promise<void> {
    try {
      this.client = createClient({ url: config.redisUrl });

      this.client.on('error', (err) => {
        logger.error('Redis client error', { error: err });
      });

      this.client.on('reconnecting', () => {
        logger.info('Redis reconnecting');
      });

      await this.client.connect();
      logger.info('Connected to Redis', redisLogTarget(config.redisUrl));
    } catch (err) {
      logger.error('Failed to connect to Redis', { error: err });
      throw err;
    }
  }

  /** Add a user to a voice room in Redis */
  async addParticipant(channelId: string, userId: string): Promise<void> {
    if (!this.client) return;

    try {
      const roomKey = `voice:room:${channelId}`;
      const userKey = `voice:user:${userId}`;

      await this.client
        .multi()
        .sAdd(roomKey, userId)
        .expire(roomKey, ROOM_TTL)
        .hSet(userKey, { channelId, joinedAt: new Date().toISOString() })
        .expire(userKey, USER_TTL)
        .exec();
    } catch (err) {
      logger.error('Redis addParticipant failed', { channelId, userId, error: err });
    }
  }

  /** Remove a user from a voice room in Redis */
  async removeParticipant(channelId: string, userId: string): Promise<void> {
    if (!this.client) return;

    try {
      const roomKey = `voice:room:${channelId}`;
      const userKey = `voice:user:${userId}`;

      await this.client.multi().sRem(roomKey, userId).del(userKey).exec();
    } catch (err) {
      logger.error('Redis removeParticipant failed', { channelId, userId, error: err });
    }
  }

  /** Remove all participants from a room (room empty) */
  async clearRoom(channelId: string): Promise<void> {
    if (!this.client) return;

    try {
      const roomKey = `voice:room:${channelId}`;

      // Get all users in room to clean up their keys
      const userIds = await this.client.sMembers(roomKey);
      if (userIds.length > 0) {
        const userKeys = userIds.map((id) => `voice:user:${id}`);
        await this.client.del([roomKey, ...userKeys]);
      } else {
        await this.client.del(roomKey);
      }
    } catch (err) {
      logger.error('Redis clearRoom failed', { channelId, error: err });
    }
  }

  /** Get all user IDs in a voice room */
  async getRoomParticipants(channelId: string): Promise<string[]> {
    if (!this.client) return [];

    try {
      return await this.client.sMembers(`voice:room:${channelId}`);
    } catch (err) {
      logger.error('Redis getRoomParticipants failed', { channelId, error: err });
      return [];
    }
  }

  /** Get which room a user is in */
  async getUserRoom(userId: string): Promise<string | null> {
    if (!this.client) return null;

    try {
      return (await this.client.hGet(`voice:user:${userId}`, 'channelId')) ?? null;
    } catch (err) {
      logger.error('Redis getUserRoom failed', { userId, error: err });
      return null;
    }
  }

  /**
   * Returns a RoomEventHandler that updates Redis state on room events.
   * Wire into RoomManager.onEvent().
   */
  createRoomEventHandler(): RoomEventHandler {
    return (event: RoomEvent) => {
      switch (event.type) {
        case 'user-joined':
          this.addParticipant(event.roomId, event.userId);
          break;
        case 'user-left':
          this.removeParticipant(event.roomId, event.userId);
          break;
        case 'room-empty':
          this.clearRoom(event.roomId);
          break;
      }
    };
  }

  async close(): Promise<void> {
    if (this.client) {
      await this.client.close();
      logger.info('Redis connection closed');
    }
  }
}
