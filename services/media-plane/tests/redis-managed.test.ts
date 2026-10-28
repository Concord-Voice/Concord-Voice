import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';

const redisMocks = vi.hoisted(() => ({ createClient: vi.fn() }));
vi.mock('redis', () => ({ createClient: redisMocks.createClient }));
vi.mock('../src/lib/logger.js', () => ({
  logger: { error: vi.fn(), info: vi.fn(), warn: vi.fn(), debug: vi.fn() },
}));

vi.mock('@/config/index.js', () => ({
  config: {
    redisCredentialMode: 'managed',
    redisEndpoint: 'redis.internal:6379',
    redisTls: false,
    redisTlsCaFile: undefined,
    redisInfisicalUrl: 'https://infisical.synthetic.test',
    redisInfisicalProjectId: 'synthetic-project',
    redisInfisicalEnvironment: 'test',
    redisInfisicalPath: '/synthetic/redis',
    redisInfisicalClientId: 'synthetic-client-id',
    redisUrl: 'redis://:legacy-fixture@redis:6379',
    roomPlacementEnabled: true,
  },
}));

import { parseManagedCredential } from '../src/lib/redis.js';
import { RedisService } from '../src/lib/redis.js';
import { config } from '@/config/index.js';
import { hostname } from 'node:os';
import { logger } from '../src/lib/logger.js';

type FakeClient = {
  isReady: boolean;
  on: ReturnType<typeof vi.fn>;
  connect: ReturnType<typeof vi.fn>;
  ping: ReturnType<typeof vi.fn>;
  aclWhoAmI: ReturnType<typeof vi.fn>;
  close: ReturnType<typeof vi.fn>;
  destroy: ReturnType<typeof vi.fn>;
  multi: ReturnType<typeof vi.fn>;
  sAdd: ReturnType<typeof vi.fn>;
  sRem: ReturnType<typeof vi.fn>;
  expire: ReturnType<typeof vi.fn>;
  hSet: ReturnType<typeof vi.fn>;
  hGet: ReturnType<typeof vi.fn>;
  sMembers: ReturnType<typeof vi.fn>;
  mGet: ReturnType<typeof vi.fn>;
  get: ReturnType<typeof vi.fn>;
  set: ReturnType<typeof vi.fn>;
  incr: ReturnType<typeof vi.fn>;
  del: ReturnType<typeof vi.fn>;
  eval: ReturnType<typeof vi.fn>;
};

let defaultRedisClient: FakeClient;

function fakeClient(): FakeClient {
  const transaction = {
    sAdd: vi.fn().mockReturnThis(),
    sRem: vi.fn().mockReturnThis(),
    expire: vi.fn().mockReturnThis(),
    hSet: vi.fn().mockReturnThis(),
    del: vi.fn().mockReturnThis(),
    eval: vi.fn().mockReturnThis(),
    exec: vi.fn().mockResolvedValue([]),
  };
  return {
    isReady: true,
    on: vi.fn().mockReturnThis(),
    connect: vi.fn().mockResolvedValue(undefined),
    ping: vi.fn().mockResolvedValue('PONG'),
    aclWhoAmI: vi.fn().mockResolvedValue('cv_mp_12'),
    close: vi.fn().mockResolvedValue(undefined),
    destroy: vi.fn(),
    multi: vi.fn(() => transaction),
    sAdd: transaction.sAdd,
    sRem: transaction.sRem,
    expire: transaction.expire,
    hSet: transaction.hSet,
    hGet: vi.fn().mockResolvedValue(null),
    sMembers: vi.fn().mockResolvedValue([]),
    mGet: vi.fn().mockResolvedValue([null, null, null]),
    get: vi.fn().mockResolvedValue(null),
    set: vi.fn().mockResolvedValue('OK'),
    incr: vi.fn().mockResolvedValue(1),
    del: vi.fn().mockResolvedValue(0),
    eval: vi.fn().mockResolvedValue(1),
  };
}

beforeEach(() => {
  Object.assign(config, {
    redisCredentialMode: 'managed',
    redisEndpoint: 'redis.internal:6379',
    redisTls: false,
    redisTlsCaFile: '',
    redisInfisicalUrl: 'https://infisical.synthetic.test',
    redisInfisicalProjectId: scope.projectId,
    redisInfisicalEnvironment: scope.environment,
    redisInfisicalPath: scope.path,
    redisInfisicalClientId: 'synthetic-client-id',
  });
  defaultRedisClient = fakeClient();
  redisMocks.createClient.mockReturnValue(defaultRedisClient);
});

