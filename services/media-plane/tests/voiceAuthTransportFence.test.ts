import { describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
vi.mock('mediasoup', () => ({ createWorker: vi.fn() }));
import { RoomManager, SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION } from '../src/lib/roomManager.js';
import type { MediasoupService } from '../src/lib/mediasoup.js';
import { createMockRouter, createMockTransport, createRtpCapabilities } from './mocks/mediasoup.js';
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import { type VoiceEnforcementSession } from '../src/lib/voiceEnforcementSession.js';

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

// Only external HTTP/native-worker boundaries are controlled. Room ownership,
// participant replacement, receive-drain tracking, and terminal teardown are real.
const nextTurn = () => new Promise<void>((resolve) => setImmediate(resolve));

async function nativeFixture() {
  const f = fixture();
  const router = createMockRouter({ id: 'fence-router' });
  const service = {
    getWebRtcServer: vi.fn(() => undefined), getOrCreateRouter: vi.fn(async () => router),
    removeRouter: vi.fn(),
    captureNativeTermination: vi.fn((targetRouter, transportIds?: readonly string[]) => ({
      worker: { closed: false }, router: targetRouter, routerId: targetRouter.id, transportIds,
    })),
    confirmNativeTermination: vi.fn(async () => undefined),
  };
  const manager = new RoomManager(service as unknown as MediasoupService);
  manager.setVoiceAuthConnections(f.owners);
  const owner = f.reserve();
  await f.owners.register(owner);
  async function join(handle: typeof owner, session: VoiceEnforcementSession) {
    manager.reserveVoiceAuthRoom(handle, session);
    await f.owners.trackRoomOperation(session, () => manager.joinRoom(roomId, userId, session.socketId,
      { username: 'transport-fence-test' }, createRtpCapabilities(), {
        credentialEpoch, voiceEnforcementSessionGeneration: session.sessionGeneration,
        mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
        roomContext: { roomKind: 'channel' }, permissions: (1n << 63n) - 1n,
      }));
    await f.owners.attachRoom(handle, session, 'test-original-token');
  }
  await join(owner, f.session);
  return { ...f, manager, router, service, owner, join };
}

async function heldReceiveDrain(f: Awaited<ReturnType<typeof nativeFixture>>) {
  const old = createMockTransport({ id: 'closed-recv' });
  f.router.createWebRtcTransport.mockResolvedValue(old);
  await f.manager.createTransport(roomId, userId, 'recv');
  // Simulate worker-side closure without firing the JS removal event. The real
  // receive-drain path must prove absence before it can retire this slot.
  old.closed = true;
  const drain = deferred<void>();
  const entered = deferred<void>();
  f.service.confirmNativeTermination.mockImplementation(async () => {
    entered.resolve();
    await drain.promise;
  });
  const fresh = createMockTransport({ id: 'fresh-recv' });
  f.router.createWebRtcTransport.mockClear().mockResolvedValue(fresh);
  const pending = f.manager.createTransport(roomId, userId, 'recv');
  const outcome = pending.then(value => ({ value, error: undefined }), error => ({ value: undefined, error }));
  await entered.promise;
  return { drain, fresh, outcome };
}

describe('exact terminal generation across native transport awaits', () => {
  it('continues a live owner after the actual receive-drain proof completes', async () => {
    const f = await nativeFixture();
    const h = await heldReceiveDrain(f);
    expect(f.router.createWebRtcTransport).not.toHaveBeenCalled();
    h.drain.resolve();
    const result = await h.outcome;
    expect(result.error).toBeUndefined();
    expect(result.value?.id).toBe('fresh-recv');
    expect(f.router.createWebRtcTransport).toHaveBeenCalledOnce();
    expect(h.fresh.setMaxIncomingBitrate).toHaveBeenCalledOnce();
    expect(h.fresh.close).not.toHaveBeenCalled();
    await f.owners.terminate(f.owner);
  });

  it('starts no native create when its exact owner terminalizes during receive-drain', async () => {
    const f = await nativeFixture();
    const h = await heldReceiveDrain(f);
    const terminal = f.owners.terminate(f.owner);
    expect(() => f.owners.assertProductive(f.owner)).toThrow();
    expect(f.manager.getParticipant(roomId, userId)?.socketId).toBe(f.session.socketId);
    expect(f.router.createWebRtcTransport).not.toHaveBeenCalled();
    h.drain.resolve();
    const result = await h.outcome;
    await terminal;
    expect(result.error).toBeInstanceOf(Error);
    expect(f.router.createWebRtcTransport).not.toHaveBeenCalled();
    expect(h.fresh.setMaxIncomingBitrate).not.toHaveBeenCalled();
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it.each(['send', 'recv'] as const)('closes a late %s orphan without starting a held bitrate request', async direction => {
    const f = await nativeFixture();
    const creation = deferred<ReturnType<typeof createMockTransport>>();
    const entered = deferred<void>();
    const cap = deferred<void>();
    const orphan = createMockTransport({ id: `orphan-${direction}` });
    orphan.setMaxIncomingBitrate.mockImplementation(() => cap.promise);
    f.router.createWebRtcTransport.mockImplementation(() => { entered.resolve(); return creation.promise; });
    const pending = f.manager.createTransport(roomId, userId, direction);
    const outcome = pending.then(() => undefined, error => error);
    await entered.promise;
    const terminal = f.owners.terminate(f.owner);
    creation.resolve(orphan);
    await nextTurn();
    // Snapshot before releasing the deliberately unresolved cap. Clean up even
    // on RED so later tests cannot inherit retained ownership or promises.
    const closedBeforeCapRelease = orphan.close.mock.calls.length;
    const capCallsBeforeRelease = orphan.setMaxIncomingBitrate.mock.calls.length;
    const orphanCaptured = f.service.captureNativeTermination.mock.calls.some(([, ids]) => ids?.includes(orphan.id));
    cap.resolve();
    const error = await outcome;
    await terminal;
    expect(error).toBeInstanceOf(Error);
    expect({ closedBeforeCapRelease, capCallsBeforeRelease, orphanCaptured }).toEqual({
      closedBeforeCapRelease: 1, capCallsBeforeRelease: 0, orphanCaptured: true,
    });
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('waits for a live send transport bitrate cap before committing its successful result', async () => {
    const f = await nativeFixture();
    const cap = deferred<void>();
    const entered = deferred<void>();
    const transport = createMockTransport({ id: 'live-send' });
    transport.setMaxIncomingBitrate.mockImplementation(() => { entered.resolve(); return cap.promise; });
    f.router.createWebRtcTransport.mockResolvedValue(transport);
    let settled = false;
    const pending = f.manager.createTransport(roomId, userId, 'send').then(value => { settled = true; return value; });
    await entered.promise;
    expect(settled).toBe(false);
    expect(f.manager.getParticipant(roomId, userId)?.sendTransport).toBeNull();
    expect(transport.close).not.toHaveBeenCalled();
    cap.resolve();
    expect((await pending).id).toBe('live-send');
    expect(f.manager.getParticipant(roomId, userId)?.sendTransport?.id).toBe('live-send');
    expect(transport.setMaxIncomingBitrate).toHaveBeenCalledWith(expect.any(Number));
    await f.owners.terminate(f.owner);
  });

  it('isolates a live same-user successor from the old terminal owner and late transport', async () => {
    const f = await nativeFixture();
    const creation = deferred<ReturnType<typeof createMockTransport>>();
    const oldTransport = createMockTransport({ id: 'old-late-send' });
    f.router.createWebRtcTransport.mockImplementation(() => creation.promise);
    const pending = f.manager.createTransport(roomId, userId, 'send');
    const outcome = pending.then(() => undefined, error => error);
    const terminal = f.owners.terminate(f.owner);
    const successor = f.owners.reserve({ socketId: 'successor-socket', userId, credentialEpoch,
      token: 'test-successor-token', tokenExpiresAt: now + 60_000, disconnect: vi.fn() });
    await f.owners.register(successor);
    const session = { ...f.session, socketId: 'successor-socket', sessionGeneration: '66666666-6666-4666-8666-666666666666' };
    await f.join(successor, session);
    const successorTransport = createMockTransport({ id: 'successor-send' });
    f.router.createWebRtcTransport.mockResolvedValue(successorTransport);
    await f.manager.createTransport(roomId, userId, 'send');
    creation.resolve(oldTransport);
    expect(await outcome).toBeInstanceOf(Error);
    await terminal;
    expect(oldTransport.close).toHaveBeenCalledOnce();
    expect(successorTransport.close).not.toHaveBeenCalled();
    expect(f.manager.getParticipant(roomId, userId)?.sendTransport?.id).toBe('successor-send');
    expect(f.manager.getParticipant(roomId, userId)?.socketId).toBe('successor-socket');
    expect(() => f.owners.assertProductive(successor)).not.toThrow();
    await f.owners.terminate(successor);
  });
});
