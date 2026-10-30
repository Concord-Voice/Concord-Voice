import { afterEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import jwt from 'jsonwebtoken';
import type { Socket } from 'socket.io';
import { createSocketTokenRenewal } from '../src/middleware/auth.js';
vi.mock('@/config/index.js', () => ({ config: { jwtSecret: ['native', 'renewal', 'test', 'only'].join('-'), controlPlaneUrl: 'http://control.test' } }));
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import type { VoiceEnforcementSession } from '../src/lib/voiceEnforcementSession.js';

const userId = '11111111-1111-4111-8111-111111111111';
const familyId = '22222222-2222-4222-8222-222222222222';
const nodeBootId = '33333333-3333-4333-8333-333333333333';
const roomId = '44444444-4444-4444-8444-444444444444';
const sessionGeneration = '55555555-5555-4555-8555-555555555555';
const credentialEpoch = 'a'.repeat(32);
const secret = ['native', 'renewal', 'test', 'only'].join('-');
const now = 1_800_000_000_000;
function deferred<T>() {
  let resolve: (value: T) => void = () => { throw new Error('uninitialized deferred'); };
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}
function fixture() {
  const paths: string[] = [];
  const requests: protocol.Request[] = [];
  const responseIdentity = { userId, familyId };
  const replies: Partial<Record<protocol.Endpoint, () => Promise<void>>> = {};
  const fetchBoundary = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = new URL(String(input)).pathname;
    paths.push(path);
    const endpoint: protocol.Endpoint = path.endsWith('/auth-connections') ? 'connection_register'
      : path.endsWith('/auth-connections/release') ? 'connection_release'
      : path.endsWith('/enforcement-sessions/release') ? 'room_release' : 'room_register';
    const headers = Array.from(new Headers(init?.headers), ([name, value]) => ({ name, value }));
    const raw = init?.body;
    if (typeof raw !== 'string' && !(raw instanceof Uint8Array)) throw new Error('Missing exact request bytes');
    const request = protocol.decodeHttpRequest(endpoint, {
      method: 'POST', path, headers, body: Buffer.from(raw),
    });
    protocol.verifyHttpRequest(request, Buffer.from(secret), BigInt(now / 1000));
    requests.push(request);
    await replies[endpoint]?.();
    const registration = request.connectionRegister();
    const room = request.roomRegister();
    const target = registration ?? room;
    const binding = target ? { ...responseIdentity, target: {
      connectionGeneration: target.connectionGeneration, nodeBootId: target.nodeBootId,
      socketId: target.socketId, credentialEpoch: target.credentialEpoch,
    } } : undefined;
    const payload: protocol.ResponsePayload = registration && binding
      ? { ...binding, version: 1, voiceEnforcementProtocol: 4 }
      : room && binding ? { schema: 'concord.auth-room-binding', version: 1,
        voiceEnforcementProtocol: 4, binding, room: {
          sessionGeneration: room.sessionGeneration, roomId: room.roomId, roomKind: room.roomKind,
        } } : null;
    const signed = protocol.signHttpResponse(request, payload, String(now / 1000), Buffer.from(secret));
    return new Response(signed.body.length ? Buffer.from(signed.body) : null, {
      status: signed.status, headers: signed.headers.map(({ name, value }) => [name, value]),
    });
  });
  const owners = new VoiceAuthConnections({ nodeBootId, secret, controlPlaneUrl: 'http://control.test',
    fetch: fetchBoundary as typeof fetch, now: () => now });
  const reserve = () => owners.reserve({ socketId: 'same-socket', userId, credentialEpoch,
    token: 'test-original-token', tokenExpiresAt: now + 60_000, disconnect: vi.fn() });
  const session: VoiceEnforcementSession = { sessionGeneration, nodeBootId, roomId,
    roomKind: 'channel', userId, credentialEpoch, socketId: 'same-socket' };
  return { owners, reserve, session, paths, replies, requests, responseIdentity };
}

