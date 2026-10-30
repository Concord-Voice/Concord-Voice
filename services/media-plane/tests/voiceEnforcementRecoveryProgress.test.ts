import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import * as protocol from '../src/lib/voiceAuthProtocol.js';
import { createVoiceAuthReceiver } from '../src/lib/voiceAuthReceiver.js';
import { VoiceAuthConnections } from '../src/lib/voiceAuthConnection.js';
import { createVoiceAuthConnectionMiddleware } from '../src/middleware/auth.js';
import type { Socket } from 'socket.io';
import type { VoiceEnforcementSession } from '../src/lib/voiceEnforcementSession.js';
import {
  VoiceEnforcementExpiryFence,
  VoiceEnforcementHealth,
  VoiceEnforcementLease,
} from '../src/lib/voiceEnforcementLease.js';

const secret = 'recovery-progress-test-only-key';
const key = Buffer.from(secret);
const boot = '00000002-1111-4111-8111-111111111111';
const wall = 1793000000000;
const timestamp = String(wall / 1000);

function deferred() {
  let resolve: () => void = () => { throw new Error('Deferred not initialized'); };
  let reject: (error: Error) => void = () => { throw new Error('Deferred not initialized'); };
  const promise = new Promise<void>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

// Drain bounded promise continuations, without advancing a clock or sleeping.
async function settle() { for (let i = 0; i < 40; i++) await Promise.resolve(); }

function fixture() {
  let mono = 0;
  let nativeReady = true;
  let transportReady = true;
  let recoveryRuns = 0;
  let releases = 0;
  let completed = 0;
  const proofs: ReturnType<typeof deferred>[] = [];
  const grants: boolean[] = [];
  const lease = new VoiceEnforcementLease(() => mono);
  const fence = new VoiceEnforcementExpiryFence(lease, async () => {
    const proof = deferred();
    proofs.push(proof);
    await proof.promise;
    completed++;
  });
  const health = new VoiceEnforcementHealth({
    lease, nodeBootId: boot, now: () => mono,
    transportReady: () => transportReady,
    localReady: () => nativeReady && fence.ready(),
    // Preserve production recovery order: subscription confirmation, native
    // availability teardown, lease fence, then command continuity confirmation.
    recover: async () => {
      recoveryRuns++;
      await Promise.resolve();
      await Promise.resolve();
      await fence.enforce();
      await Promise.resolve();
    },
    poll: async () => {
      // The CP request and NATS challenge take asynchronous boundaries. Let
      // recovery reach its fence before the independent signed challenge.
      await settle();
      const grant = send();
      grants.push(grant);
      return { version: 1, voiceEnforcementProtocol: 4, nodeBootId: boot,
        activation: 'active', transportOK: true, admissionGranted: grant };
    },
    onTransport: () => { releases++; },
  });
  const receiver = createVoiceAuthReceiver(secret, boot, {
    verify: () => undefined, terminalKey: () => '', terminal: async () => {},
    acknowledge: () => undefined, health: async () => undefined,
  }, { healthSnapshot: command => health.healthSnapshot(command) });
  function send(activation = 'active', allowed = true): boolean {
    const command = protocol.newHealthCommand(boot, 'a'.repeat(64), activation, allowed, timestamp);
    const reply = vi.fn();
    receiver(protocol.signCommand(command, key), reply);
    expect(reply, 'signed health must reply synchronously').toHaveBeenCalledTimes(1);
    const ack = protocol.verifyAck(reply.mock.calls[0][0], command, key, BigInt(timestamp)).health();
    expect(ack?.transportOK).toBe(true);
    return ack?.admissionGranted === true;
  }
  async function healthPoll() {
    await health.poll();
    // Same post-poll watchdog decision as index.ts; never await native work.
    if (!fence.pending()) void fence.enforce().catch(() => undefined);
  }
  async function finishProofs() {
    for (const proof of proofs) proof.resolve();
    await settle();
  }
  return { lease, fence, health, send, healthPoll, finishProofs, proofs, grants,
    setMono: (value: number) => { mono = value; },
    setNativeReady: (value: boolean) => { nativeReady = value; },
    setTransportReady: (value: boolean) => { transportReady = value; },
    counts: () => ({ started: proofs.length, completed, recoveryRuns, releases }),
  };
}

beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(wall); });
afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

