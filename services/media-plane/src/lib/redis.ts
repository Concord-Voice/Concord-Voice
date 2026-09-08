import { createClient, RedisClientType } from 'redis';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import type { RoomEvent, RoomEventHandler } from './roomManager.js';
import type { EmitSecurityEvent } from './securityEvent.js';

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
  private emitSecurityEvent: EmitSecurityEvent | undefined;
  private securityDegraded = false;

  setSecurityEventEmitter(emit: EmitSecurityEvent | undefined): void {
    this.emitSecurityEvent = emit;
  }
  private security(
    outcome: 'degraded' | 'restored',
    reasonCode: 'dependency_unavailable' | 'dependency_recovered'
  ): void {
    try {
      this.emitSecurityEvent?.({
        eventType: 'dependency',
        outcome,
        severity: outcome === 'degraded' ? 'high' : 'informational',
        reasonCode,
      });
    } catch {
      // The observer is intentionally outside Redis availability semantics.
    }
  }
  private degraded(): void {
    if (this.securityDegraded) return;
    this.security('degraded', 'dependency_unavailable');
    this.securityDegraded = true;
  }
  private restored(): void {
    if (!this.securityDegraded) return;
    this.security('restored', 'dependency_recovered');
    this.securityDegraded = false;
  }

  async connect(): Promise<void> {
    try {
      this.client = createClient({ url: config.redisUrl });

      this.client.on('error', (err) => {
        this.degraded();
        logger.error('Redis client error', { error: err });
      });

      this.client.on('reconnecting', () => {
        this.degraded();
        logger.info('Redis reconnecting');
      });

      this.client.on('ready', () => {
        this.restored();
      });

      await this.client.connect();
      this.restored();
      logger.info('Connected to Redis', redisLogTarget(config.redisUrl));
    } catch (err) {
      this.degraded();
      logger.error('Failed to connect to Redis', { error: err });
      throw err;
    }
  }

  /** Add a user to a voice room in Redis */
  async addParticipant(channelId: string, userId: string): Promise<void> {
    if (!this.client) {
      this.degraded();
      return;
    }

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
      this.restored();
    } catch (err) {
      this.degraded();
      logger.error('Redis addParticipant failed', { channelId, userId, error: err });
    }
  }

  /** Remove a user from a voice room in Redis */
  async removeParticipant(channelId: string, userId: string): Promise<void> {
    if (!this.client) {
      this.degraded();
      return;
    }

    try {
      const roomKey = `voice:room:${channelId}`;
      const userKey = `voice:user:${userId}`;

      await this.client.multi().sRem(roomKey, userId).del(userKey).exec();
      this.restored();
    } catch (err) {
      this.degraded();
      logger.error('Redis removeParticipant failed', { channelId, userId, error: err });
    }
  }

  /** Remove all participants from a room (room empty) */
  async clearRoom(channelId: string): Promise<void> {
    if (!this.client) {
      this.degraded();
      return;
    }

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
      this.restored();
    } catch (err) {
      this.degraded();
      logger.error('Redis clearRoom failed', { channelId, error: err });
    }
  }

  /** Get all user IDs in a voice room */
  async getRoomParticipants(channelId: string): Promise<string[]> {
    if (!this.client) {
      this.degraded();
      return [];
    }

    try {
      const participants = await this.client.sMembers(`voice:room:${channelId}`);
      this.restored();
      return participants;
    } catch (err) {
      this.degraded();
      logger.error('Redis getRoomParticipants failed', { channelId, error: err });
      return [];
    }
  }

  /** Get which room a user is in */
  async getUserRoom(userId: string): Promise<string | null> {
    if (!this.client) {
      this.degraded();
      return null;
    }

    try {
      const room = (await this.client.hGet(`voice:user:${userId}`, 'channelId')) ?? null;
      this.restored();
      return room;
    } catch (err) {
      this.degraded();
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
