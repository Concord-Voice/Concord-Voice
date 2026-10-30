import { EventEmitter } from 'node:events';
import type { Socket } from 'socket.io';
import jwt from 'jsonwebtoken';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';

vi.mock('@/config/index.js', () => ({
  config: {
    jwtSecret: ['vitest', 'mock', 'jwt'].join('-'),
    controlPlaneUrl: 'http://control-plane.test',
  },
}));
import { createAuthMiddleware, createSocketTokenRenewal } from '../src/middleware/auth.js';
import {
  type VoiceEnforcementSession,
} from '../src/lib/voiceEnforcementSession.js';

import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';

const userId = 'd8fe119d-a28c-4573-be19-7847469aa59e';
const signingKey = ['vitest', 'mock', 'jwt'].join('-');
const start = Date.UTC(2026, 9, 15);
function token(expiresIn: number, claims: Record<string, unknown> = {}) {
  return jwt.sign({ user_id: userId, cred_epoch: 'epoch-1', ...claims }, signingKey, {
    issuer: 'concordvoice-control-plane',
    expiresIn,
  });
}

function connectedSocket(validateRenewal = vi.fn(async (_token: string) => {})) {
  const events = new EventEmitter();
  let packetMiddleware: Parameters<Socket['use']>[0] = (_, next) => next();
  const socket = {
    id: 'expiry-socket',
    connected: true,
    handshake: { auth: { token: token(10), username: 'alice' } },
    data: {} as Record<string, unknown>,
    once: events.once.bind(events),
    use: (middleware: typeof packetMiddleware) => {
      packetMiddleware = middleware;
    },
    disconnect: vi.fn(() => {
      socket.connected = false;
      events.emit('disconnect');
    }),
  };
  const next = vi.fn();
  createAuthMiddleware()(socket as unknown as Socket, next);
  expect(next).toHaveBeenCalledWith();
  const renew = createSocketTokenRenewal(socket as unknown as Socket, validateRenewal);
  return {
    socket,
    renew,
    packet: (nextPacket: () => void) => packetMiddleware(['produce', {}], nextPacket),
  };
}

function enforcementSession(sessionGeneration = '55555555-5555-4555-8555-555555555555'): VoiceEnforcementSession {
  return {
    sessionGeneration,
    nodeBootId: '33333333-3333-4333-8333-333333333333',
    roomId: '44444444-4444-4444-8444-444444444444',
    roomKind: 'dm',
    userId,
    credentialEpoch: 'a'.repeat(32),
    socketId: 'expiry-socket',
  };
}