describe('admitted exact room renewal', () => {
  it('sends renew mode for the retained binding and refuses idle renewal before network effects', async () => {
    const f = fixture();
    const owner = f.reserve();
    await f.owners.register(owner);
    await expect(f.owners.renewRoom(owner, f.session, 'test-next-token', () => true)).rejects.toThrow();
    expect(f.paths).toHaveLength(1);
    f.owners.reserveRoom(owner, f.session, async () => undefined);
    await f.owners.attachRoom(owner, f.session, 'test-original-token');
    await f.owners.renewRoom(owner, f.session, 'test-next-token', () => true);
    expect(f.paths).toHaveLength(3);
    expect(f.requests[2].roomRegister()).toEqual({
      ...f.requests[1].roomRegister(), mode: 'renew',
    });
    await f.owners.terminate(owner);
  });

  it.each(['userId', 'familyId'] as const)('rejects a signed renewal response with another %s', async (field) => {
    const f = fixture();
    const owner = f.reserve();
    await f.owners.register(owner);
    f.owners.reserveRoom(owner, f.session, async () => undefined);
    await f.owners.attachRoom(owner, f.session, 'test-original-token');
    f.responseIdentity[field] = '66666666-6666-4666-8666-666666666666';
    await expect(f.owners.renewRoom(owner, f.session, 'test-next-token', () => true)).rejects.toThrow();
    expect(f.owners.snapshot(owner).tokenExpiresAt).toBe(now + 60_000);
    await f.owners.terminate(owner);
  });

  it('rechecks participant ownership after the signed response instead of accepting delayed success', async () => {
    const f = fixture();
    const owner = f.reserve();
    await f.owners.register(owner);
    f.owners.reserveRoom(owner, f.session, async () => undefined);
    await f.owners.attachRoom(owner, f.session, 'test-original-token');
    const response = deferred<void>();
    f.replies.room_register = () => response.promise;
    let current = true;
    const renewal = f.owners.renewRoom(owner, f.session, 'test-next-token', () => current);
    const denied = expect(renewal).rejects.toThrow();
    current = false;
    response.resolve();
    await denied;
    await f.owners.terminate(owner);
  });

  it('isolates a successor on the same socket ID from an older held renewal', async () => {
    const f = fixture();
    const oldOwner = f.reserve();
    await f.owners.register(oldOwner);
    f.owners.reserveRoom(oldOwner, f.session, async () => undefined);
    await f.owners.attachRoom(oldOwner, f.session, 'test-original-token');
    const response = deferred<void>();
    f.replies.room_register = () => response.promise;
    const oldRenewal = f.owners.renewRoom(oldOwner, f.session, 'test-next-token', () => true);
    const denied = expect(oldRenewal).rejects.toThrow();
    const oldTerminal = f.owners.terminate(oldOwner);
    delete f.replies.room_register;
    const successor = f.reserve();
    await f.owners.register(successor);
    const successorSession = { ...f.session, sessionGeneration: '77777777-7777-4777-8777-777777777777' };
    f.owners.reserveRoom(successor, successorSession, async () => undefined);
    await f.owners.attachRoom(successor, successorSession, 'test-original-token');
    expect(f.owners.snapshot(successor).target.connectionGeneration).not.toBe(f.owners.snapshot(oldOwner).target.connectionGeneration);
    response.resolve();
    await denied;
    await oldTerminal;
    expect(() => f.owners.assertProductive(successor)).not.toThrow();
    await f.owners.renewRoom(successor, successorSession, 'test-successor-token', () => true);
    expect(f.owners.snapshot(successor).tokenExpiresAt).toBe(now + 60_000);
    await f.owners.terminate(successor);
  });

  it('cannot restore productivity when a valid renewal reply arrives after terminal fencing', async () => {
    const f = fixture();
    const owner = f.reserve();
    await f.owners.register(owner);
    const closeNative = vi.fn(async () => undefined);
    f.owners.reserveRoom(owner, f.session, closeNative);
    await f.owners.attachRoom(owner, f.session, 'test-original-token');
    const response = deferred<void>();
    f.replies.room_register = () => response.promise;
    const renewal = f.owners.renewRoom(owner, f.session, 'test-next-token', () => true);
    const denied = expect(renewal).rejects.toThrow();
    const terminal = f.owners.terminate(owner);
    expect(() => f.owners.assertProductive(owner)).toThrow();
    response.resolve();
    await denied;
    await terminal;
    expect(closeNative).toHaveBeenCalledOnce();
    expect(() => f.owners.assertProductive(owner)).toThrow();
  });
});


