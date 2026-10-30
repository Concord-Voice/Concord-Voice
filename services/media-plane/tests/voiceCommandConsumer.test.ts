import { describe, expect, it, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { spawn, type ChildProcess } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer } from 'node:net';
import './mocks/logger.js';
import {
  natsServerAcquisitionTimeoutMs,
  resolveNatsServerBinary,
} from './helpers/natsServerBinary.js';
import {
  AckPolicy,
  DeliverPolicy,
  DiscardPolicy,
  RetentionPolicy,
  StorageType,
  nanos,
  JSONCodec,
  createInbox,
  type NatsConnection,
} from 'nats';
import { NatsService } from '../src/lib/nats.js';
import {
  decodeVoiceCommandOwnerRecord,
  VoiceCommandOwner,
  type VoiceCommandOwnerRecord,
} from '../src/lib/voiceCommandOwner.js';
import type { VoiceCommandDispatcher } from '../src/lib/voiceCommandDispatcher.js';
import { createRecoveryCallerFixture } from './helpers/voiceEnforcementRecoveryCallers.js';

const mocked = vi.hoisted(() => ({ connect: vi.fn(), url: 'nats://127.0.0.1:4222' }));
vi.mock('nats', async (actual) => ({
  ...(await actual<typeof import('nats')>()),
  connect: (...args: unknown[]) => mocked.connect(...args),
}));
vi.mock('@/config/index.js', () => ({
  config: {
    get natsUrl() {
      return mocked.url;
    },
  },
}));
const generation = '11111111-1111-4111-8111-111111111111';
const nodeBootId = '55555555-5555-4555-8555-555555555555';
const stream = 'VOICE_COMMANDS_V1';
const durable = 'voice-enforcement-v1';
const codec = JSONCodec();
const description = `concord.voice-enforcement generation=${generation}`;
const bucket = 'VOICE_COMMAND_OWNER_V1';
const ownerBackingStream = `KV_${bucket}`;
const resourceBirth = {
  streamCreatedAt: '2026-10-13T18:07:41.123456789Z',
  ownerStoreCreatedAt: '2026-10-13T18:07:41.987654321Z',
};
const releaseProof = { ...resourceBirth, initializingRevision: '1' };
const ownerStreamConfig = {
  name: ownerBackingStream,
  subjects: [`$KV.${bucket}.>`],
  description,
  metadata: { 'concord.voice-enforcement.generation': generation },
  storage: StorageType.File,
  retention: RetentionPolicy.Limits,
  discard: DiscardPolicy.New,
  num_replicas: 1,
  max_age: 0,
  max_bytes: -1,
  max_msgs: -1,
  max_msg_size: -1,
  max_msgs_per_subject: 1,
  max_consumers: -1,
  duplicate_window: nanos(120_000),
  allow_rollup_hdrs: true,
  deny_delete: true,
  allow_direct: true,
};
const streamConfig = {
  name: stream,
  subjects: ['voice.commands.v1'],
  description,
  metadata: { 'concord.voice-enforcement.generation': generation },
  storage: StorageType.File,
  retention: RetentionPolicy.Limits,
  discard: DiscardPolicy.New,
  num_replicas: 1,
  max_age: 0,
  max_bytes: 67108864,
  max_msgs: 50000,
  max_msg_size: 4096,
  max_msgs_per_subject: -1,
  max_consumers: -1,
  allow_direct: false,
  duplicate_window: nanos(120_000),
};
const consumerConfig = {
  durable_name: durable,
  name: durable,
  ack_policy: AckPolicy.Explicit,
  deliver_policy: DeliverPolicy.All,
  ack_wait: nanos(30_000),
  max_ack_pending: 1,
  max_deliver: -1,
  filter_subject: 'voice.commands.v1',
};
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}
function command(sequence = '1') {
  return {
    schema: 'concord.voice-enforcement',
    version: 1,
    streamGeneration: generation,
    commandId:
      sequence === '1'
        ? '66666666-6666-4666-8666-666666666666'
        : '77777777-7777-4777-8777-777777777777',
    sequence,
    scope: 'dm',
    kind: 'disconnect',
    action: 'disconnect',
    channelId: '22222222-2222-4222-8222-222222222222',
    userId: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
    target: null,
    reason: 'membership_removed',
  };
}
function message(data: unknown = command()) {
  return {
    data: codec.encode(data),
    seq: 1,
    redelivered: false,
    info: { stream, consumer: durable, streamSequence: 1, deliverySequence: 1, redeliveryCount: 1 },
    subject: 'voice.commands.v1',
    ack: vi.fn(),
    ackAck: vi.fn(async () => true),
    working: vi.fn(),
    term: vi.fn(),
    nak: vi.fn(),
  };
}
function fixture() {
  const idle = deferred<ReturnType<typeof message> | null>();
  const next = vi.fn(() => idle.promise);
  const consumerInfo = vi.fn(async () => ({
    stream_name: stream,
    name: durable,
    created: '2026-10-13T18:07:42.123456789Z',
    config: consumerConfig,
  }));
  const consumer = { next, info: consumerInfo, delete: vi.fn() };
  let epoch = 0;
  let lastValidity = true;
  const owner = {
    streamGeneration: generation,
    nodeBootId,
    acquire: vi.fn(async () => undefined),
    renew: vi.fn(async () => undefined),
    valid: vi.fn(() => true),
    authorityEpoch: vi.fn(() => {
      const current = owner.valid();
      if (current !== lastValidity) {
        epoch++;
        lastValidity = current;
      }
      return epoch;
    }),
    confirmContinuity: vi.fn(async () => {
      try {
        if (!owner.valid() || acquiredRevision === undefined) throw new Error('owner invalid');
        const entry = await kv.get('authoritative');
        if (entry.operation === 'DEL') throw new Error('owner record absent');
        const current = decodeVoiceCommandOwnerRecord(codec.decode(entry.value), generation);
        if (
          entry.revision !== acquiredRevision ||
          current.phase !== 'held' ||
          current.nodeBootId !== nodeBootId ||
          JSON.stringify(current.proof) !== JSON.stringify(releaseProof)
        )
          throw new Error('owner continuity changed');
      } catch {
        owner.invalidate();
        throw new Error('Voice command ownership unavailable');
      }
    }),
    invalidate: vi.fn(() => {
      if (lastValidity) epoch++;
      lastValidity = false;
      owner.valid.mockReturnValue(false);
    }),
    release: vi.fn(async () => undefined),
  };
  const dispatcher = {
    applyDurable: vi.fn(async (_data: unknown) => 'target_absent'),
    handleCompatibility: vi.fn(),
  };
  const quarantine = vi.fn(async (_reason: string, _episode: object) => undefined);
  const deleteConsumer = vi.fn();
  const birth = { ...resourceBirth };
  const manager = {
    streams: {
      info: vi.fn(async (name: string) =>
        name === ownerBackingStream
          ? {
              config: ownerStreamConfig,
              created: birth.ownerStoreCreatedAt,
              state: { messages: 0 },
            }
          : { config: streamConfig, created: birth.streamCreatedAt, state: { messages: 0 } }
      ),
    },
    consumers: {
      info: consumerInfo,
      add: vi.fn(async () => ({
        stream_name: stream,
        name: durable,
        created: '2026-10-13T18:07:42.123456789Z',
        config: consumerConfig,
      })),
      delete: deleteConsumer,
    },
  };
  let ownerValue = {
    version: 1,
    streamGeneration: generation,
    phase: 'held',
    nodeBootId,
    proof: releaseProof,
  };
  let ownerRevision = 2;
  let acquiredRevision: number | undefined;
  const kv = {
    get: vi.fn(async () => ({
      value: codec.encode(ownerValue),
      revision: ownerRevision,
      operation: 'PUT',
    })),
    update: vi.fn(async (_key: string, value: Uint8Array, revision: number) => {
      if (revision !== ownerRevision) throw new Error('fixture CAS conflict');
      ownerValue = codec.decode(value) as typeof ownerValue;
      return ++ownerRevision;
    }),
    status: vi.fn(async () => ({
      bucket: 'VOICE_COMMAND_OWNER_V1',
      history: 1,
      ttl: 0,
      storage: StorageType.File,
      replicas: 1,
      description,
      metadata: { 'concord.voice-enforcement.generation': generation },
    })),
  };
  owner.acquire.mockImplementation(async () => {
    const entry = await kv.get('authoritative');
    if (entry.operation === 'DEL') throw new Error('Voice command ownership unavailable');
    const current = decodeVoiceCommandOwnerRecord(codec.decode(entry.value), generation);
    if (
      current.phase !== 'held' ||
      current.nodeBootId !== nodeBootId ||
      JSON.stringify(current.proof) !== JSON.stringify(releaseProof)
    )
      throw new Error('Voice command ownership unavailable');
    acquiredRevision = entry.revision;
  });
  const bindOwner = vi.fn(async () => kv);
  const statusQueue = deferred<void>();
  let statusSignal = deferred<{ type: string; data: string }>();
  const nc = {
    isClosed: vi.fn(() => false),
    jetstreamManager: vi.fn(async () => manager),
    jetstream: vi.fn(() => ({
      consumers: { get: vi.fn(async () => consumer) },
      views: { kv: bindOwner },
    })),
    status: vi.fn(() => ({
      async *[Symbol.asyncIterator]() {
        for (;;) {
          const status = await Promise.race([statusSignal.promise, statusQueue.promise.then(() => undefined)]);
          if (!status) return;
          statusSignal = deferred<{ type: string; data: string }>();
          yield status;
        }
      },
    })),
    drain: vi.fn(async () => undefined),
    publish: vi.fn(),
  };
  mocked.connect.mockResolvedValue(nc);
  const service = new NatsService();
  return {
    service,
    owner,
    dispatcher,
    quarantine,
    next,
    idle,
    nc,
    manager,
    consumer,
    consumerInfo,
    deleteConsumer,
    kv,
    bindOwner,
    birth,
    status: (type: string) => statusSignal.resolve({ type, data: 'fixture' }),
    start: async () => {
      await service.connect();
      await service.startVoiceCommandConsumer({
        owner: owner as unknown as VoiceCommandOwner,
        dispatcher: dispatcher as unknown as VoiceCommandDispatcher,
        quarantine,
      });
    },
    close: async () => {
      idle.resolve(null);
      await service.stopVoiceCommandConsumer();
      await service.close();
      statusQueue.resolve();
    },
  };
}

