import { afterEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import type { ConnectionHandle } from '../src/lib/voiceAuthConnection.js';
import { createVoiceAuthReceiver } from '../src/lib/voiceAuthReceiver.js';
import type { VoiceEnforcementSession } from '../src/lib/voiceEnforcementSession.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';

const boot = '11111111-1111-4111-8111-111111111111';
const user = '22222222-2222-4222-8222-222222222222';
const family = '33333333-3333-4333-8333-333333333333';
const roomId = '44444444-4444-4444-8444-444444444444';
const epoch = 'a'.repeat(32);
const secret = ['capacity', 'test', 'only'].join('-');
const key = Buffer.from(secret);
const clock = 1_800_000_000_000;
const token = 'test-capacity-token';

function deferred<T>() {
  let resolve: (value: T) => void = () => { throw new Error('Uninitialized deferred'); };
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}
async function settledTurn() { await new Promise<void>(resolve => setImmediate(resolve)); }

interface Exchange { request: protocol.Request; signal: AbortSignal | null | undefined }
function response(exchange: Exchange, unsigned = false): Response {
  const request = exchange.request;
  const register = request.connectionRegister();
  const room = request.roomRegister();
  const provisional = request.provisionalClose();
  const target = register ?? room;
  const binding = target ? { userId: user, familyId: family, target: {
    connectionGeneration: target.connectionGeneration, nodeBootId: target.nodeBootId,
    socketId: target.socketId, credentialEpoch: target.credentialEpoch,
  } } : undefined;
  const payload: protocol.ResponsePayload = register && binding
    ? { ...binding, version: 1, voiceEnforcementProtocol: 4 }
    : room && binding ? { schema: 'concord.auth-room-binding', version: 1,
      voiceEnforcementProtocol: 4, binding, room: {
        sessionGeneration: room.sessionGeneration, roomId: room.roomId, roomKind: room.roomKind,
      } }
      : provisional ? { ...provisional, schema: 'concord.auth-connection-provisional-close-result', disposition: 'unregistered' }
        : null;
  const signed = protocol.signHttpResponse(request, payload, String(clock / 1000), key);
  return new Response(signed.body.length ? Buffer.from(signed.body) : null, {
    status: signed.status, headers: unsigned ? [] : signed.headers.map(({ name, value }) => [name, value]),
  });
}
function fixture() {
  const calls: Exchange[] = [];
  let answer: (exchange: Exchange) => Response | Promise<Response> = exchange => response(exchange);
  const boundary = vi.fn<typeof fetch>(async (input, init) => {
    const path = new URL(String(input)).pathname;
    const endpoint: protocol.Endpoint = path.endsWith('/auth-connections') ? 'connection_register'
      : path.endsWith('/auth-connections/release') ? 'connection_release'
        : path.endsWith('/provisional-close') ? 'provisional_close'
          : path.endsWith('/enforcement-sessions/release') ? 'room_release' : 'room_register';
    const body = init?.body;
    if (typeof body !== 'string' && !(body instanceof Uint8Array)) throw new Error('Missing raw request');
    const request = protocol.decodeHttpRequest(endpoint, {
      method: init?.method ?? '', path, body: Buffer.from(body),
      headers: Array.from(new Headers(init?.headers), ([name, value]) => ({ name, value })),
    });
    protocol.verifyHttpRequest(request, key, BigInt(clock / 1000));
    const exchange = { request, signal: init?.signal };
    calls.push(exchange);
    return answer(exchange);
  });
  const owners = new VoiceAuthConnections({ nodeBootId: boot, secret,
    controlPlaneUrl: 'https://control.test.invalid', now: () => clock, fetch: boundary });
  const disconnects = new Map<string, ReturnType<typeof vi.fn>>();
  const reserve = (socketId: string) => {
    const disconnect = vi.fn(); disconnects.set(socketId, disconnect);
    return owners.reserve({ socketId, userId: user, credentialEpoch: epoch, token,
      tokenExpiresAt: clock + 60_000, disconnect });
  };
  const session = (owner: ConnectionHandle, index: number): VoiceEnforcementSession => ({
    sessionGeneration: `${index.toString(16).padStart(8, '0')}-5555-4555-8555-555555555555`,
    connectionGeneration: owners.snapshot(owner).target.connectionGeneration,
    nodeBootId: boot, roomId, roomKind: 'channel', userId: user, credentialEpoch: epoch,
    socketId: owners.snapshot(owner).target.socketId,
  });
  return { owners, reserve, session, calls, boundary, disconnects,
    answer(handler: typeof answer) { answer = handler; } };
}
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });


// Diagnostic samples are whole-process observations: fixture/codec allocations,
// Vitest, JIT and allocator retention are included. No RSS pass limit is implied.
function memory(phase: string, identities: number, waiters: number) {
  process.stdout.write(`task8-workload-memory ${JSON.stringify({ phase, identities, waiters,
    ...process.memoryUsage() })}\n`);
}

