import { describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { randomUUID } from 'node:crypto';
import { Script } from 'node:vm';
import ts from 'typescript';
import * as lifecycle from '../src/lib/socketJoinLifecycle.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import type { MediasoupService } from '../src/lib/mediasoup.js';
import { attestationRelayHeaders, resolveParticipantIdentity } from '../src/middleware/auth.js';
import { getIdentityAuthorityReasonCode } from '../src/lib/identityAuthority.js';
import { mediaPolicyCooldownAck } from '../src/lib/mediaPolicyWire.js';
import {
  cleanupEmptyDMJoin,
  DMJoinCallIdTracker,
  KeyedJoinFence,
  reauthorizeAdmission,
  rollbackSocketRoomJoin,
  SocketRoomClaim,
  runSocketBoundJoin,
  withKeyedJoinFence,
  roomCreationRefusalAck,
} from '../src/lib/socketJoinLifecycle.js';
import { RoomManager, RoomCreationRefusedError, CryptoVersionMismatchError, MediaPolicyCooldownError, parseMediaFrameCryptoVersion, SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION } from '../src/lib/roomManager.js';
import { isCanonicalEnforcementUUID } from '../src/lib/enforcementCommand.js';

describe('room creation refusal acknowledgement', () => {
  it.each(['room_draining', 'room_owner_mismatch', 'room_owner_unavailable'] as const)(
    'returns a typed ack for %s without requiring a socket close',
    (code) => {
      expect(roomCreationRefusalAck(new RoomCreationRefusedError(code))).toEqual({
        error: code,
        code,
      });
    }
  );

  it('does not reinterpret unrelated failures as placement refusals', () => {
    expect(roomCreationRefusalAck(new Error('native failure'))).toBeUndefined();
  });

  it('does not log an expected placement refusal as an unexpected join error', () => {
    const source = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8');
    const emitJoinError = source.slice(
      source.indexOf('function emitJoinError('),
      source.indexOf('function getKeyframeSenderUserId(')
    );

    expect(emitJoinError).toMatch(
      /if \(\s*!\(error instanceof MediaPolicyCooldownError\) &&\s*!\(error instanceof RoomCreationRefusedError\)\s*\)/
    );
  });

  it.each(['room_draining', 'room_owner_mismatch', 'room_owner_unavailable'] as const)(
    'preserves DM authorization after actual caller receives %s (ADR-0042 V17)', async (code) => {
      const now = Date.now();
      const boot = '11111111-1111-4111-8111-111111111111';
      const user = '22222222-2222-4222-8222-222222222222';
      const family = '33333333-3333-4333-8333-333333333333';
      const roomId = '44444444-4444-4444-8444-444444444444';
      const callId = '55555555-5555-4555-8555-555555555555';
      const epoch = 'a'.repeat(32);
      const secret = ['placement', 'fixture', 'only'].join('-');
      const key = Buffer.from(secret);
      const requests: protocol.Request[] = [];
      const owners = new VoiceAuthConnections({ nodeBootId: boot, secret,
        controlPlaneUrl: 'https://control.test.invalid', now: () => now,
        fetch: async (input, init) => {
          const path = new URL(String(input)).pathname;
          const endpoint: protocol.Endpoint = path.endsWith('/auth-connections') ? 'connection_register'
            : path.endsWith('/auth-connections/release') ? 'connection_release' : 'room_release';
          const raw = init?.body;
          if (typeof raw !== 'string' && !(raw instanceof Uint8Array)) throw new Error('Missing request body');
          const request = protocol.decodeHttpRequest(endpoint, { method: init?.method ?? '', path,
            body: Buffer.from(raw), headers: Array.from(new Headers(init?.headers), ([name, value]) => ({ name, value })) });
          protocol.verifyHttpRequest(request, key, BigInt(Math.floor(now / 1000)));
          requests.push(request);
          const registration = request.connectionRegister();
          const payload: protocol.ResponsePayload = registration ? { version: 1, voiceEnforcementProtocol: 4,
            userId: user, familyId: family, target: { connectionGeneration: registration.connectionGeneration,
              nodeBootId: boot, socketId: registration.socketId, credentialEpoch: epoch } } : null;
          const signed = protocol.signHttpResponse(request, payload, String(Math.floor(now / 1000)), key);
          return new Response(signed.body.length ? Buffer.from(signed.body) : null,
            { status: signed.status, headers: signed.headers.map(({ name, value }) => [name, value]) });
        } });
      const token = 'test-placement-token';
      const attestationHandshake = { attestation_token: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
        session_id: '0a1b2c3d-4e5f-4a6b-8c7d-8e9fa0b1c2d3', machine_id: '9e8d7c6b-5a49-4382-9716-05f4e3d2c1b0' };
      const handle = owners.reserve({ socketId: 'placement-socket', userId: user, credentialEpoch: epoch,
        token, tokenExpiresAt: now + 60_000, disconnect: () => {} });
      await owners.register(handle);
      const nativeService = { getOrCreateRouter: vi.fn(async () => { throw new Error('Unexpected native creation'); }) };
      const manager = new RoomManager(nativeService as unknown as MediasoupService);
      manager.setVoiceAuthConnections(owners);
      const placement = vi.fn(async () => { throw new RoomCreationRefusedError(code); });
      manager.setRoomCreationGuard(placement);
      const closeEmpty = vi.spyOn(manager, 'closeEmptyDMRoom');
      const remove = vi.spyOn(manager, 'removeProvisionalParticipantForEnforcement');
      const releaseDMVoiceAuthorization = vi.fn();
      const registerVoiceEnforcementSession = vi.fn();
      const validateChannelAccess = vi.fn(async () => ({ allowed: true, callId, userTier: 'free' }));
      const source = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8');
      const ast = ts.createSourceFile('index.ts', source, ts.ScriptTarget.Latest, true);
      const names = ['registerJoinRoomHandler', 'parseJoinMediaFrameCryptoVersion', 'joinErrorAck', 'emitJoinError',
        'rollbackRegisteredRoomJoin', 'commitChannelAdmission', 'commitDMAdmission', 'rollbackSocketMembership'];
      const declarations = names.map(name => {
        const matches = ast.statements.filter(node => ts.isFunctionDeclaration(node) && node.name?.text === name);
        if (matches.length !== 1) throw new Error(`Expected one actual function ${name}`);
        return matches[0].getText(ast);
      });
      const compiled = ts.transpileModule(declarations.join('\n'), {
        compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
      }).outputText;
      const register = new Script(`${compiled}\nregisterJoinRoomHandler;`).runInNewContext({
        ...lifecycle, randomUUID, Date, Error, Promise,
        getVoiceAuthConnection: () => handle, voiceEnforcementNodeBootId: boot,
        validateChannelAccess, attestationRelayHeaders, releaseDMVoiceAuthorization, registerVoiceEnforcementSession,
        isCanonicalEnforcementUUID, parseMediaFrameCryptoVersion, resolveParticipantIdentity,
        RoomCreationRefusedError, CryptoVersionMismatchError, MediaPolicyCooldownError, mediaPolicyCooldownAck,
        getIdentityAuthorityReasonCode, logger: { info() {}, debug() {}, warn() {}, error() {} },
      }) as (socket: unknown, data: unknown, deps: unknown) => void;
      const socket = { id: 'placement-socket', connected: true, handshake: { auth: { token, room_kind: 'dm', ...attestationHandshake } },
        emit: vi.fn(), join: vi.fn(), leave: vi.fn(), to: () => ({ emit: vi.fn() }) };
      type Handler = (request: object, callback: (value: unknown) => void) => Promise<void>;
      let handler: Handler | undefined;
      const data = { userId: user, username: 'placement-fixture', credentialEpoch: epoch, tokenExpiresAt: now + 60_000 };
      register(socket, data, { roomManager: manager, voiceAuthConnections: owners,
        dmJoinFence: new KeyedJoinFence(), channelAdmissionFence: new KeyedJoinFence(), isShuttingDown: () => false,
        withRateLimit: (_socket: unknown, event: string, listener: Handler) => {
          if (event !== 'join-room') throw new Error('Unexpected handler');
          handler = listener;
        } });
      const callback = vi.fn();
      try {
        if (!handler) throw new Error('Actual handler was not registered');
        await handler({ roomId, callId, rtpCapabilities: {}, mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION }, callback);
        // The admission hop relays this socket's own attestation token and machine id
        // (#4206); the client's session_id is never relayed.
        expect(validateChannelAccess).toHaveBeenCalledExactlyOnceWith(user, roomId, token, 'dm', callId, undefined, {
          'X-Attestation-Token': attestationHandshake.attestation_token,
          'X-Machine-Id': attestationHandshake.machine_id,
        });
        expect(placement).toHaveBeenCalledExactlyOnceWith(roomId);
        expect(callback).toHaveBeenCalledExactlyOnceWith({ error: code, code });
        expect(closeEmpty).not.toHaveBeenCalled();

        expect(releaseDMVoiceAuthorization).not.toHaveBeenCalled();
        expect(registerVoiceEnforcementSession).not.toHaveBeenCalled();
        expect(socket.join).not.toHaveBeenCalled();
        expect(nativeService.getOrCreateRouter).not.toHaveBeenCalled();
        expect(manager.getRoom(roomId)).toBeUndefined();
        expect(owners.retainedIdentityCount()).toBe(1);
        expect(owners.snapshot(handle).state).toBe('registered');
        const releases = requests.filter(request => request.endpoint() === 'room_release');
        expect(releases).toHaveLength(1);
        const released = releases[0].roomRelease();
        expect(remove).toHaveBeenCalledExactlyOnceWith(roomId, user, socket.id, released?.room.sessionGeneration);
        expect(released).toMatchObject({
          binding: { userId: user, familyId: family, target: owners.snapshot(handle).target },
          room: { roomId, roomKind: 'dm', sessionGeneration: expect.any(String) },
        });
      } finally {
        await owners.terminateAll();
      }
    }
  );
});

describe('join-room placement identity', () => {
  it('positive control: canonical UUID spelling is admitted by the existing validator', () => {
    const canonical = '550e8400-e29b-41d4-a716-446655440010';

    expect(isCanonicalEnforcementUUID(canonical)).toBe(true);
    expect(isCanonicalEnforcementUUID(canonical.toUpperCase())).toBe(false);
  });
});

describe('DMJoinCallIdTracker', () => {
  it('uses the requested call ID until authorization returns its canonical ID', () => {
    const tracker = new DMJoinCallIdTracker('client-call');

    expect(tracker.current()).toBe('client-call');
    tracker.observe('server-call');
    expect(tracker.current()).toBe('server-call');
  });

  it('retains the last server-authoritative ID when a later denial omits it', () => {
    const tracker = new DMJoinCallIdTracker('client-call');

    tracker.observe('server-call');
    tracker.observe(undefined);
    expect(tracker.current()).toBe('server-call');
  });

  it('retains the A1 call ID when A2 reports a rotated generation (#2407)', () => {
    const tracker = new DMJoinCallIdTracker('client-call');

    tracker.observe('call-a1');
    tracker.observe('call-a2-rotated');

    expect(tracker.current()).toBe('call-a1');
  });
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe('runSocketBoundJoin', () => {
  it('does not join when the socket disconnects while authorization is pending', async () => {
    const authorization = deferred<{ allowed: boolean }>();
    let connected = true;
    const join = vi.fn(async () => ({ room: 'dm-1' }));
    const rollback = vi.fn(async () => undefined);

    const operation = runSocketBoundJoin({
      authorize: () => authorization.promise,
      isAllowed: (access) => access.allowed,
      isConnected: () => connected,
      join,
      rollback,
    });
    connected = false;
    authorization.resolve({ allowed: true });

    await expect(operation).resolves.toEqual({
      access: { allowed: true },
      status: 'canceled',
    });
    expect(join).not.toHaveBeenCalled();
    expect(rollback).toHaveBeenCalledOnce();
  });

  it('rolls back the exact participant when disconnect lands during room admission', async () => {
    const joined = deferred<{ room: string }>();
    let connected = true;
    const join = vi.fn(() => joined.promise);
    const rollback = vi.fn(async () => undefined);
    const operation = runSocketBoundJoin({
      authorize: async () => ({ allowed: true }),
      isAllowed: (access) => access.allowed,
      isConnected: () => connected,
      join,
      rollback,
    });
    await vi.waitFor(() => expect(join).toHaveBeenCalledOnce());
    connected = false;
    joined.resolve({ room: 'dm-1' });

    await expect(operation).resolves.toEqual({
      access: { allowed: true },
      status: 'canceled',
    });
    expect(rollback).toHaveBeenCalledOnce();
  });

  it('rolls back when admission throws after it may have mutated room state', async () => {
    const rollback = vi.fn(async () => undefined);
    const failure = new Error('server mute failed');

    await expect(
      runSocketBoundJoin({
        authorize: async () => ({ allowed: true }),
        isAllowed: (access) => access.allowed,
        isConnected: () => true,
        join: async () => {
          throw failure;
        },
        rollback,
      })
    ).rejects.toBe(failure);
    expect(rollback).toHaveBeenCalledOnce();
  });

  it('rolls back when DM access is revoked after room registration but before admission', async () => {
    const initialAccess = { allowed: true, callId: 'call-a', serverMuted: false };
    const revokedAccess = { allowed: false, callId: undefined, serverMuted: false };
    const order: string[] = [];
    const rollback = vi.fn(async (access: typeof initialAccess) => {
      expect(access).toBe(initialAccess);
      order.push('rollback');
    });
    const finalize = vi.fn(async () => {
      order.push('finalize');
    });

    await expect(
      runSocketBoundJoin({
        authorize: async () => initialAccess,
        isAllowed: (access) => access.allowed,
        isConnected: () => true,
        join: async () => {
          order.push('join');
          return { room: 'dm-1' };
        },
        reauthorize: async (access) => {
          expect(access).toBe(initialAccess);
          order.push('reauthorize');
          return revokedAccess;
        },
        finalize,
        rollback,
      })
    ).resolves.toEqual({ access: revokedAccess, status: 'revoked' });

    expect(order).toEqual(['join', 'reauthorize', 'rollback']);
    expect(finalize).not.toHaveBeenCalled();
  });

  it('rolls back a registered channel session when its credential epoch rotates in flight', async () => {
    const presentedEpoch = 'old';
    let currentEpoch = presentedEpoch;
    let registered = false;
    const authorize = vi.fn(async () => ({ allowed: presentedEpoch === currentEpoch }));

    await expect(
      runSocketBoundJoin({
        authorize: () => authorize(undefined, false),
        isAllowed: (access) => access.allowed,
        isConnected: () => true,
        join: async () => {
          registered = true;
          currentEpoch = 'new';
          return 'registered';
        },
        reauthorize: (access) => reauthorizeAdmission('channel', access, undefined, authorize),
        rollback: async () => {
          registered = false;
        },
      })
    ).resolves.toMatchObject({ status: 'revoked' });

    expect(authorize).toHaveBeenCalledTimes(2);
    expect(authorize).toHaveBeenNthCalledWith(1, undefined, false);
    expect(authorize).toHaveBeenNthCalledWith(2, undefined, true);
    expect(registered).toBe(false);
  });

  it('finalizes admission from the revalidated moderation state', async () => {
    const initialAccess = { allowed: true, serverMuted: false };
    const refreshedAccess = { allowed: true, serverMuted: true };
    const order: string[] = [];
    const value = { room: 'dm-1' };

    await expect(
      runSocketBoundJoin({
        authorize: async () => initialAccess,
        isAllowed: (access) => access.allowed,
        isConnected: () => true,
        join: async () => {
          order.push('join');
          return value;
        },
        reauthorize: async () => {
          order.push('reauthorize');
          return refreshedAccess;
        },
        finalize: async (access, joined) => {
          expect(access).toBe(refreshedAccess);
          expect(joined).toBe(value);
          order.push('finalize');
        },
        rollback: vi.fn(async () => undefined),
      })
    ).resolves.toEqual({ access: refreshedAccess, status: 'joined', value });

    expect(order).toEqual(['join', 'reauthorize', 'finalize']);
  });

  it('commits only after A2 refresh and the final connected check (#2407)', async () => {
    const initialAccess = { allowed: true, callId: 'call-a1' };
    const refreshedAccess = { allowed: true, callId: 'call-a1' };
    const order: string[] = [];
    let connectedChecks = 0;

    await expect(
      runSocketBoundJoin({
        authorize: async () => initialAccess,
        isAllowed: (access, authorizedAccess) => {
          if (access === refreshedAccess) order.push('validate-a2');
          return (
            access.allowed &&
            (authorizedAccess === undefined || access.callId === authorizedAccess.callId)
          );
        },
        isConnected: () => {
          connectedChecks++;
          if (connectedChecks === 2) order.push('pre-reauthorize-connected-check');
          if (connectedChecks === 3) order.push('final-connected-check');
          return true;
        },
        join: async () => {
          order.push('register');
          return { room: 'dm-1', committed: false };
        },
        reauthorize: async () => {
          order.push('authorize-a2');
          return refreshedAccess;
        },
        finalize: async () => {
          order.push('apply-refreshed-state');
        },
        commit: (_access, value) => {
          order.push('commit');
          return { ...value, committed: true };
        },
        rollback: vi.fn(async () => undefined),
      })
    ).resolves.toEqual({
      access: refreshedAccess,
      status: 'joined',
      value: { room: 'dm-1', committed: true },
    });

    expect(order).toEqual([
      'register',
      'pre-reauthorize-connected-check',
      'authorize-a2',
      'validate-a2',
      'apply-refreshed-state',
      'final-connected-check',
      'commit',
    ]);
  });

  it('does not start A2 after the staged socket disconnects', async () => {
    let connected = true;
    const reauthorize = vi.fn(async () => ({ allowed: true }));
    const rollback = vi.fn(async () => undefined);

    await expect(
      runSocketBoundJoin({
        authorize: async () => ({ allowed: true }),
        isAllowed: (access) => access.allowed,
        isConnected: () => connected,
        join: async () => {
          connected = false;
          return 'staged';
        },
        reauthorize,
        rollback,
      })
    ).resolves.toEqual({ access: { allowed: true }, status: 'canceled' });

    expect(reauthorize).not.toHaveBeenCalled();
    expect(rollback).toHaveBeenCalledOnce();
  });

  it('rejects a rotated A2 generation before commit (#2407)', async () => {
    const initialAccess = { allowed: true, callId: 'call-a1' };
    const rotatedAccess = { allowed: true, callId: 'call-a2' };
    const commit = vi.fn((_: typeof initialAccess, value: string) => value);
    const rollback = vi.fn(async () => undefined);

    await expect(
      runSocketBoundJoin({
        authorize: async () => initialAccess,
        isAllowed: (access, authorizedAccess) =>
          access.allowed &&
          (authorizedAccess === undefined || access.callId === authorizedAccess.callId),
        isConnected: () => true,
        join: async () => 'registered',
        reauthorize: async () => rotatedAccess,
        commit,
        rollback,
      })
    ).resolves.toEqual({ access: rotatedAccess, status: 'revoked' });

    expect(commit).not.toHaveBeenCalled();
    expect(rollback).toHaveBeenCalledExactlyOnceWith(initialAccess);
  });

  it('rolls back instead of committing when the socket disconnects after refresh (#2407)', async () => {
    let connected = true;
    const commit = vi.fn((_: { allowed: boolean }, value: string) => value);
    const rollback = vi.fn(async () => undefined);

    await expect(
      runSocketBoundJoin({
        authorize: async () => ({ allowed: true }),
        isAllowed: (access) => access.allowed,
        isConnected: () => connected,
        join: async () => 'registered',
        reauthorize: async () => ({ allowed: true }),
        finalize: async () => {
          connected = false;
        },
        commit,
        rollback,
      })
    ).resolves.toEqual({ access: { allowed: true }, status: 'canceled' });

    expect(commit).not.toHaveBeenCalled();
    expect(rollback).toHaveBeenCalledOnce();
  });

  it('rejects when a canceled join cannot be rolled back', async () => {
    const failure = new Error('rollback failed');
    await expect(
      runSocketBoundJoin({
        authorize: async () => ({ allowed: true }),
        isAllowed: (access) => access.allowed,
        isConnected: () => false,
        join: vi.fn(),
        rollback: async () => {
          throw failure;
        },
      })
    ).rejects.toBe(failure);
  });
});

describe('reauthorizeAdmission', () => {
  it('binds the second DM authorization to the exact call ID returned by the first', async () => {
    const access = { allowed: true, callId: 'server-call' };
    const authorize = vi.fn(async () => access);

    await expect(reauthorizeAdmission('dm', access, 'client-call', authorize)).resolves.toBe(
      access
    );
    expect(authorize).toHaveBeenCalledExactlyOnceWith('server-call', false);
  });

  it('reauthorizes channels during A2 instead of trusting the A1 result', async () => {
    const access = { allowed: true, callId: 'channel-id' };
    const refreshed = { allowed: false, callId: 'channel-id' };
    const authorize = vi.fn(async () => refreshed);

    await expect(reauthorizeAdmission('channel', access, undefined, authorize)).resolves.toBe(
      refreshed
    );
    expect(authorize).toHaveBeenCalledExactlyOnceWith(undefined, true);
  });
});

it('serializes admission and rollback for the same room', async () => {
  const fence = new KeyedJoinFence();
  const releaseFirst = await fence.acquire('dm-1');
  let secondAcquired = false;
  const second = fence.acquire('dm-1').then((release) => {
    secondAcquired = true;
    return release;
  });
  await Promise.resolve();
  expect(secondAcquired).toBe(false);
  expect(fence.pending('dm-1')).toBe(2);
  releaseFirst();
  const releaseSecond = await second;
  expect(secondAcquired).toBe(true);
  expect(fence.pending('dm-1')).toBe(1);
  releaseSecond();
  expect(fence.pending('dm-1')).toBe(0);
});

it('serializes one channel user while allowing another user to proceed', async () => {
  const fence = new KeyedJoinFence();
  const releaseFirst = deferred<void>();
  const firstStarted = deferred<void>();
  const order: string[] = [];
  const run = (userId: string, operation: () => Promise<void>) =>
    withKeyedJoinFence(fence, JSON.stringify(['channel-1', userId]), operation);

  const first = run('user-1', async () => {
    order.push('user-1:start');
    firstStarted.resolve();
    await releaseFirst.promise;
    order.push('user-1:end');
  });
  await firstStarted.promise;

  const second = run('user-1', async () => {
    order.push('user-1:second');
  });
  const independent = run('user-2', async () => {
    order.push('user-2:start');
  });

  await independent;
  expect(order).toEqual(['user-1:start', 'user-2:start']);
  expect(fence.pending(JSON.stringify(['channel-1', 'user-1']))).toBe(2);

  releaseFirst.resolve();
  await Promise.all([first, second]);
  expect(order).toEqual(['user-1:start', 'user-2:start', 'user-1:end', 'user-1:second']);
});

it('keeps the channel admission fence wired to the channel/user key', () => {
  const source = readFileSync(new URL('../src/index.ts', import.meta.url), 'utf8');
  const registration = source.slice(
    source.indexOf('function registerJoinRoomHandler'),
    source.indexOf('async function main')
  );

  expect(registration).toContain('JSON.stringify([roomId, data.userId]),\n            executeJoin');
  expect(source).toContain('registerJoinRoomHandler(socket, data, {');
  expect(source).toContain('channelAdmissionFence,');
});

it('hands a deferred A1 call ID to the last queued cleanup (#2407)', async () => {
  const fence = new KeyedJoinFence();
  const releaseAuthorization = vi.fn(async () => undefined);
  const firstStarted = deferred<void>();

  const cleanup = async (callId?: string) => {
    const cleanupCallId = fence.callIdForCleanup('dm-1', callId);
    const result = await cleanupEmptyDMJoin({
      authorizedCallId: cleanupCallId,
      closeEmptyRoom: vi.fn(),
      queuedJoinCount: fence.pending('dm-1'),
      releaseAuthorization,
    });
    if (result === 'deferred') fence.deferCleanupCallId('dm-1', cleanupCallId);
    return result;
  };

  const first = withKeyedJoinFence(fence, 'dm-1', async () => {
    firstStarted.resolve();
    await vi.waitFor(() => expect(fence.pending('dm-1')).toBe(2));
    return cleanup('call-a1');
  });
  await firstStarted.promise;
  const finalNoIdJoin = withKeyedJoinFence(fence, 'dm-1', () => cleanup());

  await expect(first).resolves.toBe('deferred');
  await expect(finalNoIdJoin).resolves.toBe('released');
  expect(releaseAuthorization).toHaveBeenCalledExactlyOnceWith('call-a1');
  expect(fence.pending('dm-1')).toBe(0);
  expect(fence.callIdForCleanup('dm-1')).toBeUndefined();
});

it('releases the room fence when a stalled authorization aborts', async () => {
  vi.useFakeTimers();
  try {
    const fence = new KeyedJoinFence();
    const first = withKeyedJoinFence(fence, 'dm-1', async () => {
      const controller = new AbortController();
      setTimeout(() => controller.abort(), 5_000);
      await new Promise((_, reject) => {
        controller.signal.addEventListener('abort', () => reject(controller.signal.reason), {
          once: true,
        });
      });
    });
    const firstRejected = first.then(
      () => false,
      () => true
    );
    const second = withKeyedJoinFence(fence, 'dm-1', async () => 'joined');

    await vi.advanceTimersByTimeAsync(5_000);
    await expect(firstRejected).resolves.toBe(true);
    await expect(second).resolves.toBe('joined');
    expect(fence.pending('dm-1')).toBe(0);
  } finally {
    vi.useRealTimers();
  }
});

it('allows one in-flight or admitted room per socket so disconnect leaves no ghost', async () => {
  const claim = new SocketRoomClaim();
  const admittedRooms = new Set<string>();
  let joinedRoomId: string | undefined;

  const join = (roomId: string) => {
    const release = claim.claim(joinedRoomId, roomId);
    if (!release) return false;
    admittedRooms.add(roomId);
    joinedRoomId = roomId;
    release();
    return true;
  };

  expect(join('dm-a')).toBe(true);
  expect(join('dm-b')).toBe(false);
  expect([...admittedRooms]).toEqual(['dm-a']);

  admittedRooms.delete(joinedRoomId!);
  joinedRoomId = undefined;
  expect(admittedRooms).toHaveLength(0);
  expect(claim.claim(joinedRoomId, 'dm-b')).toBeTypeOf('function');
});

it('rejects a second room while the first socket admission is still pending', () => {
  const claim = new SocketRoomClaim();
  const releaseFirst = claim.claim(undefined, 'dm-a');

  expect(releaseFirst).toBeTypeOf('function');
  expect(claim.claim(undefined, 'dm-b')).toBeUndefined();

  releaseFirst?.();
  expect(claim.claim(undefined, 'dm-b')).toBeTypeOf('function');
});

describe('cleanupEmptyDMJoin', () => {
  it('preserves the admitted call while a queued successor takes over the empty room', async () => {
    const room = { callId: 'call-a', participantCount: 0 };
    const closeEmptyRoom = vi.fn(async () => 'terminal' as const);
    const releaseAuthorization = vi.fn(async () => undefined);

    await expect(
      cleanupEmptyDMJoin({
        authorizedCallId: 'call-a',
        closeEmptyRoom,
        queuedJoinCount: 2,
        releaseAuthorization,
        room,
      })
    ).resolves.toBe('deferred');
    expect(closeEmptyRoom).not.toHaveBeenCalled();
    expect(releaseAuthorization).not.toHaveBeenCalled();

    room.participantCount = 1;
    await expect(
      cleanupEmptyDMJoin({
        authorizedCallId: 'call-a',
        closeEmptyRoom,
        queuedJoinCount: 1,
        releaseAuthorization,
        room,
      })
    ).resolves.toBe('active');
    expect(closeEmptyRoom).not.toHaveBeenCalled();
    expect(releaseAuthorization).not.toHaveBeenCalled();
  });

  it('terminalizes exactly once when the last queued successor also fails', async () => {
    const closeEmptyRoom = vi.fn(async () => 'terminal' as const);
    const releaseAuthorization = vi.fn(async () => undefined);

    await expect(
      cleanupEmptyDMJoin({
        authorizedCallId: 'call-a',
        closeEmptyRoom,
        queuedJoinCount: 1,
        releaseAuthorization,
        room: { callId: 'call-a', participantCount: 0 },
      })
    ).resolves.toBe('terminal');
    expect(closeEmptyRoom).toHaveBeenCalledExactlyOnceWith('call-a');
    expect(releaseAuthorization).not.toHaveBeenCalled();
  });

  it('silently discards an unadmitted local room before releasing its reservation', async () => {
    const order: string[] = [];

    await expect(
      cleanupEmptyDMJoin({
        authorizedCallId: 'call-a',
        closeEmptyRoom: async () => {
          order.push('close');
          return 'discarded';
        },
        queuedJoinCount: 1,
        releaseAuthorization: async () => {
          order.push('release');
        },
        room: { callId: 'call-a', participantCount: 0 },
      })
    ).resolves.toBe('released');
    expect(order).toEqual(['close', 'release']);
  });
});

it('uses exact-owner normal teardown when a channel socket disconnects during admission', async () => {
  const leaveChannelIfOwned = vi.fn(async () => true);
  const removeDMParticipant = vi.fn();
  const cleanupDMJoin = vi.fn(async () => undefined);

  await expect(
    runSocketBoundJoin({
      authorize: async () => ({ allowed: true }),
      isAllowed: (access) => access.allowed,
      isConnected: () => false,
      join: vi.fn(),
      rollback: () =>
        rollbackSocketRoomJoin({
          cleanupDMJoin,
          leaveChannelIfOwned,
          removeDMParticipant,
          roomKind: 'channel',
        }),
    })
  ).resolves.toEqual({ access: { allowed: true }, status: 'canceled' });

  expect(leaveChannelIfOwned).toHaveBeenCalledOnce();
  expect(removeDMParticipant).not.toHaveBeenCalled();
  expect(cleanupDMJoin).not.toHaveBeenCalled();
});
