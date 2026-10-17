import { randomUUID } from 'node:crypto';
import { createClient, RedisClientType } from 'redis';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import type { RoomEvent, RoomEventHandler } from './roomManager.js';
import type { EmitSecurityEvent } from './securityEvent.js';
import { RoomCreationRefusedError } from './roomManager.js';

const ROOM_LIVE_TTL_SECONDS = 90;
const CREATE_GUARD_TIMEOUT_MS = 1_000;
// shortcut: 256 is an unmeasured fail-closed queue ceiling; replace with observed peak in-flight joins before enabling placement.
const REDIS_COMMAND_QUEUE_MAX_LENGTH = 256;
const REMOVE_OWNED_ROOM_LIVE_SCRIPT =
  "if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) else return 0 end";
const CREATE_ROOM_LIVE_WITH_FENCE_SCRIPT = `
local owner = redis.call('GET', KEYS[1])
local live = redis.call('GET', KEYS[2])
local draining = redis.call('GET', KEYS[3])
if draining then return -1 end
if owner and owner ~= ARGV[1] then return -2 end
if live and live ~= ARGV[2] and owner ~= ARGV[1] then return -2 end
redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[3])
return 1
`;
const REFRESH_ROOM_LIVE_WITH_FENCE_SCRIPT = `
local owner = redis.call('GET', KEYS[1])
local live = redis.call('GET', KEYS[2])
if owner and owner ~= ARGV[1] then return 0 end
if live and live ~= ARGV[2] then return 0 end
redis.call('SET', KEYS[2], ARGV[2], 'EX', ARGV[3])
return 1
`;

export interface MediaInstanceIdentity {
  color: 'blue' | 'green';
  generation: string;
  instanceId: string;
}

