import { createHash } from 'node:crypto';
import { EventEmitter } from 'node:events';
import type { Socket } from 'socket.io';
import { describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import { createVoiceAuthConnectionMiddleware, getVoiceAuthConnection } from '../src/middleware/auth.js';

const secret = 'voice-auth-connection-test-only-key'; // pragma: allowlist secret
const key = Buffer.from(secret);
const now = 1793000000000;
const boot = '00000002-1111-4111-8111-111111111111';
const user = '00000003-1111-4111-8111-111111111111';
const family = '00000004-1111-4111-8111-111111111111';
const epoch = '0123456789abcdef0123456789abcdef'; // pragma: allowlist secret
const token = 'test-original-token';
const timestamp = String(now / 1000);
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
const paths: Record<string, protocol.Endpoint> = {
  '/api/v1/internal/voice/auth-connections': 'connection_register',
  '/api/v1/internal/voice/auth-connections/release': 'connection_release',
  '/api/v1/internal/voice/auth-connections/provisional-close': 'provisional_close',
};
interface Exchange { request: protocol.Request; wire: protocol.HTTPRequest }
function cp() {
  const calls: Exchange[] = [];
  let respond: (exchange: Exchange) => Response | Promise<Response> = exchange => signed(exchange);
  const fetcher = vi.fn<typeof fetch>(async (input, init) => {
    const path = new URL(String(input)).pathname;
    const endpoint = paths[path];
    if (!endpoint) throw new Error(`Unexpected endpoint ${path}`);
    const headers: protocol.Header[] = [];
    new Headers(init?.headers).forEach((value, name) => headers.push({ name, value }));
    const raw = init?.body;
    if (typeof raw !== 'string' && !(raw instanceof Uint8Array)) throw new Error('Missing exact raw request');
    const wire = { method: init?.method ?? '', path, headers, body: Buffer.from(raw) };
    const request = protocol.decodeHttpRequest(endpoint, wire);
    protocol.verifyHttpRequest(request, key, BigInt(timestamp));
    const exchange = { request, wire };
    calls.push(exchange);
    return respond(exchange);
  });
  return { calls, fetcher, respondWith(handler: typeof respond) { respond = handler; } };
}
function signed(exchange: Exchange, mutate?: (wire: protocol.HTTPResponse) => void): Response {
  const registration = exchange.request.connectionRegister();
  const close = exchange.request.provisionalClose();
  const payload: protocol.ResponsePayload = registration
    ? { version: 1, voiceEnforcementProtocol: 4, userId: user, familyId: family, target: {
      connectionGeneration: registration.connectionGeneration, nodeBootId: registration.nodeBootId,
      socketId: registration.socketId, credentialEpoch: registration.credentialEpoch,
    } }
    : close ? { ...close, schema: 'concord.auth-connection-provisional-close-result', disposition: 'unregistered' }
      : null;
  const wire = protocol.signHttpResponse(exchange.request, payload, timestamp, key);
  mutate?.(wire);
  return new Response(wire.status === 204 ? null : Buffer.from(wire.body), {
    status: wire.status, headers: wire.headers.map(header => [header.name, header.value]),
  });
}
// Distinct accounts, so filling the shared budget never trips the per-user cap.
const filler = (index: number) => `${index.toString(16).padStart(8, '0')}-9999-4999-8999-999999999999`;
function fixture() {
  const remote = cp();
  const owners = new VoiceAuthConnections({ nodeBootId: boot, secret, controlPlaneUrl: 'https://cp.test.invalid', fetch: remote.fetcher, now: () => now });
  const disconnect = vi.fn();
  const reserve = (socketId = 'connection-test', userId = user) => owners.reserve({ socketId, userId, credentialEpoch: epoch, token, tokenExpiresAt: now + 60000, disconnect });
  return { owners, remote, disconnect, reserve };
}

// Network answers are controlled here; protocol validation and private ownership
// execute unchanged. These idle owners have no native work by construction.
describe('private auth connection ownership', () => {
  it('reserves synchronously before registration awaits and admits only a signed reply', async () => {
    const f = fixture();
    const reply = deferred<Response>();
    f.remote.respondWith(exchange => exchange.request.connectionRegister() ? reply.promise : signed(exchange));
    const handle = f.reserve();
    expect(f.owners.retainedIdentityCount()).toBe(1);
    expect(f.remote.fetcher).not.toHaveBeenCalled();
    expect(() => f.owners.assertProductive(handle)).toThrow();
    const registration = f.owners.register(handle);
    expect(f.remote.calls).toHaveLength(1);
    expect(() => f.owners.assertProductive(handle)).toThrow();
    reply.resolve(signed(f.remote.calls[0]));
    await registration;
    expect(() => f.owners.assertProductive(handle)).not.toThrow();
    await f.owners.terminate(handle);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('rejects a forged handle without making network requests', async () => {
    const f = fixture();
    expect(() => f.owners.register({} as ReturnType<typeof f.reserve>)).toThrow();
    expect(f.remote.calls).toHaveLength(0);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('refuses the 1001st retained connection before any network effect', () => {
    const f = fixture();
    for (let i = 0; i < 1000; i++) f.reserve(`capacity-${i}`, filler(i));
    expect(f.owners.retainedIdentityCount()).toBe(1000);
    expect(() => f.reserve('over-capacity')).toThrow();
    expect(f.remote.calls).toHaveLength(0);
    expect(f.owners.retainedIdentityCount()).toBe(1000);
  });

  it('caps one account at 16 live connections without touching other accounts', async () => {
    const f = fixture();
    const held = Array.from({ length: 16 }, (_, i) => f.reserve(`per-user-${i}`));
    expect(() => f.reserve('per-user-over')).toThrow(/for user/);
    expect(f.owners.retainedIdentityCount()).toBe(16);
    expect(() => f.reserve('other-account', filler(1))).not.toThrow();
    await f.owners.terminate(held[0]);
    expect(() => f.reserve('per-user-freed')).not.toThrow();
    expect(f.remote.calls.filter(call => call.request.endpoint() === 'connection_register')).toHaveLength(0);
  });

  it('shares the same 1000 identity budget between connections and rooms before native effects', async () => {
    const f = fixture();
    const handle = f.reserve(); await f.owners.register(handle);
    const closeNative = vi.fn(async () => {});
    for (let i = 1; i < 1000; i++) {
      f.owners.reserveRoom(handle, {
        sessionGeneration: `${i.toString(16).padStart(8, '0')}-2222-4222-8222-222222222222`,
        nodeBootId: boot, roomId: '00000006-1111-4111-8111-111111111111',
        roomKind: 'channel', userId: user, credentialEpoch: epoch, socketId: 'connection-test',
      }, closeNative);
    }
    expect(f.owners.retainedIdentityCount()).toBe(1000);
    expect(() => f.reserve('mixed-over-cap')).toThrow();
    expect(() => f.owners.reserveRoom(handle, {
      sessionGeneration: '00000000-2222-4222-8222-222222222222', nodeBootId: boot,
      roomId: '00000006-1111-4111-8111-111111111111', roomKind: 'channel',
      userId: user, credentialEpoch: epoch, socketId: 'connection-test',
    }, closeNative)).toThrow();
    expect(closeNative).not.toHaveBeenCalled();
    expect(f.remote.calls).toHaveLength(1);
    expect(f.owners.retainedIdentityCount()).toBe(1000);
  });

  it('refuses an unsigned successful registration and closes provisionally', async () => {
    const f = fixture();
    f.remote.respondWith(exchange => exchange.request.connectionRegister()
      ? signed(exchange, wire => { wire.headers = []; }) : signed(exchange));
    const handle = f.reserve();
    await expect(f.owners.register(handle)).rejects.toThrow();
    expect(() => f.owners.assertProductive(handle)).toThrow();
    await f.owners.terminate(handle);
    expect(f.remote.calls.map(call => call.request.endpoint())).toEqual(['connection_register', 'provisional_close']);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('uses exact original registration commitments after a lost reply', async () => {
    const f = fixture();
    f.remote.respondWith(exchange => {
      if (exchange.request.connectionRegister()) throw new Error('reply lost after CP commit');
      return signed(exchange);
    });
    const handle = f.reserve();
    await expect(f.owners.register(handle)).rejects.toThrow();
    await f.owners.terminate(handle);
    const original = f.remote.calls[0];
    const close = f.remote.calls[1].request.provisionalClose();
    expect(close?.registrationBodyDigest).toBe(createHash('sha256').update(original.wire.body).digest('hex'));
    expect(close?.registrationTokenDigest).toBe(createHash('sha256').update(token).digest('hex'));
    expect(close?.target).toEqual(f.owners.snapshot(handle).target);
    expect(f.remote.calls[1].wire.headers.some(header => header.name.toLowerCase() === 'authorization')).toBe(false);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('fences immediately and joins one terminal task while registration is pending', async () => {
    const f = fixture();
    const reply = deferred<Response>();
    f.remote.respondWith(exchange => exchange.request.connectionRegister() ? reply.promise : signed(exchange));
    const handle = f.reserve();
    const registration = f.owners.register(handle).catch(error => error);
    const terminal = f.owners.terminate(handle);
    expect(f.owners.terminate(handle)).toBe(terminal);
    expect(() => f.owners.assertProductive(handle)).toThrow();
    reply.resolve(signed(f.remote.calls[0]));
    await registration;
    await terminal;
    expect(() => f.owners.assertProductive(handle)).toThrow();
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('retains a lost-release liability and the same terminal task until signed retry succeeds', async () => {
    const f = fixture();
    const releaseStarted = deferred<void>();
    let lose = true;
    f.remote.respondWith(exchange => {
      if (exchange.request.connectionRelease() && lose) { releaseStarted.resolve(); throw new Error('release receipt lost'); }
      return signed(exchange);
    });
    const handle = f.reserve(); await f.owners.register(handle);
    const terminal = f.owners.terminate(handle);
    await releaseStarted.promise;
    expect(f.owners.retainedIdentityCount()).toBe(1);
    expect(f.owners.terminate(handle)).toBe(terminal);
    expect(() => f.owners.assertProductive(handle)).toThrow();
    // Let the rejected fetch propagate through the retained attempt's finally.
    await new Promise<void>(resolve => setImmediate(resolve));
    lose = false;
    await f.owners.retryPending();
    await terminal;
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('does not apply an old generation command to a reused socket ID', async () => {
    const f = fixture();
    const old = f.reserve('same-socket'); await f.owners.register(old);
    const oldTarget = f.owners.snapshot(old).target;
    await f.owners.terminate(old);
    const fresh = f.reserve('same-socket'); await f.owners.register(fresh);
    expect(f.owners.snapshot(fresh).target.connectionGeneration).not.toBe(oldTarget.connectionGeneration);
    const command = protocol.newAuthCommand({ userId: user, familyId: family, target: oldTarget },
      '00000005-1111-4111-8111-111111111111', 'session', 'logout', { timestamp, nonce: 'ab'.repeat(32) });
    const joined = f.owners.fenceAndJoin(command);
    if (joined) await joined;
    expect(() => f.owners.assertProductive(fresh)).not.toThrow();
    expect(f.owners.retainedIdentityCount()).toBe(1);
    await f.owners.terminate(fresh);
  });

  it('withholds command witnesses while family is unknown and joins only the verified exact binding', async () => {
    const f = fixture();
    const registrationReply = deferred<Response>();
    const releaseReply = deferred<Response>();
    const releaseStarted = deferred<Exchange>();
    f.remote.respondWith(exchange => {
      if (exchange.request.connectionRegister()) return registrationReply.promise;
      releaseStarted.resolve(exchange); return releaseReply.promise;
    });
    const handle = f.reserve();
    const registration = f.owners.register(handle).catch(error => error);
    const target = f.owners.snapshot(handle).target;
    const command = (claimedFamily: string, nonceByte = 'ab') => {
      const unsigned = protocol.newAuthCommand({ userId: user, familyId: claimedFamily, target },
        '00000005-1111-4111-8111-111111111111', 'session', 'logout',
        { timestamp, nonce: nonceByte.repeat(32) });
      const verified = protocol.decodeCommand(protocol.signCommand(unsigned, key));
      protocol.verifyCommand(verified, key, BigInt(timestamp));
      return verified;
    };
    const mismatch = command('00000007-1111-4111-8111-111111111111');
    const match = command(family);
    // A signed caller claim cannot supply the owner's not-yet-known family.
    expect(f.owners.fenceAndJoin(mismatch)).toBeUndefined();
    expect(f.owners.snapshot(handle).state).toBe('terminal');
    expect(f.owners.fenceAndJoin(match)).toBeUndefined();
    expect(f.owners.fenceAndJoin(mismatch)).toBeUndefined();
    expect(() => f.owners.assertProductive(handle)).toThrow();
    const terminal = f.owners.terminate(handle);
    registrationReply.resolve(signed(f.remote.calls[0]));
    const closing = await releaseStarted.promise;
    await registration;
    expect(closing.request.connectionRelease()?.familyId).toBe(family);
    expect(f.owners.fenceAndJoin(mismatch)).toBeUndefined();
    expect(f.owners.fenceAndJoin(match)).toBe(terminal);
    // New wire attempts also join the one generation-owned terminal promise.
    expect(f.owners.fenceAndJoin(command(family, 'cd'))).toBe(terminal);
    expect(() => f.owners.assertProductive(handle)).toThrow();
    releaseReply.resolve(signed(closing));
    await terminal;
    expect(f.owners.retainedIdentityCount()).toBe(0);
    expect(() => f.owners.assertProductive(handle)).toThrow();
  });

  it('does not turn an unbound command claim into a witness when lost registration closes with another family', async () => {
    const f = fixture();
    const registrationReply = deferred<Response>();
    const closeReply = deferred<Response>();
    const closeStarted = deferred<Exchange>();
    f.remote.respondWith(exchange => {
      if (exchange.request.connectionRegister()) return registrationReply.promise;
      closeStarted.resolve(exchange); return closeReply.promise;
    });
    const handle = f.reserve();
    const registration = f.owners.register(handle).catch(error => error);
    const target = f.owners.snapshot(handle).target;
    const unsigned = protocol.newAuthCommand({ userId: user,
      familyId: '00000007-1111-4111-8111-111111111111', target },
      '00000005-1111-4111-8111-111111111111', 'session', 'logout',
      { timestamp, nonce: 'ab'.repeat(32) });
    const command = protocol.decodeCommand(protocol.signCommand(unsigned, key));
    protocol.verifyCommand(command, key, BigInt(timestamp));
    expect(f.owners.fenceAndJoin(command)).toBeUndefined();
    expect(f.owners.snapshot(handle).state).toBe('terminal');
    const terminal = f.owners.terminate(handle);
    registrationReply.reject(new Error('registration committed but response lost'));
    const closing = await closeStarted.promise;
    await registration;
    expect(f.owners.fenceAndJoin(command)).toBeUndefined();
    const close = closing.request.provisionalClose();
    if (!close) throw new Error('Expected exact provisional close');
    const wire = protocol.signHttpResponse(closing.request, {
      ...close, schema: 'concord.auth-connection-provisional-close-result', disposition: 'registered',
      binding: { userId: user, familyId: family, target },
    }, timestamp, key);
    closeReply.resolve(new Response(Buffer.from(wire.body), {
      status: wire.status, headers: wire.headers.map(header => [header.name, header.value]),
    }));
    await terminal;
    expect(f.owners.fenceAndJoin(command)).toBeUndefined();
    expect(f.owners.retainedIdentityCount()).toBe(0);
    expect(() => f.owners.assertProductive(handle)).toThrow();
  });

  it('joins cleanup on failed namespace middleware even when no disconnect event occurs', async () => {
    const f = fixture();
    const registered = deferred<Response>();
    const release = deferred<Response>();
    const releaseStarted = deferred<Exchange>();
    f.remote.respondWith(exchange => {
      if (exchange.request.connectionRegister()) return registered.promise;
      releaseStarted.resolve(exchange); return release.promise;
    });
    const socket = Object.assign(new EventEmitter(), {
      id: 'middleware-socket', connected: false,
      handshake: { auth: { token } }, conn: Object.assign(new EventEmitter(), { readyState: 'open' }),
      data: { userId: user, credentialEpoch: epoch, tokenExpiresAt: now + 60000 }, disconnect: f.disconnect,
    });
    let available = true;
    const next = vi.fn();
    const middleware = createVoiceAuthConnectionMiddleware(f.owners, () => available);
    const result = middleware(socket as unknown as Socket, next);
    expect(getVoiceAuthConnection(socket as unknown as Socket)).toBeDefined();
    expect(f.owners.retainedIdentityCount()).toBe(1);
    available = false;
    registered.resolve(signed(f.remote.calls[0]));
    const closing = await releaseStarted.promise;
    // The handshake is refused immediately; cleanup continues in the background.
    expect(next).toHaveBeenCalledWith(expect.any(Error));
    expect(f.owners.retainedIdentityCount()).toBe(1);
    release.resolve(signed(closing));
    await result;
    await vi.waitFor(() => expect(f.owners.retainedIdentityCount()).toBe(0));
  });

  it('refuses the handshake even when terminate never settles', async () => {
    const f = fixture();
    let available = true;
    f.remote.respondWith(exchange => { available = false; return Promise.resolve(signed(exchange)); });
    // terminate() settles only after a successful control-plane release.
    const terminate = vi.spyOn(f.owners, 'terminate').mockReturnValue(new Promise<void>(() => undefined));
    const socket = Object.assign(new EventEmitter(), {
      id: 'hung-socket', connected: false,
      handshake: { auth: { token } }, conn: Object.assign(new EventEmitter(), { readyState: 'open' }),
      data: { userId: user, credentialEpoch: epoch, tokenExpiresAt: now + 60000 }, disconnect: f.disconnect,
    });
    const next = vi.fn();
    const middleware = createVoiceAuthConnectionMiddleware(f.owners, () => available);
    await middleware(socket as unknown as Socket, next);
    expect(terminate).toHaveBeenCalledOnce();
    expect(next).toHaveBeenCalledOnce();
    expect(next).toHaveBeenCalledWith(expect.any(Error));
  });
});
