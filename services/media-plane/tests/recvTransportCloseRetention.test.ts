import { describe, expect, it, vi } from 'vitest';
import type { Router, Worker } from 'mediasoup/types';
import type { MediasoupService, NativeTerminationTarget } from '../src/lib/mediasoup.js';
import { createMockRouter, createMockTransport, createMockWorker } from './mocks/mediasoup.js';
import './mocks/logger.js';

vi.mock('mediasoup', () => ({ createWorker: vi.fn() }));
vi.mock('@/config/index.js', () => ({
  config: {
    freeVideoPublisherCap: 8,
    freeScreenProducerCap: 1,
    freeAudioLastN: 8,
    audioLastNHoldMs: 2500,
    audioLevelObserver: { maxEntries: 1, threshold: -60, interval: 300 },
    mediasoup: {
      webRtcTransport: {
        listenIps: [{ ip: '127.0.0.1' }],
        enableUdp: true,
        enableTcp: false,
        maxIncomingBitrate: 50_000_000,
      },
    },
  },
}));

import {
  MAX_RECV_TRANSPORTS_PER_PARTICIPANT,
  RoomManager,
  SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
} from '../src/lib/roomManager.js';

const ROOM_ID = '10000000-0000-4000-8000-000000000001';
const USER_ID = '10000000-0000-4000-8000-000000000002';
const SOCKET_ID = 'recv-retention-socket';
const SESSION_GENERATION = '10000000-0000-4000-8000-000000000003';

// Drain asynchronous close continuations, without a timed wait or real worker.
async function flushCloseContinuations(): Promise<void> {
  await new Promise<void>((resolve) => setImmediate(resolve));
}

async function admittedSession() {
  const nativeTransportIds = new Set<string>();
  const unresolvedTransportIds = new Set<string>();
  const transports: ReturnType<typeof createMockTransport>[] = [];
  const router = createMockRouter({ id: 'recv-retention-router' });
  const worker = createMockWorker({ pid: 2624 });
  router.createWebRtcTransport.mockImplementation(async () => {
    const transport = createMockTransport({ id: `recv-${transports.length}` });
    nativeTransportIds.add(transport.id);
    transport.close.mockImplementation(() => {
      transport.closed = true;
      if (!unresolvedTransportIds.has(transport.id)) nativeTransportIds.delete(transport.id);
    });
    transports.push(transport);
    return transport;
  });
  const captureNativeTermination = vi.fn(
    (targetRouter: Router, transportIds?: readonly string[]): NativeTerminationTarget => ({
      worker: worker as unknown as Worker,
      router: targetRouter,
      routerId: targetRouter.id,
      transportIds: transportIds === undefined ? undefined : [...transportIds],
    })
  );
  const confirmNativeTermination = vi.fn(async (target: NativeTerminationTarget) => {
    if (target.transportIds?.some((id) => nativeTransportIds.has(id))) {
      throw new Error('Controlled native transport remains present');
    }
  });
  const adapter = {
    getWebRtcServer: () => undefined,
    getOrCreateRouter: vi.fn().mockResolvedValue(router),
    getWebRtcServer: vi.fn().mockReturnValue({ closed: false }),
    removeRouter: vi.fn(),
    captureNativeTermination,
    confirmNativeTermination,
    confirmAllRoutersClosed: vi.fn().mockResolvedValue(undefined),
  };
  const manager = new RoomManager(adapter as unknown as MediasoupService);
  await manager.joinRoom(ROOM_ID, USER_ID, SOCKET_ID, { username: 'fixture-user' }, undefined, {
    roomContext: { roomKind: 'channel' },
    mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
    credentialEpoch: '11111111111111111111111111111111',
    voiceEnforcementSessionGeneration: SESSION_GENERATION,
  });
  expect(manager.getParticipant(ROOM_ID, USER_ID)).toMatchObject({
    socketId: SOCKET_ID,
    voiceEnforcementSessionGeneration: SESSION_GENERATION,
  });
  const terminalProof = () =>
    manager.awaitNativeTermination(ROOM_ID, USER_ID, SOCKET_ID, SESSION_GENERATION);
  const queriedTransportIds = (since = 0) =>
    confirmNativeTermination.mock.calls
      .slice(since)
      .flatMap(([target]) => target.transportIds ?? []);
  return {
    manager,
    router,
    transports,
    nativeTransportIds,
    unresolvedTransportIds,
    captureNativeTermination,
    confirmNativeTermination,
    terminalProof,
    queriedTransportIds,
  };
}

