import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import { NatsService } from '../src/lib/nats.js';
const broker = vi.hoisted(() => ({ subscribe: vi.fn(), flush: vi.fn(async () => {}), drain: vi.fn(async () => {}), isClosed: () => false,
  status: () => ({ async *[Symbol.asyncIterator]() { await new Promise(() => {}); yield { type: 'update' }; } }),
}));
vi.mock('nats', async original => ({ ...await original<typeof import('nats')>(), connect: async () => broker }));
import { pollVoiceEnforcementHealth } from '../src/lib/voiceEnforcementSession.js';

const secret = 'voice-auth-health-test-only-key';
const key = Buffer.from(secret);
const boot = '00000002-1111-4111-8111-111111111111';
const otherBoot = '00000009-1111-4111-8111-111111111111';
const services: NatsService[] = [];
const wall = 1793000000000;
const timestamp = String(wall / 1000);
const healthPath = '/api/v1/internal/voice/enforcement/health';
vi.mock('../src/config/index.js', () => ({ config: {
  jwtSecret: 'voice-auth-health-test-only-key', controlPlaneUrl: 'https://cp.test.invalid', natsUrl: 'nats://test.invalid:4222',
} }));

function asResponse(wire: protocol.HTTPResponse) {
  return new Response(wire.status === 204 ? null : Buffer.from(wire.body), {
    status: wire.status, headers: wire.headers.map(header => [header.name, header.value]),
  });
}
function decodeRequest(init: RequestInit | undefined): protocol.Request {
  const headers: protocol.Header[] = [];
  new Headers(init?.headers).forEach((value, name) => headers.push({ name, value }));
  if (typeof init?.body !== 'string' && !(init?.body instanceof Uint8Array)) throw new Error('Missing raw bootstrap bytes');
  const request = protocol.decodeHttpRequest('health_bootstrap', {
    method: init?.method ?? '', path: healthPath, headers, body: Buffer.from(init.body),
  });
  protocol.verifyHttpRequest(request, key, BigInt(timestamp));
  return request;
}
function bootstrap(request: protocol.Request, admissionGranted = false): protocol.HTTPResponse {
  return protocol.signHttpResponse(request, {
    version: 1, voiceEnforcementProtocol: 4, nodeBootId: boot,
    activation: admissionGranted ? 'active' : 'prepared', transportOK: true, admissionGranted,
  }, timestamp, key);
}

beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(wall); });
afterEach(async () => { for (const service of services.splice(0)) await service.close(); broker.subscribe.mockReset(); vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); });