function ownedFixture() {
  const userId = '00000003-1111-4111-8111-111111111111';
  const familyId = '00000004-1111-4111-8111-111111111111';
  const credentialEpoch = '0123456789abcdef0123456789abcdef';
  const requests: protocol.Request[] = [];
  const registrationReplies: { request: protocol.Request; reply: ReturnType<typeof deferredResponse> }[] = [];
  let delayRegistration = false;
  function signed(request: protocol.Request) {
    const registration = request.connectionRegister();
    const close = request.provisionalClose();
    const wire = protocol.signHttpResponse(request, registration
      ? { version: 1, voiceEnforcementProtocol: 4, userId, familyId, target: {
        connectionGeneration: registration.connectionGeneration, nodeBootId: registration.nodeBootId,
        socketId: registration.socketId, credentialEpoch: registration.credentialEpoch,
      } }
      : close ? { ...close, schema: 'concord.auth-connection-provisional-close-result', disposition: 'unregistered' }
        : null, timestamp, key);
    return new Response(wire.status === 204 ? null : Buffer.from(wire.body), {
      status: wire.status, headers: wire.headers.map(header => [header.name, header.value]),
    });
  }
  function deferredResponse() {
    let resolve!: (response: Response) => void;
    const promise = new Promise<Response>(done => { resolve = done; });
    return { promise, resolve };
  }
  const paths: Record<string, protocol.Endpoint> = {
    '/api/v1/internal/voice/auth-connections': 'connection_register',
    '/api/v1/internal/voice/auth-connections/release': 'connection_release',
    '/api/v1/internal/voice/auth-connections/provisional-close': 'provisional_close',
    '/api/v1/internal/voice/enforcement-sessions/release': 'room_release',
  };
  const fetcher: typeof fetch = async (input, init) => {
    const path = new URL(String(input)).pathname;
    const endpoint = paths[path];
    if (!endpoint || !(typeof init?.body === 'string' || init?.body instanceof Uint8Array))
      throw new Error(`Unexpected owned HTTP request ${path}`);
    const headers: protocol.Header[] = [];
    new Headers(init.headers).forEach((value, name) => headers.push({ name, value }));
    const request = protocol.decodeHttpRequest(endpoint, {
      method: init.method ?? '', path, headers, body: Buffer.from(init.body),
    });
    protocol.verifyHttpRequest(request, key, BigInt(timestamp));
    requests.push(request);
    if (delayRegistration && request.connectionRegister()) {
      const reply = deferredResponse(); registrationReplies.push({ request, reply });
      return reply.promise;
    }
    return signed(request);
  };
  const owners = new VoiceAuthConnections({ nodeBootId: boot, secret,
    controlPlaneUrl: 'https://cp.test.invalid', fetch: fetcher, now: () => wall });
  const reserve = (socketId: string) => owners.reserve({ socketId, userId, credentialEpoch,
    token: 'test-only-owned-token', tokenExpiresAt: wall + 60_000, disconnect: () => {} });
  const session = (handle: ReturnType<typeof reserve>, sessionGeneration: string): VoiceEnforcementSession => ({
    ...owners.snapshot(handle).target, userId, sessionGeneration,
    roomId: '00000006-1111-4111-8111-111111111111', roomKind: 'channel',
  });
  const lease = new VoiceEnforcementLease(() => 0);
  let drains = 0;
  const fence = new VoiceEnforcementExpiryFence(lease, () => {
    drains++; return owners.terminateAll();
  }, () => owners.reservationGeneration());
  return { owners, reserve, session, lease, fence, requests, drains: () => drains,
    delayRegistration: () => { delayRegistration = true; },
    finishRegistrations: () => {
      for (const entry of registrationReplies) entry.reply.resolve(signed(entry.request));
    } };
}