// Reuse the broker boundary while exercising the real owner and NATS lifecycle.
async function ownershipLifecycleFixture() {
  const f = fixture();
  let value: VoiceCommandOwnerRecord = {
    version: 1,
    streamGeneration: generation,
    phase: 'released',
    nodeBootId: null,
    proof: releaseProof,
  };
  let revision = 2;
  let afterReleaseWrite: (() => Promise<void>) | undefined;
  f.kv.get.mockImplementation(async () => ({
    value: codec.encode(value),
    revision,
    operation: 'PUT',
  }));
  f.kv.update.mockImplementation(async (_key, encoded, expectedRevision) => {
    if (expectedRevision !== revision) throw new Error('fixture CAS conflict');
    value = codec.decode(encoded) as VoiceCommandOwnerRecord;
    const acknowledgedRevision = ++revision;
    if (value.phase === 'released') await afterReleaseWrite?.();
    return acknowledgedRevision;
  });
  await f.service.connect();
  const transport = await f.service.getVoiceCommandTransport();
  const invalidated = vi.fn();
  const actualOwner = new VoiceCommandOwner({
    store: transport.store,
    streamGeneration: transport.streamGeneration,
    nodeBootId,
    now: () => Date.now(),
    onInvalidated: invalidated,
  });
  const renew = vi.spyOn(actualOwner, 'renew');
  return {
    ...f,
    actualOwner,
    renew,
    invalidated,
    record: () => structuredClone(value),
    pauseReleaseAcknowledgement: (pause: () => Promise<void>) => {
      afterReleaseWrite = pause;
    },
    start: () =>
      f.service.startVoiceCommandConsumer({
        owner: actualOwner,
        dispatcher: f.dispatcher as unknown as VoiceCommandDispatcher,
        quarantine: f.quarantine,
      }),
  };
}

beforeEach(() => {
  mocked.connect.mockReset();
});
afterEach(() => {
  vi.useRealTimers();
});

