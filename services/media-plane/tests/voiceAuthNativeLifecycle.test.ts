import { describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import { logger } from '../src/lib/logger.js';
vi.mock('mediasoup', () => ({ createWorker: vi.fn() }));
import { RoomManager, FREE_MEDIA_ENTITLEMENT, SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION } from '../src/lib/roomManager.js';
import type { MediasoupService } from '../src/lib/mediasoup.js';
import { createMockRouter, createMockTransport, createMockProducer, createMockConsumer, createRtpCapabilities, createRtpParameters } from './mocks/mediasoup.js';
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import { setVoiceAuthConnections, releaseVoiceEnforcementSession, type VoiceEnforcementSession } from '../src/lib/voiceEnforcementSession.js';

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

describe('exact retained native lifecycle', () => {
  it('fences synchronously and waits for an in-flight native operation before closing and releasing', async () => {
    const f = fixture();
    const owner = f.reserve();
    await f.owners.register(owner);
    const creation = deferred<void>();
    const teardown = deferred<void>();
    const closeNative = vi.fn(() => teardown.promise);
    f.owners.reserveRoom(owner, f.session, closeNative);
    await f.owners.attachRoom(owner, f.session, 'test-original-token');
    const operation = f.owners.trackRoomOperation(f.session, () => creation.promise);
    const operationOutcome = Promise.resolve(operation).catch(() => undefined);
    const terminal = f.owners.terminate(owner);
    expect(() => f.owners.assertProductive(owner)).toThrow();
    expect(f.owners.terminate(owner)).toBe(terminal);
    expect(f.owners.retainedIdentityCount()).toBe(2);
    expect(closeNative).not.toHaveBeenCalled();
    expect(f.paths.some(path => path.endsWith('/release'))).toBe(false);
    creation.resolve();
    await operationOutcome;
    await vi.waitFor(() => expect(closeNative).toHaveBeenCalledOnce());
    expect(f.paths.some(path => path.endsWith('/release'))).toBe(false);
    expect(f.owners.retainedIdentityCount()).toBe(2);
    teardown.resolve();
    await terminal;
    expect(f.paths.slice(-2)).toEqual([
      '/api/v1/internal/voice/enforcement-sessions/release',
      '/api/v1/internal/voice/auth-connections/release',
    ]);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('retains one terminal task across lost room release replies and never repeats native teardown', async () => {
    const f = fixture();
    const owner = f.reserve();
    await f.owners.register(owner);
    const closeNative = vi.fn(async () => undefined);
    vi.mocked(logger.error).mockClear();
    f.owners.reserveRoom(owner, f.session, closeNative);
    await f.owners.attachRoom(owner, f.session, 'test-original-token');
    let loseReply = true;
    f.replies.room_release = async () => { if (loseReply) throw new Error('lost signed reply'); };
    const terminal = f.owners.terminate(owner);
    await vi.waitFor(() => expect(f.paths.filter(path => path.endsWith('/enforcement-sessions/release'))).toHaveLength(1));
    expect(f.owners.terminate(owner)).toBe(terminal);
    expect(f.owners.retainedIdentityCount()).toBe(2);
    expect(f.paths).not.toContain('/api/v1/internal/voice/auth-connections/release');
    // The rejected terminal attempt is logged once (fixed text, no identifiers).
    await vi.waitFor(() => expect(logger.error).toHaveBeenCalledTimes(1));
    expect(logger.error).toHaveBeenCalledWith('Voice terminal release failed; will retry');
    loseReply = false;
    await f.owners.retryPending();
    await terminal;
    expect(closeNative).toHaveBeenCalledOnce();
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });
});


async function nativeFixture(options: { createSend?: boolean; provisional?: boolean } = {}) {
  const f = fixture();
  const router = createMockRouter({ id: 'native-router' });
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
  manager.reserveVoiceAuthRoom(owner, f.session);
  await f.owners.trackRoomOperation(f.session, () => manager.joinRoom(roomId, userId, f.session.socketId, { username: 'native-test' }, createRtpCapabilities(), {
    credentialEpoch, voiceEnforcementSessionGeneration: sessionGeneration,
    mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
    roomContext: { roomKind: 'channel' }, permissions: (1n << 63n) - 1n,
    deferChannelPromotion: options.provisional,
  }));
  await f.owners.attachRoom(owner, f.session, 'test-original-token');
  const transport = createMockTransport({ id: 'native-send' });
  router.createWebRtcTransport.mockResolvedValue(transport);
  if (options.createSend !== false && !options.provisional) await manager.createTransport(roomId, userId, 'send');
  return { ...f, manager, router, service, owner, transport };
}

describe('RoomManager native create ownership', () => {
  it('owns a held transport create and closes its late result before release', async () => {
    const f = await nativeFixture({ createSend: false });
    const creation = deferred<ReturnType<typeof createMockTransport>>();
    f.router.createWebRtcTransport.mockImplementation(() => creation.promise);
    const pending = f.manager.createTransport(roomId, userId, 'send');
    const rejected = expect(pending).rejects.toThrow();
    expect(f.router.createWebRtcTransport).toHaveBeenCalledOnce();
    const terminal = f.owners.terminate(f.owner);
    await Promise.resolve();
    expect(f.paths.some(path => path.endsWith('/release'))).toBe(false);
    creation.resolve(f.transport);
    await rejected;
    await terminal;
    expect(f.transport.close).toHaveBeenCalledOnce();
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('refuses exact A2 promotion synchronously when its connection is terminal', async () => {
    const f = await nativeFixture({ provisional: true });
    const terminal = f.owners.terminate(f.owner);
    const membership = vi.fn();
    expect(() => f.manager.promoteChannelParticipant(roomId, userId, f.session.socketId, {
      identity: { username: 'native-test' }, entitlement: FREE_MEDIA_ENTITLEMENT,
      ownerTier: 'free', permissions: (1n << 63n) - 1n, serverMuted: false, serverDeafened: false,
    }, membership)).toThrow('Terminal voice room');
    expect(membership).not.toHaveBeenCalled();
    expect(f.manager.getParticipant(roomId, userId)).toBeUndefined();
    await terminal;
  });

  it('joins a held producer create and closes its late native result before exact release', async () => {
    const f = await nativeFixture();
    const creation = deferred<ReturnType<typeof createMockProducer>>();
    f.transport.produce.mockImplementation(() => creation.promise);
    const pending = f.manager.produce(roomId, userId, f.transport.id, 'audio', createRtpParameters(), 'mic');
    const rejected = expect(pending).rejects.toThrow();
    expect(f.transport.produce).toHaveBeenCalledOnce();
    const terminal = f.owners.terminate(f.owner);
    await Promise.resolve();
    expect(f.paths.some(path => path.endsWith('/release'))).toBe(false);
    const producer = createMockProducer({ id: 'late-producer' });
    creation.resolve(producer);
    await rejected;
    await terminal;
    expect(producer.close).toHaveBeenCalledOnce();
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('joins a held consumer create and closes its late result before exact release', async () => {
    const f = await nativeFixture();
    const producer = createMockProducer({ id: 'source-producer' });
    f.transport.produce.mockResolvedValue(producer);
    await f.manager.produce(roomId, userId, f.transport.id, 'audio', createRtpParameters(), 'mic');
    const recv = createMockTransport({ id: 'native-recv' });
    f.router.createWebRtcTransport.mockResolvedValue(recv);
    await f.manager.createTransport(roomId, userId, 'recv');
    const creation = deferred<ReturnType<typeof createMockConsumer>>();
    recv.consume.mockImplementation(() => creation.promise);
    const pending = f.manager.consume(roomId, userId, producer.id, recv.id);
    const rejected = expect(pending).rejects.toThrow();
    expect(recv.consume).toHaveBeenCalledOnce();
    const terminal = f.owners.terminate(f.owner);
    await Promise.resolve();
    expect(f.paths.some(path => path.endsWith('/release'))).toBe(false);
    const consumer = createMockConsumer({ id: 'late-consumer', producerId: producer.id });
    creation.resolve(consumer);
    await rejected;
    await terminal;
    expect(consumer.close).toHaveBeenCalledOnce();
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });

  it('clears layer demands and recomputes the camera gate when an owned room closes', async () => {
    const f = await nativeFixture();
    const mic = createMockProducer({ id: 'mic-producer' });
    const camera = createMockProducer({ id: 'camera-producer', kind: 'video' });
    f.transport.produce.mockResolvedValueOnce(mic).mockResolvedValueOnce(camera);
    await f.manager.produce(roomId, userId, f.transport.id, 'audio', createRtpParameters(), 'mic');
    const videoParameters = { ...createRtpParameters(), codecs: [{
      mimeType: 'video/VP8', payloadType: 96, clockRate: 90000 }], encodings: [{ ssrc: 777 }] };
    await f.manager.produce(roomId, userId, f.transport.id, 'video',
      videoParameters as unknown as ReturnType<typeof createRtpParameters>, 'camera');
    const recv = createMockTransport({ id: 'native-recv' });
    f.router.createWebRtcTransport.mockResolvedValue(recv);
    await f.manager.createTransport(roomId, userId, 'recv');
    const consumer = createMockConsumer({ id: 'viewer-consumer', producerId: mic.id });
    recv.consume.mockResolvedValue(consumer);
    await f.manager.consume(roomId, userId, mic.id, recv.id);
    const room = f.manager.getRoom(roomId)!;
    room.cameraLayerDemands.set(consumer.id, {
      consumerId: consumer.id, visible: true, maxUsefulSpatialLayer: 1, pressureStepDown: false,
    });
    room.screenLayerDemands.set(consumer.id, {
      consumerId: consumer.id, userId, sharerUserId: userId, visible: true,
      maxUsefulSpatialLayer: 1, pressureStepDown: false,
    });
    room.cameraLayeringGateEnabled = true;
    const recompute = vi.spyOn(f.manager as unknown as { recomputeCameraLayeringGate: (r: unknown) => void },
      'recomputeCameraLayeringGate');
    await f.owners.terminate(f.owner);
    expect(room.cameraLayerDemands.size).toBe(0);
    expect(room.screenLayerDemands.size).toBe(0);
    // A removed camera producer must recompute the room gate itself.
    expect(recompute).toHaveBeenCalled();
    if (room.cameraGateOffTimer) clearTimeout(room.cameraGateOffTimer);
  });

  it('blocks produce immediately while teardown is held', async () => {
    const f = await nativeFixture();
    const teardown = deferred<void>();
    f.service.confirmNativeTermination.mockImplementation(() => teardown.promise);
    const terminal = f.owners.terminate(f.owner);
    await expect(f.manager.produce(roomId, userId, f.transport.id, 'audio', createRtpParameters(), 'mic')).rejects.toThrow('Terminal voice room');
    expect(f.transport.produce).not.toHaveBeenCalled();
    expect(f.paths.some(path => path.endsWith('/release'))).toBe(false);
    teardown.resolve();
    await terminal;
  });

  it('blocks valid consume and resume paths synchronously before native teardown runs', async () => {
    const f = await nativeFixture();
    const producer = createMockProducer({ id: 'source-producer' });
    f.transport.produce.mockResolvedValue(producer);
    await f.manager.produce(roomId, userId, f.transport.id, 'audio', createRtpParameters(), 'mic');
    const recv = createMockTransport({ id: 'native-recv' });
    f.router.createWebRtcTransport.mockResolvedValue(recv);
    await f.manager.createTransport(roomId, userId, 'recv');
    const consumer = createMockConsumer({ id: 'valid-consumer', producerId: producer.id });
    recv.consume.mockResolvedValue(consumer);
    await f.manager.consume(roomId, userId, producer.id, recv.id);
    const teardown = deferred<void>();
    f.service.confirmNativeTermination.mockImplementation(() => teardown.promise);
    const terminal = f.owners.terminate(f.owner);
    // Invoke before yielding: both valid resources still exist locally.
    const consume = f.manager.consume(roomId, userId, producer.id, recv.id);
    const resume = f.manager.resumeConsumer(roomId, userId, consumer.id);
    await expect(consume).rejects.toThrow('Terminal voice room');
    await expect(resume).rejects.toThrow('Terminal voice room');
    expect(consumer.resume).not.toHaveBeenCalled();
    expect(recv.consume).toHaveBeenCalledOnce();
    teardown.resolve();
    await terminal;
  });

});


describe('legacy exact-room command adapter for retained family ownership', () => {
  it('uses retained private parent and strict signed room release when the command omits its connection marker', async () => {
    const f = await nativeFixture();
    setVoiceAuthConnections(f.owners);
    // The legacy exact-room command carries the seven room address fields only.
    const commandSession: VoiceEnforcementSession = {
      sessionGeneration, nodeBootId, roomId, roomKind: 'channel', userId, credentialEpoch,
      socketId: f.session.socketId,
    };
    const legacyVerifier = vi.fn(async () => undefined);
    await expect(releaseVoiceEnforcementSession({
      ...commandSession, sessionGeneration: '88888888-8888-4888-8888-888888888888',
      connectionGeneration: f.owners.snapshot(f.owner).target.connectionGeneration,
    }, legacyVerifier)).rejects.toThrow('Voice connection owner required');
    expect(legacyVerifier).not.toHaveBeenCalled();
    const proof = deferred<void>();
    f.service.confirmNativeTermination.mockImplementation(() => proof.promise);
    const released = releaseVoiceEnforcementSession(commandSession, legacyVerifier);
    await Promise.resolve();
    expect(f.paths.some(path => path.endsWith('/release'))).toBe(false);
    expect(f.owners.retainedIdentityCount()).toBe(2);
    proof.resolve();
    await released;
    expect(legacyVerifier).not.toHaveBeenCalled();
    expect(f.requests.at(-1)?.roomRelease()).toMatchObject({
      schema: 'concord.auth-room-binding', binding: { userId, familyId,
        target: f.owners.snapshot(f.owner).target },
      room: { sessionGeneration, roomId, roomKind: 'channel' },
    });
    expect(f.owners.retainedIdentityCount()).toBe(1);
    await f.owners.terminate(f.owner);
  });
});