describe('Task 8 retained workload measurements', () => {
  it('records repeated mixed1000 native/release recovery without losing exact liabilities', async () => {
    for (let round = 0; round < 3; round++) {
      const f = fixture();
      memory(`round${round}:baseline`, f.owners.retainedIdentityCount(), 0);
      const parent = f.reserve(`parent-${round}`);
      await f.owners.register(parent);
      const native = deferred<void>();
      const started = deferred<void>();
      const close = vi.fn(() => { started.resolve(); return native.promise; });
      const first = f.session(parent, 1);
      f.owners.reserveRoom(parent, first, close);
      await f.owners.attachRoom(parent, first, token);
      for (let n = 2; n <= 500; n++) f.owners.reserveRoom(parent, f.session(parent, n), async () => {});
      const idle = Array.from({ length: 499 }, (_, n) => f.reserve(`idle-${round}-${n}`));
      expect(f.owners.retainedIdentityCount()).toBe(1000);
      memory(`round${round}:mixed1000`, f.owners.retainedIdentityCount(), 0);
      const calls = f.calls.length;
      for (let n = 0; n < 300; n++) expect(() => f.reserve(`overflow-${n}`)).toThrow();
      expect(f.calls.length).toBe(calls);
      let unsigned = true;
      f.answer(exchange => response(exchange, unsigned && exchange.request.endpoint() === 'room_release'));
      const terminal = f.owners.releaseRoom(first);
      await started.promise;
      memory(`round${round}:hung-native`, f.owners.retainedIdentityCount(), 0);
      native.resolve();
      await settledTurn();
      expect(f.owners.retainedIdentityCount()).toBe(1000);
      memory(`round${round}:unsigned-release`, f.owners.retainedIdentityCount(), 0);
      unsigned = false;
      await f.owners.retryPending(); await terminal;
      expect(close).toHaveBeenCalledOnce();
      expect(f.owners.retainedIdentityCount()).toBe(999);
      await Promise.all(idle.map(owner => f.owners.terminate(owner)));
      await f.owners.terminate(parent);
      expect(f.owners.retainedIdentityCount()).toBe(0);
      memory(`round${round}:signed-cleanup`, f.owners.retainedIdentityCount(), 0);
    }
  }, 30_000);

  it('records 63 hung waiters plus300 overflow across repeated timeout and late-work rounds', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(clock);
    const native = deferred<void>();
    const observed = vi.spyOn(native.promise, 'then');
    const receiver = createVoiceAuthReceiver(secret, boot, {
      verify: () => undefined, terminalKey: () => '', terminal: async () => {},
      acknowledge: () => undefined, health: async () => undefined,
    }, { fenceAndJoin: () => native.promise, healthSnapshot: () => false });
    const target = { connectionGeneration: '55555555-5555-4555-8555-555555555555',
      nodeBootId: boot, socketId: 'workload-socket', credentialEpoch: epoch };
    const command = protocol.newAuthCommand({ userId: user, familyId: family, target },
      '66666666-6666-4666-8666-666666666666', 'session', 'logout',
      { timestamp: String(clock / 1000), nonce: 'ab'.repeat(32) });
    const raw = protocol.signCommand(command, key);
    const expired: ReturnType<typeof vi.fn>[] = [];
    for (let round = 0; round < 3; round++) {
      vi.setSystemTime(clock);
      const replies = Array.from({ length: 363 }, () => {
        const reply = vi.fn(); receiver(raw, reply); return reply;
      });
      expect(vi.getTimerCount()).toBe(63);
      expect(observed).toHaveBeenCalledTimes(1);
      memory(`receiver${round}:63hung300overflow`, 1, vi.getTimerCount());
      const healthCommand = protocol.newHealthCommand(boot, 'cd'.repeat(32), 'prepared', false, String(clock / 1000));
      const healthReply = vi.fn();
      receiver(protocol.signCommand(healthCommand, key), healthReply);
      expect(healthReply).toHaveBeenCalledOnce();
      expect(protocol.verifyAck(healthReply.mock.calls[0][0], healthCommand, key, BigInt(clock / 1000)).health()?.admissionGranted).toBe(false);
      await vi.advanceTimersByTimeAsync(5001);
      expect(vi.getTimerCount()).toBe(0);
      expect(replies.every(reply => reply.mock.calls.length === 0)).toBe(true);
      expired.push(...replies);
      memory(`receiver${round}:timeout`, 1, vi.getTimerCount());
    }
    native.resolve();
    await Promise.resolve(); await Promise.resolve();
    expect(expired.every(reply => reply.mock.calls.length === 0)).toBe(true);
    vi.setSystemTime(clock);
    const fresh = vi.fn(); receiver(raw, fresh);
    await Promise.resolve();
    expect(fresh).toHaveBeenCalledOnce();
    expect(() => protocol.verifyAck(fresh.mock.calls[0][0], command, key, BigInt(clock / 1000))).not.toThrow();
    expect(vi.getTimerCount()).toBe(0);
    memory('receiver:late-completion-fresh-ack', 0, vi.getTimerCount());
  });
});