describe('protocol-4 bootstrap through the production HTTP caller', () => {
  it.each([false, true])('accepts only a signed exact bootstrap (admissionGranted=%s)', async granted => {
    const fetcher = vi.fn<typeof fetch>(async (url, init) => {
      expect(url).toBe(`https://cp.test.invalid${healthPath}`);
      const request = decodeRequest(init);
      expect(request.healthBootstrap()).toEqual({ version: 1, nodeBootId: boot, nonce: expect.stringMatching(/^[0-9a-f]{64}$/) });
      return asResponse(bootstrap(request, granted));
    });
    vi.stubGlobal('fetch', fetcher);
    await expect(pollVoiceEnforcementHealth(boot)).resolves.toMatchObject({
      voiceEnforcementProtocol: 4, nodeBootId: boot, activation: granted ? 'active' : 'prepared', transportOK: true, admissionGranted: granted,
    });
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it.each(['unsigned', 'forged-proof', 'stale-proof', 'wrong-boot', 'wrong-schema', 'wrong-status', 'wrong-request', 'duplicate-key', 'oversize'])(
    'rejects %s success at the real bootstrap response boundary', async fault => {
      let delivered = false;
      vi.stubGlobal('fetch', vi.fn<typeof fetch>(async (_url, init) => {
        const request = decodeRequest(init);
        let wire = bootstrap(request, true);
        if (fault === 'unsigned') wire.headers = [];
        if (fault === 'forged-proof') wire.headers = wire.headers.map(header => header.name.toLowerCase().endsWith('proof') ? { ...header, value: '0'.repeat(64) } : header);
        if (fault === 'stale-proof') wire = protocol.signHttpResponse(request, {
          version: 1, voiceEnforcementProtocol: 4, nodeBootId: boot, activation: 'active', transportOK: true, admissionGranted: true,
        }, String(Number(timestamp) - 31), key);
        if (fault === 'wrong-boot') {
          const nonce = request.healthBootstrap()!.nonce;
          const other = protocol.signHttpRequest('health_bootstrap', Buffer.from(JSON.stringify({ version: 1, nodeBootId: otherBoot, nonce })), '', { timestamp, nonce }, key);
          wire = protocol.signHttpResponse(other.request, { version: 1, voiceEnforcementProtocol: 4, nodeBootId: otherBoot, activation: 'active', transportOK: true, admissionGranted: true }, timestamp, key);
        }
        if (fault === 'wrong-schema') wire.body = Buffer.from(JSON.stringify({ ...JSON.parse(Buffer.from(wire.body).toString()), unexpected: true }));
        if (fault === 'wrong-status') wire.status = 201;
        if (fault === 'wrong-request') {
          const signed = protocol.signHttpRequest('health_bootstrap', Buffer.from(JSON.stringify({ version: 1, nodeBootId: boot, nonce: 'b'.repeat(64) })), '', { timestamp, nonce: 'b'.repeat(64) }, key);
          wire = bootstrap(protocol.decodeHttpRequest('health_bootstrap', signed.wire), true);
        }
        if (fault === 'duplicate-key') wire.body = Buffer.from(Buffer.from(wire.body).toString().replace(/^\{/, '{"version":1,'));
        if (fault === 'oversize') wire.body = Buffer.concat([Buffer.from(wire.body), Buffer.alloc(4097, 32)]);
        delivered = true;
        return asResponse(wire);
      }));
      if (fault === 'oversize') await expect(pollVoiceEnforcementHealth(boot)).rejects.toThrow('Voice response too large');
      else await expect(pollVoiceEnforcementHealth(boot)).rejects.toBeInstanceOf(protocol.ProtocolError);
      expect(delivered).toBe(true);
    }
  );

  it('uses fresh request binding on successive prepared probes without needing clients', async () => {
    const nonces: string[] = [];
    vi.stubGlobal('fetch', vi.fn<typeof fetch>(async (_url, init) => {
      const request = decodeRequest(init); nonces.push(request.healthBootstrap()!.nonce);
      return asResponse(bootstrap(request));
    }));
    await pollVoiceEnforcementHealth(boot); await pollVoiceEnforcementHealth(boot);
    expect(nonces).toHaveLength(2); expect(nonces[0]).not.toBe(nonces[1]);
  });
});

// The native/network boundaries are controlled; the receiver, proof codec,
// authority lease and readiness coordinator execute their production code.
import { VoiceEnforcementHealth, VoiceEnforcementLease } from '../src/lib/voiceEnforcementLease.js';
import { createVoiceAuthReceiver } from '../src/lib/voiceAuthReceiver.js';

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}
function healthFixture() {
  let mono = 0;
  let local = true;
  let transport = true;
  const lease = new VoiceEnforcementLease(() => mono);
  const recovery = deferred<void>();
  const pollResult = { version: 1, voiceEnforcementProtocol: 4, nodeBootId: boot, activation: 'active', transportOK: true, admissionGranted: true } as const;
  const remote = vi.fn(async (): Promise<protocol.HealthBootstrapResult> => pollResult);
  const recover = vi.fn(() => recovery.promise);
  const onTransport = vi.fn();
  const health = new VoiceEnforcementHealth({ lease, nodeBootId: boot, localReady: () => local,
    transportReady: () => transport, poll: remote, recover, onTransport, now: () => mono });
  const terminal = deferred<void>();
  const legacy = { verify: (value: Record<string, unknown>) => value.kind === 'health' ? 'health' as const : undefined,
    terminalKey: () => '', terminal: async () => {}, acknowledge: () => ({}), health: async () => ({ ok: true }) };
  const receiver = createVoiceAuthReceiver(secret, boot, legacy, {
    fenceAndJoin: () => terminal.promise, healthSnapshot: command => health.healthSnapshot(command),
  });
  function send(activation = 'active', allowed = true) {
    const command = protocol.newHealthCommand(boot, 'a'.repeat(64), activation, allowed, timestamp);
    const response = vi.fn(); receiver(protocol.signCommand(command, key), response);
    expect(response).toHaveBeenCalledTimes(1);
    return protocol.verifyAck(response.mock.calls[0][0], command, key, BigInt(timestamp)).health()!;
  }
  return { health, lease, remote, recovery, recover, onTransport, receiver, terminal, send,
    setMono: (value: number) => { mono = value; }, setLocal: (value: boolean) => { local = value; },
    setTransport: (value: boolean) => { transport = value; } };
}

describe('real protocol-4 receiver and local readiness coordinator', () => {
  it('answers prepared false-grant health with no admitted clients or productive lease', () => {
    const f = healthFixture();
    expect(f.lease.valid()).toBe(false);
    expect(f.send('prepared', false)).toMatchObject({ transportOK: true, admissionGranted: false });
    expect(f.remote).not.toHaveBeenCalled();
    expect(f.lease.valid()).toBe(false);
  });

  it('keeps the health lane synchronous under 63 hung terminal waiters and overflow', () => {
    const f = healthFixture();
    const binding: protocol.Binding = { userId: '00000003-1111-4111-8111-111111111111', familyId: '00000004-1111-4111-8111-111111111111', target: {
      connectionGeneration: '00000005-1111-4111-8111-111111111111', nodeBootId: boot, socketId: 'health-terminal', credentialEpoch: '',
    } };
    const terminal = protocol.newAuthCommand(binding, '00000006-1111-4111-8111-111111111111', 'session', 'logout', { timestamp, nonce: 'b'.repeat(64) });
    const raw = protocol.signCommand(terminal, key);
    const replies = Array.from({ length: 363 }, () => vi.fn());
    for (const reply of replies) f.receiver(raw, reply);
    expect(f.send('prepared', false).admissionGranted).toBe(false);
    for (const reply of replies) expect(reply).not.toHaveBeenCalled();
    expect(f.remote).not.toHaveBeenCalled();
  });

  it.each(['prepared', 'cp-denied', 'native-unready', 'subscription-lost'])(
    'denies %s independently after a valid active bootstrap', async fault => {
      const f = healthFixture(); await f.health.poll();
      expect(f.send().admissionGranted).toBe(true);
      if (fault === 'native-unready') f.setLocal(false);
      if (fault === 'subscription-lost') f.setTransport(false);
      expect(f.send(fault === 'prepared' ? 'prepared' : 'active', fault !== 'cp-denied' && fault !== 'prepared').admissionGranted).toBe(false);
    }
  );

  it('can acknowledge first active challenge from established local prerequisites without prematurely renewing', () => {
    const f = healthFixture();
    expect(f.send().admissionGranted).toBe(true);
    expect(f.health.status().voiceEnforcementAdmissionGranted).toBe(false);
    expect(f.lease.valid()).toBe(false);
  });

  it('uses the existing monotonic lease and cannot extend it through raw or legacy health', async () => {
    const f = healthFixture(); await f.health.poll();
    expect(f.lease.valid()).toBe(true);
    f.setMono(29000); expect(f.send().admissionGranted).toBe(true);
    const legacyReply = vi.fn(); f.receiver(Buffer.from('{"kind":"health"}'), legacyReply);
    f.setMono(30001);
    expect(f.health.status().voiceEnforcementAdmissionGranted).toBe(false);
    expect(f.lease.valid()).toBe(false);
  });

  it('does not restart the 30-second lease at the end of a slow bootstrap', async () => {
    const f = healthFixture(); const reply = deferred<protocol.HealthBootstrapResult>();
    f.remote.mockImplementation(() => reply.promise);
    const pending = f.health.poll(); f.setMono(30001);
    reply.resolve({ version: 1, voiceEnforcementProtocol: 4, nodeBootId: boot, activation: 'active', transportOK: true, admissionGranted: true });
    await pending;
    expect(f.lease.valid()).toBe(false); expect(f.health.status().voiceEnforcementAdmissionGranted).toBe(false);
  });

  it('does not wait for hung recovery before completing a prepared bootstrap', async () => {
    const f = healthFixture(); f.setLocal(false);
    f.remote.mockResolvedValue({ version: 1, voiceEnforcementProtocol: 4, nodeBootId: boot, activation: 'prepared', transportOK: true, admissionGranted: false });
    await f.health.poll();
    expect(f.onTransport).toHaveBeenCalled();
    expect(f.send('prepared', false).admissionGranted).toBe(false);
    expect(f.health.status()).toMatchObject({ voiceEnforcementProtocol: 4, voiceEnforcementTransportOK: true, voiceEnforcementAdmissionGranted: false, nodeBootId: boot });
    await f.health.poll(); await f.health.poll();
    expect(f.recover).toHaveBeenCalledTimes(1);
    expect(f.remote).toHaveBeenCalledTimes(3);
    f.recovery.resolve();
  });

  it('requires verified fresh bootstrap plus restored subscription/native prerequisites after quarantine', async () => {
    const f = healthFixture(); await f.health.poll();
    f.setTransport(false); expect(f.send().admissionGranted).toBe(false);
    f.setMono(30001); f.setTransport(true); f.setLocal(false);
    expect(f.send().admissionGranted).toBe(false);
    await f.health.poll(); expect(f.send().admissionGranted).toBe(false);
    f.setLocal(true);
    expect(f.health.status().voiceEnforcementAdmissionGranted).toBe(false);
    await f.health.poll();
    expect(f.send().admissionGranted).toBe(true);
    expect(f.health.status().voiceEnforcementAdmissionGranted).toBe(true);
  });

  it('withholds grants after failed bootstrap while preserving transport capability', async () => {
    const f = healthFixture(); await f.health.poll();
    f.remote.mockRejectedValue(new Error('authority unavailable'));
    await f.health.poll().catch(() => undefined);
    expect(f.health.status().voiceEnforcementAdmissionGranted).toBe(false);
    expect(f.health.status().voiceEnforcementProtocol).toBe(4);
  });
});

import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
// Distinct accounts, so filling the shared budget never trips the per-user cap.
const filler = (index: number) => `${index.toString(16).padStart(8, '0')}-9999-4999-8999-999999999999`;
async function settle() { for (let i = 0; i < 30; i++) await Promise.resolve(); }

it('retains saturated real owners during hung native closure, then retries release under false productive authority', async () => {
  const userId = '00000003-1111-4111-8111-111111111111';
  const familyId = '00000004-1111-4111-8111-111111111111';
  let releaseAvailable = false;
  const attempts: protocol.Endpoint[] = [];
  const native = deferred<void>();
  const nativeEntered = deferred<void>();
  const closeNative = vi.fn(() => { nativeEntered.resolve(); return native.promise; });
  const fetcher = vi.fn<typeof fetch>(async (url, init) => {
    const path = new URL(String(url)).pathname;
    const endpoint: protocol.Endpoint = path.endsWith('/provisional-close') ? 'provisional_close' : path.endsWith('/auth-connections/release') ? 'connection_release' : path.endsWith('/enforcement-sessions/release') ? 'room_release' : 'connection_register';
    const headers: protocol.Header[] = [];
    new Headers(init?.headers).forEach((value, name) => headers.push({ name, value }));
    const request = protocol.decodeHttpRequest(endpoint, { method: init?.method ?? '', path, headers, body: Buffer.from(init!.body as string) });
    protocol.verifyHttpRequest(request, key, BigInt(timestamp)); attempts.push(endpoint);
    if (endpoint === 'room_release' && !releaseAvailable) throw new Error('release response unavailable');
    const registration = request.connectionRegister(); const provisional = request.provisionalClose();
    const payload: protocol.ResponsePayload = registration ? {
      version: 1, voiceEnforcementProtocol: 4, userId, familyId,
      target: { connectionGeneration: registration.connectionGeneration, nodeBootId: registration.nodeBootId, socketId: registration.socketId, credentialEpoch: registration.credentialEpoch },
    } : provisional ? { ...provisional, schema: 'concord.auth-connection-provisional-close-result', disposition: 'unregistered' } : null;
    return asResponse(protocol.signHttpResponse(request, payload, timestamp, key));
  });
  const owner = new VoiceAuthConnections({ nodeBootId: boot, secret, controlPlaneUrl: 'https://cp.test.invalid', fetch: fetcher, now: () => wall });
  const handle = owner.reserve({ socketId: 'health-native', userId, credentialEpoch: '', token: 'test-health-token', tokenExpiresAt: wall + 60000, disconnect: () => {} });
  await owner.register(handle);
  owner.reserveRoom(handle, { sessionGeneration: '00000007-1111-4111-8111-111111111111', nodeBootId: boot,
    roomId: '00000008-1111-4111-8111-111111111111', roomKind: 'channel', userId, credentialEpoch: '', socketId: 'health-native' }, closeNative);
  for (let i = 2; i < 1000; i++) owner.reserve({ socketId: `health-capacity-${i}`, userId: filler(i), credentialEpoch: '', token: 'test-health-token', tokenExpiresAt: wall + 60000, disconnect: () => {} });
  expect(owner.retainedIdentityCount()).toBe(1000);
  const lease = new VoiceEnforcementLease(() => 0);
  const health = new VoiceEnforcementHealth({ lease, nodeBootId: boot,
    localReady: () => owner.retainedIdentityCount() < 1000 && !owner.hasPendingTermination(), transportReady: () => true,
    poll: async () => ({ version: 1, voiceEnforcementProtocol: 4, nodeBootId: boot, activation: 'prepared', transportOK: true, admissionGranted: false }),
    recover: () => owner.retryPending(), onTransport: () => {} });
  const receiver = createVoiceAuthReceiver(secret, boot, {
    verify: () => undefined, terminalKey: () => '', terminal: async () => {}, acknowledge: () => undefined, health: async () => undefined,
  }, { fenceAndJoin: command => owner.fenceAndJoin(command), healthSnapshot: command => health.healthSnapshot(command) });
  type Message = { data: Uint8Array; reply: string; respond: ReturnType<typeof vi.fn> };
  let callback!: (error: Error | null, message: Message) => void;
  let closed = false;
  let finish!: () => void;
  broker.subscribe.mockImplementation((_subject: string, options: { callback: typeof callback }) => {
    closed = false;
    callback = options.callback;
    return { closed: new Promise<void>(resolve => { finish = resolve; }), isClosed: () => closed, unsubscribe: () => { closed = true; finish(); } };
  });
  const service = new NatsService(); services.push(service);
  await service.connect();
  expect(service.subscribeRequest(`voice.enforce.session.${boot}`, Object.assign(async () => undefined, { receive: receiver }), { required: true })).toBe(true);
  await service.confirmRequiredSubscriptions();
  expect(service.exactVoiceTransportReady(`voice.enforce.session.${boot}`)).toBe(true);
  const deliver = (data: Uint8Array, respond: ReturnType<typeof vi.fn>) => callback(null, { data, reply: '_INBOX.health-test', respond });
  const command = protocol.newAuthCommand(owner.snapshot(handle).binding!, '00000006-1111-4111-8111-111111111111', 'session', 'logout', { timestamp, nonce: 'b'.repeat(64) });
  const replies = Array.from({ length: 363 }, () => vi.fn());
  for (const reply of replies) deliver(protocol.signCommand(command, key), reply);
  const probe = protocol.newHealthCommand(boot, 'a'.repeat(64), 'prepared', false, timestamp);
  const respond = vi.fn(); deliver(protocol.signCommand(probe, key), respond);
  expect(respond).toHaveBeenCalledTimes(1);
  expect(protocol.verifyAck(respond.mock.calls[0][0], probe, key, BigInt(timestamp)).health()?.admissionGranted).toBe(false);
  expect(owner.retainedIdentityCount()).toBe(1000);
  await nativeEntered.promise; expect(closeNative).toHaveBeenCalledTimes(1);
  native.resolve(); await settle();
  expect(attempts).toContain('room_release');
  expect(owner.retainedIdentityCount()).toBe(1000);
  for (const reply of replies) expect(reply).not.toHaveBeenCalled();
  releaseAvailable = true; await health.poll(); await owner.retryPending(); await owner.terminate(handle);
  expect(owner.retainedIdentityCount()).toBe(998);
  expect(closeNative).toHaveBeenCalledTimes(1);
  expect(lease.valid()).toBe(false);
  expect(attempts).toContain('connection_release');
  expect(broker.subscribe).toHaveBeenCalledTimes(1);
  callback(new Error('exact subscription lost'), { data: new Uint8Array(), reply: '', respond: vi.fn() });
  expect(service.exactVoiceTransportReady(`voice.enforce.session.${boot}`)).toBe(false);
  await vi.advanceTimersByTimeAsync(2000);
  await service.confirmRequiredSubscriptions();
  expect(service.exactVoiceTransportReady(`voice.enforce.session.${boot}`)).toBe(true);
  expect(broker.subscribe).toHaveBeenCalledTimes(2);
  await service.close(); broker.subscribe.mockReset();
});