const now = new Date('2026-10-17T12:00:00.000Z');
const scope = {
  projectId: 'synthetic-project',
  environment: 'test',
  path: '/synthetic/redis',
  name: 'REDIS_MEDIA_PLANE_CREDENTIAL',
};

function credential(overrides: Record<string, unknown> = {}): string {
  return JSON.stringify({
    version: 1,
    scope,
    rotationId: 'rotation-test-12',
    service: 'media-plane',
    generation: 12,
    username: 'cv_mp_12',
    password: 'synthetic-password-only', // pragma: allowlist secret -- test fixture
    validFrom: '2026-10-17T11:00:00.000Z',
    validUntil: '2026-10-17T13:00:00.000Z',
    ...overrides,
  });
}

describe('parseManagedCredential', () => {
  beforeEach(() => {
    expect(typeof parseManagedCredential).toBe('function');
  });

  it('accepts the exact media credential contract and retains its rotation ID', () => {
    expect(parseManagedCredential(credential(), scope, now)).toEqual({
      version: 1,
      scope,
      rotationId: 'rotation-test-12',
      service: 'media-plane',
      generation: 12,
      username: 'cv_mp_12',
      password: 'synthetic-password-only', // pragma: allowlist secret -- test fixture
      validFrom: '2026-10-17T11:00:00.000Z',
      validUntil: '2026-10-17T13:00:00.000Z',
    });
  });

  it('rejects an ACL-safe username that is not bound to the credential generation', () => {
    expect(() =>
      parseManagedCredential(credential({ username: 'redis_media_blue' }), scope, now)
    ).toThrow('managed_redis_credential_invalid');
  });

  it('accepts a shared-contract password above 4096 bytes within the bounded document', () => {
    const password = 'p'.repeat(5000);
    expect(parseManagedCredential(credential({ password }), scope, now).password).toBe(password);
    expect(() =>
      parseManagedCredential(credential({ password: 'p'.repeat(16_384) }), scope, now)
    ).toThrow();
  });

  it.each([
    ['wrong project', { scope: { ...scope, projectId: 'other-project' } }],
    ['wrong environment', { scope: { ...scope, environment: 'prod' } }],
    ['wrong path', { scope: { ...scope, path: '/other/path' } }],
    ['wrong name', { scope: { ...scope, name: 'REDIS_CONTROL_PLANE_CREDENTIAL' } }],
  ])('rejects %s even when the provider response has no scope metadata', (_case, override) => {
    expect(() => parseManagedCredential(credential(override), scope, now)).toThrow();
  });

  it.each([
    ['unknown top-level field', credential({ extra: 'ignored-by-JSON.parse' })],
    ['wrong service', credential({ service: 'control-plane' })],
    ['wrong version', credential({ version: 2 })],
    ['zero generation', credential({ generation: 0 })],
    ['non-integer generation', credential({ generation: 1.5 })],
    ['empty password', credential({ password: '' })],
    ['unsafe ACL username', credential({ username: 'cv mp 12' })],
    ['reserved ACL username', credential({ username: 'default' })],
    ['invalid rotation ID', credential({ rotationId: 'bad/id' })],
    ['future validFrom', credential({ validFrom: '2026-10-17T12:00:01.000Z' })],
    ['expired validUntil', credential({ validUntil: '2026-10-17T11:59:59.000Z' })],
    ['invalid timestamp', credential({ validUntil: 'tomorrow' })],
  ])('rejects %s', (_case, raw) => {
    expect(() => parseManagedCredential(raw, scope, now)).toThrow();
  });

  it('rejects duplicate keys, including escaped aliases', () => {
    const raw = credential().replace('"generation":12', '"generation":11,"genera\\u0074ion":12');
    expect(() => parseManagedCredential(raw, scope, now)).toThrow();
  });

  it('rejects unknown fields nested inside scope', () => {
    const raw = credential({ scope: { ...scope, namespace: 'extra' } });
    expect(() => parseManagedCredential(raw, scope, now)).toThrow();
  });

  it('does not include raw credential material in validation errors', () => {
    const raw = credential({
      password: 'synthetic-secret-that-must-not-leak', // pragma: allowlist secret -- test fixture
      service: 'control-plane',
    });
    try {
      parseManagedCredential(raw, scope, now);
      throw new Error('expected parser to reject the wrong service');
    } catch (error) {
      expect(String(error)).not.toContain('synthetic-secret-that-must-not-leak');
    }
  });
});