describe('voice command ownership renewal lifecycle regressions for #2624', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(0);
  });

  afterEach(() => {
    vi.clearAllTimers();
  });

  it('recovery leaves exactly one active ownership renewer', async () => {
    const f = await ownershipLifecycleFixture();
    const malformed = message({ ...command(), version: 99 });
    f.next.mockResolvedValueOnce(malformed);
    try {
      await f.start();
      await vi.advanceTimersByTimeAsync(0);
      expect(malformed.term).toHaveBeenCalledOnce();
      expect(f.quarantine).toHaveBeenCalledExactlyOnceWith('schema_rejected', expect.any(Object));
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.service.voiceCommandReady()).toBe(false);

      await vi.advanceTimersByTimeAsync(500);
      await f.service.recoverVoiceCommandConsumerAfterTeardown();
      expect(f.service.voiceCommandReady()).toBe(true);
      expect(f.renew).toHaveBeenCalledOnce();
      f.renew.mockClear();

      await vi.advanceTimersByTimeAsync(2000);

      expect(
        f.renew,
        'Consumer recovery leaves exactly one active ownership renewer'
      ).toHaveBeenCalledTimes(1);
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.record().phase).toBe('held');
    } finally {
      await f.close();
    }
  });

  it.each(['invalidation', 'close'] as const)(
    'later %s completely stops the ownership renewer after recovery',
    async (ending) => {
      const f = await ownershipLifecycleFixture();
      const malformed = message({ ...command(), version: 99 });
      f.next.mockResolvedValueOnce(malformed);
      try {
        await f.start();
        await vi.advanceTimersByTimeAsync(0);
        expect(malformed.term).toHaveBeenCalledOnce();
        await vi.advanceTimersByTimeAsync(500);
        await f.service.recoverVoiceCommandConsumerAfterTeardown();
        expect(f.service.voiceCommandReady()).toBe(true);
        f.renew.mockClear();
        await vi.advanceTimersByTimeAsync(2000);
        expect(f.renew).toHaveBeenCalled();

        if (ending === 'invalidation') {
          f.consumerInfo.mockRejectedValue(new Error('fixture continuity unavailable'));
          await vi.advanceTimersByTimeAsync(1500);
          expect(f.quarantine).toHaveBeenCalledWith('continuity_unavailable', expect.any(Object));
          expect(f.actualOwner.valid()).toBe(false);
        } else {
          await f.close();
          expect(f.nc.drain).toHaveBeenCalledOnce();
        }
        expect(f.service.voiceCommandReady()).toBe(false);
        expect(
          vi.getTimerCount(),
          `Consumer recovery: later ${ending} stops the ownership renewer completely`
        ).toBe(0);
      } finally {
        await f.close();
      }
    }
  );

  it('successful conservative teardown permits same-boot recovery and a new command', async () => {
    const f = await ownershipLifecycleFixture();
    const malformed = message({ ...command(), version: 99 });
    const subsequent = message();
    f.next.mockResolvedValueOnce(malformed);
    try {
      await f.start();
      await vi.advanceTimersByTimeAsync(500);
      expect(malformed.term).toHaveBeenCalledOnce();
      f.next.mockResolvedValueOnce(subsequent);
      await f.service.recoverVoiceCommandConsumerAfterTeardown();
      await vi.advanceTimersByTimeAsync(0);
      expect(f.service.voiceCommandReady()).toBe(true);
      expect(f.renew).toHaveBeenCalledOnce();
      expect(f.actualOwner.valid()).toBe(true);
      expect(subsequent.ackAck).toHaveBeenCalledOnce();
      expect(f.record().phase).toBe('held');
    } finally {
      await f.close();
    }
  });

  it('successful renewal and release are reached without a concurrent tick', async () => {
    const f = await ownershipLifecycleFixture();
    try {
      await f.start();
      await vi.advanceTimersByTimeAsync(2000);
      expect(f.renew).toHaveBeenCalledOnce();
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.record().phase).toBe('held');
      f.idle.resolve(null);
      await f.service.stopVoiceCommandConsumer();
      // The owner remains renewed while graceful native teardown is pending.
      await vi.advanceTimersByTimeAsync(2000);
      expect(f.renew).toHaveBeenCalledTimes(2);
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.record().phase).toBe('held');
      await f.actualOwner.release();
      expect(f.record()).toEqual({
        version: 1,
        streamGeneration: generation,
        phase: 'released',
        nodeBootId,
        proof: releaseProof,
      });
      expect(f.actualOwner.valid()).toBe(false);
      await f.close();
      await vi.advanceTimersByTimeAsync(4000);
      expect(vi.getTimerCount()).toBe(0);
      expect(f.renew).toHaveBeenCalledTimes(2);
    } finally {
      await f.close();
    }
  });

  it('graceful release after native teardown survives a concurrent renewal tick', async () => {
    const f = await ownershipLifecycleFixture();
    const releaseWritten = deferred<void>();
    const acknowledgement = deferred<void>();
    try {
      await f.start();
      await vi.advanceTimersByTimeAsync(1750);
      expect(f.actualOwner.valid()).toBe(true);
      f.idle.resolve(null);
      await f.service.stopVoiceCommandConsumer();
      const nativeTeardown = vi.fn(async () => undefined);
      await nativeTeardown();
      expect(nativeTeardown).toHaveBeenCalledOnce();
      f.pauseReleaseAcknowledgement(async () => {
        releaseWritten.resolve();
        await acknowledgement.promise;
      });
      const outcome = f.actualOwner.release().then(
        () => 'released',
        () => 'release_failed'
      );
      await releaseWritten.promise;
      expect(f.record().phase).toBe('released');

      await vi.advanceTimersByTimeAsync(250);
      acknowledgement.resolve();

      expect(
        await outcome,
        'Graceful ownership release after native teardown cannot be invalidated by a concurrent renewal tick'
      ).toBe('released');
      expect(f.record()).toEqual({
        version: 1,
        streamGeneration: generation,
        phase: 'released',
        nodeBootId,
        proof: releaseProof,
      });
      await f.close();
      expect(
        vi.getTimerCount(),
        'Graceful ownership release reports the verified released record without leaving an active renewer'
      ).toBe(0);
    } finally {
      acknowledgement.resolve();
      await f.close();
    }
  });

  it('failed teardown remains fail-closed and retains the held ownership record', async () => {
    const f = await ownershipLifecycleFixture();
    const malformed = message({ ...command(), version: 99 });
    const release = vi.spyOn(f.actualOwner, 'release');
    f.next.mockResolvedValueOnce(malformed);
    f.quarantine.mockRejectedValueOnce(new Error('fixture native teardown failed'));
    try {
      await f.start();
      await vi.advanceTimersByTimeAsync(0);
      expect(f.quarantine).toHaveBeenCalledWith('schema_rejected', expect.any(Object));
      // The failure transition retains the rejected episode attempt; only
      // explicit recovery may retry teardown, under that same episode.
      expect(f.quarantine).toHaveBeenCalledTimes(1);
      expect(f.actualOwner.valid()).toBe(false);
      expect(f.service.voiceCommandReady()).toBe(false);
      expect(f.record().phase).toBe('held');
      expect(malformed.term).not.toHaveBeenCalled();
      expect(malformed.ackAck).not.toHaveBeenCalled();
      expect(release).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(4000);
      expect(f.renew).not.toHaveBeenCalled();
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      await f.close();
    }
  });
});