describe('socket renewal deadline commit', () => {
  afterEach(() => vi.useRealTimers());
  function socketFixture() {
    vi.useFakeTimers();
    vi.setSystemTime(now);
    const socket = {
      connected: true, id: 'same-socket',
      data: { userId, credentialEpoch, tokenExpiresAt: now + 60_000 },
      handshake: { auth: { token: 'original-token' } },
      once: vi.fn(), use: vi.fn(), disconnect: vi.fn(),
    };
    socket.disconnect.mockImplementation(() => { socket.connected = false; });
    const token = (overrides: Record<string, unknown> = {}) => jwt.sign({
      user_id: userId, cred_epoch: credentialEpoch, exp: now / 1000 + 120,
      ...overrides,
    }, secret, { algorithm: 'HS256', issuer: 'concordvoice-control-plane' });
    return { socket, token };
  }

  it('updates the deadline and bearer only after authorization succeeds', async () => {
    const { socket, token } = socketFixture();
    const authorized = deferred<void>();
    const validate = vi.fn(() => authorized.promise);
    const renew = createSocketTokenRenewal(socket as unknown as Socket, validate);
    const callback = vi.fn();
    const replacement = token();
    const pending = renew(replacement, callback);
    expect(socket.data.tokenExpiresAt).toBe(now + 60_000);
    expect(socket.handshake.auth.token).toBe('original-token');
    expect(callback).not.toHaveBeenCalled();
    authorized.resolve();
    await pending;
    expect(callback).toHaveBeenCalledWith({ success: true });
    expect(socket.data.tokenExpiresAt).toBe(now + 120_000);
    expect(socket.handshake.auth.token).toBe(replacement);
    await vi.advanceTimersByTimeAsync(120_000);
    expect(socket.disconnect).toHaveBeenCalledWith(true);
  });

  it.each([
    ['different user', { user_id: familyId }],
    ['different epoch', { cred_epoch: 'b'.repeat(32) }],
    ['equal deadline', { exp: now / 1000 + 60 }],
    ['earlier deadline', { exp: now / 1000 + 30 }],
  ])('denies %s before authorization or deadline mutation', async (_, claims) => {
    const { socket, token } = socketFixture();
    const validate = vi.fn(async () => undefined);
    const renew = createSocketTokenRenewal(socket as unknown as Socket, validate);
    const callback = vi.fn();
    await renew(token(claims), callback);
    expect(validate).not.toHaveBeenCalled();
    expect(callback).toHaveBeenCalledWith({ error: 'Authentication failed' });
    expect(socket.data.tokenExpiresAt).toBe(now + 60_000);
    expect(socket.handshake.auth.token).toBe('original-token');
  });

  it('does not extend the original deadline after it expires during authorization', async () => {
    const { socket, token } = socketFixture();
    const authorized = deferred<void>();
    const renew = createSocketTokenRenewal(socket as unknown as Socket, () => authorized.promise);
    const callback = vi.fn();
    const pending = renew(token(), callback);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(socket.disconnect).toHaveBeenCalledWith(true);
    authorized.resolve();
    await pending;
    expect(callback).toHaveBeenCalledWith({ error: 'Authentication failed' });
    expect(socket.data.tokenExpiresAt).toBe(now + 60_000);
  });
});
