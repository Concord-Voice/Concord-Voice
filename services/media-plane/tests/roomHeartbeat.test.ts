import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import ts from 'typescript';
import { describe, expect, it, vi } from 'vitest';

const require = createRequire(import.meta.url);
const mediaRoot = join(dirname(fileURLToPath(import.meta.url)), '..');

type Deferred<T> = { promise: Promise<T>; resolve: (value: T) => void };
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function createRedisService() {
  const values = new Map<string, string>();
  const commands: Array<{ script: string; options: { keys: string[]; arguments: string[] } }> = [];
  let nextDrainRead: Promise<string | null> | undefined;
  const evaluate = async (script: string, options: { keys: string[]; arguments: string[] }) => {
    commands.push({ script, options });
    const [ownerKey, liveKey] = options.keys;
    if (script.includes("return redis.call('DEL', KEYS[1])")) {
      if (values.get(ownerKey) === options.arguments[0]) {
        values.delete(ownerKey);
        return 1;
      }
      return 0;
    }
    if (script.includes('if live and live ~= ARGV[2] then return 0 end')) {
      if (
        (values.has(ownerKey) && values.get(ownerKey) !== options.arguments[0]) ||
        (values.has(liveKey) && values.get(liveKey) !== options.arguments[1])
      )
        return 0;
      values.set(liveKey, options.arguments[1]);
      return 1;
    }
    throw new Error('unexpected Redis EVAL');
  };
  const client = {
    on() {},
    async connect() {},
    async close() {},
    async set(key: string, value: string, options?: { NX?: boolean }) {
      if (!options?.NX || !values.has(key)) values.set(key, value);
      return 'OK';
    },
    async incr(key: string) {
      const value = Number(values.get(key) ?? 0) + 1;
      values.set(key, String(value));
      return value;
    },
    async get(key: string) {
      if (key.startsWith('media:colour_draining:') && nextDrainRead) {
        const read = nextDrainRead;
        nextDrainRead = undefined;
        return read;
      }
      return values.get(key) ?? null;
    },
    async mGet(keys: string[]) {
      return keys.map((key) => values.get(key) ?? null);
    },
    eval: evaluate,
    async sMembers() {
      return [];
    },
    async del(key: string) {
      values.delete(key);
    },
    multi() {
      const pending: Array<[string, { keys: string[]; arguments: string[] }]> = [];
      return {
        eval(script: string, options: { keys: string[]; arguments: string[] }) {
          pending.push([script, options]);
          return this;
        },
        async exec() {
          return Promise.all(pending.map(([script, options]) => evaluate(script, options)));
        },
      };
    },
  };

  class RoomCreationRefusedError extends Error {}
  const redisSource = readFileSync(join(mediaRoot, 'src/lib/redis.ts'), 'utf8');
  const compiled = ts.transpileModule(redisSource, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText;
  const module = { exports: {} as Record<string, unknown> };
  new Function('require', 'module', 'exports', compiled)(
    (id: string) => {
      if (id === 'redis') return { createClient: () => client };
      if (id.endsWith('/config/index.js'))
        return { config: { redisUrl: 'redis://unused', roomPlacementEnabled: true } };
      if (id.endsWith('/logger.js'))
        return { logger: { info() {}, warn() {}, error() {}, debug() {} } };
      if (id.endsWith('/roomManager.js')) return { RoomCreationRefusedError };
      return require(id);
    },
    module,
    module.exports
  );

  return {
    service: new (
      module.exports.RedisService as new () => {
        connect(): Promise<void>;
        initializeMediaInstance(color: 'blue'): Promise<{ instanceId: string }>;
        refreshAcceptingNewRooms(): Promise<boolean>;
        heartbeatRoomOccupancy(roomIds: readonly string[]): Promise<void>;
        createRoomEventHandler(): (event: { type: 'room-empty'; roomId: string }) => void;
        close(): Promise<void>;
      }
    )(),
    commands,
    values,
    setNextDrainRead(read: Promise<string | null>) {
      nextDrainRead = read;
    },
  };
}

function productionHeartbeatCallback(
  roomManager: {
    getRoomIds(): string[];
    getRoom(roomId: string): { participants: Map<string, unknown> } | undefined;
  },
  redisService: {
    refreshAcceptingNewRooms(): Promise<boolean>;
    heartbeatRoomOccupancy(ids: string[]): Promise<void>;
  },
  mediasoupService: { demoteIdleWorkersToInitial(): Promise<void> },
  natsService: {
    publishRoomHeartbeat(payload: unknown): void;
    nextVoiceLifecycleTimestamp(): string;
  }
) {
  const source = readFileSync(join(mediaRoot, 'src/index.ts'), 'utf8');
  const start = source.indexOf('  const roomHeartbeatInterval = setInterval(async () => {');
  const bodyStart = source.indexOf('\n', start) + 1;
  const end = source.indexOf('    // #1553 measurement:', bodyStart);
  if (start < 0 || end < bodyStart)
    throw new Error('room heartbeat callback source boundary changed');
  const callback = new Function(
    'roomManager',
    'redisService',
    'config',
    'mediasoupService',
    'natsService',
    `return async () => {\n${source.slice(bodyStart, end)}\n}`
  );
  return callback(
    roomManager,
    redisService,
    { roomPlacementEnabled: true },
    mediasoupService,
    natsService
  ) as () => Promise<void>;
}

describe('room occupancy heartbeat', () => {
  it.each(['drain refresh', 'worker demotion'] as const)(
    'does not resurrect a room closed during %s and still refreshes a retained room',
    async (blockedAt) => {
      const redis = createRedisService();
      try {
        await redis.service.connect();
        const identity = await redis.service.initializeMediaInstance('blue');
        const roomId = '550e8400-e29b-41d4-a716-446655440001';
        const liveKey = `voice:room_live:${roomId}`;
        redis.values.set(`voice:room_owner:${roomId}`, 'blue');
        redis.values.set(liveKey, identity.instanceId);
        const rooms = new Map([[roomId, { participants: new Map([['user-1', {}]]) }]]);
        const publications: unknown[] = [];
        const roomManager = {
          getRoomIds: () => [...rooms.keys()],
          getRoom: (id: string) => rooms.get(id),
        };
        const demotion = deferred<void>();
        const drainRead = deferred<string | null>();
        const mediasoupService = {
          demoteIdleWorkersToInitial: vi.fn(() => demotion.promise),
        };
        if (blockedAt === 'drain refresh') redis.setNextDrainRead(drainRead.promise);
        else redis.values.set('media:colour_draining:blue', 'armed');

        const callback = productionHeartbeatCallback(roomManager, redis.service, mediasoupService, {
          publishRoomHeartbeat: (payload) => publications.push(payload),
          nextVoiceLifecycleTimestamp: () => '2026-10-18T12:00:00.000000Z',
        });
        const tick = callback();
        if (blockedAt === 'worker demotion') {
          await vi.waitFor(() =>
            expect(mediasoupService.demoteIdleWorkersToInitial).toHaveBeenCalledOnce()
          );
        } else {
          await Promise.resolve();
        }

        rooms.delete(roomId);
        redis.service.createRoomEventHandler()({ type: 'room-empty', roomId });
        await vi.waitFor(() => expect(redis.values.has(liveKey)).toBe(false));
        const commandsBeforeResume = redis.commands.length;
        if (blockedAt === 'drain refresh') drainRead.resolve('armed');
        else demotion.resolve();
        if (blockedAt === 'drain refresh') {
          await vi.waitFor(() =>
            expect(mediasoupService.demoteIdleWorkersToInitial).toHaveBeenCalledOnce()
          );
          demotion.resolve();
        }
        await tick;

        const refreshes = redis.commands
          .slice(commandsBeforeResume)
          .filter(({ script }) => script.includes('if live and live ~= ARGV[2] then return 0 end'));
        expect.soft(refreshes).toHaveLength(0);
        expect.soft(redis.values.has(liveKey)).toBe(false);
        expect.soft(publications).toHaveLength(0);

        rooms.set(roomId, { participants: new Map([['user-2', {}]]) });
        const retainedCommandsStart = redis.commands.length;
        await callback();
        const retainedRefreshes = redis.commands
          .slice(retainedCommandsStart)
          .filter(({ script }) => script.includes('if live and live ~= ARGV[2] then return 0 end'));
        expect(retainedRefreshes).toHaveLength(1);
        expect(redis.values.get(liveKey)).toBe(identity.instanceId);
        expect(publications).toHaveLength(1);
      } finally {
        await redis.service.close();
      }
    }
  );
});