describe('voice command quarantine recovery episodes', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(0); });
  afterEach(() => { vi.clearAllTimers(); });

  it('does not attach another observer to the retained attempt for each actual transport notification', async () => {
    const f = await ownershipLifecycleFixture();
    const native = deferred<void>();
    f.quarantine.mockImplementationOnce(() => native.promise);
    await f.start();
    const catches = vi.spyOn(Promise.prototype, 'catch');
    try {
      f.status('disconnect'); await vi.advanceTimersByTimeAsync(0);
      expect(f.quarantine).toHaveBeenCalledTimes(1);
      expect(catches.mock.contexts.length).toBeGreaterThan(0);
      // The first observer belongs to the task installed before quarantine is
      // invoked. Observe its actual identity without inspecting private state.
      const retained = catches.mock.contexts[0];
      catches.mockClear();
      for (const notification of ['reconnecting', 'disconnect', 'reconnecting']) {
        f.status(notification); await vi.advanceTimersByTimeAsync(0);
      }
      expect(f.quarantine).toHaveBeenCalledTimes(1);
      expect(f.service.voiceCommandReady()).toBe(false);
      expect(catches.mock.contexts.filter(task => task === retained),
        'a hung episode must not gain one retained rejection reaction per status notification').toHaveLength(0);
    } finally { catches.mockRestore(); native.resolve(); await f.close(); }
  });

  it('actual NATS episodes invalidate the actual index fence once across retry and again after completed recovery', async () => {
    const f = await ownershipLifecycleFixture();
    const callers = await createRecoveryCallerFixture(f.service);
    const invalidations = vi.spyOn(callers.fence, 'invalidate');
    const quarantine = vi.fn(callers.quarantine);
    f.next.mockResolvedValueOnce(message({ ...command(), version: 99 }));
    try {
      await f.service.startVoiceCommandConsumer({ owner: callers.owner,
        dispatcher: f.dispatcher as unknown as VoiceCommandDispatcher, quarantine });
      await vi.advanceTimersByTimeAsync(0);
      expect(callers.proofs).toHaveLength(1);
      expect(invalidations).toHaveBeenCalledTimes(1);
      expect(callers.owner.valid()).toBe(true); // schema needs no owner notification
      const episode = quarantine.mock.calls[0][1];
      callers.proofs[0].reject(new Error('actual native teardown unavailable'));
      await vi.advanceTimersByTimeAsync(0);
      // The genuine owner notification adds one separate finite obligation.
      expect(callers.owner.valid()).toBe(false);
      expect(invalidations).toHaveBeenCalledTimes(2);
      expect(callers.proofs).toHaveLength(2);
      const recovery = f.service.recoverVoiceCommandConsumerAfterTeardown();
      await vi.advanceTimersByTimeAsync(0);
      expect(quarantine.mock.calls[1]).toEqual(['recovery_teardown', episode]);
      expect(invalidations).toHaveBeenCalledTimes(2);
      await callers.finish(); await recovery;
      expect(f.service.voiceCommandReady()).toBe(true);
      expect(callers.fence.ready()).toBe(true);
      f.idle.resolve(message({ ...command('2'), version: 99 }));
      await vi.advanceTimersByTimeAsync(0);
      expect(quarantine).toHaveBeenCalledTimes(3);
      expect(quarantine.mock.calls[2][1]).not.toBe(episode);
      expect(invalidations).toHaveBeenCalledTimes(3);
      expect(callers.fence.ready()).toBe(false);
      expect(callers.proofs).toHaveLength(3);
      await callers.finish();
      expect(callers.fence.ready()).toBe(true);
      expect(f.service.voiceCommandReady()).toBe(false); // teardown grants no consumer authority
    } finally { await callers.finish(); callers.availability.stop(); await f.close(); }
  });

  it('retains failed schema teardown through invalidation and retries once under the same episode', async () => {
    const f = await ownershipLifecycleFixture();
    const malformed = message({ ...command(), version: 99 });
    const native = deferred<void>();
    f.next.mockResolvedValueOnce(malformed);
    f.quarantine.mockRejectedValueOnce(new Error('native unavailable'))
      .mockImplementationOnce(() => native.promise);
    try {
      await f.start(); await vi.advanceTimersByTimeAsync(0);
      expect(f.quarantine).toHaveBeenCalledTimes(1);
      expect(f.quarantine.mock.calls[0][0]).toBe('schema_rejected');
      const episode = f.quarantine.mock.calls[0][1];
      expect(episode).toBeDefined();
      // Real invalidation follows schema_teardown_unavailable. It must not
      // duplicate the failed schema callback or acknowledge the delivery.
      expect(f.actualOwner.valid()).toBe(false);
      expect(f.invalidated).toHaveBeenCalledTimes(1);
      expect(f.service.voiceCommandReady()).toBe(false);
      expect(malformed.term).not.toHaveBeenCalled();
      expect(malformed.ackAck).not.toHaveBeenCalled();
      // Real transport notifications advance runtime epoch repeatedly, but
      // must retain the same failed attempt and episode without new waiters.
      for (let i = 0; i < 10; i++) {
        f.status(i % 2 ? 'disconnect' : 'reconnecting');
        await vi.advanceTimersByTimeAsync(0);
        expect(f.quarantine).toHaveBeenCalledTimes(1);
      }
      f.status('reconnect'); await vi.advanceTimersByTimeAsync(0);
      const recovery = f.service.recoverVoiceCommandConsumerAfterTeardown();
      await vi.advanceTimersByTimeAsync(0);
      expect(f.quarantine).toHaveBeenCalledTimes(2);
      expect(f.quarantine.mock.calls[1]).toEqual(['recovery_teardown', episode]);
      for (let i = 0; i < 30; i++) {
        expect(f.service.voiceCommandReady()).toBe(false);
        expect(f.service.recoverVoiceCommandConsumerAfterTeardown()).toBe(recovery);
      }
      expect(f.quarantine).toHaveBeenCalledTimes(2);
      native.resolve(); await recovery;
      expect(f.service.voiceCommandReady()).toBe(true);
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.record().phase).toBe('held');

      // A distinct actual schema delivery after successful recovery starts a
      // fresh episode, even though the first episode's teardown succeeded.
      f.idle.resolve(message({ ...command('2'), version: 99 }));
      await vi.advanceTimersByTimeAsync(0);
      expect(f.quarantine).toHaveBeenCalledTimes(3);
      expect(f.quarantine.mock.calls[2][0]).toBe('schema_rejected');
      expect(f.quarantine.mock.calls[2][1]).not.toBe(episode);
      expect(f.service.voiceCommandReady()).toBe(false);
    } finally { native.resolve(); await f.close(); }
  });

  it('does not invalidate the owner for a successful schema quarantine before consumer recovery', async () => {
    const f = await ownershipLifecycleFixture();
    const malformed = message({ ...command(), version: 99 });
    f.next.mockResolvedValueOnce(malformed);
    try {
      await f.start(); await vi.advanceTimersByTimeAsync(0);
      expect(f.invalidated).not.toHaveBeenCalled();
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.service.voiceCommandReady()).toBe(false);
      expect(malformed.term).toHaveBeenCalledTimes(1);
      expect(malformed.ackAck).not.toHaveBeenCalled();
      expect(f.quarantine).toHaveBeenCalledTimes(1);
      await f.service.recoverVoiceCommandConsumerAfterTeardown();
      expect(f.service.voiceCommandReady()).toBe(true);
      expect(f.quarantine).toHaveBeenCalledTimes(1);
    } finally { await f.close(); }
  });

  it('rejects stale recovery after a newer actual owner invalidation and retains the episode for retry', async () => {
    const f = await ownershipLifecycleFixture();
    f.next.mockResolvedValueOnce(message({ ...command(), version: 99 }));
    try {
      await f.start(); await vi.advanceTimersByTimeAsync(0);
      const episode = f.quarantine.mock.calls[0][1];
      const info = deferred<Awaited<ReturnType<typeof f.consumerInfo>>>();
      const originalInfo = await f.consumerInfo();
      f.consumerInfo.mockImplementationOnce(() => info.promise);
      const recovery = f.service.recoverVoiceCommandConsumerAfterTeardown();
      const rejected = expect(recovery).rejects.toThrow();
      await vi.advanceTimersByTimeAsync(0);
      // Invalidate through the actual status consumer while recovery is
      // awaiting a healthy broker response, then restore only transport.
      f.status('disconnect'); await vi.advanceTimersByTimeAsync(0);
      f.status('reconnect'); await vi.advanceTimersByTimeAsync(0);
      info.resolve(originalInfo); await rejected;
      expect(f.service.voiceCommandReady()).toBe(false);
      expect(f.actualOwner.valid()).toBe(false);
      expect(f.quarantine).toHaveBeenCalledTimes(1);
      await f.service.recoverVoiceCommandConsumerAfterTeardown();
      expect(f.service.voiceCommandReady()).toBe(true);
      expect(f.quarantine.mock.calls[0][1]).toBe(episode);
      expect(f.quarantine).toHaveBeenCalledTimes(1);
    } finally { await f.close(); }
  });
});

describe('voice command stream configuration parity', () => {
  const sharedDrifts: Array<{ name: string; patch: Record<string, unknown> }> = [
    { name: 'per-message TTL', patch: { allow_msg_ttl: true } },
    { name: 'subject delete-marker TTL', patch: { subject_delete_marker_ttl: nanos(1000) } },
    {
      name: 'default consumer inactivity',
      patch: { consumer_limits: { inactive_threshold: nanos(1000), max_ack_pending: 0 } },
    },
    {
      name: 'default consumer pending acknowledgements',
      patch: { consumer_limits: { inactive_threshold: 0, max_ack_pending: 2 } },
    },
    { name: 'mirror direct access', patch: { mirror_direct: true } },
    { name: 'storage compression', patch: { compression: 's2' } },
    { name: 'stream placement', patch: { placement: { cluster: 'fixture-alternate-cluster' } } },
    { name: 'template ownership', patch: { template_owner: 'fixture-template' } },
    { name: 'absent per-subject message limit', patch: { max_msgs_per_subject: undefined } },
    { name: 'absent consumer limit', patch: { max_consumers: undefined } },
    { name: 'first sequence', patch: { first_seq: 100 } },
    { name: 'message counters', patch: { allow_msg_counter: true } },
    { name: 'atomic publishes', patch: { allow_atomic: true } },
    { name: 'message schedules', patch: { allow_msg_schedules: true } },
    { name: 'batched publishes', patch: { allow_batched: true } },
    { name: 'asynchronous persistence', patch: { persist_mode: 'async' } },
    { name: 'unknown persistence mode', patch: { persist_mode: 'fixture-unknown-mode' } },
  ];
  const cases = (['command', 'owner'] as const).flatMap((resource) => [
    ...sharedDrifts.map(({ name, patch }) => ({ resource, name, patch })),
    { resource, name: 'direct access mode', patch: { allow_direct: resource === 'command' } },
  ]);

  function configure(
    f: ReturnType<typeof fixture>,
    resource: 'command' | 'owner',
    patch: Record<string, unknown>
  ) {
    const original = f.manager.streams.info.getMockImplementation()!;
    const resourceName = resource === 'owner' ? ownerBackingStream : stream;
    f.manager.streams.info.mockImplementation(async (name: string) => {
      const info = await original(name);
      if (name !== resourceName) return info;
      const config = { ...info.config, ...patch };
      for (const [key, value] of Object.entries(patch))
        if (value === undefined) Reflect.deleteProperty(config, key);
      return { ...info, config };
    });
  }

  const healthyCases = (['command', 'owner'] as const).flatMap((resource) =>
    (['absent', 'explicit'] as const).map((defaults) => ({ resource, defaults }))
  );
  it.each(healthyCases)(
    'accepts the unchanged $resource contract with $defaults modern defaults',
    async ({ resource, defaults }) => {
      const f = fixture();
      const explicit = defaults === 'explicit';
      configure(f, resource, {
        allow_msg_ttl: false,
        subject_delete_marker_ttl: 0,
        consumer_limits: { inactive_threshold: 0, max_ack_pending: 0 },
        mirror_direct: false,
        compression: 'none',
        placement: null,
        template_owner: '',
        allow_direct: resource === 'owner',
        first_seq: explicit ? 0 : undefined,
        allow_msg_counter: explicit ? false : undefined,
        allow_atomic: explicit ? false : undefined,
        allow_msg_schedules: explicit ? false : undefined,
        allow_batched: explicit ? false : undefined,
        persist_mode: explicit ? 'default' : undefined,
      });
      await f.service.connect();
      try {
        const transport = await f.service.getVoiceCommandTransport();
        expect(transport.streamGeneration).toBe(generation);
        expect((await transport.store.read())?.value.phase).toBe('held');
        expect(f.kv.update).not.toHaveBeenCalled();
      } finally {
        await f.service.close();
      }
    }
  );

  it.each(cases)(
    'rejects $resource broker configuration drift: $name',
    async ({ resource, patch }) => {
      const f = fixture();
      configure(f, resource, patch);
      await f.service.connect();
      try {
        await expect(f.service.getVoiceCommandTransport()).rejects.toThrow(
          'Voice command transport unavailable'
        );
        expect(f.kv.update).not.toHaveBeenCalled();
      } finally {
        await f.service.close();
      }
    }
  );
});

