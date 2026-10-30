import { randomBytes } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { Script } from 'node:vm';
import ts from 'typescript';
import { describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import { VoiceAuthConnections, readVoiceAuthResponse } from '../src/lib/voiceAuthConnection.js';
import { FailureStateLogger, VoiceEnforcementHealth, VoiceEnforcementLease } from '../src/lib/voiceEnforcementLease.js';
import { VoiceEnforcementSessionReleaseQueue, releaseVoiceEnforcementSessionsAfterClose, snapshotVoiceEnforcementSessions } from '../src/lib/voiceEnforcementShutdown.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';

const boot = '11111111-1111-4111-8111-111111111111';
const otherBoot = '99999999-9999-4999-8999-999999999999';
const user = '22222222-2222-4222-8222-222222222222';
const family = '33333333-3333-4333-8333-333333333333';
const secret = ['boot', 'retirement', 'test', 'only'].join('-');
const key = Buffer.from(secret);
const settle = () => new Promise<void>(resolve => setImmediate(resolve));
function deferred<T = void>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}
type Receipt = 'signed' | 'unsigned' | 'wrong boot' | 'wrong request' | 'lost' | 'nonempty' | 'stale';
type Timer = { callback: () => void; delay: number; interval: boolean };
interface ShutdownCaller {
  shutdown(): Promise<void>;
  healthPoll(): Promise<void>;
  health: VoiceEnforcementHealth;
  terminal(): boolean;
}

// Execute complete production declarations. Main/startup, listeners, imports and
// network/native construction are never evaluated. The private continuation is
// the caller under test; no exported retirement adapter stands in for it.
function actualCaller(bindings: Record<string, unknown>): ShutdownCaller {
  const source = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8');
  const ast = ts.createSourceFile('index.ts', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  const main = ast.statements.find((node): node is ts.FunctionDeclaration => ts.isFunctionDeclaration(node) && node.name?.text === 'main');
  if (!main?.body) throw new Error('Missing production main');
  const name = (node: ts.Statement): string | undefined => {
    if (ts.isFunctionDeclaration(node)) return node.name?.text;
    if (!ts.isVariableStatement(node)) return undefined;
    const first = node.declarationList.declarations[0];
    return ts.isIdentifier(first.name) ? first.name.text : undefined;
  };
  const statements = main.body.statements;
  const first = statements.findIndex(node => name(node) === 'shutdownReleaseDrainTimeoutMs');
  const last = statements.findIndex(node => name(node) === 'shutdown');
  if (first < 0 || last < first) throw new Error('Missing production shutdown continuation');
  const selected = new Set(statements.slice(first, last + 1));
  for (const required of ['enforceVoiceEnforcementLease', 'leaseEnforcementFailures', 'enforceLeaseObserved', 'voiceEnforcementHealth', 'healthPoll', 'retireVoiceEnforcementBoot']) {
    const statement = statements.find(node => name(node) === required);
    if (!statement) throw new Error(`Missing production ${required}`);
    selected.add(statement);
  }
  const text = statements.filter(node => selected.has(node)).map(node => node.getText(ast)).join('\n');
  const compiled = ts.transpileModule(`let shuttingDown = false;\n${text}\n({ shutdown, healthPoll, health: voiceEnforcementHealth, terminal: () => shuttingDown });`, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
  }).outputText;
  return new Script(compiled, { filename: 'actual-index-boot-shutdown.js' }).runInNewContext(bindings) as ShutdownCaller;
}