describe('expiry proof observes actual private ownership', () => {
  it('keeps active admission productive when a permitted connection and room are reserved', async () => {
    const f = ownedFixture();
    await f.fence.enforce(); f.lease.renew();
    const handle = f.reserve('active-owner');
    await f.owners.register(handle);
    f.owners.reserveRoom(handle, f.session(handle, '00000007-1111-4111-8111-111111111111'), async () => {});
    expect(f.fence.ready()).toBe(true);
    expect(() => f.owners.assertProductive(handle)).not.toThrow();
    await f.fence.enforce();
    expect(f.drains()).toBe(1);
    f.lease.expire();
    expect(f.fence.ready()).toBe(false);
    await f.fence.enforce();
    expect(f.owners.retainedIdentityCount()).toBe(0);
    expect(() => f.owners.assertProductive(handle)).toThrow();
  });

  it('captures a new idle identity even when retained count is unchanged', async () => {
    const f = ownedFixture();
    const first = f.reserve('old-idle');
    await f.owners.register(first); await f.fence.enforce();
    expect(f.owners.retainedIdentityCount()).toBe(0);
    const second = f.reserve('replacement-idle');
    await f.owners.register(second);
    expect(f.owners.retainedIdentityCount()).toBe(1);
    expect(f.fence.ready()).toBe(false);
    await f.fence.enforce();
    expect(f.drains()).toBe(2);
    expect(f.owners.retainedIdentityCount()).toBe(0);
    expect(() => f.owners.assertProductive(second)).toThrow();
  });

  it('joins pending registration and refuses late revival after the exact owner is terminal', async () => {
    const f = ownedFixture();
    await f.fence.enforce();
    f.delayRegistration();
    const handle = f.reserve('provisional-owner');
    const registration = f.owners.register(handle);
    const denied = expect(registration).rejects.toThrow('Terminal voice connection');
    let complete = false;
    const proof = f.fence.enforce().then(() => { complete = true; });
    await settle();
    expect(complete).toBe(false);
    expect(f.fence.ready()).toBe(false);
    expect(f.owners.snapshot(handle).state).toBe('terminal');
    f.finishRegistrations(); await denied; await proof;
    expect(f.owners.retainedIdentityCount()).toBe(0);
    expect(() => f.owners.assertProductive(handle)).toThrow();
  });

  it('joins tracked native work and requires another pass for a reservation during the old drain', async () => {
    const f = ownedFixture();
    const handle = f.reserve('native-owner'); await f.owners.register(handle);
    const room = f.session(handle, '00000007-1111-4111-8111-111111111111');
    let nativeClosed = false;
    f.owners.reserveRoom(handle, room, async () => { nativeClosed = true; });
    const native = deferred();
    const effect = f.owners.trackRoomOperation(room, () => native.promise);
    const denied = expect(effect).rejects.toThrow('Terminal voice room');
    const old = f.fence.enforce();
    const superseded = expect(old).rejects.toThrow();
    const newcomer = f.reserve('during-drain');
    await f.owners.register(newcomer);
    await settle();
    expect(nativeClosed).toBe(false);
    expect(f.fence.ready()).toBe(false);
    native.resolve(); await denied; await superseded;
    expect(nativeClosed).toBe(true);
    expect(f.owners.retainedIdentityCount()).toBe(1);
    expect(() => f.owners.assertProductive(newcomer)).not.toThrow();
    expect(f.fence.ready()).toBe(false);
    await f.fence.enforce();
    expect(f.owners.retainedIdentityCount()).toBe(0);
    expect(() => f.owners.assertProductive(newcomer)).toThrow();
    expect(f.drains()).toBe(2);
  });

  it('does not create liabilities for unregistered transport churn refused by real middleware', async () => {
    const f = ownedFixture();
    await f.fence.enforce();
    const middleware = createVoiceAuthConnectionMiddleware(f.owners, () => f.lease.valid() && f.fence.ready());
    for (let i = 0; i < 10; i++) {
      const next = vi.fn();
      await middleware({ id: `transport-${i}`, data: {}, handshake: { auth: { token: 'test-token' } } } as unknown as Socket, next);
      expect(next).toHaveBeenCalledWith(expect.any(Error));
      await f.fence.enforce();
      expect(f.fence.ready()).toBe(true);
    }
    expect(f.owners.retainedIdentityCount()).toBe(0);
    expect(f.requests).toHaveLength(0);
    expect(f.drains()).toBe(1);
  });

  it('ignores duplicate legacy reservations and completed releases while fresh liability requires proof', async () => {
    const f = ownedFixture();
    await f.fence.enforce();
    const legacy: VoiceEnforcementSession = {
      nodeBootId: boot, socketId: 'legacy-liability', userId: '00000003-1111-4111-8111-111111111111',
      credentialEpoch: '0123456789abcdef0123456789abcdef',
      sessionGeneration: '00000007-1111-4111-8111-111111111111',
      roomId: '00000006-1111-4111-8111-111111111111', roomKind: 'channel',
    };
    const native = deferred();
    expect(f.owners.reserveLegacyRoom(legacy, () => native.promise)).toBe(true);
    expect(f.fence.ready()).toBe(false);
    const horizon = f.owners.reservationGeneration();
    expect(f.owners.reserveLegacyRoom(legacy, () => { throw new Error('Duplicate callback must not replace owner'); })).toBe(true);
    expect(f.owners.reservationGeneration()).toBe(horizon);
    const proof = f.fence.enforce();
    const release = f.owners.releaseLegacyRoom(legacy);
    expect(f.owners.releaseLegacyRoom(legacy)).toBe(release);
    expect(f.fence.ready()).toBe(false);
    native.resolve(); await release; await proof;
    expect(f.fence.ready()).toBe(true);
    expect(f.owners.reserveLegacyRoom(legacy, async () => {})).toBe(false);
    await f.owners.retryPending();
    await f.owners.releaseLegacyRoom(legacy);
    expect(f.owners.reservationGeneration()).toBe(horizon);
    await f.fence.enforce();
    expect(f.drains()).toBe(2);
    expect(f.owners.retainedIdentityCount()).toBe(0);
  });
});