describe('voice command NATS adapter incarnation proof', () => {
  it('derives the generation and reads complete nanosecond proof from existing resources', async () => {
    const f = fixture();
    await f.service.connect();
    try {
      const transport = await f.service.getVoiceCommandTransport();
      expect(transport.streamGeneration).toBe(generation);
      expect(f.bindOwner).toHaveBeenCalledWith(
        bucket,
        expect.objectContaining({ bindOnly: true, allow_direct: false })
      );
      expect(await transport.store.read()).toEqual({
        value: {
          version: 1,
          streamGeneration: generation,
          phase: 'held',
          nodeBootId,
          proof: releaseProof,
        },
        revision: 2,
      });
    } finally {
      await f.service.close();
    }
  });

  it.each(['streamCreatedAt', 'ownerStoreCreatedAt'] as const)(
    'rejects changed %s at the nanosecond boundary',
    async (field) => {
      const f = fixture();
      await f.service.connect();
      try {
        const transport = await f.service.getVoiceCommandTransport();
        const valid = await transport.store.read();
        expect(valid?.value.phase).toBe('held');
        f.birth[field] =
          field === 'streamCreatedAt'
            ? '2026-10-13T18:07:41.123456788Z'
            : '2026-10-13T18:07:41.987654320Z';
        await expect(transport.store.read()).rejects.toThrow();
        await expect(transport.store.compareAndSet(2, valid!.value)).rejects.toThrow();
        expect(f.kv.update).not.toHaveBeenCalled();
      } finally {
        await f.service.close();
      }
    }
  );

  it('checks both resource births after CAS rather than trusting a successful write', async () => {
    const f = fixture();
    await f.service.connect();
    try {
      const transport = await f.service.getVoiceCommandTransport();
      const record = await transport.store.read();
      f.kv.update.mockImplementationOnce(async () => {
        f.birth.ownerStoreCreatedAt = '2026-10-13T18:07:41.987654320Z';
        return 3;
      });
      await expect(transport.store.compareAndSet(2, record!.value)).rejects.toThrow();
      expect(f.kv.update).toHaveBeenCalledTimes(1);
    } finally {
      await f.service.close();
    }
  });
});