describe('receive-close proof retirement (#2624)', () => {
  // Repeated bounded receive-transport lifecycles for the same admitted session must retire
  // authoritatively closed transports so terminal verification does not replay a growing
  // history, while an unresolved close must still block completion.
  it('bounds completed receive history replay at terminal verification', async () => {
    const fixture = await admittedSession();
    const closedIds: string[] = [];
    // Only one transport is live at a time; this is six ordinary lifecycle operations.
    const cycles = MAX_RECV_TRANSPORTS_PER_PARTICIPANT + 2;
    for (let cycle = 0; cycle < cycles; cycle++) {
      const created = await fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv');
      const transport = fixture.transports[cycle];
      expect(created.id).toBe(transport.id);
      expect(fixture.manager.closeRecvTransport(ROOM_ID, USER_ID, created.id)).toBe(true);
      expect(transport.close).toHaveBeenCalledOnce();
      expect(transport.closed).toBe(true);
      expect(fixture.nativeTransportIds.has(created.id)).toBe(false);
      closedIds.push(created.id);
      await flushCloseContinuations();
    }
    expect(fixture.router.createWebRtcTransport).toHaveBeenCalledTimes(cycles);
    expect(new Set(closedIds).size).toBe(cycles);
    const beforeTerminal = fixture.confirmNativeTermination.mock.calls.length;
    await expect(fixture.terminalProof()).resolves.toBeUndefined();
    // Every close must reach native proof; silently dropping obligations is not retirement.
    expect(new Set(fixture.queriedTransportIds())).toEqual(new Set(closedIds));
    expect(
      fixture.queriedTransportIds(beforeTerminal).length,
      'bounded receive-close retention: terminal proof must not replay completed transport history beyond the live receive-transport bound'
    ).toBeLessThanOrEqual(MAX_RECV_TRANSPORTS_PER_PARTICIPANT);
  });

  it('positive control reaches native proof after an ordinary owned receive close', async () => {
    const fixture = await admittedSession();
    const created = await fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv');
    expect(fixture.manager.closeRecvTransport(ROOM_ID, USER_ID, created.id)).toBe(true);
    await flushCloseContinuations();
    await expect(fixture.terminalProof()).resolves.toBeUndefined();
    expect(fixture.transports[0].close).toHaveBeenCalledOnce();
    expect(fixture.captureNativeTermination).toHaveBeenCalledWith(fixture.router, [created.id]);
    expect(fixture.queriedTransportIds()).toContain(created.id);
    const afterProof = fixture.confirmNativeTermination.mock.calls.length;
    await fixture.terminalProof();
    expect(fixture.confirmNativeTermination).toHaveBeenCalledTimes(afterProof);
  });

  it('retains an unresolved native close through repeated terminal refusal', async () => {
    const fixture = await admittedSession();
    const created = await fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv');
    fixture.unresolvedTransportIds.add(created.id);
    expect(fixture.manager.closeRecvTransport(ROOM_ID, USER_ID, created.id)).toBe(true);
    await flushCloseContinuations();
    expect(fixture.transports[0].closed).toBe(true);
    expect(fixture.nativeTransportIds.has(created.id)).toBe(true);
    await expect(fixture.terminalProof()).rejects.toThrow('Native media termination unavailable');
    await expect(fixture.terminalProof()).rejects.toThrow('Native media termination unavailable');
    expect(fixture.queriedTransportIds()).toContain(created.id);
    // The same retained obligation must succeed once native absence is authoritative.
    fixture.nativeTransportIds.delete(created.id);
    await expect(fixture.terminalProof()).resolves.toBeUndefined();
    expect(
      fixture.queriedTransportIds().filter((id) => id === created.id).length
    ).toBeGreaterThanOrEqual(3);
  });
});

type ReceiveCloseFixture = Awaited<ReturnType<typeof admittedSession>>;

