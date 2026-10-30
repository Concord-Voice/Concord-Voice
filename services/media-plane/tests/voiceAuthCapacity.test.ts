import { afterEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import type { ConnectionHandle } from '../src/lib/voiceAuthConnection.js';
import { createVoiceAuthReceiver } from '../src/lib/voiceAuthReceiver.js';
import type { VoiceEnforcementSession } from '../src/lib/voiceEnforcementSession.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import { VoiceEnforcementSessionReleaseQueue, snapshotVoiceEnforcementSessions, releaseVoiceEnforcementSessionsAfterClose } from '../src/lib/voiceEnforcementShutdown.js';
import { VoiceEnforcementExpiryFence, VoiceEnforcementLease } from '../src/lib/voiceEnforcementLease.js';

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
function command(f: ReturnType<typeof fixture>, owner: ConnectionHandle, nonce = 'ab') {
  const unsigned = protocol.newAuthCommand({ userId: user, familyId: family, target: f.owners.snapshot(owner).target },
    '66666666-6666-4666-8666-666666666666', 'session', 'logout',
    { timestamp: String(clock / 1000), nonce: nonce.repeat(32) });
  const raw = protocol.signCommand(unsigned, key);
  const verified = protocol.decodeCommand(raw);
  protocol.verifyCommand(verified, key, BigInt(clock / 1000));
  return { raw, verified };
}

afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

// Removing shared reservation, freeing on attempt failure, or treating native
// completion alone as release must make these integration cases fail.
describe('shared retained connection and room capacity', () => {
  it('keeps exactly 1000 mixed provisional/live/terminal identities through hung native and unsigned release', async () => {
    const f = fixture();
    const parent = f.reserve('parent'); await f.owners.register(parent);
    const native = deferred<void>();
    const closeStarted = deferred<void>();
    const closeNative = vi.fn(() => { closeStarted.resolve(); return native.promise; });
    const retainedRoom = f.session(parent, 1);
    f.owners.reserveRoom(parent, retainedRoom, closeNative);
    await f.owners.attachRoom(parent, retainedRoom, token);
    for (let index = 2; index <= 500; index++) f.owners.reserveRoom(parent, f.session(parent, index), async () => {});
    const idle = Array.from({ length: 499 }, (_, index) => f.reserve(`provisional-${index}`));
    const before = f.calls.length;
    expect(f.owners.retainedIdentityCount()).toBe(1000);
    expect(() => f.reserve('overflow-connection')).toThrow();
    expect(() => f.owners.reserveRoom(parent, f.session(parent, 501), closeNative)).toThrow();
    expect(f.calls).toHaveLength(before);
    expect(closeNative).not.toHaveBeenCalled();
    let unsigned = true;
    f.answer(exchange => response(exchange, unsigned && exchange.request.endpoint() === 'room_release'));
    const terminal = f.owners.releaseRoom(retainedRoom);
    await closeStarted.promise;
    expect(f.owners.retainedIdentityCount()).toBe(1000);
    expect(() => f.reserve('overflow-hung-native')).toThrow();
    native.resolve();
    await settledTurn();
    expect(f.owners.retainedIdentityCount()).toBe(1000);
    expect(() => f.reserve('overflow-unverified-release')).toThrow();
    expect(f.owners.releaseRoom(retainedRoom)).toBe(terminal);
    await settledTurn();
    unsigned = false;
    await f.owners.retryPending(); await terminal;
    expect(closeNative).toHaveBeenCalledOnce();
    expect(f.owners.retainedIdentityCount()).toBe(999);
    const replacement = f.reserve('one-freed-slot');
    expect(f.owners.retainedIdentityCount()).toBe(1000);
    expect(() => f.reserve('no-second-freed-slot')).toThrow();
    await f.owners.terminate(replacement);
    await Promise.all(idle.map(owner => f.owners.terminate(owner)));
    await f.owners.terminate(parent);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('retains a lost registration and lost provisional-close receipt until an exact signed retry', async () => {
    const f = fixture();
    const lost = f.reserve('lost-register');
    const parent = f.reserve('live-parent'); await f.owners.register(parent);
    for (let index = 1; index <= 998; index++) f.owners.reserveRoom(parent, f.session(parent, index), async () => {});
    let recovered = false;
    f.answer(exchange => {
      if (!recovered && (exchange.request.endpoint() === 'connection_register' ||
          exchange.request.endpoint() === 'provisional_close')) throw new Error('Response lost after commit');
      return response(exchange);
    });
    await expect(f.owners.register(lost)).rejects.toThrow();
    const terminal = f.owners.terminate(lost);
    await settledTurn();
    expect(f.owners.retainedIdentityCount()).toBe(1000);
    expect(() => f.reserve('lost-receipt-overflow')).toThrow();
    expect(f.owners.terminate(lost)).toBe(terminal);
    await settledTurn();
    recovered = true;
    await f.owners.retryPending(); await terminal;
    expect(f.owners.retainedIdentityCount()).toBe(999);
    const successor = f.reserve('lost-register'); await f.owners.register(successor);
    expect(f.owners.fenceAndJoin(command(f, lost).verified)).toBeUndefined();
    expect(() => f.owners.assertProductive(successor)).not.toThrow();
    expect(f.owners.retainedIdentityCount()).toBe(1000);
    await f.owners.terminateAll();
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('joins one native terminal task across verified receiver attempt timeouts and acknowledges only signed cleanup', async () => {
    const f = fixture();
    const owner = f.reserve('timeout-owner'); await f.owners.register(owner);
    const room = f.session(owner, 1);
    const native = deferred<void>();
    const nativeStarted = deferred<void>();
    const close = vi.fn(() => { nativeStarted.resolve(); return native.promise; });
    f.owners.reserveRoom(owner, room, close);
    await f.owners.attachRoom(owner, room, token);
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout'] });
    vi.setSystemTime(clock);
    const receive = createVoiceAuthReceiver(secret, boot, {
      verify: () => undefined, terminalKey: () => '', terminal: async () => {}, acknowledge: () => undefined, health: async () => undefined,
    }, { fenceAndJoin: value => f.owners.fenceAndJoin(value) });
    const first = command(f, owner);
    const reply = vi.fn();
    receive(first.raw, reply);
    const task = f.owners.fenceAndJoin(first.verified);
    await nativeStarted.promise;
    expect(task).toBeDefined();
    let latest = first;
    for (let attempt = 0; attempt < 3; attempt++) {
      await vi.advanceTimersByTimeAsync(5_001);
      expect(reply).not.toHaveBeenCalled();
      // Requests use a fresh attempt timestamp while the owner/task stays exact.
      vi.setSystemTime(clock);
      latest = command(f, owner, attempt % 2 ? 'cd' : 'ef');
      receive(latest.raw, reply);
      expect(f.owners.fenceAndJoin(first.verified)).toBe(task);
      expect(f.owners.retainedIdentityCount()).toBe(2);
      expect(close).toHaveBeenCalledOnce();
    }
    native.resolve(); await task; await Promise.resolve();
    expect(reply).toHaveBeenCalledOnce();
    protocol.verifyAck(reply.mock.calls[0][0], latest.verified, key, BigInt(clock / 1000));
    expect(f.owners.retainedIdentityCount()).toBe(0);
    const successor = f.reserve('timeout-owner'); await f.owners.register(successor);
    receive(first.raw, reply);
    expect(reply).toHaveBeenCalledOnce();
    expect(() => f.owners.assertProductive(successor)).not.toThrow();
    await f.owners.terminate(successor);
  });
});


describe('shared lifecycle release accounting', () => {
  it('keeps copied family snapshots in their private room task while expiry includes idle and provisional connections', async () => {
    const f = fixture();
    const live = f.reserve('live-room'); await f.owners.register(live);
    const idle = f.reserve('idle-registered'); await f.owners.register(idle);
    const provisional = f.reserve('idle-provisional');
    const room = f.session(live, 1);
    const native = deferred<void>();
    const nativeStarted = deferred<void>();
    const close = vi.fn(() => { nativeStarted.resolve(); return native.promise; });
    f.owners.reserveRoom(live, room, close);
    await f.owners.attachRoom(live, room, token);
    const verifyLegacyNative = vi.fn(async () => {});
    const queue = new VoiceEnforcementSessionReleaseQueue(verifyLegacyNative, undefined, f.owners);
    // An old socket-owned copy is untrusted as a capability but still resolves
    // the same immutable private identity. It must never allocate another slot.
    const sockets = [{ data: { voiceEnforcementSession: { ...room } } }, { data: {} }];
    expect(queue.enqueue({ ...room })).toBe(true);
    queue.holdAll([{ ...room }, { ...room }]);
    expect(queue.outstandingCount()).toBe(4);
    expect(snapshotVoiceEnforcementSessions(sockets, f.owners)).toEqual([]);
    let monotonic = 0;
    const lease = new VoiceEnforcementLease(() => monotonic);
    lease.renew(); monotonic = 30_001;
    const fence = new VoiceEnforcementExpiryFence(lease, () => f.owners.terminateAll());
    const terminal = fence.enforce();
    await nativeStarted.promise;
    expect(fence.enforce()).toBe(terminal);
    expect(() => f.owners.assertProductive(live)).toThrow();
    expect(() => f.owners.assertProductive(idle)).toThrow();
    expect(() => f.owners.assertProductive(provisional)).toThrow();
    expect(f.disconnects.get('idle-registered')).toHaveBeenCalledOnce();
    expect(f.disconnects.get('idle-provisional')).toHaveBeenCalledOnce();
    await settledTurn();
    expect(queue.outstandingCount()).toBe(2);
    queue.promoteHeld(); await queue.retryPending();
    expect(queue.outstandingCount()).toBe(2);
    expect(close).toHaveBeenCalledOnce();
    expect(verifyLegacyNative).not.toHaveBeenCalled();
    native.resolve(); await terminal; await queue.retryPending();
    expect(queue.outstandingCount()).toBe(0);
    expect(f.calls.filter(call => call.request.endpoint() === 'room_release')).toHaveLength(1);
    expect(f.calls.filter(call => call.request.endpoint() === 'connection_release')).toHaveLength(2);
    expect(f.calls.filter(call => call.request.endpoint() === 'provisional_close')).toHaveLength(1);
  });

  it('shutdown returns outstanding private identities promptly and drains lost proofs without cloned-family ghosts', async () => {
    const f = fixture();
    const live = f.reserve('shutdown-room'); await f.owners.register(live);
    const idle = f.reserve('shutdown-idle'); await f.owners.register(idle);
    f.reserve('shutdown-provisional');
    const room = f.session(live, 1);
    const native = deferred<void>();
    f.owners.reserveRoom(live, room, () => native.promise);
    await f.owners.attachRoom(live, room, token);
    let lose = true;
    f.answer(exchange => {
      if (lose && exchange.request.endpoint() !== 'connection_register') throw new Error('Committed release response lost');
      return response(exchange);
    });
    const queue = new VoiceEnforcementSessionReleaseQueue(async () => { throw new Error('Family entered legacy release'); }, undefined, f.owners);
    const sockets = [{ data: { voiceEnforcementSession: { ...room } } }, { data: {} }];
    const remaining = await releaseVoiceEnforcementSessionsAfterClose(sockets, queue, f.owners);
    expect(remaining).toBe(4);
    expect(queue.outstandingCount()).toBe(4);
    expect(f.owners.hasPendingTermination()).toBe(true);
    native.resolve(); await settledTurn();
    expect(queue.outstandingCount()).toBe(4);
    const all = f.owners.terminateAll();
    lose = false;
    await queue.retryPending(); await all;
    expect(queue.outstandingCount()).toBe(0);
    expect(f.owners.hasPendingTermination()).toBe(false);
    expect(f.calls.filter(call => call.request.endpoint() === 'room_release')).toHaveLength(2);
  });

  it('reserves legacy release liability in the same full budget before invoking native or remote effects', async () => {
    const f = fixture();
    const parent = f.reserve('legacy-budget-parent'); await f.owners.register(parent);
    for (let index = 1; index <= 998; index++) f.owners.reserveRoom(parent, f.session(parent, index), async () => {});
    const legacy = { ...f.session(parent, 999), connectionGeneration: undefined };
    const release = deferred<void>();
    const effectStarted = deferred<void>();
    const legacyEffect = vi.fn(() => { effectStarted.resolve(); return release.promise; });
    f.owners.reserveLegacyRoom(legacy, legacyEffect);
    expect(f.owners.retainedIdentityCount()).toBe(1000);
    expect(legacyEffect).not.toHaveBeenCalled();
    const overflow = { ...legacy, sessionGeneration: 'ffffffff-5555-4555-8555-555555555555' };
    expect(() => f.owners.reserveLegacyRoom(overflow, legacyEffect)).toThrow();
    expect(legacyEffect).not.toHaveBeenCalled();
    const terminal = f.owners.releaseLegacyRoom(legacy);
    await effectStarted.promise;
    expect(f.owners.releaseLegacyRoom({ ...legacy })).toBe(terminal);
    expect(f.owners.retainedIdentityCount()).toBe(1000);
    expect(() => f.reserve('legacy-retrying-full')).toThrow();
    release.resolve(); await terminal;
    expect(f.owners.retainedIdentityCount()).toBe(999);
    const next = f.reserve('legacy-one-slot');
    expect(() => f.reserve('legacy-over-one-slot')).toThrow();
    await f.owners.terminate(next); await f.owners.terminate(parent);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });
});


describe('expiry barrier publication', () => {
  it('publishes one pending barrier before synchronous disconnect reentry joins native and terminal cleanup', async () => {
    const f = fixture();
    const owner = f.reserve('reentrant-disconnect'); await f.owners.register(owner);
    const room = f.session(owner, 1);
    const native = deferred<void>();
    const nativeStarted = deferred<void>();
    const close = vi.fn(() => { nativeStarted.resolve(); return native.promise; });
    f.owners.reserveRoom(owner, room, close);
    await f.owners.attachRoom(owner, room, token);
    const lease = new VoiceEnforcementLease(() => 0);
    let nested: Promise<void> | undefined;
    let ownerTask: Promise<void> | undefined;
    const teardown = vi.fn(() => {
      ownerTask = f.owners.terminateAll();
      return ownerTask;
    });
    const fence = new VoiceEnforcementExpiryFence(lease, teardown);
    f.disconnects.get('reentrant-disconnect')?.mockImplementation(() => {
      expect(fence.pending()).toBe(true);
      expect(fence.ready()).toBe(false);
      nested = fence.enforce();
    });
    const outer = fence.enforce();
    expect(nested).toBe(outer);
    expect(fence.enforce()).toBe(outer);
    expect(teardown).toHaveBeenCalledOnce();
    await nativeStarted.promise;
    const terminal = f.owners.terminate(owner);
    expect(f.owners.terminate(owner)).toBe(terminal);
    expect(close).toHaveBeenCalledOnce();
    expect(f.owners.retainedIdentityCount()).toBe(2);
    native.resolve(); await outer; await ownerTask; await terminal;
    expect(fence.pending()).toBe(false);
    expect(fence.ready()).toBe(true);
    expect(f.owners.retainedIdentityCount()).toBe(0);
    expect(f.calls.filter(call => call.request.endpoint() === 'room_release')).toHaveLength(1);
    expect(f.calls.filter(call => call.request.endpoint() === 'connection_release')).toHaveLength(1);
  });
});


describe('legacy generation collision refusal', () => {
  it('refuses a conflicting unmarked legacy tuple sharing a private family room generation before effects or allocation', async () => {
    const f = fixture();
    const original = f.reserve('collision-original'); await f.owners.register(original);
    const successor = f.reserve('collision-successor'); await f.owners.register(successor);
    const exact = f.session(original, 1);
    const closeNative = vi.fn(async () => {});
    f.owners.reserveRoom(original, exact, closeNative);
    await f.owners.attachRoom(original, exact, token);
    const conflicting = { ...exact, connectionGeneration: undefined,
      socketId: 'collision-successor', roomId: '77777777-7777-4777-8777-777777777777' };
    const legacyEffect = vi.fn(async () => {});
    const callsBefore = f.calls.length;
    expect(f.owners.retainedIdentityCount()).toBe(3);
    expect(() => f.owners.reserveLegacyRoom(conflicting, legacyEffect)).toThrow();
    expect(f.owners.retainedIdentityCount()).toBe(3);
    expect(f.calls).toHaveLength(callsBefore);
    expect(legacyEffect).not.toHaveBeenCalled();
    expect(closeNative).not.toHaveBeenCalled();
    expect(() => f.owners.assertRoomProductive(exact.sessionGeneration)).not.toThrow();
    expect(() => f.owners.assertProductive(original)).not.toThrow();
    expect(() => f.owners.assertProductive(successor)).not.toThrow();
    await f.owners.terminateAll();
    expect(closeNative).toHaveBeenCalledOnce();
    expect(legacyEffect).not.toHaveBeenCalled();
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });
});

describe('stalled termination readiness', () => {
  it('keeps a routine in-flight departure from expiring the boot lease, but not a failed one', async () => {
    const f = fixture();
    const live = f.reserve('routine-leave'); await f.owners.register(live);
    const room = f.session(live, 1);
    const native = deferred<void>();
    f.owners.reserveRoom(live, room, () => native.promise);
    await f.owners.attachRoom(live, room, token);
    let fail = true;
    f.answer(exchange => {
      if (fail && exchange.request.endpoint() === 'room_release') throw new Error('release unavailable');
      return response(exchange);
    });
    const terminal = f.owners.releaseRoom(room);
    await settledTurn();
    expect(f.owners.hasPendingTermination()).toBe(true);
    expect(f.owners.hasStalledTermination()).toBe(false);
    native.resolve(); await settledTurn(); await settledTurn();
    expect(f.owners.hasStalledTermination()).toBe(true);
    fail = false;
    await f.owners.retryPending(); await terminal;
    expect(f.owners.hasPendingTermination()).toBe(false);
    expect(f.owners.hasStalledTermination()).toBe(false);
  });

  it('treats a release that outlives its bounded budget as stalled', async () => {
    let now = clock;
    const owners = new VoiceAuthConnections({ nodeBootId: boot, secret, controlPlaneUrl: 'https://control.test.invalid',
      now: () => now, fetch: () => new Promise<Response>(() => undefined) });
    const handle = owners.reserve({ socketId: 'hung-close', userId: user, credentialEpoch: epoch, token,
      tokenExpiresAt: clock + 60_000, disconnect: () => undefined });
    void owners.terminate(handle);
    await settledTurn();
    now += 14_999;
    expect(owners.hasStalledTermination()).toBe(false);
    now += 1;
    expect(owners.hasStalledTermination()).toBe(true);
  });
});