describe('RedisService managed credential lifecycle', () => {
  const first = credential();
  const next = credential({
    rotationId: 'rotation-test-13',
    generation: 13,
    username: 'cv_mp_13',
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('probes managed Redis readiness without room placement identity', async () => {
    const client = fakeClient();
    const service = new RedisService({
      readExact: vi.fn().mockResolvedValue({ value: first, returnedName: scope.name }),
      createManagedClient: vi.fn(() => client),
      acknowledge: vi.fn().mockResolvedValue(undefined),
      now: () => now,
    });

    await service.connect();
    client.ping.mockRejectedValueOnce(new Error('synthetic command failure'));
    await service.refreshAcceptingNewRooms();
    expect(service.healthState().redisReachable).toBe(false);

    await service.refreshAcceptingNewRooms();
    expect(service.healthState().redisReachable).toBe(true);
  });

  it('makes readiness red after a Redis room-state authorization failure', async () => {
    const client = fakeClient();
    const service = new RedisService({
      readExact: vi.fn().mockResolvedValue({ value: first, returnedName: scope.name }),
      createManagedClient: vi.fn(() => client),
      acknowledge: vi.fn().mockResolvedValue(undefined),
      now: () => now,
    });
    await service.connect();
    client.multi().exec.mockRejectedValueOnce(new Error('READONLY synthetic failure'));

    await service.addParticipant('channel-1', 'user-1');

    expect(service.healthState().redisReachable).toBe(false);
    await service.addParticipant('channel-1', 'user-1');
    expect(service.healthState().redisReachable).toBe(true);
  });

  it('authenticates a fresh candidate before publishing the applied ack', async () => {
    const candidate = fakeClient();
    const events: string[] = [];
    const readExact = vi.fn().mockResolvedValue({ value: first, returnedName: scope.name });
    const acknowledge = vi.fn(async (ack: Record<string, unknown>) => {
      events.push('ack');
      expect(candidate.connect).toHaveBeenCalledOnce();
      expect(candidate.ping).toHaveBeenCalledOnce();
      expect(ack).toEqual({
        version: 1,
        rotationId: 'rotation-test-12',
        service: 'media-plane',
        instanceId: hostname(),
        generation: 12,
        username: 'cv_mp_12',
      });
    });
    const service = new RedisService({
      readExact,
      createManagedClient: vi.fn(() => candidate),
      acknowledge,
      now: () => now,
    });

    await service.connect();
    events.push('connected');
    await service.addParticipant('channel-1', 'user-1');

    expect(readExact).toHaveBeenCalledWith(scope);
    expect(candidate.ping).toHaveBeenCalledOnce();
    expect(service.appliedGeneration).toBe(12);
    expect(acknowledge).toHaveBeenCalledOnce();
    expect(events).toEqual(['ack', 'connected']);
    expect(candidate.multi).toHaveBeenCalledOnce();
  });

  it('does not promote or acknowledge when fresh candidate authentication fails', async () => {
    const active = fakeClient();
    const rejected = fakeClient();
    rejected.connect.mockRejectedValueOnce(new Error('synthetic auth detail'));
    let reads = 0;
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    const service = new RedisService({
      readExact: vi.fn(async () => ({ value: reads++ === 0 ? first : next })),
      createManagedClient: vi.fn().mockReturnValueOnce(active).mockReturnValueOnce(rejected),
      acknowledge,
      now: () => now,
    });

    await service.connect();
    await expect(service.refreshManagedCredential()).rejects.toThrow();
    await service.addParticipant('channel-1', 'user-1');

    expect(service.appliedGeneration).toBe(12);
    expect(acknowledge).toHaveBeenCalledOnce();
    expect(active.close).not.toHaveBeenCalled();
    expect(active.multi).toHaveBeenCalledOnce();
    expect(rejected.ping).not.toHaveBeenCalled();
  });

  it('bounds an injected credential reader by the 15-second provider fetch budget', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'performance'] });
    const createManagedClient = vi.fn(() => fakeClient());
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    const service = new RedisService({
      readExact: vi.fn(() => new Promise<{ value: string; returnedName?: string }>(() => {})),
      createManagedClient,
      acknowledge,
      now: () => now,
    });

    const connecting = service.connect();
    const rejected = expect(connecting).rejects.toThrow('managed_redis_cold_start_failed');
    await vi.advanceTimersByTimeAsync(14_999);
    expect(createManagedClient).not.toHaveBeenCalled();
    expect(acknowledge).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await rejected;

    expect(createManagedClient).not.toHaveBeenCalled();
    expect(acknowledge).not.toHaveBeenCalled();
  });

  it('uses one 10-second budget for candidate connect and PING', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'performance'] });
    const candidate = fakeClient();
    let finishConnect!: () => void;
    candidate.connect.mockImplementationOnce(
      () => new Promise<void>((resolve) => (finishConnect = resolve))
    );
    candidate.ping.mockImplementationOnce(() => new Promise<string>(() => {}));
    const service = new RedisService({
      readExact: vi.fn().mockResolvedValue({ value: first, returnedName: scope.name }),
      createManagedClient: vi.fn(() => candidate),
      acknowledge: vi.fn().mockResolvedValue(undefined),
      now: () => now,
    });

    const connecting = service.connect();
    const rejected = expect(connecting).rejects.toThrow('managed_redis_cold_start_failed');
    await vi.advanceTimersByTimeAsync(6_000);
    finishConnect();
    await vi.advanceTimersByTimeAsync(0);
    expect(candidate.ping).toHaveBeenCalledOnce();

    await vi.advanceTimersByTimeAsync(3_999);
    expect(candidate.destroy).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await rejected;

    expect(candidate.destroy).toHaveBeenCalledOnce();
    expect(candidate.aclWhoAmI).not.toHaveBeenCalled();
  });

  it('does not grant ACL WHOAMI a second 10-second budget or acknowledge the candidate', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'performance'] });
    const candidate = fakeClient();
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    let finishConnect!: () => void;
    candidate.connect.mockImplementationOnce(
      () => new Promise<void>((resolve) => (finishConnect = resolve))
    );
    candidate.aclWhoAmI.mockImplementationOnce(() => new Promise<string>(() => {}));
    const service = new RedisService({
      readExact: vi.fn().mockResolvedValue({ value: first, returnedName: scope.name }),
      createManagedClient: vi.fn(() => candidate),
      acknowledge,
      now: () => now,
    });

    const connecting = service.connect();
    const rejected = expect(connecting).rejects.toThrow('managed_redis_cold_start_failed');
    await vi.advanceTimersByTimeAsync(6_000);
    finishConnect();
    await vi.advanceTimersByTimeAsync(0);
    expect(candidate.aclWhoAmI).toHaveBeenCalledOnce();

    await vi.advanceTimersByTimeAsync(3_999);
    expect(acknowledge).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await rejected;

    expect(candidate.destroy).toHaveBeenCalledOnce();
    expect(acknowledge).not.toHaveBeenCalled();
  });

  it('rejects a candidate whose authenticated Redis username differs from the target', async () => {
    const candidate = fakeClient();
    candidate.aclWhoAmI.mockResolvedValue('cv_mp_11');
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    const service = new RedisService({
      readExact: vi.fn(async () => ({ value: first })),
      createManagedClient: vi.fn(() => candidate),
      acknowledge,
      now: () => now,
    });

    await expect(service.connect()).rejects.toThrow('managed_redis_cold_start_failed');
    expect(candidate.aclWhoAmI).toHaveBeenCalledOnce();
    expect(acknowledge).not.toHaveBeenCalled();
  });

  it('does not ACK when the applied Redis connection reports a different username', async () => {
    const candidate = fakeClient();
    candidate.aclWhoAmI.mockResolvedValueOnce('cv_mp_12').mockResolvedValueOnce('cv_mp_11');
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    const service = new RedisService({
      readExact: vi.fn(async () => ({ value: first })),
      createManagedClient: vi.fn(() => candidate),
      acknowledge,
      now: () => now,
    });

    await expect(service.connect()).rejects.toThrow('managed_redis_cold_start_failed');
    expect(candidate.aclWhoAmI).toHaveBeenCalledTimes(2);
    expect(acknowledge).not.toHaveBeenCalled();
  });

  it('does not ACK when the credential expires during the applied identity check', async () => {
    let current = now;
    const candidate = fakeClient();
    candidate.aclWhoAmI.mockResolvedValueOnce('cv_mp_12').mockImplementationOnce(async () => {
      current = new Date(now.getTime() + 2_000);
      return 'cv_mp_12';
    });
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    const service = new RedisService({
      readExact: vi.fn(async () => ({
        value: credential({ validUntil: '2026-10-17T12:00:01.000Z' }),
      })),
      createManagedClient: vi.fn(() => candidate),
      acknowledge,
      now: () => current,
    });

    await expect(service.connect()).rejects.toThrow('managed_redis_cold_start_failed');
    expect(candidate.aclWhoAmI).toHaveBeenCalledTimes(2);
    expect(acknowledge).not.toHaveBeenCalled();
  });

  it('discards a candidate that expires while authentication is pending', async () => {
    const elapsed = 2_000;
    const validUntil = '2026-10-17T12:00:01.000Z';
    let current = now;
    const active = fakeClient();
    const candidate = fakeClient();
    let finishPing!: (value: string) => void;
    candidate.ping.mockImplementationOnce(
      () => new Promise<string>((resolve) => (finishPing = resolve))
    );
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    let reads = 0;
    const service = new RedisService({
      readExact: vi.fn(async () => ({
        value:
          reads++ === 0
            ? first
            : credential({
                rotationId: 'rotation-test-13',
                generation: 13,
                username: 'cv_mp_13',
                validUntil,
              }),
      })),
      createManagedClient: vi.fn().mockReturnValueOnce(active).mockReturnValueOnce(candidate),
      acknowledge,
      now: () => current,
    });

    await service.connect();
    const refresh = service.refreshManagedCredential();
    while (candidate.ping.mock.calls.length === 0)
      await new Promise((resolve) => setTimeout(resolve, 0));
    current = new Date(now.getTime() + elapsed);
    finishPing('PONG');
    await expect(refresh).rejects.toThrow();
    await service.addParticipant('still-active', 'user-1');

    expect(service.appliedGeneration).toBe(12);
    expect(acknowledge).toHaveBeenCalledTimes(1);
    expect(candidate.destroy).toHaveBeenCalledOnce();
    expect(active.close).not.toHaveBeenCalled();
    expect(active.multi).toHaveBeenCalledOnce();
    expect(candidate.multi).not.toHaveBeenCalled();
  });

  it('switches the active client before acknowledging and closes the previous client', async () => {
    const active = fakeClient();
    const candidate = fakeClient();
    candidate.aclWhoAmI.mockResolvedValue('cv_mp_13');
    let reads = 0;
    const acknowledge = vi.fn(async () => {
      if (service.appliedGeneration === 13) {
        expect(active.close).toHaveBeenCalledOnce();
        await service.addParticipant('after-switch', 'user-1');
        expect(candidate.multi).toHaveBeenCalledOnce();
        expect(active.multi).not.toHaveBeenCalled();
      }
    });
    const service = new RedisService({
      readExact: vi.fn(async () => ({ value: reads++ === 0 ? first : next })),
      createManagedClient: vi.fn().mockReturnValueOnce(active).mockReturnValueOnce(candidate),
      acknowledge,
      now: () => now,
    });

    await service.connect();
    await service.refreshManagedCredential();

    expect(service.appliedGeneration).toBe(13);
    expect(acknowledge).toHaveBeenCalledTimes(2);
    expect(candidate.connect).toHaveBeenCalledOnce();
    expect(candidate.ping).toHaveBeenCalledOnce();
  });

  it('rejects a lower generation while keeping the current client active', async () => {
    const initial = fakeClient();
    const active = fakeClient();
    active.aclWhoAmI.mockResolvedValue('cv_mp_13');
    let reads = 0;
    const service = new RedisService({
      readExact: vi.fn(async () => ({ value: [first, next, first][reads++] })),
      createManagedClient: vi.fn().mockReturnValueOnce(initial).mockReturnValueOnce(active),
      acknowledge: vi.fn().mockResolvedValue(undefined),
      now: () => now,
    });

    await service.connect();
    await service.refreshManagedCredential();
    await expect(service.refreshManagedCredential()).rejects.toThrow(
      'managed_redis_generation_replay'
    );
    await service.addParticipant('still-active', 'user-1');

    expect(service.appliedGeneration).toBe(13);
    expect(active.multi).toHaveBeenCalledOnce();
    expect(initial.multi).not.toHaveBeenCalled();
    expect(active.connect).toHaveBeenCalledOnce();
  });

  it('destroys a candidate that finishes authentication after service close', async () => {
    const active = fakeClient();
    const candidate = fakeClient();
    candidate.aclWhoAmI.mockResolvedValue('cv_mp_13');
    let finishPing!: (value: string) => void;
    candidate.ping.mockImplementationOnce(
      () => new Promise<string>((resolve) => (finishPing = resolve))
    );
    let reads = 0;
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    const service = new RedisService({
      readExact: vi.fn(async () => ({ value: reads++ === 0 ? first : next })),
      createManagedClient: vi.fn().mockReturnValueOnce(active).mockReturnValueOnce(candidate),
      acknowledge,
      now: () => now,
    });

    await service.connect();
    const refresh = service.refreshManagedCredential();
    while (candidate.ping.mock.calls.length === 0)
      await new Promise((resolve) => setTimeout(resolve, 0));
    await service.close();
    finishPing('PONG');
    await expect(refresh).rejects.toThrow('managed_redis_closed');

    expect(candidate.destroy).toHaveBeenCalledOnce();
    expect(candidate.close).not.toHaveBeenCalled();
    expect(service.appliedGeneration).toBe(12);
    expect(acknowledge).toHaveBeenCalledOnce();
  });

  it('refuses an equal-generation credential whose contents changed', async () => {
    const active = fakeClient();
    const changed = fakeClient();
    let reads = 0;
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    const service = new RedisService({
      readExact: vi.fn(async () => ({
        value: reads++ === 0 ? first : credential({ password: 'different-synthetic-password' }),
      })),
      createManagedClient: vi.fn().mockReturnValueOnce(active).mockReturnValueOnce(changed),
      acknowledge,
      now: () => now,
    });

    await service.connect();
    await expect(service.refreshManagedCredential()).rejects.toThrow();

    expect(service.appliedGeneration).toBe(12);
    expect(acknowledge).toHaveBeenCalledOnce();
    expect(active.close).not.toHaveBeenCalled();
    expect(changed.connect).not.toHaveBeenCalled();
  });

  it('renews freshness for equivalent validity instants but rejects a nanosecond change', async () => {
    let current = now;
    const active = fakeClient();
    const equivalent = credential({
      validFrom: '2026-10-17T11:00:00+00:00',
      validUntil: '2026-10-17T15:00:00+02:00',
    });
    const changed = credential({ validUntil: '2026-10-17T13:00:00.000000001Z' });
    let reads = 0;
    const service = new RedisService({
      readExact: vi.fn(async () => ({ value: [first, equivalent, changed][reads++] })),
      createManagedClient: vi.fn(() => active),
      acknowledge: vi.fn().mockResolvedValue(undefined),
      now: () => current,
    });

    await service.connect();
    current = new Date(now.getTime() + 60_000);
    await service.refreshManagedCredential();
    current = new Date(now.getTime() + 120_001);
    await service.addParticipant('still-fresh', 'user-1');
    expect(active.multi).toHaveBeenCalledOnce();
    await expect(service.refreshManagedCredential()).rejects.toThrow(
      'managed_redis_generation_changed'
    );
    expect(active.close).not.toHaveBeenCalled();
  });

  it('rejects every Redis command family once the credential reaches validUntil', async () => {
    let current = now;
    const client = fakeClient();
    const service = new RedisService({
      readExact: vi.fn().mockResolvedValue({ value: first }),
      createManagedClient: vi.fn(() => client),
      acknowledge: vi.fn().mockResolvedValue(undefined),
      now: () => current,
    });
    await service.connect();
    await service.initializeMediaInstance('blue');
    expect(client.connect).toHaveBeenCalledOnce();
    expect(client.set).toHaveBeenCalled();
    expect(client.incr).toHaveBeenCalled();
    vi.clearAllMocks();
    current = new Date('2026-10-17T13:00:00.000Z');

    await service.initializeMediaInstance('blue').catch(() => undefined); // set/incr
    await service.guardRoomCreation('room-1').catch(() => undefined); // mGet
    await service.heartbeatRoomOccupancy(['room-1']); // transaction/eval
    await service.addParticipant('room-1', 'user-1'); // set/hash/expiry transaction
    await service.removeParticipant('room-1', 'user-1'); // set/delete transaction
    await service.clearRoom('room-1'); // set lookup/delete
    await service.getRoomParticipants('room-1'); // set read
    await service.getUserRoom('user-1'); // hash read
    await expect(
      service.createRoomEventHandler()({ type: 'room-created', roomId: 'room-1' })
    ).rejects.toThrow(); // eval

    expect(client.set).not.toHaveBeenCalled();
    expect(client.incr).not.toHaveBeenCalled();
    expect(client.mGet).not.toHaveBeenCalled();
    expect(client.multi).not.toHaveBeenCalled();
    expect(client.sMembers).not.toHaveBeenCalled();
    expect(client.hGet).not.toHaveBeenCalled();
    expect(client.get).not.toHaveBeenCalled();
    expect(client.eval).not.toHaveBeenCalled();
    expect(client.del).not.toHaveBeenCalled();
  });

  it.each([
    ['just before validUntil', '2026-10-17T12:00:01.000Z', 999, true],
    ['at validUntil', '2026-10-17T12:00:01.000Z', 1_000, false],
    ['past the 120-second stale age', '2026-10-17T13:00:00.000Z', 120_001, true],
  ])(
    'rechecks the managed credential %s before executing a built room-live batch',
    async (_case, validUntil, elapsed, shouldExecute) => {
      let current = now;
      const client = fakeClient();
      const transaction = client.multi();
      transaction.eval.mockImplementationOnce(() => {
        current = new Date(now.getTime() + elapsed);
        return transaction;
      });
      const service = new RedisService({
        readExact: vi.fn().mockResolvedValue({ value: credential({ validUntil }) }),
        createManagedClient: vi.fn(() => client),
        acknowledge: vi.fn().mockResolvedValue(undefined),
        now: () => current,
      });

      await service.connect();
      await service.initializeMediaInstance('blue');
      await service.heartbeatRoomOccupancy(['room-1']);

      expect(transaction.eval).toHaveBeenCalledOnce();
      expect(transaction.exec).toHaveBeenCalledTimes(shouldExecute ? 1 : 0);
    }
  );

  it('fails managed cold startup when the exact reader is unavailable', async () => {
    const service = new RedisService({
      readExact: vi.fn().mockRejectedValue(new Error('synthetic provider failure')),
      createManagedClient: vi.fn(() => fakeClient()),
      acknowledge: vi.fn().mockResolvedValue(undefined),
      now: () => now,
    });

    await expect(service.connect()).rejects.toThrow();
  });

  const staleAlerts = (): number =>
    vi
      .mocked(logger.error)
      .mock.calls.filter(
        ([, meta]) =>
          (meta as { code?: string } | undefined)?.code === 'managed_redis_credential_stale'
      ).length;

  it('alerts on every failed poll from 120 s and keeps serving until validUntil', async () => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    let current = now;
    const active = fakeClient();
    let reads = 0;
    const service = new RedisService({
      readExact: vi.fn(async () => {
        reads += 1;
        if (reads === 1) return { value: first };
        throw new Error('synthetic provider outage');
      }),
      createManagedClient: vi.fn(() => active),
      acknowledge: vi.fn().mockResolvedValue(undefined),
      now: () => current,
    });
    const pollAt = async (elapsedMs: number) => {
      current = new Date(now.getTime() + elapsedMs);
      await vi.advanceTimersByTimeAsync(30_000);
    };

    try {
      await service.connect();
      await pollAt(119_999);
      expect(staleAlerts()).toBe(0);
      await pollAt(120_000);
      expect(staleAlerts()).toBe(1);
      expect(logger.warn).not.toHaveBeenCalled();
      await service.getRoomParticipants('room-1');
      expect(active.sMembers).toHaveBeenCalledOnce();

      // validUntil is 13:00, one hour after the fetch: serving stops, the alert does not.
      await pollAt(3_600_000);
      expect(staleAlerts()).toBe(2);
      active.sMembers.mockClear();
      await service.getRoomParticipants('room-1');
      expect(active.sMembers).not.toHaveBeenCalled();
    } finally {
      await service.close();
      vi.useRealTimers();
    }
  });

  it('measures staleness on the monotonic clock when no clock is injected', async () => {
    vi.clearAllMocks();
    vi.useFakeTimers({ now });
    let reads = 0;
    const service = new RedisService({
      readExact: vi.fn(async () => {
        reads += 1;
        if (reads === 1) return { value: first };
        throw new Error('synthetic provider outage');
      }),
      createManagedClient: vi.fn(() => fakeClient()),
      acknowledge: vi.fn().mockResolvedValue(undefined),
    });

    try {
      await service.connect();
      await vi.advanceTimersByTimeAsync(90_000);
      expect(staleAlerts()).toBe(0);
      await vi.advanceTimersByTimeAsync(30_000);
      expect(staleAlerts()).toBe(1);
    } finally {
      await service.close();
      vi.useRealTimers();
    }
  });

  it('does not report the provider stale while Redis is down and the provider answers', async () => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    let current = now;
    const active = fakeClient();
    const candidate = fakeClient();
    candidate.connect.mockRejectedValue(new Error('ECONNREFUSED'));
    const service = new RedisService({
      readExact: vi.fn().mockResolvedValue({ value: first, returnedName: scope.name }),
      createManagedClient: vi.fn().mockReturnValueOnce(active).mockReturnValue(candidate),
      acknowledge: vi.fn().mockResolvedValue(undefined),
      now: () => current,
    });

    try {
      await service.connect();
      active.isReady = false;
      for (let poll = 1; poll <= 5; poll += 1) {
        current = new Date(now.getTime() + poll * 30_000);
        await vi.advanceTimersByTimeAsync(30_000);
      }
      expect(staleAlerts()).toBe(0);
    } finally {
      await service.close();
      vi.useRealTimers();
    }
  });

  it('rebuilds a retired client from the cached credential during a provider outage', async () => {
    const active = fakeClient();
    const replacement = fakeClient();
    const createManagedClient = vi
      .fn()
      .mockReturnValueOnce(active)
      .mockReturnValueOnce(replacement);
    const acknowledge = vi.fn().mockResolvedValue(undefined);
    const service = new RedisService({
      readExact: vi
        .fn()
        .mockResolvedValueOnce({ value: first, returnedName: scope.name })
        .mockRejectedValue(new Error('synthetic provider outage')),
      createManagedClient,
      acknowledge,
      now: () => now,
    });

    try {
      await service.connect();
      const terminated = active.on.mock.calls.find(([event]) => event === 'terminated')?.[1] as
        | (() => void)
        | undefined;
      terminated?.();

      expect(active.destroy).toHaveBeenCalledOnce();
      await vi.waitFor(() => expect(replacement.connect).toHaveBeenCalledOnce());
      await vi.waitFor(() => expect(service.healthState().acceptingNewRooms).toBe(true));
      await service.addParticipant('rebuilt-room', 'user-1');
      expect(replacement.multi).toHaveBeenCalled();
      expect(acknowledge).toHaveBeenCalledOnce();
    } finally {
      await service.close();
    }
  });

  it.each(['terminated', 'ready'] as const)(
    'stops reconnecting at validUntil and replaces the client on %s',
    async (event) => {
      let current = now;
      const active = fakeClient();
      const replacement = fakeClient();
      const createManagedClient = vi
        .fn()
        .mockReturnValueOnce(active)
        .mockReturnValueOnce(replacement);
      const service = new RedisService({
        readExact: vi
          .fn()
          .mockResolvedValueOnce({ value: first, returnedName: scope.name })
          .mockResolvedValue({
            value: credential({
              rotationId: 'rotation-test-13',
              generation: 13,
              username: 'cv_mp_13',
              validUntil: '2026-10-17T14:00:00.000Z',
            }),
            returnedName: scope.name,
          }),
        createManagedClient,
        acknowledge: vi.fn().mockResolvedValue(undefined),
        now: () => current,
      });
      replacement.aclWhoAmI.mockResolvedValue('cv_mp_13');
      await service.connect();
      const socket = createManagedClient.mock.calls[0]?.[0]?.socket as {
        reconnectStrategy: (retries: number) => number | false;
      };
      expect(socket.reconnectStrategy(1)).toBeGreaterThan(0);
      const handler = active.on.mock.calls.find(([registered]) => registered === event)?.[1] as
        | (() => void)
        | undefined;
      expect(handler).toBeTypeOf('function');

      // Past the 120 s stale age but before validUntil, the client keeps reconnecting.
      current = new Date(now.getTime() + 120_001);
      expect(socket.reconnectStrategy(1)).toBeGreaterThan(0);
      if (event === 'ready') {
        handler?.();
        expect(active.destroy).not.toHaveBeenCalled();
      }

      current = new Date('2026-10-17T13:00:00.000Z');
      expect(socket.reconnectStrategy(1)).toBe(false);
      handler?.();

      expect(active.destroy).toHaveBeenCalledOnce();
      await vi.waitFor(() => expect(replacement.connect).toHaveBeenCalledOnce());
      await service.addParticipant('replacement-room', 'user-1');
      expect(replacement.multi).toHaveBeenCalled();
    }
  );

  it('reprovisions a terminal managed client after a fresh provider read', async () => {
    const active = fakeClient();
    const replacement = fakeClient();
    const createManagedClient = vi
      .fn()
      .mockReturnValueOnce(active)
      .mockReturnValueOnce(replacement);
    const service = new RedisService({
      readExact: vi.fn().mockResolvedValue({ value: first, returnedName: scope.name }),
      createManagedClient,
      acknowledge: vi.fn().mockResolvedValue(undefined),
      now: () => now,
    });
    await service.connect();
    active.isReady = false;

    await service.refreshManagedCredential();

    expect(createManagedClient).toHaveBeenCalledTimes(2);
    expect(replacement.connect).toHaveBeenCalledOnce();
  });
});

describe('managed Redis readiness wiring', () => {
  it('requires fresh Redis readiness even when room placement is disabled', () => {
    const source = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8');
    const start = source.indexOf("app.get('/readyz'");
    const end = source.indexOf('// Express error handler', start);
    expect(start).toBeGreaterThanOrEqual(0);
    expect(end).toBeGreaterThan(start);
    expect(source.slice(start, end)).toMatch(
      /config\.redisCredentialMode\s*!==\s*'managed'\s*\|\|\s*placement\.redisReachable/
    );
  });
});
