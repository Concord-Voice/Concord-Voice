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
  renewVoiceEnforcementSession,
  type VoiceEnforcementSession,
} from '../src/lib/voiceEnforcementSession.js';

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

function enforcementSession(sessionGeneration = 'refresh-generation'): VoiceEnforcementSession {
  return {
    sessionGeneration,
    nodeBootId: 'node-boot',
    roomId: 'dm-room',
    roomKind: 'dm',
    userId,
    credentialEpoch: 'epoch-1',
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

  it('does not re-register a session that left before refresh starts', async () => {
    const release = vi.fn(async () => {});
    vi.stubGlobal('fetch', vi.fn());

    await expect(
      renewVoiceEnforcementSession(token(20), enforcementSession(), () => false, release)
    ).rejects.toThrow('Session changed during renewal');

    expect(fetch).not.toHaveBeenCalled();
    expect(release).not.toHaveBeenCalled();
  });

  it.each(['leave', 'disconnect'] as const)(
    'releases the exact durable session when %s races refresh registration',
    async (race) => {
      const session = enforcementSession();
      const successor = enforcementSession('successor-generation');
      let resolveRegistration!: (response: Response) => void;
      vi.stubGlobal(
        'fetch',
        vi.fn(
          () =>
            new Promise<Response>((resolve) => {
              resolveRegistration = resolve;
            })
        )
      );
      let connected = true;
      let roomOwnsSession = true;
      const socketSession = session;
      const release = vi.fn(async () => {});
      const pending = renewVoiceEnforcementSession(
        token(20),
        session,
        () => connected && socketSession === session && roomOwnsSession,
        release
      );

      expect(fetch).toHaveBeenCalledOnce();
      if (race === 'leave') roomOwnsSession = false;
      else connected = false;
      resolveRegistration({ ok: true } as Response);

      await expect(pending).rejects.toThrow('Session changed during renewal');
      expect(release).toHaveBeenCalledExactlyOnceWith(session);
      expect(release).not.toHaveBeenCalledWith(successor);
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