function delayedCompletion() {
  let complete = (): void => undefined;
  const promise = new Promise<void>((resolve) => {
    complete = resolve;
  });
  return { promise, complete: () => complete() };
}

function holdCloseProofs(fixture: ReceiveCloseFixture) {
  const nativeProof = fixture.confirmNativeTermination.getMockImplementation();
  if (!nativeProof) throw new Error('Fixture native proof adapter is missing');
  const held = new Map<string, ReturnType<typeof delayedCompletion>>();
  fixture.confirmNativeTermination.mockImplementation(async (target) => {
    for (const id of target.transportIds ?? []) await held.get(id)?.promise;
    await nativeProof(target);
  });
  return {
    hold(transportId: string) {
      const completion = delayedCompletion();
      held.set(transportId, completion);
      return completion;
    },
    completeAll() {
      for (const completion of held.values()) completion.complete();
    },
  };
}

async function expectReceiveCapacityRefusal(fixture: ReceiveCloseFixture): Promise<void> {
  const nativeCreates = fixture.router.createWebRtcTransport.mock.calls.length;
  await expect(
    fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv'),
    'receive capacity must include live transports, pending creates, and unproven native closes'
  ).rejects.toThrow(`Receive transport limit reached (max ${MAX_RECV_TRANSPORTS_PER_PARTICIPANT})`);
  expect(fixture.router.createWebRtcTransport).toHaveBeenCalledTimes(nativeCreates);
}