async function fixture(options: { retained?: boolean; receipt?: Receipt; recovery?: boolean; holdRelease?: boolean; empty?: boolean } = {}) {
  let monotonic = 0;
  const wall = Date.now();
  const events: string[] = [];
  const requests: protocol.Request[] = [];
  const timers = new Map<number, Timer>();
  const scheduled: Timer[] = [];
  let nextTimer = 0;
  const install = (callback: () => void, delay: number, interval: boolean) => {
    const timer = { callback, delay, interval };
    scheduled.push(timer);
    const id = ++nextTimer; timers.set(id, timer); return id;
  };
  const recovery = deferred();
  const native = deferred();
  const renewal = deferred();
  const owner = deferred();
  const retirement = deferred();
  const release = deferred();
  let releaseAllowed = !options.retained;
  let holdPoll = false;
  const poll = deferred<protocol.HealthBootstrapResult>();
  let ready = false;
  let stopped = false;
  let retirementAllowed = false;
  const result: protocol.HealthBootstrapResult = { version: 1, voiceEnforcementProtocol: 4, nodeBootId: boot,
    activation: 'active', transportOK: true, admissionGranted: false };
  const fetchBoundary = vi.fn<typeof fetch>(async (input, init) => {
    const path = new URL(String(input)).pathname;
    const endpoint: protocol.Endpoint = path.endsWith('/retire-boot') ? 'boot_retire'
      : path.endsWith('/release') ? 'connection_release' : 'connection_register';
    const body = init?.body;
    if (typeof body !== 'string' && !(body instanceof Uint8Array)) throw new Error('Missing request bytes');
    const request = protocol.decodeHttpRequest(endpoint, { method: init?.method ?? '', path, body: Buffer.from(body),
      headers: Array.from(new Headers(init?.headers), ([name, value]) => ({ name, value })) });
    protocol.verifyHttpRequest(request, key, BigInt(Math.floor(wall / 1000)));
    requests.push(request); events.push(endpoint);
    if (endpoint === 'connection_release') {
      if (!releaseAllowed) throw new Error('Exact release reply lost');
      if (options.holdRelease) await release.promise;
      events.push('exact-release-complete');
    }
    if (endpoint === 'boot_retire') {
      if (!retirementAllowed) await retirement.promise;
      if (options.receipt === 'lost') throw new Error('Retirement committed; reply lost');
      if (options.receipt === 'unsigned') return new Response(null, { status: 204 });
    }
    const registration = request.connectionRegister();
    const payload: protocol.ResponsePayload = registration ? { version: 1, voiceEnforcementProtocol: 4, userId: user, familyId: family,
      target: { connectionGeneration: registration.connectionGeneration, nodeBootId: boot,
        socketId: registration.socketId, credentialEpoch: 'a'.repeat(32) } } : null;
    let signedRequest = request;
    if (endpoint === 'boot_retire' && (options.receipt === 'wrong boot' || options.receipt === 'wrong request')) {
      signedRequest = protocol.signHttpRequest('boot_retire', Buffer.from(JSON.stringify({ schema: 'concord.auth-boot-retirement', version: 1,
        nodeBootId: options.receipt === 'wrong boot' ? otherBoot : boot })), '', {
        timestamp: String(Math.floor(wall / 1000)), nonce: 'b'.repeat(64),
      }, key).request;
    }
    const timestamp = String(Math.floor(wall / 1000) - (options.receipt === 'stale' && endpoint === 'boot_retire' ? 3600 : 0));
    const signed = protocol.signHttpResponse(signedRequest, payload, timestamp, key);
    if (endpoint === 'boot_retire' && options.receipt === 'nonempty') {
      // A custom boundary object permits the invalid 204 body forbidden by the
      // platform Response constructor; the production bounded reader sees it.
      return { status: 204, headers: new Headers(signed.headers.map(({ name, value }) => [name, value])),
        body: new Response('unexpected').body } as Response;
    }
    return new Response(signed.body.length ? Buffer.from(signed.body) : null, { status: signed.status,
      headers: signed.headers.map(({ name, value }) => [name, value]) });
  });
  const connections = new VoiceAuthConnections({ nodeBootId: boot, secret, controlPlaneUrl: 'https://control.test.invalid',
    now: () => wall, fetch: fetchBoundary });
  if (!options.empty) {
    const handle = connections.reserve({ socketId: 'shutdown-owner', userId: user, credentialEpoch: 'a'.repeat(32),
    token: 'test-boot-shutdown-token', tokenExpiresAt: wall + 60_000, disconnect: () => { events.push('disconnect'); } });
    await connections.register(handle);
  }
  const queue = new VoiceEnforcementSessionReleaseQueue(async () => {}, undefined, connections);
  const retry = vi.spyOn(queue, 'retryPending');
  const lease = new VoiceEnforcementLease(() => monotonic); lease.renew();
  const nats = {
    exactVoiceTransportReady: () => true,
    voiceCommandReady: () => ready && !stopped,
    confirmRequiredSubscriptions: async () => {},
    recoverVoiceCommandConsumerAfterTeardown: vi.fn(async () => {
      events.push('recovery-await'); if (options.recovery) await recovery.promise;
      stopped = false; ready = true; events.push('recovery-restored');
    }),
    confirmVoiceCommandContinuity: async () => {},
    stopRequiredSubscriptionRecovery: () => {},
    stopVoiceCommandConsumer: vi.fn(async () => { stopped = true; ready = false; events.push('consumer-stopped'); }),
    stopVoiceCommandRenewal: async () => { events.push('renewal-await'); await renewal.promise; events.push('renewal-stopped'); },
    close: async () => { events.push('nats-closed'); },
  };
  const exit = vi.fn<(code: number) => void>();
  const logger = { info: vi.fn(), warn: vi.fn(), error: vi.fn(), debug: vi.fn() };
  const healthInterval = install(() => { void caller.healthPoll(); }, 10_000, true);
  const caller = actualCaller({ ...protocol, randomBytes, Buffer, Date, Promise, Error, AbortSignal,
    performance: { now: () => monotonic }, config: { jwtSecret: secret, controlPlaneUrl: 'https://control.test.invalid' },
    fetch: fetchBoundary, readVoiceAuthResponse,
    setTimeout: (cb: () => void, ms: number) => install(cb, ms, false),
    setInterval: (cb: () => void, ms: number) => install(cb, ms, true),
    clearTimeout: (id: number) => timers.delete(id), clearInterval: (id: number) => timers.delete(id),
    process: { exit }, logger,
    FailureStateLogger, VoiceEnforcementHealth, voiceEnforcementLease: lease, voiceEnforcementNodeBootId: boot,
    voiceAuthConnections: connections, voiceEnforcementReleaseQueue: queue,
    snapshotVoiceEnforcementSessions, releaseVoiceEnforcementSessionsAfterClose,
    io: { sockets: { sockets: new Map() } }, natsService: nats,
    mediasoupService: { isReady: () => true, close: async () => { events.push('native-await'); await native.promise; events.push('native-exited'); } },
    roomManager: { closeAll: async () => { events.push('rooms-closed'); } },
    voiceCommandOwner: { release: async () => { events.push('owner-await'); await owner.promise; events.push('owner-released'); } },
    enforcementAvailability: { stop: () => {}, awaitTeardown: async () => {}, isReady: () => true },
    voiceEnforcementExpiryFence: { ready: () => true, pending: () => false, enforce: async () => {} },
    enforceVoiceEnforcementLease: async () => {},
    pollVoiceEnforcementHealth: async () => { events.push('health-poll'); return holdPoll ? poll.promise : result; },
    stopObservingRequiredSubscriptions: () => {}, mediaPolicerTick: { stop: () => {} },
    opsMetricsPublisher: { stop: async () => {} }, redisService: { close: async () => { events.push('redis-closed'); } },
    securityEvents: { close: async () => true }, httpServer: { close: (done: () => void) => { events.push('http-closed'); done(); } },
    epochSyncInterval: -1, roomHeartbeatInterval: -2, voiceEnforcementLeaseInterval: -3, voiceEnforcementHealthInterval: healthInterval,
  });
  return { caller, connections, queue, retry, lease, events, requests, nats, exit, logger, timers, scheduled,
    recovery, native, renewal, owner, retirement, release,
    allowRelease() { releaseAllowed = true; },
    allowRetirement() { retirementAllowed = true; retirement.resolve(); },
    holdPoll() { holdPoll = true; }, finishPoll() { poll.resolve(result); },
    advance(ms: number) { monotonic += ms; },
    async tick(delay: number) { for (const [id, timer] of [...timers]) {
      if (timer.delay !== delay || !timers.has(id)) continue;
      if (!timer.interval) timers.delete(id); timer.callback();
    } await settle(); },
  };
}
const retireRequests = (f: Awaited<ReturnType<typeof fixture>>) => f.requests.filter(request => request.endpoint() === 'boot_retire');

