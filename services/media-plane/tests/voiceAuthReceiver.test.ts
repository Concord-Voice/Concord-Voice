import { readFileSync } from 'node:fs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';

const transport = vi.hoisted(() => ({
  subscribe: vi.fn(), flush: vi.fn().mockResolvedValue(undefined),
  drain: vi.fn().mockResolvedValue(undefined), isClosed: vi.fn(() => false),
  status: vi.fn(() => ({ async *[Symbol.asyncIterator]() { await new Promise(() => {}); yield { type: 'update' }; } })),
  decode: vi.fn((raw: Uint8Array) => JSON.parse(Buffer.from(raw).toString())),
  encode: vi.fn((value: unknown) => Buffer.from(JSON.stringify(value))),
}));
vi.mock('nats', async (original) => ({
  ...await original<typeof import('nats')>(),
  connect: vi.fn(async () => transport),
  JSONCodec: () => ({ encode: transport.encode, decode: transport.decode }),
}));
vi.mock('../src/config/index.js', () => ({ config: { natsUrl: 'nats://test.invalid:4222' } }));
import { NatsService } from '../src/lib/nats.js';
import { createVoiceEnforcementSessionEjectionHandler } from '../src/lib/forceDisconnect.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import { createVoiceAuthReceiver } from '../src/lib/voiceAuthReceiver.js';

interface Vector { id: string; rawHex: string; verifierUnixSeconds: string }
const corpus = JSON.parse(readFileSync(new URL('../../control-plane/internal/api/testdata/auth_connection_protocol.json', import.meta.url), 'utf8')) as { testOnlySecretHex: string; records: Vector[] };
const secret = Buffer.from(corpus.testOnlySecretHex, 'hex');
const boot = '00000002-1111-4111-8111-111111111111';
const subject = `voice.enforce.session.${boot}`;
function raw(id = 'eject-session-logout'): Buffer {
  const vector = corpus.records.find(record => record.id === id);
  if (!vector) throw new Error(`Missing raw vector ${id}`);
  return Buffer.from(vector.rawHex, 'hex');
}
function deferred() {
  let resolve!: () => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<void>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
async function settle() { for (let i = 0; i < 12; i++) await Promise.resolve(); }

// These are Task 5a adapter contracts. They deliberately do not pretend to
// implement the Task 5b ownership inventory or Task 6b readiness snapshots.
function adapters() {
  return {
    fenceAndJoin: vi.fn<(command: protocol.Command) => Promise<void> | undefined>(),
    healthSnapshot: vi.fn<(command: protocol.Command) => boolean>(() => false),
  };
}
function manager() {
  return {
    getParticipant: vi.fn(() => undefined), getProvisionalParticipant: vi.fn(() => undefined),
    leaveRoomIfSocketOwned: vi.fn(async () => false),
    removeProvisionalParticipantForEnforcement: vi.fn(async () => false),
    awaitNativeTermination: vi.fn(async () => {}),
  };
}
type Callback = (error: Error | null, message: { data: Uint8Array; reply: string; respond: ReturnType<typeof vi.fn> }) => void;
let service: NatsService;
let callback: Callback;
let closed: boolean;
let end: () => void;
let iterator: ReturnType<typeof vi.fn>;
async function install(hooks = adapters(), localBoot = boot) {
  const roomManager = manager();
  const legacyHealth = vi.fn(async () => {});
  const handler = createVoiceEnforcementSessionEjectionHandler(roomManager, { sockets: { sockets: new Map() } }, secret.toString(), localBoot, legacyHealth, hooks);
  await service.connect();
  expect(service.subscribeRequest(subject, handler, { required: true })).toBe(true);
  const options = transport.subscribe.mock.calls.at(-1)?.[1] as { callback: Callback };
  callback = options.callback;
  expect(typeof callback).toBe('function');
  return { hooks, roomManager, legacyHealth };
}
function deliver(data: Uint8Array = raw(), reply = '_INBOX.test') {
  const respond = vi.fn();
  expect(callback(null, { data, reply, respond })).toBeUndefined();
  return respond;
}
beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date(1793000000_000));
  vi.clearAllMocks();
  closed = false;
  iterator = vi.fn(() => ({ next: () => new Promise(() => {}) }));
  transport.subscribe.mockImplementation(() => ({
    closed: new Promise<void>(resolve => { end = resolve; }),
    isClosed: () => closed,
    unsubscribe: () => { closed = true; end(); },
    [Symbol.asyncIterator]: iterator,
  }));
  service = new NatsService();
});
afterEach(async () => { await service.close(); vi.useRealTimers(); });