describe('receive capacity until authoritative close proof (#2624)', () => {
  it('keeps delayed close proofs in the bound and frees only the proven transport slot', async () => {
    const fixture = await admittedSession();
    const proofs = holdCloseProofs(fixture);
    const closed: { id: string; completion: ReturnType<typeof delayedCompletion> }[] = [];
    try {
      for (let index = 0; index < MAX_RECV_TRANSPORTS_PER_PARTICIPANT; index++) {
        const transport = await fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv');
        const completion = proofs.hold(transport.id);
        expect(fixture.manager.closeRecvTransport(ROOM_ID, USER_ID, transport.id)).toBe(true);
        expect(fixture.transports[index].close).toHaveBeenCalledOnce();
        expect(fixture.nativeTransportIds.has(transport.id)).toBe(false);
        closed.push({ id: transport.id, completion });
        await flushCloseContinuations();
      }
      expect(new Set(fixture.queriedTransportIds())).toEqual(new Set(closed.map(({ id }) => id)));
      await expectReceiveCapacityRefusal(fixture);

      closed[0].completion.complete();
      await flushCloseContinuations();
      const replacement = await fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv');
      expect(fixture.nativeTransportIds.has(replacement.id)).toBe(true);
      expect(closed.map(({ id }) => id)).not.toContain(replacement.id);
      expect(fixture.router.createWebRtcTransport).toHaveBeenCalledTimes(
        MAX_RECV_TRANSPORTS_PER_PARTICIPANT + 1
      );
      // One proven close permits exactly one replacement; the other three still occupy slots.
      await expectReceiveCapacityRefusal(fixture);

      proofs.completeAll();
      await flushCloseContinuations();
      const beforeTerminal = fixture.confirmNativeTermination.mock.calls.length;
      await expect(fixture.terminalProof()).resolves.toBeUndefined();
      expect(fixture.queriedTransportIds(beforeTerminal)).toEqual([]);
    } finally {
      proofs.completeAll();
      await flushCloseContinuations();
    }
  });

  it('preserves capacity and every native witness across repeated unresolved-close refusal', async () => {
    const fixture = await admittedSession();
    const closedIds: string[] = [];
    for (let index = 0; index < MAX_RECV_TRANSPORTS_PER_PARTICIPANT; index++) {
      const transport = await fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv');
      fixture.unresolvedTransportIds.add(transport.id);
      expect(fixture.manager.closeRecvTransport(ROOM_ID, USER_ID, transport.id)).toBe(true);
      expect(fixture.transports[index].closed).toBe(true);
      expect(fixture.nativeTransportIds.has(transport.id)).toBe(true);
      closedIds.push(transport.id);
      await flushCloseContinuations();
    }
    expect(new Set(fixture.queriedTransportIds())).toEqual(new Set(closedIds));
    await expectReceiveCapacityRefusal(fixture);
    await expect(fixture.terminalProof()).rejects.toThrow('Native media termination unavailable');
    await expect(fixture.terminalProof()).rejects.toThrow('Native media termination unavailable');
    await expectReceiveCapacityRefusal(fixture);

    const beforeConfirmedAbsence = fixture.confirmNativeTermination.mock.calls.length;
    for (const id of closedIds) fixture.nativeTransportIds.delete(id);
    await expect(fixture.terminalProof()).resolves.toBeUndefined();
    // Refusal must not discard any witness: the successful retry proves each exact closed ID.
    expect(new Set(fixture.queriedTransportIds(beforeConfirmedAbsence))).toEqual(
      new Set(closedIds)
    );
    const replacement = await fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv');
    expect(fixture.nativeTransportIds.has(replacement.id)).toBe(true);
    expect(fixture.router.createWebRtcTransport).toHaveBeenCalledTimes(
      MAX_RECV_TRANSPORTS_PER_PARTICIPANT + 1
    );
    const afterProof = fixture.confirmNativeTermination.mock.calls.length;
    await fixture.terminalProof();
    expect(fixture.queriedTransportIds(afterProof)).toEqual([]);
  });

  it('shares one bound across live transports, a pending create, and an unproven close', async () => {
    const fixture = await admittedSession();
    const proofs = holdCloseProofs(fixture);
    const creation = delayedCompletion();
    const nativeCreate = fixture.router.createWebRtcTransport.getMockImplementation();
    if (!nativeCreate) throw new Error('Fixture transport create adapter is missing');
    let pendingCreate: ReturnType<RoomManager['createTransport']> | undefined;
    try {
      const initiallyLive = [];
      for (let index = 0; index < MAX_RECV_TRANSPORTS_PER_PARTICIPANT - 1; index++) {
        initiallyLive.push(await fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv'));
      }
      const closed = initiallyLive[0];
      const closeProof = proofs.hold(closed.id);
      expect(fixture.manager.closeRecvTransport(ROOM_ID, USER_ID, closed.id)).toBe(true);
      await flushCloseContinuations();
      expect(fixture.queriedTransportIds()).toContain(closed.id);
      for (const transport of initiallyLive.slice(1)) {
        expect(fixture.nativeTransportIds.has(transport.id)).toBe(true);
      }

      fixture.router.createWebRtcTransport.mockImplementationOnce(async () => {
        await creation.promise;
        return nativeCreate();
      });
      pendingCreate = fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv');
      let pendingSettled = false;
      void pendingCreate.then(
        () => {
          pendingSettled = true;
        },
        () => {
          pendingSettled = true;
        }
      );
      await flushCloseContinuations();
      expect(fixture.router.createWebRtcTransport).toHaveBeenCalledTimes(
        MAX_RECV_TRANSPORTS_PER_PARTICIPANT
      );
      expect(pendingSettled).toBe(false);
      // Two live + one native create in flight + one unproven close consumes all four slots.
      await expectReceiveCapacityRefusal(fixture);

      closeProof.complete();
      await flushCloseContinuations();
      const replacement = await fixture.manager.createTransport(ROOM_ID, USER_ID, 'recv');
      expect(fixture.nativeTransportIds.has(replacement.id)).toBe(true);
      expect(pendingSettled).toBe(false);
      await expectReceiveCapacityRefusal(fixture);

      creation.complete();
      const completedCreate = await pendingCreate;
      expect(fixture.nativeTransportIds.has(completedCreate.id)).toBe(true);
      expect(pendingSettled).toBe(true);
      expect(
        new Set([...initiallyLive.slice(1).map(({ id }) => id), replacement.id, completedCreate.id])
          .size
      ).toBe(MAX_RECV_TRANSPORTS_PER_PARTICIPANT);
      await expectReceiveCapacityRefusal(fixture);
      const beforeTerminal = fixture.confirmNativeTermination.mock.calls.length;
      await fixture.terminalProof();
      expect(fixture.queriedTransportIds(beforeTerminal)).toEqual([]);
    } finally {
      proofs.completeAll();
      creation.complete();
      if (pendingCreate) await pendingCreate.catch(() => undefined);
      await flushCloseContinuations();
    }
  });
});
