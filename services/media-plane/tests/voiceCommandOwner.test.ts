import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  decodeVoiceCommandOwnerRecord,
  VoiceCommandOwner,
  type VoiceCommandOwnerRecord,
  type VoiceCommandOwnerStore,
} from '../src/lib/voiceCommandOwner.js';

// Frozen constructor: {store, streamGeneration, nodeBootId, now, onInvalidated};
// streamGeneration is public readonly for the generation-bound consumer.
// acquire/renew/release return Promise<void>; valid is synchronous. The caller
// schedules renewals and must await terminal media teardown before calling
// recoverAfterTeardown. Recovery reconciles only this boot's existing held row.
// Released/held records carry an immutable proof of both resource births and the
// initializing KV revision. Initializing records carry birth only. The real NATS
// adapter verifies those births against broker state; this suite pins preservation
// and refusal to adopt another proof through the public ownership API.
const streamGeneration = '11111111-1111-4111-8111-111111111111';
const firstBoot = '55555555-5555-4555-8555-555555555555';
const secondBoot = '77777777-7777-4777-8777-777777777777';
const birth = {
  streamCreatedAt: '2026-10-13T18:07:41.123456789Z',
  ownerStoreCreatedAt: '2026-10-13T18:07:41.987654321Z',
};
const proof = { ...birth, initializingRevision: '1' };