describe('composed expiry teardown and signed bootstrap progress', () => {
  it('makes productive progress across three existing-cadence polls after finite teardown proof', async () => {
    const f = fixture();
    const initial = f.fence.enforce();
    expect(f.send()).toBe(false);
    await f.finishProofs();
    await initial;
    expect(f.fence.ready()).toBe(true);
    const trace: unknown[] = [];
    for (let cycle = 1; cycle <= 3; cycle++) {
      f.setMono(cycle * 10_000);
      await f.healthPoll();
      await f.finishProofs();
      expect(f.fence.pending()).toBe(false);
      expect(f.fence.ready()).toBe(true);
      trace.push({ cycle, ...f.counts(), grant: f.grants.at(-1), valid: f.lease.valid() });
    }
    // Before repair the trace contains THREE completed, finite self-vetoes,
    // not a hung fixture or a single startup race. Completing teardown itself
    // never grants authority; the verified subsequent bootstrap must do so.
    expect(f.grants, `productive progress after completed proof: ${JSON.stringify(trace)}`)
      .toEqual([true, true, true]);
    expect(f.health.status().voiceEnforcementAdmissionGranted).toBe(true);
    expect(f.counts().started).toBe(1);
  });

  it('retains initial proof and answers false immediately while recovery hangs beyond the lease deadline', async () => {
    const f = fixture();
    const initial = f.fence.enforce();
    expect(f.fence.ready()).toBe(false);
    expect(f.send()).toBe(false);
    expect(f.send('prepared', false)).toBe(false);
    for (const now of [10_000, 30_001, 60_000]) {
      f.setMono(now);
      await f.healthPoll();
      expect(f.send()).toBe(false);
      expect(f.lease.valid()).toBe(false);
      expect(f.fence.enforce()).toBe(initial);
    }
    expect(f.counts()).toEqual({ started: 1, completed: 0, recoveryRuns: 1, releases: 3 });
    expect(f.grants).toEqual([false, false, false]);
    await f.finishProofs();
    await initial;
  });

  it('requires fresh teardown when productive renewal and expiry both occur between watchdog checks', async () => {
    const f = fixture();
    const first = f.fence.enforce();
    await f.finishProofs(); await first;
    f.lease.renew();
    f.setMono(30_001);
    const next = f.fence.enforce();
    expect(f.proofs).toHaveLength(2);
    expect(f.fence.ready()).toBe(false);
    expect(f.send()).toBe(false);
    await f.finishProofs(); await next;
    expect(f.fence.ready()).toBe(true);
    expect(f.lease.valid()).toBe(false);
  });

  it('cannot use an older completion to clear a newer productive authority obligation', async () => {
    const f = fixture();
    const old = f.fence.enforce();
    let consumerRecovered = false;
    // Production health recovery awaits the fence before consumer recovery.
    // A stale successful await must not authorize that continuation.
    const recovery = old.then(() => { consumerRecovered = true; });
    const rejected = expect(recovery).rejects.toThrow();
    // A renewal can race a still-running close callback; expiry of that newer
    // authority needs its own proof even if the watchdog never observed valid.
    f.lease.renew();
    f.lease.expire();
    void f.fence.enforce().catch(() => undefined);
    f.proofs[0].resolve();
    await rejected; await settle();
    expect(consumerRecovered).toBe(false);
    expect(f.fence.ready(), 'old completion must not clear the newer expiry obligation').toBe(false);
    expect(f.send()).toBe(false);
    // Permit either eager or next-watchdog creation of the fresh proof.
    const newer = f.fence.enforce();
    expect(f.fence.ready()).toBe(false);
    expect(f.send()).toBe(false);
    f.proofs[0].resolve(); await settle();
    expect(f.fence.ready()).toBe(false);
    expect(f.proofs).toHaveLength(2);
    await f.finishProofs(); await newer;
    expect(f.fence.ready()).toBe(true);
  });

  it.each(['native', 'transport'])('withholds admission when %s prerequisites are unhealthy after proof', async fault => {
    const f = fixture();
    const initial = f.fence.enforce();
    await f.finishProofs(); await initial;
    expect(f.send()).toBe(true);
    if (fault === 'native') f.setNativeReady(false);
    else f.setTransportReady(false);
    expect(f.send()).toBe(false);
    expect(f.health.status().voiceEnforcementAdmissionGranted).toBe(false);
  });

  it('keeps failed native proof fenced until a later successful retry', async () => {
    const f = fixture();
    const failed = f.fence.enforce();
    const result = expect(failed).rejects.toThrow('test native proof unavailable');
    f.proofs[0].reject(new Error('test native proof unavailable'));
    await result;
    expect(f.fence.ready()).toBe(false);
    expect(f.send()).toBe(false);
    const retry = f.fence.enforce();
    expect(f.send()).toBe(false);
    await f.finishProofs(); await retry;
    expect(f.fence.ready()).toBe(true);
  });

  it('does not invent new authority from repeated expire calls after a completed proof', async () => {
    const f = fixture();
    const initial = f.fence.enforce();
    await f.finishProofs(); await initial;
    for (let i = 0; i < 4; i++) {
      f.lease.expire();
      await f.fence.enforce();
      expect(f.fence.ready()).toBe(true);
    }
    expect(f.proofs).toHaveLength(1);
    expect(f.lease.valid()).toBe(false);
  });

  it('requires a fresh proof after explicit expiry entirely between enforcement checks', async () => {
    const f = fixture();
    const initial = f.fence.enforce();
    await f.finishProofs(); await initial;
    f.lease.renew();
    expect(f.fence.ready()).toBe(true);
    expect(f.send()).toBe(true);
    f.lease.expire();
    expect(f.fence.ready()).toBe(false);
    const fresh = f.fence.enforce();
    expect(f.proofs).toHaveLength(2);
    expect(f.send()).toBe(false);
    await f.finishProofs(); await fresh;
  });

  it('invalidates synchronously under valid authority and retains one task across notifications', async () => {
    const f = fixture();
    const initial = f.fence.enforce();
    await f.finishProofs(); await initial;
    f.lease.renew();
    expect(f.send()).toBe(true);
    f.fence.invalidate();
    expect(f.lease.valid()).toBe(true);
    expect(f.fence.ready()).toBe(false);
    expect(f.send()).toBe(false);
    const stale = f.fence.enforce();
    const rejected = expect(stale).rejects.toThrow();
    for (let i = 0; i < 20; i++) {
      f.fence.invalidate();
      expect(f.fence.enforce()).toBe(stale);
    }
    expect(f.proofs).toHaveLength(2);
    f.proofs[1].resolve(); await rejected;
    expect(f.fence.ready()).toBe(false);
    const current = f.fence.enforce();
    await f.finishProofs(); await current;
    expect(f.fence.ready()).toBe(true);
    expect(f.send()).toBe(true);
  });

  it('keeps later invalidation fenced after late failure of an older pending proof', async () => {
    const f = fixture();
    const old = f.fence.enforce();
    const rejected = expect(old).rejects.toThrow('old native proof failed');
    f.lease.renew(); f.lease.expire();
    f.fence.invalidate();
    f.proofs[0].reject(new Error('old native proof failed'));
    await rejected;
    expect(f.fence.ready()).toBe(false);
    expect(f.send()).toBe(false);
    const fresh = f.fence.enforce();
    expect(f.proofs).toHaveLength(2);
    await f.finishProofs(); await fresh;
    expect(f.fence.ready()).toBe(true);
  });

  it('never resumes health recovery or renews a stopped boot after late proof completion', async () => {
    const f = fixture();
    await f.healthPoll();
    expect(f.proofs).toHaveLength(1);
    const recoveryJoined = f.health.fenceRecoveryAndJoin();
    await f.health.stopPollingAndJoin();
    expect(f.send()).toBe(false);
    await f.finishProofs(); await recoveryJoined;
    await f.health.poll();
    expect(f.counts().recoveryRuns).toBe(1);
    expect(f.lease.valid()).toBe(false);
    expect(f.send()).toBe(false);
    expect(f.health.status().voiceEnforcementAdmissionGranted).toBe(false);
  });
});