describe('socket access-token lifetime', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(start);
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('cannot renew with a signed token rejected by live control-plane validation', async () => {
    const validate = vi.fn(async () => {
      throw new Error('revoked credential');
    });
    const { socket, renew } = connectedSocket(validate);
    const deadline = socket.data.tokenExpiresAt;
    const ack = vi.fn();
    await renew(token(100), ack);
    expect(ack).toHaveBeenCalledWith({ error: 'Authentication failed' });
    expect(socket.data.tokenExpiresAt).toBe(deadline);
  });

  it.each(['expiry', 'revocation'])(
    'cannot commit renewal after %s races live validation',
    async (event) => {
      let finish!: () => void;
      const validate = vi.fn(
        () =>
          new Promise<void>((resolve) => {
            finish = resolve;
          })
      );
      const { socket, renew } = connectedSocket(validate);
      const deadline = socket.data.tokenExpiresAt;
      const ack = vi.fn();
      const pending = renew(token(100), ack);
      expect(socket.data.tokenExpiresAt).toBe(deadline);
      expect(ack).not.toHaveBeenCalled();
      if (event === 'expiry') vi.setSystemTime(start + 10_000);
      else socket.disconnect();
      finish();
      await pending;
      expect(ack).toHaveBeenCalledWith({ error: 'Authentication failed' });
      expect(socket.data.tokenExpiresAt).toBe(deadline);
    }
  );

  it('disconnects at expiry even without signaling traffic and clears the timer', async () => {
    const { socket } = connectedSocket();
    await vi.advanceTimersByTimeAsync(9_999);
    expect(socket.connected).toBe(true);
    await vi.advanceTimersByTimeAsync(1);
    expect(socket.connected).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('renews before expiry without touching room or enforcement ownership', async () => {
    const { socket, renew, packet } = connectedSocket();
    socket.data.roomId = 'dm-room';
    const session = { sessionGeneration: 'same-generation' };
    socket.data.voiceEnforcementSession = session;
    await vi.advanceTimersByTimeAsync(5_000);
    const fresh = token(20, { sid: 'rotated-refresh-session' });
    const ack = vi.fn();
    await renew(fresh, ack);
    expect(ack).toHaveBeenCalledWith({ success: true });
    await vi.advanceTimersByTimeAsync(5_000);
    const next = vi.fn();
    packet(next);
    expect(next).toHaveBeenCalledWith();
    expect(socket.connected).toBe(true);
    expect(socket.data.roomId).toBe('dm-room');
    expect(socket.data.voiceEnforcementSession).toBe(session);
    expect(socket.handshake.auth.token).toBe(fresh);
    await vi.advanceTimersByTimeAsync(15_000);
    expect(socket.connected).toBe(false);
  });

  it('does not renew an admitted session that left before refresh starts', async () => {
    const f = await admittedFixture();
    await expect(f.api.renewVoiceEnforcementSession(token(20), f.session, () => false, f.legacyRelease))
      .rejects.toThrow('No admitted voice session');
    expect(f.requests).toHaveLength(2);
    expect(f.legacyRelease).not.toHaveBeenCalled();
    await f.owners.terminate(f.owner);
  });

  it.each(['leave', 'disconnect'] as const)(
    'releases the exact durable session when %s races admitted renewal',
    async race => {
      const f = await admittedFixture();
      let finish!: () => void;
      const reply = new Promise<void>(resolve => { finish = resolve; });
      f.hold.value = reply;
      let current = true;
      const pending = f.api.renewVoiceEnforcementSession(token(20), f.session, () => current, f.legacyRelease);
      // Observe the rejection before the terminal operation can settle the renewal.
      const denied = expect(pending).rejects.toThrow('Terminal voice room');
      expect(f.requests).toHaveLength(3);
      expect(f.requests[2].roomRegister()).toEqual({ ...f.requests[1].roomRegister(), mode: 'renew' });
      current = false;
      const terminal = race === 'leave'
        ? f.api.releaseVoiceEnforcementSession(f.session, f.legacyVerifier)
        : f.owners.terminate(f.owner);
      expect(() => f.owners.assertRoomProductive(f.session.sessionGeneration)).toThrow('Terminal voice room');
      expect(f.closeNative).not.toHaveBeenCalled();
      expect(f.requests.filter(request => request.roomRelease())).toHaveLength(0);
      finish();
      await denied;
      await terminal;
      expect(f.closeNative).toHaveBeenCalledOnce();
      expect(f.legacyVerifier).not.toHaveBeenCalled();
      expect(f.legacyRelease).not.toHaveBeenCalled();
      expect(f.requests.filter(request => request.roomRelease()).map(request => request.roomRelease())).toEqual([{
        schema: 'concord.auth-room-binding', version: 1,
        binding: f.owners.snapshot(f.owner).binding,
        room: { sessionGeneration: f.session.sessionGeneration, roomId: f.session.roomId, roomKind: f.session.roomKind },
      }]);
      expect(f.owners.snapshot(f.owner).tokenExpiresAt).toBe(start + 60_000);
      if (race === 'leave') await f.owners.terminate(f.owner);
      expect(f.owners.retainedIdentityCount()).toBe(0);
    }
  );

  it.each([
    ['another user', () => token(100, { user_id: 'd8fe119d-a28c-4573-be19-7847469aa59f' })],
    ['another epoch', () => token(100, { cred_epoch: 'epoch-2' })],
    ['invalid signature', () => 'not-a-valid-token'],
    ['expired token', () => token(-1)],
    [
      'missing expiration',
      () => jwt.sign({ user_id: userId }, signingKey, { issuer: 'concordvoice-control-plane' }),
    ],
  ])('cannot extend the deadline with %s', async (_, fresh) => {
    const { socket, renew } = connectedSocket();
    const deadline = socket.data.tokenExpiresAt;
    const ack = vi.fn();
    await renew(fresh(), ack);
    expect(ack).toHaveBeenCalledWith({ error: 'Authentication failed' });
    expect(socket.data.tokenExpiresAt).toBe(deadline);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(socket.connected).toBe(false);
  });

  it.each([
    ['a string', 'websocket-ticket'],
    ['an object', { audience: 'media' }],
  ])('does not renew a media session with a signed %s purpose token', async (_, purpose) => {
    const { socket, renew } = connectedSocket();
    const deadline = socket.data.tokenExpiresAt;
    const ack = vi.fn();

    await renew(token(100, { purpose }), ack);

    expect(ack).toHaveBeenCalledWith({ error: 'Authentication failed' });
    expect(socket.data.tokenExpiresAt).toBe(deadline);
  });

  it('rejects an equal-expiry replay instead of reporting a successful renewal', async () => {
    const { socket, renew } = connectedSocket();
    await vi.advanceTimersByTimeAsync(9_000);
    const ack = vi.fn();
    await renew(socket.handshake.auth.token, ack);
    expect(ack).toHaveBeenCalledWith({ error: 'Authentication failed' });
    await vi.advanceTimersByTimeAsync(1_000);
    expect(socket.connected).toBe(false);
  });

  it('rejects packets and renewal past expiry even before a delayed timer executes', async () => {
    const { socket, renew, packet } = connectedSocket();
    vi.setSystemTime(start + 10_000);
    const ack = vi.fn();
    await renew(token(100), ack);
    expect(ack).toHaveBeenCalledWith({ error: 'Authentication failed' });
    const next = vi.fn();
    packet(next);
    expect(socket.connected).toBe(false);
    expect(next).not.toHaveBeenCalled();
  });

  it('does not let renewal resurrect a revoked connection', async () => {
    const { socket, renew } = connectedSocket();
    socket.disconnect();
    await renew(token(100));
    expect(socket.connected).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('rechecks long signed deadlines without overflowing the Node timer', async () => {
    const { socket, renew } = connectedSocket();
    await renew(token(3_000_000));
    await vi.advanceTimersByTimeAsync(2_147_483_647);
    expect(socket.connected).toBe(true);
    await vi.advanceTimersByTimeAsync(3_000_000_000 - 2_147_483_647);
    expect(socket.connected).toBe(false);
  });
});

async function admittedFixture() {
  vi.resetModules();
  const api = await import('../src/lib/voiceEnforcementSession.js');
  const session = enforcementSession();
  const key = Buffer.from(signingKey);
  const requests: protocol.Request[] = [];
  const hold: { value?: Promise<void> } = {};
  const closeNative = vi.fn(async () => undefined);
  const legacyVerifier = vi.fn(async () => undefined);
  const legacyRelease = vi.fn(async () => undefined);
  const originalToken = token(60);
  const owners = new VoiceAuthConnections({ nodeBootId: session.nodeBootId,
    secret: signingKey, controlPlaneUrl: 'http://control-plane.test', now: () => Date.now(),
    fetch: vi.fn<typeof fetch>(async (input, init) => {
      const path = new URL(String(input)).pathname;
      const endpoint = path.endsWith('/auth-connections') ? 'connection_register'
        : path.endsWith('/auth-connections/release') ? 'connection_release'
          : path.endsWith('/enforcement-sessions/release') ? 'room_release' : 'room_register';
      const raw = init?.body;
      if (typeof raw !== 'string' && !(raw instanceof Uint8Array)) throw new Error('Missing exact request bytes');
      const request = protocol.decodeHttpRequest(endpoint, { method: init?.method ?? '', path,
        headers: Array.from(new Headers(init?.headers), ([name, value]) => ({ name, value })), body: Buffer.from(raw) });
      protocol.verifyHttpRequest(request, key, BigInt(Math.floor(Date.now() / 1000)));
      requests.push(request);
      if (request.roomRegister()?.mode === 'renew') await hold.value;
      const target = request.connectionRegister() ?? request.roomRegister();
      const binding = target && { userId, familyId: '22222222-2222-4222-8222-222222222222',
        target: { connectionGeneration: target.connectionGeneration, nodeBootId: target.nodeBootId,
          socketId: target.socketId, credentialEpoch: target.credentialEpoch } };
      const payload: protocol.ResponsePayload = request.connectionRegister() && binding
        ? { ...binding, version: 1, voiceEnforcementProtocol: 4 }
        : request.roomRegister() && binding ? { schema: 'concord.auth-room-binding', version: 1,
          voiceEnforcementProtocol: 4, binding, room: { sessionGeneration: session.sessionGeneration,
            roomId: session.roomId, roomKind: session.roomKind } } : null;
      const wire = protocol.signHttpResponse(request, payload, String(Math.floor(Date.now() / 1000)), key);
      return new Response(wire.status === 204 ? null : Buffer.from(wire.body), {
        status: wire.status, headers: wire.headers.map(({ name, value }) => [name, value]),
      });
    }),
  });
  api.setVoiceAuthConnections(owners);
  const owner = owners.reserve({ socketId: session.socketId, userId, credentialEpoch: session.credentialEpoch,
    token: originalToken, tokenExpiresAt: start + 60_000, disconnect: vi.fn() });
  await owners.register(owner);
  session.connectionGeneration = owners.snapshot(owner).target.connectionGeneration;
  owners.reserveRoom(owner, session, closeNative);
  await api.registerVoiceEnforcementSession(originalToken, session);
  return { api, owners, owner, session, requests, hold, closeNative, legacyRelease, legacyVerifier };
}