export interface MediaHealthState {
  color: 'blue' | 'green' | null;
  generation: string | null;
  acceptingNewRooms: boolean;
  redisReachable: boolean;
  ownerRefusalsTotal: number;
}

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
  private instanceIdentity: MediaInstanceIdentity | undefined;
  private redisReachable = false;
  private acceptingNewRooms = false;
  private ownerRefusalsTotal = 0;
  private pendingOwnerReadCommands = 0;

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
    const client = createClient({
      url: config.redisUrl,
      ...(config.roomPlacementEnabled && {
        disableOfflineQueue: true,
        commandsQueueMaxLength: REDIS_COMMAND_QUEUE_MAX_LENGTH,
      }),
    });
    this.client = client;
    try {
      client.on('error', (err) => {
        if (this.client !== client) return;
        this.redisReachable = false;
        this.degraded();
        logger.error('Redis client error', { error: err });
      });

      client.on('reconnecting', () => {
        if (this.client !== client) return;
        this.redisReachable = false;
        this.degraded();
        logger.info('Redis reconnecting');
      });

      client.on('ready', () => {
        if (this.client !== client) return;
        this.redisReachable = true;
        this.restored();
      });

      await client.connect();
      if (this.client !== client) return;
      this.redisReachable = true;
      this.restored();
      logger.info('Connected to Redis', redisLogTarget(config.redisUrl));
    } catch (err) {
      this.degraded();
      logger.error('Failed to connect to Redis', { error: err });
      throw err;
    }
  }

  /** Allocate a non-reused process generation before this colour serves joins. */
  async initializeMediaInstance(color: 'blue' | 'green'): Promise<MediaInstanceIdentity> {
    if (!this.client) throw new Error('Redis is not connected');
    const generationKey = 'media:generation';
    const seed = Math.floor(Date.now() / 1_000).toString();
    await this.withDeadline(this.client.set(generationKey, seed, { NX: true }));
    const sequence = await this.withDeadline(this.client.incr(generationKey));
    const generation = `${sequence}-${randomUUID()}`;
    const identity = {
      color,
      generation,
      instanceId: `${color}-${generation}`,
    };
    this.instanceIdentity = identity;
    this.acceptingNewRooms = await this.refreshAcceptingNewRooms();
    return identity;
  }

  /** Run D2's fail-closed, bounded owner/drain/occupancy read before gate creation. */
  async guardRoomCreation(roomId: string): Promise<void> {
    const identity = this.instanceIdentity;
    if (!identity) return; // Legacy single-colour mode; activation is opt-in.
    const client = this.client;
    if (!client) return this.refuseOwner('room_owner_unavailable');

    let owner: string | null;
    let live: string | null;
    let draining: string | null;
    try {
      this.pendingOwnerReadCommands += 1;
      const command = client.mGet([
        `voice:room_owner:${roomId}`,
        `voice:room_live:${roomId}`,
        `media:colour_draining:${identity.color}`,
      ]);
      void command.then(
        () => {
          this.pendingOwnerReadCommands -= 1;
        },
        () => {
          this.pendingOwnerReadCommands -= 1;
        }
      );
      [owner, live, draining] = await this.withDeadline(command, () =>
        this.retireTimedOutClient(client)
      );
      this.redisReachable = true;
      this.acceptingNewRooms = draining === null;
    } catch {
      if (this.client === client) this.redisReachable = false;
      return this.refuseOwner('room_owner_unavailable');
    }

    if (draining !== null) return this.refuseOwner('room_draining');
    if (owner === identity.color) return;
    if (owner !== null) return this.refuseOwner('room_owner_mismatch');
    if (live !== null && live !== identity.instanceId) {
      return this.refuseOwner('room_owner_mismatch');
    }
  }

  pendingRoomOwnerReadCount(): number {
    return this.pendingOwnerReadCommands;
  }

  private refuseOwner(
    code: 'room_draining' | 'room_owner_mismatch' | 'room_owner_unavailable'
  ): never {
    this.ownerRefusalsTotal += 1;
    throw new RoomCreationRefusedError(code);
  }

  private retireTimedOutClient(client: RedisClientType): void {
    if (this.client !== client) return;
    this.client = null;
    this.redisReachable = false;
    this.acceptingNewRooms = false;
    this.degraded();
    client.destroy(); // Flushes already-written reads, which caller-side deadlines cannot cancel.
    void this.connect()
      .then(() => this.refreshAcceptingNewRooms())
      .catch(() => {});
  }

  private async withDeadline<T>(operation: Promise<T>, onTimeout?: () => void): Promise<T> {
    let timer: NodeJS.Timeout | undefined;
    try {
      return await Promise.race([
        operation,
        new Promise<T>((_, reject) => {
          timer = setTimeout(() => {
            onTimeout?.();
            reject(new Error('redis_operation_deadline'));
          }, CREATE_GUARD_TIMEOUT_MS);
          timer.unref();
        }),
      ]);
    } finally {
      if (timer) clearTimeout(timer);
    }
  }

  /** Refresh the drain projection; unreadable reads preserve the last good value. */
  async refreshAcceptingNewRooms(): Promise<boolean> {
    const identity = this.instanceIdentity;
    if (!identity) {
      this.acceptingNewRooms = true;
      return true;
    }
    const client = this.client;
    if (!client) return this.acceptingNewRooms;
    try {
      const draining = await this.withDeadline(
        client.get(`media:colour_draining:${identity.color}`),
        () => this.retireTimedOutClient(client)
      );
      this.redisReachable = true;
      this.acceptingNewRooms = draining === null;
    } catch {
      this.redisReachable = false;
    }
    return this.acceptingNewRooms;
  }

  healthState(): MediaHealthState {
    return {
      color: this.instanceIdentity?.color ?? null,
      generation: this.instanceIdentity?.generation ?? null,
      acceptingNewRooms: this.acceptingNewRooms,
      redisReachable: this.redisReachable,
      ownerRefusalsTotal: this.ownerRefusalsTotal,
    };
  }

  /** Refresh occupancy from the complete room map, including ring-only DMs. */
  async heartbeatRoomOccupancy(roomIds: readonly string[]): Promise<void> {
    const identity = this.instanceIdentity;
    const client = this.client;
    if (!identity || !client || roomIds.length === 0) return;
    try {
      const transaction = client.multi();
      for (const roomId of roomIds) {
        transaction.eval(REFRESH_ROOM_LIVE_WITH_FENCE_SCRIPT, {
          keys: [`voice:room_owner:${roomId}`, `voice:room_live:${roomId}`],
          arguments: [identity.color, identity.instanceId, String(ROOM_LIVE_TTL_SECONDS)],
        });
      }
      await this.withDeadline(transaction.exec(), () => this.retireTimedOutClient(client));
      this.redisReachable = true;
    } catch (err) {
      this.redisReachable = false;
      this.degraded();
      logger.error('Redis room occupancy heartbeat failed', { error: err });
    }
  }

  private async markRoomLive(roomId: string, required = false): Promise<void> {
    const identity = this.instanceIdentity;
    const client = this.client;
    if (!identity || !client) {
      if (required) this.refuseOwner('room_owner_unavailable');
      return;
    }
    if (required) {
      let result: unknown;
      try {
        result = await this.withDeadline(
          client.eval(CREATE_ROOM_LIVE_WITH_FENCE_SCRIPT, {
            keys: [
              `voice:room_owner:${roomId}`,
              `voice:room_live:${roomId}`,
              `media:colour_draining:${identity.color}`,
            ],
            arguments: [identity.color, identity.instanceId, String(ROOM_LIVE_TTL_SECONDS)],
          }),
          () => this.retireTimedOutClient(client)
        );
      } catch {
        this.redisReachable = false;
        this.refuseOwner('room_owner_unavailable');
      }
      if (Number(result) === -1) this.refuseOwner('room_draining');
      if (Number(result) === -2) this.refuseOwner('room_owner_mismatch');
      if (Number(result) !== 1) this.refuseOwner('room_owner_unavailable');
      this.redisReachable = true;
      return;
    }

    try {
      await this.withDeadline(
        client.eval(REFRESH_ROOM_LIVE_WITH_FENCE_SCRIPT, {
          keys: [`voice:room_owner:${roomId}`, `voice:room_live:${roomId}`],
          arguments: [identity.color, identity.instanceId, String(ROOM_LIVE_TTL_SECONDS)],
        }),
        () => this.retireTimedOutClient(client)
      );
      this.redisReachable = true;
    } catch {
      this.redisReachable = false;
    }
  }

  private async clearOwnedRoomLive(roomId: string): Promise<void> {
    const identity = this.instanceIdentity;
    if (!identity || !this.client) return;
    try {
      await this.withDeadline(
        this.client.eval(REMOVE_OWNED_ROOM_LIVE_SCRIPT, {
          keys: [`voice:room_live:${roomId}`],
          arguments: [identity.instanceId],
        })
      );
      this.redisReachable = true;
    } catch {
      this.redisReachable = false;
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
        case 'room-created':
          return this.markRoomLive(event.roomId, config.roomPlacementEnabled);
        case 'user-joined':
          void this.markRoomLive(event.roomId);
          this.addParticipant(event.roomId, event.userId);
          break;
        case 'user-left':
          this.removeParticipant(event.roomId, event.userId);
          break;
        case 'room-empty':
          void this.clearOwnedRoomLive(event.roomId);
          this.clearRoom(event.roomId);
          break;
      }
      return undefined;
    };
  }

  async close(): Promise<void> {
    if (this.client) {
      await this.client.close();
      logger.info('Redis connection closed');
    }
  }
}