describe('required serial voice command consumer', () => {
  it.each([
    ['broker-paused', { paused: true }],
    [
      'future pause deadline',
      {
        config: { ...consumerConfig, pause_until: '2999-01-01T00:00:00.000Z' },
      },
    ],
  ] as const)(
    'refuses readiness and message admission for a %s durable consumer',
    async (_state, state) => {
      const f = fixture();
      await f.service.connect();
      const healthyInfo = await f.consumerInfo();
      expect(healthyInfo.stream_name).toBe(stream);
      expect(healthyInfo.name).toBe(durable);
      expect(healthyInfo.config).toEqual(consumerConfig);
      f.consumerInfo.mockResolvedValue({ ...healthyInfo, ...state } as typeof healthyInfo);

      try {
        const startup = await f.service
          .startVoiceCommandConsumer({
            owner: f.owner as unknown as VoiceCommandOwner,
            dispatcher: f.dispatcher as unknown as VoiceCommandDispatcher,
            quarantine: f.quarantine,
          })
          .then(
            () => ({ status: 'ready' as const }),
            (error: Error) => ({ status: 'rejected' as const, error })
          );
        expect(
          startup.status,
          'A paused or future-paused durable consumer must not become ready or admit commands'
        ).toBe('rejected');
        if (startup.status === 'rejected')
          expect(startup.error.message).toBe('Voice command transport unavailable');
        expect(f.service.voiceCommandReady()).toBe(false);
        expect(f.next).not.toHaveBeenCalled();
        expect(f.dispatcher.applyDurable).not.toHaveBeenCalled();
      } finally {
        await f.close();
      }
    }
  );

  it.each([
    ['absent paused state', {}],
    ['explicitly unpaused state', { paused: false }],
    [
      'elapsed pause deadline',
      { paused: false, config: { ...consumerConfig, pause_until: '2000-01-01T00:00:00.000Z' } },
    ],
  ] as const)('accepts the healthy consumer positive control: %s', async (_name, state) => {
    const f = fixture();
    await f.service.connect();
    const healthyInfo = await f.consumerInfo();
    expect(healthyInfo.stream_name).toBe(stream);
    expect(healthyInfo.name).toBe(durable);
    expect(healthyInfo.config).toEqual(consumerConfig);
    f.consumerInfo.mockResolvedValue({ ...healthyInfo, ...state } as typeof healthyInfo);
    try {
      await f.service.startVoiceCommandConsumer({
        owner: f.owner as unknown as VoiceCommandOwner,
        dispatcher: f.dispatcher as unknown as VoiceCommandDispatcher,
        quarantine: f.quarantine,
      });
      await vi.waitFor(() => expect(f.next).toHaveBeenCalledWith({ expires: 2000 }));
      expect(f.service.voiceCommandReady()).toBe(true);
    } finally {
      await f.close();
    }
  });

  it('starts from exact stream/consumer configuration and requests only one message at a time', async () => {
    const f = fixture();
    await f.start();
    await vi.waitFor(() => expect(f.next).toHaveBeenCalledWith({ expires: 2000 }));
    expect(f.owner.acquire).toHaveBeenCalledTimes(1);
    expect(f.service.voiceCommandReady()).toBe(true);
    expect(f.next).toHaveBeenCalledTimes(1);
    await f.close();
    expect(f.service.voiceCommandReady()).toBe(false);
    expect(f.deleteConsumer).not.toHaveBeenCalled();
    expect(f.owner.release).not.toHaveBeenCalled();
  });

  it('confirms ack only after awaited authoritative effects and server fanout', async () => {
    const f = fixture();
    const msg = message();
    const effects = deferred<string>();
    f.next.mockResolvedValueOnce(msg);
    f.dispatcher.applyDurable.mockImplementationOnce(() => effects.promise);
    await f.start();
    await vi.waitFor(() => expect(f.dispatcher.applyDurable).toHaveBeenCalledTimes(1));
    expect(msg.ackAck).not.toHaveBeenCalled();
    expect(f.next).toHaveBeenCalledTimes(1);
    effects.resolve('applied');
    await vi.waitFor(() => expect(msg.ackAck).toHaveBeenCalledTimes(1));
    expect(msg.ack).not.toHaveBeenCalled();
    await f.close();
  });

  it('leaves transient effect failure unacknowledged for redelivery', async () => {
    const f = fixture();
    const msg = message();
    f.next.mockResolvedValueOnce(msg);
    f.dispatcher.applyDurable.mockRejectedValueOnce(new Error('pause unavailable'));
    await f.start();
    await vi.waitFor(() => expect(f.dispatcher.applyDurable).toHaveBeenCalledTimes(1));
    await Promise.resolve();
    await Promise.resolve();
    expect(msg.ackAck).not.toHaveBeenCalled();
    expect(msg.term).not.toHaveBeenCalled();
    await f.close();
  });

  it('withholds ack and readiness after ownership loss during an effect', async () => {
    const f = fixture();
    const msg = message();
    const effects = deferred<string>();
    f.next.mockResolvedValueOnce(msg);
    f.dispatcher.applyDurable.mockImplementationOnce(() => effects.promise);
    await f.start();
    await vi.waitFor(() => expect(f.dispatcher.applyDurable).toHaveBeenCalledTimes(1));
    f.owner.valid.mockReturnValue(false);
    effects.resolve('applied');
    await vi.waitFor(() => expect(f.quarantine).toHaveBeenCalled());
    expect(msg.ackAck).not.toHaveBeenCalled();
    expect(f.service.voiceCommandReady()).toBe(false);
    await f.close();
  });

  it('quarantines durable-consumer deletion before acknowledging an already completed effect', async () => {
    const f = fixture();
    const msg = message();
    const effects = deferred<string>();
    f.next.mockResolvedValueOnce(msg);
    f.dispatcher.applyDurable.mockImplementationOnce(() => effects.promise);
    await f.start();
    await vi.waitFor(() => expect(f.dispatcher.applyDurable).toHaveBeenCalledTimes(1));
    f.consumerInfo.mockRejectedValue(new Error('consumer missing'));
    effects.resolve('applied');
    await vi.waitFor(() => expect(f.quarantine).toHaveBeenCalled());
    expect(msg.ackAck).not.toHaveBeenCalled();
    expect(f.service.voiceCommandReady()).toBe(false);
    await f.close();
  });

  it('sends progress at 5 seconds and quarantines a 10-second hung effect without starting another', async () => {
    vi.useFakeTimers();
    const f = fixture();
    const msg = message();
    const hung = deferred<string>();
    f.next.mockResolvedValueOnce(msg);
    f.dispatcher.applyDurable.mockImplementationOnce(() => hung.promise);
    await f.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(f.dispatcher.applyDurable).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(4999);
    expect(msg.working).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(msg.working).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(5000);
    expect(f.quarantine).toHaveBeenCalledTimes(1);
    expect(f.service.voiceCommandReady()).toBe(false);
    expect(msg.ackAck).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(f.next).toHaveBeenCalledTimes(1);
    expect(f.dispatcher.applyDurable).toHaveBeenCalledTimes(1);
    const stop = f.service.stopVoiceCommandConsumer();
    let stopped = false;
    void stop.then(() => {
      stopped = true;
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(stopped).toBe(false);
    hung.resolve('applied');
    await stop;
    expect(msg.ackAck).not.toHaveBeenCalled();
    await f.close();
  });

  it('terms a malformed envelope only after successful conservative teardown', async () => {
    const f = fixture();
    const msg = message({ ...command(), version: 99 });
    const teardown = deferred<void>();
    f.next.mockResolvedValueOnce(msg);
    f.quarantine.mockImplementationOnce(() => teardown.promise);
    await f.start();
    await vi.waitFor(() => expect(f.quarantine).toHaveBeenCalledTimes(1));
    expect(msg.term).not.toHaveBeenCalled();
    expect(f.dispatcher.applyDurable).not.toHaveBeenCalled();
    teardown.resolve();
    await vi.waitFor(() => expect(msg.term).toHaveBeenCalledTimes(1));
    expect(msg.ackAck).not.toHaveBeenCalled();
    await f.close();
  });

  it('retains malformed delivery when teardown fails', async () => {
    const f = fixture();
    const msg = message({ ...command(), schema: 'unsupported' });
    f.next.mockResolvedValueOnce(msg);
    f.quarantine.mockRejectedValueOnce(new Error('worker close failed'));
    await f.start();
    await vi.waitFor(() => expect(f.quarantine).toHaveBeenCalled());
    await Promise.resolve();
    await Promise.resolve();
    expect(msg.term).not.toHaveBeenCalled();
    expect(msg.ackAck).not.toHaveBeenCalled();
    expect(f.service.voiceCommandReady()).toBe(false);
    await f.close();
  });

  it('treats a lost ack confirmation as uncertainty while preserving dispatcher completion for redelivery', async () => {
    const f = fixture();
    const first = message();
    const retry = message();
    retry.redelivered = true;
    first.ackAck.mockResolvedValueOnce(false);
    f.next.mockResolvedValueOnce(first).mockResolvedValueOnce(retry);
    await f.start();
    await vi.waitFor(() => expect(first.ackAck).toHaveBeenCalledTimes(1));
    // Delivery identity stays intact; the real dispatcher owns mutation/fanout dedupe.
    await vi.waitFor(() => expect(retry.ackAck).toHaveBeenCalledTimes(1));
    expect(f.dispatcher.applyDurable).toHaveBeenCalledTimes(2);
    expect(f.dispatcher.applyDurable.mock.calls[1][0]).toEqual(command());
    expect(first.ack).not.toHaveBeenCalled();
    expect(first.term).not.toHaveBeenCalled();
    await f.close();
  });

  it('awaits the tracked effect on stop without deleting the stable consumer', async () => {
    const f = fixture();
    const msg = message();
    const effects = deferred<string>();
    f.next.mockResolvedValueOnce(msg);
    f.dispatcher.applyDurable.mockImplementationOnce(() => effects.promise);
    await f.start();
    await vi.waitFor(() => expect(f.dispatcher.applyDurable).toHaveBeenCalledTimes(1));
    const stopped = f.service.stopVoiceCommandConsumer();
    let finished = false;
    void stopped.then(() => {
      finished = true;
    });
    await Promise.resolve();
    expect(finished).toBe(false);
    effects.resolve('applied');
    await stopped;
    expect(f.deleteConsumer).not.toHaveBeenCalled();
    expect(f.consumer.delete).not.toHaveBeenCalled();
    expect(f.owner.release).not.toHaveBeenCalled();
    await f.close();
  });

  it('fails startup on absent stream rather than reporting ready or creating an empty transport', async () => {
    const f = fixture();
    f.manager.streams.info.mockRejectedValueOnce(new Error('stream not found'));
    await f.service.connect();
    await expect(
      f.service.startVoiceCommandConsumer({
        owner: f.owner as unknown as VoiceCommandOwner,
        dispatcher: f.dispatcher as unknown as VoiceCommandDispatcher,
        quarantine: f.quarantine,
      })
    ).rejects.toThrow();
    expect(f.service.voiceCommandReady()).toBe(false);
    expect(f.next).not.toHaveBeenCalled();
    await f.close();
  });
});

async function broker(binary: string) {
  const listener = createServer();
  await new Promise<void>((resolve, reject) => {
    listener.once('error', reject);
    listener.listen(0, '127.0.0.1', resolve);
  });
  const address = listener.address();
  if (!address || typeof address === 'string') throw new Error('isolated port missing');
  const port = address.port;
  await new Promise<void>((resolve, reject) =>
    listener.close((error) => (error ? reject(error) : resolve()))
  );
  const directory = await mkdtemp(join(tmpdir(), 'concord-voice-consumer-'));
  const child: ChildProcess = spawn(
    binary,
    ['-js', '-sd', directory, '-a', '127.0.0.1', '-p', String(port)],
    { stdio: 'ignore' }
  );
  const exit = new Promise<void>((resolve, reject) => {
    child.once('error', reject);
    child.once('exit', () => resolve());
  });
  const actual = await vi.importActual<typeof import('nats')>('nats');
  let connection: NatsConnection | undefined;
  try {
    await vi.waitFor(
      async () => {
        connection = await actual.connect({
          servers: `nats://127.0.0.1:${port}`,
          reconnect: false,
          timeout: 100,
        });
      },
      { timeout: 5000, interval: 20 }
    );
    return {
      connection: connection!,
      url: `nats://127.0.0.1:${port}`,
      connect: actual.connect,
      async close() {
        await connection?.close();
        child.kill('SIGINT');
        await exit;
        await rm(directory, { recursive: true, force: true });
      },
    };
  } catch (error) {
    child.kill('SIGINT');
    await exit;
    await rm(directory, { recursive: true, force: true });
    throw error;
  }
}

// Test-owned bootstrap mirrors the committed initializing→released transition.
// It never updates a provisioned receipt or manufactures new proof for recovery.
async function provision(connection: NatsConnection) {
  const manager = await connection.jetstreamManager();
  const js = connection.jetstream();
  const commandStream = await manager.streams.add(streamConfig);
  const ownerStream = await manager.streams.add(ownerStreamConfig);
  const kv = await js.views.kv(bucket, { bindOnly: true, allow_direct: false });
  const birth = {
    streamCreatedAt: commandStream.created,
    ownerStoreCreatedAt: ownerStream.created,
  };
  const initial = await kv.create(
    'authoritative',
    codec.encode({
      version: 1,
      streamGeneration: generation,
      phase: 'initializing',
      nodeBootId: null,
      birth,
    })
  );
  expect(initial).toBe(1);
  const proof = { ...birth, initializingRevision: String(initial) };
  const released: VoiceCommandOwnerRecord = {
    version: 1,
    streamGeneration: generation,
    phase: 'released',
    nodeBootId: null,
    proof,
  };
  await kv.update('authoritative', codec.encode(released), initial);
  return { manager, js, kv, proof, released };
}

async function realOwner(service: NatsService, boot = nodeBootId, now = () => performance.now()) {
  const transport = await service.getVoiceCommandTransport();
  expect(transport.streamGeneration).toBe(generation);
  const invalidated = vi.fn();
  const owner = new VoiceCommandOwner({
    store: transport.store,
    streamGeneration: transport.streamGeneration,
    nodeBootId: boot,
    now,
    onInvalidated: invalidated,
  });
  return { owner, transport, invalidated };
}

// JetStream's snapshot/restore wire protocol is stable in the pinned client.
// Only this fixture's isolated broker receives these administration requests.
async function snapshotOwnerStore(connection: NatsConnection) {
  const delivery = createInbox();
  const chunks: Uint8Array[] = [];
  const completion = deferred<Uint8Array>();
  const subscription = connection.subscribe(delivery, {
    callback(error, msg) {
      if (error) {
        completion.reject(error);
        return;
      }
      if (msg.data.length === 0) completion.resolve(Buffer.concat(chunks));
      else {
        chunks.push(msg.data.slice());
        msg.respond(new Uint8Array());
      }
    },
  });
  const deadline = setTimeout(
    () => completion.reject(new Error('isolated snapshot deadline')),
    5000
  );
  try {
    const response = await connection.request(
      `$JS.API.STREAM.SNAPSHOT.${ownerBackingStream}`,
      codec.encode({ deliver_subject: delivery, no_consumers: true, chunk_size: 32768 }),
      { timeout: 2000 }
    );
    const details = response.json<{
      config: unknown;
      state: unknown;
      error?: { description: string };
    }>();
    if (details.error) throw new Error(details.error.description);
    return {
      details: { config: details.config, state: details.state },
      bytes: await completion.promise,
    };
  } finally {
    clearTimeout(deadline);
    subscription.unsubscribe();
  }
}

async function restoreOwnerStore(
  connection: NatsConnection,
  snapshot: Awaited<ReturnType<typeof snapshotOwnerStore>>
) {
  const response = await connection.request(
    `$JS.API.STREAM.RESTORE.${ownerBackingStream}`,
    codec.encode(snapshot.details),
    { timeout: 2000 }
  );
  const restored = response.json<{ deliver_subject: string; error?: { description: string } }>();
  if (restored.error) throw new Error(restored.error.description);
  for (let offset = 0; offset < snapshot.bytes.length; offset += 32768) {
    await connection.request(
      restored.deliver_subject,
      snapshot.bytes.subarray(offset, offset + 32768),
      { timeout: 2000 }
    );
  }
  const completed = await connection.request(restored.deliver_subject, new Uint8Array(), {
    timeout: 2000,
  });
  const result = completed.json<{ error?: { description: string } }>();
  if (result.error) throw new Error(result.error.description);
}

describe('voice command consumer with real JetStream', () => {
  let binary: string;
  beforeAll(async () => {
    binary = await resolveNatsServerBinary();
  }, natsServerAcquisitionTimeoutMs);

  it('redelivers an unacked command serially and preserves the stable durable after service restart', async () => {
    const b = await broker(binary);
    const service = new NatsService();
    const restarted = new NatsService();
    try {
      mocked.url = b.url;
      mocked.connect.mockImplementation(b.connect);
      const { manager, js, kv } = await provision(b.connection);
      await service.connect();
      const first = await realOwner(service);
      const firstRelease = vi.spyOn(first.owner, 'release');
      const delivered: string[] = [];
      let rejectFirst = true;
      const dispatcher = {
        applyDurable: vi.fn(async (value: ReturnType<typeof command>) => {
          delivered.push(value.sequence);
          if (rejectFirst) {
            rejectFirst = false;
            throw new Error('first pause failed');
          }
          return 'target_absent';
        }),
      } as unknown as VoiceCommandDispatcher;
      const quarantine = vi.fn(async () => undefined);
      await service.startVoiceCommandConsumer({ owner: first.owner, dispatcher, quarantine });
      await js.publish('voice.commands.v1', codec.encode(command('1')));
      await js.publish('voice.commands.v1', codec.encode(command('2')));
      await vi.waitFor(() => expect(delivered).toEqual(['1']), { timeout: 5000 });
      const pending = await manager.consumers.info(stream, durable);
      expect(pending.num_ack_pending).toBe(1);
      expect(pending.num_pending).toBe(1);
      await service.stopVoiceCommandConsumer();
      expect(firstRelease).not.toHaveBeenCalled();
      // This fixture has no admitted media/registry rows. Explicit terminal close
      // is the release boundary; stopping the consumer alone never releases it.
      await first.owner.release();
      await service.close();
      expect((await manager.consumers.info(stream, durable)).config.max_ack_pending).toBe(1);
      await restarted.connect();
      const second = await realOwner(restarted, '77777777-7777-4777-8777-777777777777');
      const secondRelease = vi.spyOn(second.owner, 'release');
      await restarted.startVoiceCommandConsumer({ owner: second.owner, dispatcher, quarantine });
      await vi.waitFor(() => expect(delivered).toEqual(['1', '1', '2']), {
        timeout: 35_000,
        interval: 50,
      });
      await vi.waitFor(
        async () => expect((await manager.consumers.info(stream, durable)).num_ack_pending).toBe(0),
        { timeout: 5000 }
      );
      expect((await manager.streams.info(stream)).state.messages).toBe(2);
      await restarted.stopVoiceCommandConsumer();
      expect((await manager.consumers.info(stream, durable)).name).toBe(durable);
      expect(secondRelease).not.toHaveBeenCalled();
      expect(codec.decode((await kv.get('authoritative'))!.value)).toMatchObject({
        phase: 'held',
        nodeBootId: '77777777-7777-4777-8777-777777777777',
      });
    } finally {
      await Promise.allSettled([
        service.stopVoiceCommandConsumer?.(),
        restarted.stopVoiceCommandConsumer?.(),
      ]);
      await Promise.allSettled([service.close(), restarted.close()]);
      await b.close();
    }
  }, 50_000);

  it('refuses a second real owner after held ownership ages or its key disappears', async () => {
    const b = await broker(binary);
    const predecessor = new NatsService();
    const replacement = new NatsService();
    try {
      mocked.url = b.url;
      mocked.connect.mockImplementation(b.connect);
      const { kv } = await provision(b.connection);
      await predecessor.connect();
      await replacement.connect();
      let time = 0;
      const first = await realOwner(predecessor, nodeBootId, () => time);
      const second = await realOwner(
        replacement,
        '77777777-7777-4777-8777-777777777777',
        () => time
      );
      await first.owner.acquire();
      await expect(second.owner.acquire()).rejects.toThrow();
      time = 5001;
      expect(first.owner.valid()).toBe(false);
      const aged = await realOwner(replacement, '77777777-7777-4777-8777-777777777777', () => time);
      await expect(aged.owner.acquire()).rejects.toThrow();
      expect(codec.decode((await kv.get('authoritative'))!.value)).toMatchObject({
        phase: 'held',
        nodeBootId,
      });
      await kv.delete('authoritative');
      await expect(replacement.getVoiceCommandTransport()).rejects.toThrow();
      await expect(first.owner.recoverAfterTeardown()).rejects.toThrow();
      const deleted = await kv.get('authoritative');
      expect(deleted?.operation).toBe('DEL');
      expect(deleted?.revision).toBe(4);
      expect(deleted?.value.byteLength).toBe(0);
    } finally {
      await Promise.allSettled([predecessor.close(), replacement.close()]);
      await b.close();
    }
  });

  it('rejects same-revision bucket recreation and refuses changed birth even after predecessor stop and snapshot restore', async () => {
    const b = await broker(binary);
    const predecessor = new NatsService();
    const replacement = new NatsService();
    try {
      mocked.url = b.url;
      mocked.connect.mockImplementation(b.connect);
      const original = await provision(b.connection);
      await predecessor.connect();
      await replacement.connect();
      const first = await realOwner(predecessor);
      await first.owner.acquire();
      const saved = await snapshotOwnerStore(b.connection);
      await original.manager.streams.delete(ownerBackingStream);
      const newInfo = await original.manager.streams.add(ownerStreamConfig);
      const newKV = await original.js.views.kv(bucket, { bindOnly: true, allow_direct: false });
      expect(newInfo.created).not.toBe(original.proof.ownerStoreCreatedAt);
      const recreatedBirth = { ...original.proof, ownerStoreCreatedAt: newInfo.created };
      const recreatedInitial = await newKV.create(
        'authoritative',
        codec.encode({
          version: 1,
          streamGeneration: generation,
          phase: 'initializing',
          nodeBootId: null,
          birth: {
            streamCreatedAt: recreatedBirth.streamCreatedAt,
            ownerStoreCreatedAt: recreatedBirth.ownerStoreCreatedAt,
          },
        })
      );
      expect(recreatedInitial).toBe(1);
      // A delayed predecessor bootstrap can win the same numeric CAS, but its
      // release proof still identifies the deleted backing-stream incarnation.
      const reusedReleaseRevision = await newKV.update(
        'authoritative',
        codec.encode(original.released),
        recreatedInitial
      );
      expect(reusedReleaseRevision).toBe(2);
      await expect(first.owner.renew()).rejects.toThrow();
      expect(first.owner.valid()).toBe(false);
      await expect(replacement.getVoiceCommandTransport()).rejects.toThrow();
      expect(codec.decode((await newKV.get('authoritative'))!.value)).toEqual(original.released);
      await predecessor.close();
      await replacement.close();
      await original.manager.streams.delete(ownerBackingStream);
      await restoreOwnerStore(b.connection, saved);
      // Native snapshot RESTORE preserves records and revisions but assigns a
      // new Created identity. The committed old proof must remain invalid.
      expect((await original.manager.streams.info(ownerBackingStream)).created).not.toBe(
        original.proof.ownerStoreCreatedAt
      );
      const restoredKV = await original.js.views.kv(bucket, { bindOnly: true });
      const held = await restoredKV.get('authoritative');
      const heldRecord = codec.decode(held!.value) as VoiceCommandOwnerRecord;
      expect(heldRecord).toMatchObject({ phase: 'held', nodeBootId, proof: original.proof });
      await replacement.connect();
      await expect(replacement.getVoiceCommandTransport()).rejects.toThrow();
      expect(codec.decode((await restoredKV.get('authoritative'))!.value)).toEqual(heldRecord);
    } finally {
      await Promise.allSettled([predecessor.close(), replacement.close()]);
      await b.close();
    }
  }, 20_000);
});

describe('NatsService public continuity epoch fencing', () => {
  it('confirms exact owner and consumer continuity after a clean stop without restoring media readiness', async () => {
    const f = await ownershipLifecycleFixture();
    try {
      await f.start();
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.service.voiceCommandReady()).toBe(true);
      await f.service.confirmVoiceCommandContinuity();

      f.idle.resolve(null);
      await f.service.stopVoiceCommandConsumer();
      expect(f.service.voiceCommandReady()).toBe(false);
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.record().phase).toBe('held');

      const shutdownConfirmation = await f.service.confirmVoiceCommandContinuity().then(
        () => ({ status: 'confirmed' as const }),
        (error: Error) => ({ status: 'rejected' as const, error })
      );
      expect(
        shutdownConfirmation.status,
        'A cleanly stopped owner with unchanged exact proof must remain confirmable for shutdown release retries'
      ).toBe('confirmed');
      expect(f.service.voiceCommandReady()).toBe(false);

      f.actualOwner.invalidate();
      await expect(f.service.confirmVoiceCommandContinuity()).rejects.toThrow(
        'Voice command transport unavailable'
      );
      expect(f.service.voiceCommandReady()).toBe(false);
      expect(f.record().phase).toBe('held');
    } finally {
      await f.close();
    }
  });

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(0);
  });

  afterEach(() => {
    vi.clearAllTimers();
  });

  it('rejects a healthy late consumer response after same-boot owner recovery, then accepts a fresh healthy check', async () => {
    const f = await ownershipLifecycleFixture();
    const consumerRead = deferred<Awaited<ReturnType<typeof f.consumerInfo>>>();
    const consumerReadStarted = deferred<void>();
    const ownerReadStarted = deferred<void>();
    const ownerConfirmationFinished = deferred<void>();
    try {
      await f.start();
      await vi.waitFor(() => expect(f.next).toHaveBeenCalledWith({ expires: 2000 }));
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.service.voiceCommandReady()).toBe(true);
      const confirmActualOwner = f.actualOwner.confirmContinuity.bind(f.actualOwner);
      vi.spyOn(f.actualOwner, 'confirmContinuity').mockImplementation(async () => {
        await confirmActualOwner();
        ownerConfirmationFinished.resolve();
      });

      f.consumerInfo.mockImplementationOnce(() => {
        consumerReadStarted.resolve();
        return consumerRead.promise;
      });
      const readOwnerStore = f.kv.get.getMockImplementation()!;
      f.kv.get.mockImplementation(async () => {
        ownerReadStarted.resolve();
        return readOwnerStore();
      });
      const readsBefore = f.kv.get.mock.calls.length;
      const staleCheck = f.service.confirmVoiceCommandContinuity();
      await consumerReadStarted.promise;
      await ownerReadStarted.promise;
      await ownerConfirmationFinished.promise;
      expect(f.kv.get.mock.calls.length).toBeGreaterThan(readsBefore);
      const staleOutcome = staleCheck.then(
        () => ({ status: 'resolved' as const }),
        (error: Error) => ({ status: 'rejected' as const, error })
      );

      f.actualOwner.invalidate();
      expect(f.actualOwner.valid()).toBe(false);
      await f.actualOwner.recoverAfterTeardown();
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.service.voiceCommandReady()).toBe(true);

      consumerRead.resolve({
        stream_name: stream,
        name: durable,
        created: '2026-10-13T18:07:42.123456789Z',
        config: consumerConfig,
      });
      const outcome = await staleOutcome;
      expect(outcome.status).toBe('rejected');
      if (outcome.status === 'rejected')
        expect(outcome.error.message).toBe('Voice command transport unavailable');

      await expect(f.service.confirmVoiceCommandContinuity()).resolves.toBeUndefined();
      expect(f.actualOwner.valid()).toBe(true);
      expect(f.service.voiceCommandReady()).toBe(true);
    } finally {
      consumerRead.resolve({
        stream_name: stream,
        name: durable,
        created: '2026-10-13T18:07:42.123456789Z',
        config: consumerConfig,
      });
      await f.close();
    }
  });
});