describe('exact NATS receiver at the raw callback boundary', () => {
  it('uses one required synchronous callback and answers only after retained terminal evidence', async () => {
    const task = deferred();
    const hooks = adapters(); hooks.fenceAndJoin.mockReturnValue(task.promise);
    await install(hooks);
    const respond = deliver();
    expect(hooks.fenceAndJoin).toHaveBeenCalledTimes(1);
    expect(respond).not.toHaveBeenCalled();
    expect(transport.subscribe).toHaveBeenCalledTimes(1);
    expect(iterator).not.toHaveBeenCalled();
    expect(transport.decode).not.toHaveBeenCalled();
    task.resolve(); await settle();
    expect(respond).toHaveBeenCalledTimes(1);
    const ack = protocol.verifyAck(respond.mock.calls[0][0], protocol.decodeCommand(raw()), secret, 1793000000n);
    expect(ack.auth()?.ok).toBe(true);
    expect(service.isRequestSubscriptionHealthy(subject)).toBe(true);
  });

  it.each(['credential-eject', 'health-prepared-false-false', 'health-active-true-true'])(
    'dispatches and signs the real %s codec variant', async id => {
      const hooks = adapters(); hooks.fenceAndJoin.mockResolvedValue(undefined);
      hooks.healthSnapshot.mockReturnValue(id === 'health-active-true-true');
      const { legacyHealth } = await install(hooks);
      const respond = deliver(raw(id)); await settle();
      expect(respond).toHaveBeenCalledTimes(1);
      const ack = protocol.verifyAck(respond.mock.calls[0][0], protocol.decodeCommand(raw(id)), secret, 1793000000n);
      if (id === 'credential-eject') expect(ack.epoch()?.ok).toBe(true);
      else expect(ack.health()?.admissionGranted).toBe(id === 'health-active-true-true');
      expect(legacyHealth).not.toHaveBeenCalled();
    }
  );

  it.each(['raw-duplicate-version', 'raw-escaped-duplicate-version', 'raw-nested-escaped-duplicate-boot', 'raw-invalid-utf8', 'raw-trailing-value', 'raw-unknown-field', 'raw-null', 'raw-missing-version', 'raw-unknown-schema', 'raw-health-spoof-unknown-schema', 'raw-wrong-version', 'proof-wrong-domain', 'proof-wrong-boot', 'proof-wrong-generation', 'proof-wrong-family', 'proof-wrong-nonce', 'credential-eject-superseded-target-mismatch'])(
    'rejects %s before adapters, legacy routing or a reply', async id => {
      const { hooks, roomManager, legacyHealth } = await install();
      const respond = deliver(raw(id)); await settle();
      expect(hooks.fenceAndJoin).not.toHaveBeenCalled(); expect(hooks.healthSnapshot).not.toHaveBeenCalled();
      expect(roomManager.getParticipant).not.toHaveBeenCalled(); expect(legacyHealth).not.toHaveBeenCalled();
      expect(respond).not.toHaveBeenCalled();
    }
  );

  it('rejects a valid signed request for a different local boot before fencing', async () => {
    const { hooks } = await install(adapters(), '00000009-1111-4111-8111-111111111111');
    const respond = deliver(); await settle();
    expect(hooks.fenceAndJoin).not.toHaveBeenCalled(); expect(respond).not.toHaveBeenCalled();
  });

  it('accepts 4096 raw bytes but refuses 4097 bytes before decoding or effects', async () => {
    const hooks = adapters(); hooks.fenceAndJoin.mockResolvedValue(undefined); await install(hooks);
    const padded = Buffer.concat([raw(), Buffer.alloc(4096 - raw().length, 32)]);
    const accepted = deliver(padded); await settle(); expect(accepted).toHaveBeenCalledTimes(1);
    hooks.fenceAndJoin.mockClear();
    // An over-cap typed array cannot be read: observing any byte proves that
    // parsing preceded the required cap check.
    let overCapReads = 0;
    const oversized = new Proxy(new Uint8Array(4097), {
      get(_target, key) { if (key === 'length' || key === 'byteLength') return 4097; overCapReads++; throw new Error(`Read over-cap payload: ${String(key)}`); },
    });
    const refused = deliver(oversized); await settle();
    expect(overCapReads).toBe(0);
    expect(refused).not.toHaveBeenCalled(); expect(hooks.fenceAndJoin).not.toHaveBeenCalled();
  });

  it('never falls back from a present new schema to legacy kind health', async () => {
    const { hooks, legacyHealth } = await install();
    for (const schema of ['concord.auth-revocation', 'unknown', null]) {
      const respond = deliver(Buffer.from(JSON.stringify({ ...JSON.parse(raw('health-prepared-false-false').toString()), schema, kind: 'health', version: 2 })));
      expect(respond).not.toHaveBeenCalled();
    }
    await settle(); expect(legacyHealth).not.toHaveBeenCalled(); expect(hooks.healthSnapshot).not.toHaveBeenCalled();
  });

  it('caps terminal waiters at 63 without queuing requests and serves health while native work hangs', async () => {
    const task = deferred(); const hooks = adapters(); hooks.fenceAndJoin.mockReturnValue(task.promise); await install(hooks);
    const replies = Array.from({ length: 63 }, () => deliver());
    const timerCount = vi.getTimerCount();
    const refused = Array.from({ length: 300 }, () => deliver());
    expect(hooks.fenceAndJoin).toHaveBeenCalledTimes(363); // saturation still fences synchronously
    expect(vi.getTimerCount()).toBe(timerCount);
    const health = deliver(raw('health-prepared-false-false')); await settle();
    expect(health).toHaveBeenCalledTimes(1);
    expect(protocol.verifyAck(health.mock.calls[0][0], protocol.decodeCommand(raw('health-prepared-false-false')), secret, 1793000000n).health()?.admissionGranted).toBe(false);
    task.resolve(); await settle();
    for (const respond of replies) expect(respond).toHaveBeenCalledTimes(1);
    for (const respond of refused) expect(respond).not.toHaveBeenCalled();
    expect(iterator).not.toHaveBeenCalled();
  });

  it.each(['resolve', 'reject', 'timeout'] as const)('releases waiter capacity after %s without late or negative success', async outcome => {
    const first = deferred(); const next = deferred(); const hooks = adapters(); hooks.fenceAndJoin.mockReturnValue(first.promise); await install(hooks);
    const replies = Array.from({ length: 63 }, () => deliver());
    if (outcome === 'resolve') first.resolve();
    else if (outcome === 'reject') first.reject(new Error('native proof unavailable'));
    else await vi.advanceTimersByTimeAsync(5000);
    await settle();
    hooks.fenceAndJoin.mockReturnValue(next.promise);
    const admitted = deliver(); next.resolve(); await settle();
    expect(admitted).toHaveBeenCalledTimes(1);
    if (outcome !== 'resolve') for (const respond of replies) expect(respond).not.toHaveBeenCalled();
    first.resolve(); await settle();
    if (outcome === 'timeout') for (const respond of replies) expect(respond).not.toHaveBeenCalled();
  });

  it('repeated deadlines retain one observer per hung identity and never revive expired inboxes', async () => {
    const task = deferred(); const observe = vi.spyOn(task.promise, 'then');
    const hooks = adapters();
    // A plain adapter avoids Vitest's own Promise observation on mock returns.
    hooks.fenceAndJoin = (() => task.promise) as typeof hooks.fenceAndJoin;
    await install(hooks);
    const expired: ReturnType<typeof vi.fn>[] = [];
    for (let round = 0; round < 3; round++) {
      vi.setSystemTime(new Date(1793000000_000));
      expired.push(...Array.from({ length: 63 }, () => deliver()));
      const timers = vi.getTimerCount();
      for (let overflow = 0; overflow < 100; overflow++) deliver();
      expect(vi.getTimerCount()).toBe(timers);
      expect(observe).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(5000);
    }
    vi.setSystemTime(new Date(1793000000_000));
    const current = deliver(); task.resolve(); await settle();
    expect(current).toHaveBeenCalledTimes(1);
    for (const reply of expired) expect(reply).not.toHaveBeenCalled();
    expect(observe).toHaveBeenCalledTimes(1);
  });

  it('fails closed for missing terminal evidence, adapter throws, snapshot throws and requests without inbox', async () => {
    const { hooks } = await install();
    const absent = deliver();
    hooks.fenceAndJoin.mockImplementation(() => { throw new Error('failed fence'); });
    const failed = deliver();
    hooks.healthSnapshot.mockImplementation(() => { throw new Error('failed snapshot'); });
    const health = deliver(raw('health-prepared-false-false'));
    hooks.fenceAndJoin.mockClear(); const noInbox = deliver(raw(), ''); await settle();
    expect(hooks.fenceAndJoin).not.toHaveBeenCalled();
    for (const respond of [absent, failed, health, noInbox]) expect(respond).not.toHaveBeenCalled();
  });

  it('binds delayed receipts to the original command and suppresses replies on closed handles', async () => {
    const task = deferred(); const hooks = adapters(); hooks.fenceAndJoin.mockReturnValue(task.promise); await install(hooks);
    const original = protocol.decodeCommand(raw());
    const input = raw(); const respond = deliver(input); input.fill(32);
    task.resolve(); await settle();
    expect(() => protocol.verifyAck(respond.mock.calls[0][0], original, secret, 1793000000n)).not.toThrow();
    expect(() => protocol.verifyAck(respond.mock.calls[0][0], protocol.decodeCommand(raw('eject-session-theft')), secret, 1793000000n)).toThrow(protocol.ProtocolError);
    const pending = deferred(); hooks.fenceAndJoin.mockReturnValue(pending.promise);
    const late = deliver(); closed = true; pending.resolve(); await settle(); expect(late).not.toHaveBeenCalled();
  });

  it('keeps ordinary request subscriptions on their original awaited JSON iterator', async () => {
    await service.connect(); const task = deferred(); const respond = vi.fn();
    transport.subscribe.mockReturnValueOnce({
      async *[Symbol.asyncIterator]() { yield { data: Buffer.from('{"legacy":true}'), reply: '_INBOX.old', respond }; },
    });
    const handler = vi.fn(async () => { await task.promise; return { ok: true }; });
    service.subscribeRequest('ordinary.request', handler);
    expect(transport.subscribe).toHaveBeenCalledWith('ordinary.request'); await settle();
    expect(handler).toHaveBeenCalledWith({ legacy: true }); expect(respond).not.toHaveBeenCalled();
    task.resolve(); await settle();
    expect(JSON.parse(Buffer.from(respond.mock.calls[0][0]).toString())).toEqual({ ok: true });
    expect(transport.decode).toHaveBeenCalledTimes(1);
  });
});