function record(
  phase: 'initializing',
  nodeBootId?: string | null
): Extract<VoiceCommandOwnerRecord, { phase: 'initializing' }>;
function record(
  phase?: 'held' | 'released',
  nodeBootId?: string | null
): Extract<VoiceCommandOwnerRecord, { phase: 'held' | 'released' }>;
function record(
  phase: VoiceCommandOwnerRecord['phase'] = 'released',
  nodeBootId: string | null = null
): VoiceCommandOwnerRecord {
  const identity = { version: 1 as const, streamGeneration, nodeBootId };
  return phase === 'initializing'
    ? { ...identity, phase, birth: { ...birth } }
    : { ...identity, phase, proof: { ...proof } };
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

// This double models the external KV atomic CAS, including remote writes whose
// acknowledgements are lost. Assertions use the resulting durable row and local
// authorization state, rather than assuming a successful call implies ownership.
class OwnerStore implements VoiceCommandOwnerStore {
  value: VoiceCommandOwnerRecord | null = record();
  revision = 2;
  beforeRead: (() => Promise<void>) | undefined;
  beforeWrite: (() => Promise<void>) | undefined;
  afterWrite: (() => Promise<void>) | undefined;
  readonly reads: number[] = [];
  readonly writes: number[] = [];

  async read() {
    this.reads.push(this.revision);
    await this.beforeRead?.();
    return this.value === null
      ? null
      : { value: structuredClone(this.value), revision: this.revision };
  }

  async compareAndSet(revision: number, value: VoiceCommandOwnerRecord): Promise<number> {
    this.writes.push(revision);
    await this.beforeWrite?.();
    if (this.value === null || revision !== this.revision) throw new Error('fixture CAS conflict');
    this.value = structuredClone(value);
    const acknowledgedRevision = ++this.revision;
    await this.afterWrite?.();
    return acknowledgedRevision;
  }
}

function owner(store: OwnerStore, boot = firstBoot, onInvalidated = vi.fn()) {
  return {
    instance: new VoiceCommandOwner({
      store,
      streamGeneration,
      nodeBootId: boot,
      now: () => Date.now(),
      onInvalidated,
    }),
    onInvalidated,
  };
}

describe('decodeVoiceCommandOwnerRecord shared adapter boundary', () => {
  it.each([
    ['initializing', record('initializing')],
    ['held', record('held', firstBoot)],
    ['released', record('released', firstBoot)],
  ])('accepts %s evidence with exact nanosecond birth strings', (_name, value) => {
    expect(decodeVoiceCommandOwnerRecord(value, streamGeneration)).toEqual(value);
  });

  it('preserves the largest PostgreSQL BIGINT initializing revision', () => {
    const value = {
      ...record(),
      proof: { ...proof, initializingRevision: '9223372036854775807' },
    };

    expect(decodeVoiceCommandOwnerRecord(value, streamGeneration).proof?.initializingRevision).toBe(
      '9223372036854775807'
    );
  });

  it.each([
    ['malformed value', 'fixture-private-payload', streamGeneration],
    ['unknown field', { ...record(), 'fixture-private-field': true }, streamGeneration],
    ['wrong record generation', { ...record(), streamGeneration: secondBoot }, streamGeneration],
    ['invalid expected generation', record(), 'fixture-private-generation'],
    ['BIGINT overflow', { ...record(), proof: { ...proof, initializingRevision: '9223372036854775808' } }, streamGeneration],
  ])('rejects %s with a fixed safe error', (_name, value, expectedGeneration) => {
    expect(() => decodeVoiceCommandOwnerRecord(value, expectedGeneration as string)).toThrow(
      /^Voice command ownership unavailable$/
    );
  });
});

describe('VoiceCommandOwner permanent single-process authority', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(0);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('starts invalid and becomes valid only after released-to-held CAS succeeds', async () => {
    const store = new OwnerStore();
    const { instance } = owner(store);
    expect(instance.valid()).toBe(false);

    await instance.acquire();

    expect(store.value).toEqual(record('held', firstBoot));
    expect(store.revision).toBe(3);
    expect(instance.valid()).toBe(true);
  });

  it('confirms the exact held proof with a fresh read without CAS, revision advance, or validity extension', async () => {
    const store = new OwnerStore();
    const { instance } = owner(store);
    await instance.acquire();
    const held = structuredClone(store.value);
    const revision = store.revision;
    const writes = [...store.writes];
    await vi.advanceTimersByTimeAsync(4000);

    await instance.confirmContinuity();

    expect(store.reads).toEqual([2, revision]);
    expect(store.writes).toEqual(writes);
    expect(store.revision).toBe(revision);
    expect(store.value).toEqual(held);
    expect(instance.valid()).toBe(true);
    await vi.advanceTimersByTimeAsync(1000);
    expect(instance.valid()).toBe(false);
  });

  it.each([
    ['absent ownership', (store: OwnerStore) => (store.value = null)],
    ['read failure', (store: OwnerStore) => (store.beforeRead = async () => { throw new Error('fixture read unavailable'); })],
  ])('invalidates authority when fresh continuity sees %s', async (_name, alterStore) => {
    const store = new OwnerStore();
    const { instance, onInvalidated } = owner(store);
    await instance.acquire();
    alterStore(store);
    const writes = [...store.writes];

    await expect(instance.confirmContinuity()).rejects.toThrow(
      /^Voice command ownership unavailable$/
    );

    expect(instance.valid()).toBe(false);
    expect(onInvalidated).toHaveBeenCalledOnce();
    expect(store.writes).toEqual(writes);
  });

  it.each([
    ['same-boot revision change', (store: OwnerStore) => store.revision++],
    [
      'same-boot proof replacement',
      (store: OwnerStore) => {
        store.value = { ...record('held', firstBoot), proof: { ...proof, initializingRevision: '2' } };
      },
    ],
  ])('invalidates fresh continuity on %s', async (_name, alterStore) => {
    const store = new OwnerStore();
    const { instance, onInvalidated } = owner(store);
    await instance.acquire();
    alterStore(store);
    const value = structuredClone(store.value);
    const revision = store.revision;
    const writes = [...store.writes];

    await expect(instance.confirmContinuity()).rejects.toThrow(
      /^Voice command ownership unavailable$/
    );

    expect(instance.valid()).toBe(false);
    expect(onInvalidated).toHaveBeenCalledOnce();
    expect(store.value).toEqual(value);
    expect(store.revision).toBe(revision);
    expect(store.writes).toEqual(writes);
  });

  it('does not let an in-flight continuity read restore authority after invalidation, while recovery uses its own epoch', async () => {
    const store = new OwnerStore();
    const { instance } = owner(store);
    await instance.acquire();
    const reading = deferred<void>();
    store.beforeRead = () => reading.promise;
    const confirmation = instance.confirmContinuity();
    await vi.waitFor(() => expect(store.reads).toHaveLength(2));

    instance.invalidate();
    const recovery = instance.recoverAfterTeardown();
    expect(instance.valid()).toBe(false);
    reading.resolve();

    await expect(confirmation).rejects.toThrow(/^Voice command ownership unavailable$/);
    await recovery;
    expect(instance.valid()).toBe(true);
    expect(store.writes).toEqual([2, 3]);
    expect(store.value).toEqual(record('held', firstBoot));
  });

  it('bounds a stalled fresh continuity read and ignores its eventual success', async () => {
    const store = new OwnerStore();
    const { instance, onInvalidated } = owner(store);
    await instance.acquire();
    const reading = deferred<void>();
    store.beforeRead = () => reading.promise;
    const outcome = instance.confirmContinuity().then(
      () => 'unexpected success',
      () => 'rejected'
    );

    await vi.advanceTimersByTimeAsync(2000);
    expect(await outcome).toBe('rejected');
    expect(instance.valid()).toBe(false);
    reading.resolve();
    await vi.advanceTimersByTimeAsync(0);

    expect(instance.valid()).toBe(false);
    expect(onInvalidated).toHaveBeenCalledOnce();
    expect(store.writes).toEqual([2]);
  });

  it('admits exactly one contender to the same released revision', async () => {
    const store = new OwnerStore();
    const first = owner(store).instance;
    const second = owner(store, secondBoot).instance;
    const outcomes = await Promise.allSettled([first.acquire(), second.acquire()]);

    expect(outcomes.filter((outcome) => outcome.status === 'fulfilled')).toHaveLength(1);
    expect(outcomes.filter((outcome) => outcome.status === 'rejected')).toHaveLength(1);
    expect([first.valid(), second.valid()].filter(Boolean)).toHaveLength(1);
    expect(store.value?.phase).toBe('held');
    expect(store.revision).toBe(3);
  });

  it('refuses replacement after the predecessor local validity expires', async () => {
    const store = new OwnerStore();
    const first = owner(store);
    const second = owner(store, secondBoot).instance;
    await first.instance.acquire();

    await vi.advanceTimersByTimeAsync(5001);

    expect(first.instance.valid()).toBe(false);
    await expect(second.acquire()).rejects.toThrow();
    expect(second.valid()).toBe(false);
    expect(store.value).toEqual(record('held', firstBoot));
    expect(first.onInvalidated).toHaveBeenCalledOnce();
  });

  it.each([
    ['initializing', record('initializing')],
    ['another boot held', record('held', secondBoot)],
    ['this boot held before teardown', record('held', firstBoot)],
    ['missing key', null],
    ['unknown generation', { ...record(), streamGeneration: secondBoot }],
    ['unsupported version', { ...record(), version: 2 }],
    ['unknown phase', { ...record(), phase: 'expired' }],
    ['held without a boot', record('held')],
    ['initializing with a boot', record('initializing', firstBoot)],
    ['malformed boot', record('released', 'not-a-uuid')],
    ['unknown field', { ...record(), expiresAt: 0 }],
    ['primitive record', 'held'],
    ['missing version', { streamGeneration, phase: 'released', nodeBootId: null }],
  ])('refuses %s without changing permanent ownership', async (_name, value) => {
    const store = new OwnerStore();
    store.value = value as VoiceCommandOwnerRecord | null;
    const previous = structuredClone(value);
    const { instance } = owner(store);

    await expect(instance.acquire()).rejects.toThrow();

    expect(instance.valid()).toBe(false);
    expect(store.value).toEqual(previous);
    expect(store.writes).toEqual([]);
  });

  it.each([
    ['missing proof', undefined],
    ['null proof', null],
    ['primitive proof', 'proof'],
    ['missing stream birth', { ownerStoreCreatedAt: birth.ownerStoreCreatedAt, initializingRevision: '1' }],
    ['missing owner-store birth', { streamCreatedAt: birth.streamCreatedAt, initializingRevision: '1' }],
    ['missing initializing revision', { ...birth }],
    ['non-UTC stream birth', { ...proof, streamCreatedAt: '2026-10-13T18:07:41.123456789+00:00' }],
    ['invalid owner-store date', { ...proof, ownerStoreCreatedAt: '2026-02-30T18:07:41.987654321Z' }],
    ['excess timestamp precision', { ...proof, streamCreatedAt: '2026-10-13T18:07:41.1234567891Z' }],
    ['numeric initializing revision', { ...proof, initializingRevision: 1 }],
    ['empty initializing revision', { ...proof, initializingRevision: '' }],
    ['zero initializing revision', { ...proof, initializingRevision: '0' }],
    ['leading-zero revision', { ...proof, initializingRevision: '01' }],
    ['fractional revision', { ...proof, initializingRevision: '1.0' }],
    ['exponential revision', { ...proof, initializingRevision: '1e3' }],
    ['PostgreSQL BIGINT overflow', { ...proof, initializingRevision: '9223372036854775808' }],
    ['unknown proof field', { ...proof, expiresAt: 0 }],
  ])('refuses released ownership with %s before any CAS', async (_name, invalidProof) => {
    const store = new OwnerStore();
    const invalidRecord = { ...record(), proof: invalidProof };
    store.value = invalidRecord as VoiceCommandOwnerRecord;
    const { instance } = owner(store);

    await expect(instance.acquire()).rejects.toThrow();

    expect(instance.valid()).toBe(false);
    expect(store.value).toEqual(invalidRecord);
    expect(store.writes).toEqual([]);
  });

  it('refuses a released row carrying only initializing birth evidence', async () => {
    const store = new OwnerStore();
    const invalidRecord = { ...record('initializing'), phase: 'released' };
    store.value = invalidRecord as VoiceCommandOwnerRecord;
    const { instance } = owner(store);

    await expect(instance.acquire()).rejects.toThrow();

    expect(instance.valid()).toBe(false);
    expect(store.value).toEqual(invalidRecord);
    expect(store.writes).toEqual([]);
  });

  it('preserves complete nanosecond proof through acquisition, renewal, recovery and release', async () => {
    const store = new OwnerStore();
    const { instance } = owner(store);
    const expectedProof = {
      streamCreatedAt: '2026-10-13T18:07:41.123456789Z',
      ownerStoreCreatedAt: '2026-10-13T18:07:41.987654321Z',
      initializingRevision: '1',
    };
    await instance.acquire();
    expect(store.value?.proof).toEqual(expectedProof);

    await instance.renew();
    expect(store.value?.proof).toEqual(expectedProof);

    await vi.advanceTimersByTimeAsync(5000);
    expect(instance.valid()).toBe(false);
    await instance.recoverAfterTeardown();
    expect(store.value?.proof).toEqual(expectedProof);

    await instance.release();
    expect(store.value).toEqual({
      version: 1,
      streamGeneration,
      phase: 'released',
      nodeBootId: firstBoot,
      proof: expectedProof,
    });
  });

  it.each([
    ['stream birth', { ...proof, streamCreatedAt: '2026-10-13T18:07:41.123456788Z' }],
    ['owner-store birth', { ...proof, ownerStoreCreatedAt: '2026-10-13T18:07:41.987654320Z' }],
    ['initializing revision', { ...proof, initializingRevision: '2' }],
  ])('refuses renewal when the acquired %s proof changed for the same boot', async (_name, changedProof) => {
    const store = new OwnerStore();
    const { instance, onInvalidated } = owner(store);
    await instance.acquire();
    const substituted = { ...record('held', firstBoot), proof: changedProof };
    store.value = substituted;
    const revision = store.revision;

    await expect(instance.renew()).rejects.toThrow();

    expect(instance.valid()).toBe(false);
    expect(onInvalidated).toHaveBeenCalledOnce();
    expect(store.value).toEqual(substituted);
    expect(store.revision).toBe(revision);
  });

  it.each([
    ['stream birth', { ...proof, streamCreatedAt: '2026-10-13T18:07:41.123456788Z' }],
    ['owner-store birth', { ...proof, ownerStoreCreatedAt: '2026-10-13T18:07:41.987654320Z' }],
    ['initializing revision', { ...proof, initializingRevision: '2' }],
  ])('refuses expired same-boot recovery that would adopt another %s proof', async (_name, changedProof) => {
    const store = new OwnerStore();
    const { instance } = owner(store);
    await instance.acquire();
    await vi.advanceTimersByTimeAsync(5000);
    expect(instance.valid()).toBe(false);
    const substituted = { ...record('held', firstBoot), proof: changedProof };
    store.value = substituted;
    const revision = store.revision;

    await expect(instance.recoverAfterTeardown()).rejects.toThrow();

    expect(instance.valid()).toBe(false);
    expect(store.value).toEqual(substituted);
    expect(store.revision).toBe(revision);
  });

  it.each([0, -1, Number.NaN, Number.MAX_SAFE_INTEGER + 1])(
    'refuses unsafe KV revision %j before CAS',
    async (revision) => {
      const store = new OwnerStore();
      store.revision = revision;
      const { instance } = owner(store);

      await expect(instance.acquire()).rejects.toThrow();

      expect(instance.valid()).toBe(false);
      expect(store.value).toEqual(record());
      expect(store.writes).toEqual([]);
    }
  );

  it('refuses acquisition when the ownership bucket cannot be read', async () => {
    const store = new OwnerStore();
    store.beforeRead = async () => {
      throw new Error('fixture bucket unavailable');
    };
    const { instance } = owner(store);

    await expect(instance.acquire()).rejects.toThrow();

    expect(instance.valid()).toBe(false);
    expect(store.value).toEqual(record());
    expect(store.writes).toEqual([]);
  });

  it('bounds validity from renewal request start instead of delayed acknowledgement', async () => {
    const store = new OwnerStore();
    const { instance } = owner(store);
    await instance.acquire();
    await vi.advanceTimersByTimeAsync(1000);
    const acknowledgement = deferred<void>();
    store.afterWrite = () => acknowledgement.promise;
    const renewing = instance.renew();
    await vi.advanceTimersByTimeAsync(1500);
    acknowledgement.resolve();
    await renewing;

    expect(instance.valid()).toBe(true);
    await vi.advanceTimersByTimeAsync(3499);
    expect(instance.valid()).toBe(true);
    await vi.advanceTimersByTimeAsync(1);
    expect(instance.valid()).toBe(false);
  });

  it('serializes concurrent renewals against acknowledged CAS revisions', async () => {
    const store = new OwnerStore();
    const { instance } = owner(store);
    await instance.acquire();
    const acknowledgement = deferred<void>();
    store.afterWrite = () => acknowledgement.promise;
    const first = instance.renew();
    const second = instance.renew();
    await vi.advanceTimersByTimeAsync(0);

    expect(store.writes).toEqual([2, 3]);
    acknowledgement.resolve();
    await Promise.all([first, second]);

    expect(instance.valid()).toBe(true);
    expect(store.writes.every((revision, index) => revision === index + 2)).toBe(true);
    expect(store.value).toEqual(record('held', firstBoot));
  });

  it.each(['acquire', 'renew'] as const)(
    'times out %s after two seconds and ignores an eventual remote success',
    async (method) => {
      const store = new OwnerStore();
      const { instance } = owner(store);
      if (method === 'renew') await instance.acquire();
      const remoteWrite = deferred<void>();
      store.beforeWrite = () => remoteWrite.promise;
      const outcome = instance[method]().then(
        () => 'unexpected success',
        () => 'rejected'
      );
      await vi.advanceTimersByTimeAsync(2000);

      expect(await outcome).toBe('rejected');
      expect(instance.valid()).toBe(false);
      remoteWrite.resolve();
      await vi.advanceTimersByTimeAsync(0);
      expect(store.value).toEqual(record('held', firstBoot));
      expect(instance.valid()).toBe(false);
    }
  );

  it.each(['acquire', 'renew'] as const)(
    'refuses late %s success when the API timeout callback has not run yet',
    async (method) => {
      const store = new OwnerStore();
      const { instance } = owner(store);
      if (method === 'renew') await instance.acquire();
      store.beforeWrite = async () => {
        // Move the injected monotonic clock without servicing pending timers,
        // modeling an event loop that returns to promise work before its timer.
        vi.setSystemTime(2001);
      };

      await expect(instance[method]()).rejects.toThrow();

      expect(instance.valid()).toBe(false);
      expect(store.value).toEqual(record('held', firstBoot));
    }
  );

  it('cannot restore authority from a late CAS acknowledgement after explicit invalidation', async () => {
    const store = new OwnerStore();
    const { instance, onInvalidated } = owner(store);
    await instance.acquire();
    const acknowledgement = deferred<void>();
    store.afterWrite = () => acknowledgement.promise;
    const outcome = instance.renew().then(
      () => 'unexpected success',
      () => 'rejected'
    );
    await vi.advanceTimersByTimeAsync(0);

    instance.invalidate();
    expect(instance.valid()).toBe(false);
    expect(onInvalidated).toHaveBeenCalledOnce();
    acknowledgement.resolve();

    expect(await outcome).toBe('rejected');
    expect(instance.valid()).toBe(false);
    expect(store.value).toEqual(record('held', firstBoot));
    expect(onInvalidated).toHaveBeenCalledOnce();
  });

  it('bounds a stalled initial KV read without creating an owner', async () => {
    const store = new OwnerStore();
    const reading = deferred<void>();
    store.beforeRead = () => reading.promise;
    const { instance } = owner(store);
    const outcome = instance.acquire().then(
      () => 'unexpected success',
      () => 'rejected'
    );

    await vi.advanceTimersByTimeAsync(2000);
    expect(await outcome).toBe('rejected');
    reading.resolve();
    await vi.advanceTimersByTimeAsync(0);

    expect(instance.valid()).toBe(false);
    expect(store.value).toEqual(record());
    expect(store.writes).toEqual([]);
  });

  it.each(['connection failure', 'missing bucket', 'missing key', 'CAS conflict'])(
    'invalidates admission immediately on renewal %s',
    async (failure) => {
      const store = new OwnerStore();
      const { instance, onInvalidated } = owner(store);
      await instance.acquire();
      if (failure === 'missing key') store.value = null;
      else if (failure === 'CAS conflict') store.revision++;
      else {
        store.beforeWrite = async () => {
          throw new Error(failure);
        };
      }

      await expect(instance.renew()).rejects.toThrow();

      expect(instance.valid()).toBe(false);
      expect(onInvalidated).toHaveBeenCalledOnce();
      const uncertainState = structuredClone(store.value);
      await expect(instance.renew()).rejects.toThrow();
      expect(store.value).toEqual(uncertainState);
    }
  );

  it('reconciles a lost renewal acknowledgement only after caller teardown', async () => {
    const store = new OwnerStore();
    const { instance } = owner(store);
    await instance.acquire();
    store.afterWrite = async () => {
      throw new Error('fixture lost acknowledgement');
    };
    await expect(instance.renew()).rejects.toThrow();
    expect(store.revision).toBe(4);
    expect(instance.valid()).toBe(false);
    store.afterWrite = undefined;

    await instance.recoverAfterTeardown();

    expect(store.value).toEqual(record('held', firstBoot));
    expect(store.revision).toBe(5);
    expect(instance.valid()).toBe(true);
  });

  it.each([
    ['another boot held', record('held', secondBoot)],
    ['released', record('released', firstBoot)],
    ['initializing', record('initializing')],
    ['missing', null],
    ['wrong generation', { ...record('held', firstBoot), streamGeneration: secondBoot }],
  ])('refuses teardown recovery of %s without resetting ownership', async (_name, value) => {
    const store = new OwnerStore();
    const { instance } = owner(store);
    await instance.acquire();
    await vi.advanceTimersByTimeAsync(5000);
    expect(instance.valid()).toBe(false);
    store.value = value;
    const previous = structuredClone(value);
    const previousRevision = store.revision;

    await expect(instance.recoverAfterTeardown()).rejects.toThrow();

    expect(instance.valid()).toBe(false);
    expect(store.value).toEqual(previous);
    expect(store.revision).toBe(previousRevision);
  });

  it('gracefully releases by CAS while retaining predecessor boot identity', async () => {
    const store = new OwnerStore();
    const first = owner(store).instance;
    await first.acquire();

    await first.release();

    expect(first.valid()).toBe(false);
    expect(store.value).toEqual(record('released', firstBoot));
    const second = owner(store, secondBoot).instance;
    await second.acquire();
    expect(second.valid()).toBe(true);
    expect(store.value).toEqual(record('held', secondBoot));
  });

  it('retains permanent held ownership when shutdown release fails', async () => {
    const store = new OwnerStore();
    const first = owner(store).instance;
    await first.acquire();
    store.beforeWrite = async () => {
      throw new Error('fixture release failure');
    };

    await expect(first.release()).rejects.toThrow();

    expect(first.valid()).toBe(false);
    expect(store.value).toEqual(record('held', firstBoot));
    store.beforeWrite = undefined;
    await expect(owner(store, secondBoot).instance.acquire()).rejects.toThrow();
  });

  it('does not release a record whose revision changed after the last renewal', async () => {
    const store = new OwnerStore();
    const { instance } = owner(store);
    await instance.acquire();
    store.revision++;

    await expect(instance.release()).rejects.toThrow();

    expect(store.value).toEqual(record('held', firstBoot));
    expect(instance.valid()).toBe(false);
  });
});
