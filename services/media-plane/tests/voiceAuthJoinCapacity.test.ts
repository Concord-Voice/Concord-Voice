import { randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { Script } from 'node:vm';
import ts from 'typescript';
import { describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import type { ConnectionHandle } from '../src/lib/voiceAuthConnection.js';
import { VoiceEnforcementLease } from '../src/lib/voiceEnforcementLease.js';
import * as lifecycle from '../src/lib/socketJoinLifecycle.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import { RoomManager, RoomCreationRefusedError, CryptoVersionMismatchError, MediaPolicyCooldownError, parseMediaFrameCryptoVersion, SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION } from '../src/lib/roomManager.js';
import { getIdentityAuthorityReasonCode } from '../src/lib/identityAuthority.js';
import type { JoinRoomResult } from '../src/lib/roomManager.js';
import type { MediasoupService } from '../src/lib/mediasoup.js';
import { isCanonicalEnforcementUUID } from '../src/lib/enforcementCommand.js';
import type { AuthenticatedSocketData } from '../src/middleware/auth.js';
import { attestationRelayHeaders, resolveParticipantIdentity } from '../src/middleware/auth.js';
import { createMockRouter, createRtpCapabilities } from './mocks/mediasoup.js';
import type { VoiceEnforcementSession } from '../src/lib/voiceEnforcementSession.js';
import { mediaPolicyCooldownAck } from '../src/lib/mediaPolicyWire.js';

const boot = '11111111-1111-4111-8111-111111111111';
const user = '22222222-2222-4222-8222-222222222222';
const family = '33333333-3333-4333-8333-333333333333';
const roomA = '44444444-4444-4444-8444-444444444444';
const roomB = '55555555-5555-4555-8555-555555555555';
const epoch = 'a'.repeat(32);
const secret = ['join', 'capacity', 'test', 'only'].join('-');
const key = Buffer.from(secret);
const token = 'test-join-capacity-token';
interface Access { allowed: boolean; error?: string }
function deferred<T>() {
  let resolve: (value: T) => void = () => { throw new Error('Uninitialized deferred'); };
  let reject: (error: Error) => void = () => { throw new Error('Uninitialized deferred'); };
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
const settle = () => new Promise<void>(resolve => setImmediate(resolve));
type JoinRequest = { roomId: string; rtpCapabilities: object; mediaFrameCryptoVersion: number };
type JoinHandler = (request: JoinRequest, callback: (value: unknown) => void) => Promise<void>;

// Select complete actual declarations with the TS parser. No startup/imports,
// copied handler implementation, source-text assertions or live main execution.
function productionRegister(bindings: Record<string, unknown>) {
  const source = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8');
  const ast = ts.createSourceFile('index.ts', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  const names = ['registerJoinRoomHandler', 'parseJoinMediaFrameCryptoVersion', 'joinErrorAck', 'emitJoinError', 'rollbackRegisteredRoomJoin', 'commitChannelAdmission', 'commitDMAdmission', 'rollbackSocketMembership'];
  const declarations = names.map(name => {
    const matches = ast.statements.filter(statement => ts.isFunctionDeclaration(statement) && statement.name?.text === name);
    if (matches.length !== 1) throw new Error(`Expected one production function ${name}`);
    return matches[0].getText(ast);
  });
  const compiled = ts.transpileModule(declarations.join('\n'), {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
  }).outputText;
  return new Script(`${compiled}\nregisterJoinRoomHandler;`, { filename: 'actual-index-join-capacity.js' })
    .runInNewContext(bindings) as (socket: unknown, data: unknown, deps: unknown) => void;
}

// Distinct accounts, so filling the shared budget never trips the per-user cap.
const filler = (index: number) => `${index.toString(16).padStart(8, '0')}-9999-4999-8999-999999999999`;
async function fixture(identityCount: number) {
  const now = Date.now();
  const requests: protocol.Request[] = [];
  let roomReleaseMode: 'signed' | 'lost' | 'unsigned' = 'signed';
  const roomReleaseStarted = deferred<protocol.Request>();
  const fetchBoundary = vi.fn<typeof fetch>(async (input, init) => {
    const path = new URL(String(input)).pathname;
    const endpoint: protocol.Endpoint = path.endsWith('/auth-connections') ? 'connection_register'
      : path.endsWith('/auth-connections/release') ? 'connection_release'
        : path.endsWith('/provisional-close') ? 'provisional_close'
          : path.endsWith('/enforcement-sessions/release') ? 'room_release' : 'room_register';
    const raw = init?.body;
    if (typeof raw !== 'string' && !(raw instanceof Uint8Array)) throw new Error('Missing raw request');
    const request = protocol.decodeHttpRequest(endpoint, { method: init?.method ?? '', path,
      body: Buffer.from(raw), headers: Array.from(new Headers(init?.headers), ([name, value]) => ({ name, value })) });
    protocol.verifyHttpRequest(request, key, BigInt(Math.floor(now / 1000)));
    requests.push(request);
    if (endpoint === 'room_release') {
      roomReleaseStarted.resolve(request);
      if (roomReleaseMode === 'lost') throw new Error('Signed release response lost after commit');
    }
    const registration = request.connectionRegister();
    const room = request.roomRegister();
    const close = request.provisionalClose();
    const payload: protocol.ResponsePayload = registration ? { version: 1, voiceEnforcementProtocol: 4,
      userId: user, familyId: family, target: { connectionGeneration: registration.connectionGeneration,
        nodeBootId: boot, socketId: registration.socketId, credentialEpoch: epoch } }
      : room ? { schema: 'concord.auth-room-binding', version: 1, voiceEnforcementProtocol: 4,
        binding: { userId: user, familyId: family, target: { connectionGeneration: room.connectionGeneration,
          nodeBootId: room.nodeBootId, socketId: room.socketId, credentialEpoch: room.credentialEpoch } },
        room: { sessionGeneration: room.sessionGeneration, roomId: room.roomId, roomKind: room.roomKind } }
        : close ? { ...close, schema: 'concord.auth-connection-provisional-close-result', disposition: 'unregistered' } : null;
    const signed = protocol.signHttpResponse(request, payload, String(Math.floor(now / 1000)), key);
    return new Response(signed.body.length ? Buffer.from(signed.body) : null, {
      status: signed.status, headers: endpoint === 'room_release' && roomReleaseMode === 'unsigned' ? []
        : signed.headers.map(({ name, value }) => [name, value]),
    });
  });
  const owners = new VoiceAuthConnections({ nodeBootId: boot, secret,
    controlPlaneUrl: 'https://control.test.invalid', now: () => now, fetch: fetchBoundary });
  const handles = new Map<string, ConnectionHandle>();
  const lease = new VoiceEnforcementLease(() => 0); lease.renew();
  const reserve = (socketId: string, userId = user) => owners.reserve({ socketId, userId, credentialEpoch: epoch,
    token, tokenExpiresAt: now + 60_000, disconnect: () => {} });
  const first = reserve('join-idle-a'); await owners.register(first); handles.set('join-idle-a', first);
  const second = reserve('join-idle-b'); await owners.register(second); handles.set('join-idle-b', second);
  for (let index = 2; index < identityCount; index++) reserve(`retained-${index}`, filler(index));
  const authorization = deferred<Access>();
  const validateChannelAccess = vi.fn(() => authorization.promise);
  const nativeService = {
    getOrCreateRouter: vi.fn<(...args: unknown[]) => Promise<unknown>>(async () => { throw new Error('Unexpected native router creation'); }),
    getWebRtcServer: vi.fn(), removeRouter: vi.fn(),
    captureNativeTermination: vi.fn((router: { id: string }, transportIds?: readonly string[]) => ({
      worker: { closed: false }, router, routerId: router.id, transportIds,
    })),
    confirmNativeTermination: vi.fn(async () => {}),
  };
  const manager = new RoomManager(nativeService as unknown as MediasoupService);
  manager.setVoiceAuthConnections(owners);
  const joinRoom = vi.spyOn(manager, 'joinRoom');
  const register = productionRegister({ ...lifecycle, randomUUID, Date, Error, Promise,
    getVoiceAuthConnection: (socket: { id: string }) => handles.get(socket.id),
    voiceEnforcementNodeBootId: boot, validateChannelAccess, attestationRelayHeaders, getIdentityAuthorityReasonCode,
    registerVoiceEnforcementSession: async (_token: string, session: Parameters<typeof owners.attachRoom>[1]) => {
      const handle = handles.get(session.socketId);
      if (!handle) throw new Error('Missing actual registered owner');
      await owners.attachRoom(handle, session, token);
    },
    isCanonicalEnforcementUUID, parseMediaFrameCryptoVersion, resolveParticipantIdentity,
    RoomCreationRefusedError, CryptoVersionMismatchError, MediaPolicyCooldownError, mediaPolicyCooldownAck,
    logger: { info() {}, debug() {}, warn() {}, error() {} },
  });
  const dmJoinFence = new lifecycle.KeyedJoinFence();
  const channelAdmissionFence = new lifecycle.KeyedJoinFence();
  const makeJoin = (socketId: string, roomId: string) => {
    let handler: JoinHandler | undefined;
    const data: AuthenticatedSocketData = { userId: user, username: 'join-capacity-test', tier: 'free', credentialEpoch: epoch, tokenExpiresAt: now + 60_000 };
    const broadcast = vi.fn();
    const socket = { id: socketId, connected: true, handshake: { auth: { token, room_kind: 'channel' } },
      emit: vi.fn(), join: vi.fn(), leave: vi.fn(), to: () => ({ emit: broadcast }) };
    register(socket, data, { roomManager: manager, dmJoinFence, channelAdmissionFence, voiceAuthConnections: owners,
      emitSecurityEvent: vi.fn(), isShuttingDown: () => !lease.valid(),
      withRateLimit: (_socket: unknown, event: string, listener: JoinHandler) => {
        if (event !== 'join-room') throw new Error('Unexpected event'); handler = listener;
      } });
    const callback = vi.fn();
    return { start: () => {
      if (!handler) throw new Error('Production handler was not registered');
      return handler({ roomId, rtpCapabilities: {}, mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION }, callback);
    }, callback, socket, data, broadcast };
  };
  return { owners, requests, lease, validateChannelAccess, nativeService, joinRoom, authorization, makeJoin, handles, manager,
    roomReleaseStarted, releaseMode(mode: typeof roomReleaseMode) { roomReleaseMode = mode; } };
}

describe('actual join handler shared capacity reservation', () => {
  it('refuses a healthy registered idle socket at combined 1000 before A1 authorization or native effects', async () => {
    const f = await fixture(1000);
    expect(f.lease.valid()).toBe(true);
    const admission = f.makeJoin('join-idle-a', roomA);
    const running = admission.start();
    await settle();
    try {
      expect(f.validateChannelAccess).not.toHaveBeenCalled();
      expect(f.joinRoom).not.toHaveBeenCalled();
      expect(f.nativeService.getOrCreateRouter).not.toHaveBeenCalled();
      expect(f.owners.retainedIdentityCount()).toBe(1000);
    } finally {
      f.authorization.resolve({ allowed: false, error: 'Denied by test authority' });
      await running;
      await f.owners.terminateAll();
    }
    expect(admission.callback).toHaveBeenCalledWith(expect.objectContaining({ error: expect.any(String) }));
    expect(f.requests.filter(request => request.endpoint() === 'room_release')).toHaveLength(0);
  });

  it('reserves the last room slot at combined 999 before either competing idle join A1 completes', async () => {
    const f = await fixture(999);
    expect(f.lease.valid()).toBe(true);
    const first = f.makeJoin('join-idle-a', roomA);
    const second = f.makeJoin('join-idle-b', roomB);
    const running = [first.start(), second.start()];
    await settle();
    try {
      expect(f.validateChannelAccess).toHaveBeenCalledTimes(1);
      expect(f.owners.retainedIdentityCount()).toBe(1000);
      expect(f.joinRoom).not.toHaveBeenCalled();
      expect(f.nativeService.getOrCreateRouter).not.toHaveBeenCalled();
    } finally {
      f.authorization.resolve({ allowed: false, error: 'Denied by test authority' });
      await Promise.all(running);
      await f.owners.terminateAll();
    }
    expect(first.callback).toHaveBeenCalledWith(expect.objectContaining({ error: expect.any(String) }));
    expect(second.callback).toHaveBeenCalledWith(expect.objectContaining({ error: expect.any(String) }));
    expect(f.requests.filter(request => request.endpoint() === 'room_release')).toHaveLength(1);
  });
});


describe('actual join caller retained cleanup', () => {
  it.each(['denied', 'throws', 'canceled'] as const)(
    'retains the pre-A1 reservation after %s until a lost room release gets a valid signed retry', async outcome => {
      const f = await fixture(2);
      f.releaseMode('lost');
      const admission = f.makeJoin('join-idle-a', roomA);
      const running = admission.start();
      await settle();
      expect(f.validateChannelAccess).toHaveBeenCalledOnce();
      expect(f.owners.retainedIdentityCount()).toBe(3);
      expect(f.joinRoom).not.toHaveBeenCalled();
      if (outcome === 'denied') f.authorization.resolve({ allowed: false, error: 'Denied by test authority' });
      else if (outcome === 'throws') f.authorization.reject(new Error('A1 authority unavailable'));
      else {
        admission.socket.connected = false;
        f.authorization.resolve({ allowed: true });
      }
      const released = await f.roomReleaseStarted.promise;
      await settle();
      expect(released.roomRelease()?.room.roomId).toBe(roomA);
      expect(released.roomRelease()?.binding.target.socketId).toBe('join-idle-a');
      expect(f.owners.retainedIdentityCount()).toBe(3);
      expect(f.requests.filter(request => request.endpoint() === 'room_release')).toHaveLength(1);
      expect(f.requests.filter(request => request.endpoint() === 'connection_release')).toHaveLength(0);
      expect(f.joinRoom).not.toHaveBeenCalled();
      expect(f.nativeService.getOrCreateRouter).not.toHaveBeenCalled();
      f.releaseMode('unsigned');
      await f.owners.retryPending(); await settle();
      expect(f.requests.filter(request => request.endpoint() === 'room_release')).toHaveLength(2);
      expect(f.owners.retainedIdentityCount()).toBe(3);
      f.releaseMode('signed');
      await f.owners.retryPending(); await running;
      expect(f.owners.retainedIdentityCount()).toBe(2);
      expect(f.requests.filter(request => request.endpoint() === 'room_release')).toHaveLength(3);
      // Denial concerns the reserved room, so both idle connection generations survive.
      for (const handle of f.handles.values()) expect(() => f.owners.assertProductive(handle)).not.toThrow();
      expect(f.requests.filter(request => request.endpoint() === 'room_register')).toHaveLength(0);
      await f.owners.terminateAll();
      expect(f.owners.retainedIdentityCount()).toBe(0);
    }
  );

  it('joins A1 in-flight work before terminal room proof and preserves the other idle generation', async () => {
    const f = await fixture(2);
    const admission = f.makeJoin('join-idle-a', roomA);
    const running = admission.start();
    await settle();
    expect(f.owners.retainedIdentityCount()).toBe(3);
    const handle = f.handles.get('join-idle-a');
    if (!handle) throw new Error('Missing fixture owner');
    const terminal = f.owners.terminate(handle);
    await settle();
    expect(f.requests.filter(request => request.endpoint() === 'room_release')).toHaveLength(0);
    expect(f.requests.filter(request => request.endpoint() === 'connection_release')).toHaveLength(0);
    expect(f.owners.retainedIdentityCount()).toBe(3);
    f.authorization.resolve({ allowed: true });
    await running; await terminal;
    expect(f.joinRoom).not.toHaveBeenCalled();
    expect(f.owners.retainedIdentityCount()).toBe(1);
    expect(f.requests.filter(request => request.endpoint() === 'room_release')).toHaveLength(1);
    expect(f.requests.filter(request => request.endpoint() === 'connection_release')).toHaveLength(1);
    const successor = f.handles.get('join-idle-b');
    if (!successor) throw new Error('Missing fixture survivor');
    expect(() => f.owners.assertProductive(successor)).not.toThrow();
    await f.owners.terminateAll();
  });
});

describe('queued actual join cancellation', () => {
  it('owns a disconnected queued reservation before its fence wait and releases it without a second A1', async () => {
    const f = await fixture(2);
    const first = f.makeJoin('join-idle-a', roomA);
    const queued = f.makeJoin('join-idle-b', roomA);
    const running = [first.start(), queued.start()];
    await settle();
    expect(f.validateChannelAccess).toHaveBeenCalledOnce();
    expect(f.owners.retainedIdentityCount()).toBe(4);
    queued.socket.connected = false;
    expect(f.requests.filter(request => request.endpoint() === 'room_release')).toHaveLength(0);
    f.authorization.resolve({ allowed: false, error: 'Denied by test authority' });
    await Promise.all(running);
    expect(f.validateChannelAccess).toHaveBeenCalledOnce();
    expect(f.joinRoom).not.toHaveBeenCalled();
    expect(f.nativeService.getOrCreateRouter).not.toHaveBeenCalled();
    const releases = f.requests.filter(request => request.endpoint() === 'room_release');
    expect(releases).toHaveLength(2);
    expect(new Set(releases.map(request => request.roomRelease()?.binding.target.socketId))).toEqual(
      new Set(['join-idle-a', 'join-idle-b'])
    );
    expect(f.owners.retainedIdentityCount()).toBe(2);
    await f.owners.terminateAll();
  });
});


describe('actual successful admission retains its reserved owner', () => {
  it.each(['normal', 'broadcast throws', 'ack throws'] as const)(
    'keeps a promoted room reserved after %s and releases only on explicit terminal cleanup', async notification => {
      const f = await fixture(2);
      const result: JoinRoomResult = { rtpCapabilities: {}, mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
        existingProducers: [], participants: [{ userId: user, username: 'join-capacity-test', isDeafened: false, isTesting: false }], e2eeEpoch: 1 };
      // Native participant creation and the final promotion boundary are controlled;
      // the production handler, commit helper and private signed owner remain real.
      f.joinRoom.mockResolvedValue(result);
      const promote = vi.spyOn(f.manager, 'promoteChannelParticipant').mockImplementation(
        (_room, _user, _socket, _promotion, commitMembership) => { commitMembership(); return result; }
      );
      const admission = f.makeJoin('join-idle-a', roomA);
      if (notification === 'broadcast throws') admission.broadcast.mockImplementationOnce(() => { throw new Error('Fanout failed after commit'); });
      if (notification === 'ack throws') admission.callback.mockImplementationOnce(() => { throw new Error('Ack failed after commit'); });
      const running = admission.start();
      await settle();
      expect(f.owners.retainedIdentityCount()).toBe(3);
      f.authorization.resolve({ allowed: true });
      await running;
      expect(promote).toHaveBeenCalledOnce();
      expect(admission.socket.join).toHaveBeenCalledWith(roomA);
      expect(admission.data.roomId).toBe(roomA);
      const exact = admission.data.voiceEnforcementSession;
      expect(exact).toBeDefined();
      if (!exact) throw new Error('Successful production join lost its exact room owner');
      expect(() => f.owners.assertRoomProductive(exact.sessionGeneration)).not.toThrow();
      expect(f.validateChannelAccess).toHaveBeenCalledTimes(2);
      expect(f.requests.filter(request => request.endpoint() === 'room_register')).toHaveLength(1);
      expect(f.requests.filter(request => request.endpoint() === 'room_release')).toHaveLength(0);
      expect(f.owners.retainedIdentityCount()).toBe(3);
      await f.owners.terminateAll();
      expect(f.requests.filter(request => request.endpoint() === 'room_release')).toHaveLength(1);
      expect(f.owners.retainedIdentityCount()).toBe(0);
    }
  );
});


describe('actual canceled join generation isolation', () => {
  it('keeps a newer native participant with the same user and socket when the older pre-A1 reservation is canceled', async () => {
    const f = await fixture(2);
    const admission = f.makeJoin('join-idle-a', roomA);
    const running = admission.start();
    await settle();
    expect(f.validateChannelAccess).toHaveBeenCalledOnce();
    expect(f.owners.retainedIdentityCount()).toBe(3);
    const handle = f.handles.get('join-idle-a');
    if (!handle) throw new Error('Missing actual owner');
    const successor: VoiceEnforcementSession = {
      connectionGeneration: f.owners.snapshot(handle).target.connectionGeneration,
      sessionGeneration: '66666666-6666-4666-8666-666666666666', nodeBootId: boot,
      roomId: roomA, roomKind: 'channel', userId: user, credentialEpoch: epoch, socketId: 'join-idle-a',
    };
    const router = createMockRouter({ id: 'generation-isolation-router' });
    f.nativeService.getOrCreateRouter.mockResolvedValue(router);
    f.manager.reserveVoiceAuthRoom(handle, successor);
    await f.owners.trackRoomOperation(successor, () => f.manager.joinRoom(roomA, user, successor.socketId,
      { username: 'successor-native-participant' }, createRtpCapabilities(), {
        credentialEpoch: epoch, voiceEnforcementSessionGeneration: successor.sessionGeneration,
        mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
        roomContext: { roomKind: 'channel' }, permissions: (1n << 63n) - 1n,
      }));
    const participant = f.manager.getParticipant(roomA, user);
    expect(participant?.voiceEnforcementSessionGeneration).toBe(successor.sessionGeneration);
    expect(f.owners.retainedIdentityCount()).toBe(4);
    admission.socket.connected = false;
    f.authorization.resolve({ allowed: true });
    await running;
    try {
      // Actual RoomManager generation selection must preserve the successor;
      // neither its leave path nor its matching decision is mocked.
      expect(f.manager.getParticipant(roomA, user)).toBe(participant);
      expect(() => f.owners.assertRoomProductive(successor.sessionGeneration)).not.toThrow();
      expect(f.owners.retainedIdentityCount()).toBe(3);
      const releases = f.requests.filter(request => request.endpoint() === 'room_release');
      expect(releases).toHaveLength(1);
      expect(releases[0].roomRelease()?.room.sessionGeneration).not.toBe(successor.sessionGeneration);
    } finally {
      await f.owners.terminateAll();
    }
  });
});