describe('actual shutdown private whole-boot retirement witness', () => {
  it.each([false, true])('joins recovery before final consumer stop and retains every terminal/native/owner boundary (initial release reply lost=%s)', async retained => {
    const f = await fixture({ retained, recovery: true, holdRelease: true });
    await f.caller.healthPoll(); await settle();
    expect(f.events).toContain('recovery-await');
    const running = f.caller.shutdown();
    expect(f.caller.shutdown()).toBe(running);
    expect(f.caller.terminal()).toBe(true);
    await settle();
    expect(f.nats.stopVoiceCommandConsumer).not.toHaveBeenCalled();
    expect(retireRequests(f)).toHaveLength(0);
    let completed = false;
    void running.then(() => { completed = true; });
    f.recovery.resolve(); await settle();
    expect(f.events.indexOf('consumer-stopped')).toBeGreaterThan(f.events.indexOf('recovery-restored'));
    expect(f.nats.voiceCommandReady()).toBe(false);
    const recoveryCalls = f.nats.recoverVoiceCommandConsumerAfterTeardown.mock.calls.length;
    // Recovery is fenced, while signed transport polls still retry exact releases.
    f.allowRelease(); await f.caller.healthPoll(); await settle();
    expect(f.retry).toHaveBeenCalled();
    expect(f.connections.retainedIdentityCount()).toBe(1);
    expect(f.events).not.toContain('native-await');
    expect(retireRequests(f)).toHaveLength(0);
    expect(completed).toBe(false);
    expect(f.scheduled.some(timer => timer.interval && timer.delay === 1_000)).toBe(true);
    f.release.resolve(); await settle();
    await f.tick(1_000);
    await settle();
    expect(f.connections.retainedIdentityCount()).toBe(0);
    expect(f.events).toContain('native-await');
    expect(retireRequests(f)).toHaveLength(0);
    f.native.resolve(); await settle();
    expect(f.events).toContain('renewal-await');
    expect(retireRequests(f)).toHaveLength(0);
    f.renewal.resolve(); await settle();
    expect(f.events).toContain('owner-await');
    expect(retireRequests(f)).toHaveLength(0);
    f.owner.resolve(); await settle();
    expect(retireRequests(f)).toHaveLength(1);
    expect(f.timers.size).toBeGreaterThan(0);
    expect(f.events).not.toContain('http-closed');
    f.allowRetirement(); await running;
    expect(f.exit).toHaveBeenCalledWith(0);
    expect(f.events.indexOf('boot_retire')).toBeGreaterThan(f.events.indexOf('owner-released'));
    expect(f.events.indexOf('boot_retire')).toBeGreaterThan(f.events.indexOf('native-exited'));
    expect(f.nats.recoverVoiceCommandConsumerAfterTeardown).toHaveBeenCalledTimes(recoveryCalls);
    expect(f.nats.voiceCommandReady()).toBe(false);
    expect(f.lease.valid()).toBe(false);
    expect(f.caller.terminal()).toBe(true);
  });

  it('joins the retained poll before native exit and blocks new poll/recovery scheduling thereafter', async () => {
    const f = await fixture({ empty: true });
    f.holdPoll(); const polling = f.caller.healthPoll();
    await settle();
    const running = f.caller.shutdown(); await settle();
    expect(f.connections.retainedIdentityCount()).toBe(0);
    expect(f.events).not.toContain('native-await');
    expect(retireRequests(f)).toHaveLength(0);
    f.finishPoll(); await polling; await settle();
    expect(f.events).toContain('native-await');
    const polls = f.events.filter(event => event === 'health-poll').length;
    await f.caller.healthPoll();
    expect(f.events.filter(event => event === 'health-poll')).toHaveLength(polls);
    f.native.resolve(); f.renewal.resolve(); f.owner.resolve(); f.allowRetirement(); await running;
    expect(f.exit).toHaveBeenCalledWith(0);
  });

  it.each<Receipt>(['signed', 'unsigned', 'wrong boot', 'wrong request', 'lost', 'nonempty', 'stale'])(
    'only strict current exact signed empty 204 permits successful shutdown: %s', async receipt => {
      const f = await fixture({ receipt, empty: true });
      f.native.resolve(); f.renewal.resolve(); f.owner.resolve(); f.allowRetirement();
      await f.caller.shutdown();
      expect(retireRequests(f)).toHaveLength(1);
      // The immediate-drain branch must reach retirement without a retained-release timer.
      expect(f.scheduled.some(timer => timer.interval && timer.delay === 1_000)).toBe(false);
      expect(retireRequests(f)[0].bootRetire()).toEqual({ schema: 'concord.auth-boot-retirement', version: 1, nodeBootId: boot });
      expect(f.exit).toHaveBeenCalledWith(receipt === 'signed' ? 0 : 1);
      expect(f.events.includes('http-closed')).toBe(receipt === 'signed');
      expect(f.caller.terminal()).toBe(true);
      expect(f.lease.valid()).toBe(false);
    }
  );

  it('exhausting the shared native deadline prevents owner release and retirement POST', async () => {
    const f = await fixture({ empty: true });
    f.renewal.resolve(); f.owner.resolve(); f.allowRetirement();
    const running = f.caller.shutdown(); await settle();
    expect(f.events).toContain('native-await');
    f.advance(35_000); await f.tick(35_000);
    expect(f.exit).toHaveBeenCalledWith(1);
    expect(retireRequests(f)).toHaveLength(0);
    f.native.resolve(); await running;
    expect(f.events).not.toContain('owner-released');
    expect(retireRequests(f)).toHaveLength(0);
    expect(f.exit).not.toHaveBeenCalledWith(0);
  });

  it('keeps the existing 35 second deadline armed while the retirement reply is unresolved', async () => {
    const f = await fixture({ empty: true });
    f.native.resolve(); f.renewal.resolve(); f.owner.resolve();
    const running = f.caller.shutdown(); await settle();
    expect(retireRequests(f)).toHaveLength(1);
    expect([...f.timers.values()].some(timer => timer.delay === 35_000 && !timer.interval)).toBe(true);
    expect([...f.timers.values()].some(timer => timer.delay === 10_000 && !timer.interval)).toBe(false);
    f.advance(35_000); await f.tick(35_000);
    expect(f.exit).toHaveBeenCalledWith(1);
    expect(f.events).not.toContain('http-closed');
    // A late valid reply cannot turn an exhausted shutdown budget into success.
    f.allowRetirement(); await running;
    expect(f.exit).not.toHaveBeenCalledWith(0);
    expect(f.events).not.toContain('http-closed');
  });
});