// The legacy adapter is a compatibility seam; these cases exercise its bounded
// scheduling, not native ownership or durable release implementation.
describe('legacy compatibility receiver lanes', () => {
  function legacySetup() {
    const task = deferred();
    const healthTask = deferred();
    const legacy = {
      verify: vi.fn((data: Record<string, unknown>) => data.kind === 'health' ? 'health' as const : data.kind === 'terminal' ? 'terminal' as const : undefined),
      terminalKey: (data: Record<string, unknown>) => String(data.id),
      terminal: vi.fn(() => task.promise),
      acknowledge: vi.fn(() => ({ ok: true })),
      health: vi.fn(async () => { await healthTask.promise; return { ok: true }; }),
    };
    const hooks = adapters();
    const receive = createVoiceAuthReceiver(secret.toString(), boot, legacy, hooks);
    function send(value: unknown) {
      const reply = vi.fn(); receive(Buffer.from(JSON.stringify(value)), reply); return reply;
    }
    return { task, healthTask, legacy, receive, send };
  }
  it('deduplicates terminal tasks, shares the 63 attempt cap and frees task entries after completion', async () => {
    const { task, legacy, send } = legacySetup();
    const replies = Array.from({ length: 63 }, () => send({ kind: 'terminal', id: 'same' }));
    const refused = send({ kind: 'terminal', id: 'other' });
    expect(legacy.terminal).toHaveBeenCalledTimes(1);
    task.resolve(); await settle();
    for (const reply of replies) expect(reply).toHaveBeenCalledTimes(1);
    expect(refused).not.toHaveBeenCalled();
    const retry = send({ kind: 'terminal', id: 'other' }); await settle();
    expect(legacy.terminal).toHaveBeenCalledTimes(2); expect(retry).toHaveBeenCalledTimes(1);
  });
  it('refuses a 64th retained legacy identity even after attempt deadlines detach waiters', async () => {
    const { task, legacy, send } = legacySetup();
    for (let id = 0; id < 63; id++) send({ kind: 'terminal', id });
    await vi.advanceTimersByTimeAsync(5000);
    send({ kind: 'terminal', id: 'overflow' });
    expect(legacy.terminal).toHaveBeenCalledTimes(63);
    task.reject(new Error('terminal failed')); await settle();
    send({ kind: 'terminal', id: 'replacement' });
    expect(legacy.terminal).toHaveBeenCalledTimes(64); await settle();
  });
  it.each([false, true])('bounds one hung legacy health lane and preserves independent new health (timeout=%s)', async timeout => {
    const { healthTask, legacy, send, receive } = legacySetup();
    const first = send({ kind: 'health' }); const refused = send({ kind: 'health' });
    expect(legacy.health).toHaveBeenCalledTimes(1);
    const fresh = vi.fn(); receive(raw('health-prepared-false-false'), fresh);
    expect(fresh).toHaveBeenCalledTimes(1);
    if (timeout) await vi.advanceTimersByTimeAsync(5000);
    healthTask.resolve(); await settle();
    expect(first).toHaveBeenCalledTimes(timeout ? 0 : 1); expect(refused).not.toHaveBeenCalled();
    const retry = send({ kind: 'health' }); await settle(); expect(retry).toHaveBeenCalledTimes(1);
  });
  it.each(['[]', 'null', '{"kind":"terminal","id":null}', '{"kind":"terminal","id":{}}', '{"kind":"terminal","kind":"health"}', '{"kind":"terminal","id":"\\ud800"}', '{"kind":"terminal","id":"\\udc00"}'])(
    'rejects malformed flat legacy envelope %s before verification', text => {
      const { receive, legacy } = legacySetup(); const reply = vi.fn(); receive(Buffer.from(text), reply);
      expect(legacy.verify).not.toHaveBeenCalled(); expect(reply).not.toHaveBeenCalled();
    }
  );
  it('allows a valid surrogate pair and silently refuses unknown legacy variants', () => {
    const { receive, legacy } = legacySetup(); const reply = vi.fn();
    receive(Buffer.from('{"kind":"unknown","id":"\\ud83d\\ude00"}'), reply);
    expect(legacy.verify).toHaveBeenCalledTimes(1); expect(reply).not.toHaveBeenCalled();
  });
});
