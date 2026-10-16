import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { EventEmitter } from 'node:events';
import { existsSync, readFileSync } from 'node:fs';
import type { Router, Worker } from 'mediasoup/types';
import { createMockRouter, createMockTransport, createMockWorker } from './mocks/mediasoup.js';
import './mocks/logger.js';

const { mockCreateWorker } = vi.hoisted(() => ({ mockCreateWorker: vi.fn() }));
vi.mock('mediasoup', () => ({ createWorker: mockCreateWorker }));
vi.mock('@/config/index.js', () => ({
  config: {
    freeVideoPublisherCap: 8,
    freeScreenProducerCap: 1,
    freeAudioLastN: 8,
    audioLastNHoldMs: 2500,
    audioLevelObserver: { maxEntries: 1, threshold: -60, interval: 300 },
    mediasoup: {
      webRtcServer: { enabled: false, ports: [], listenIp: '0.0.0.0', announcedAddress: '127.0.0.1' },
      numWorkers: 2,
      worker: { logLevel: 'error', logTags: [], rtcMinPort: 40000, rtcMaxPort: 49999 },
      router: {
        mediaCodecs: [{ kind: 'audio', mimeType: 'audio/opus', clockRate: 48000, channels: 2 }],
      },
      webRtcTransport: {
        listenIps: [{ ip: '127.0.0.1' }],
        enableUdp: true,
        enableTcp: false,
        maxIncomingBitrate: 50_000_000,
      },
    },
  },
}));

import { MediasoupService } from '../src/lib/mediasoup.js';
import { RoomManager, SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION } from '../src/lib/roomManager.js';

// The amendment fixes these required ports before implementation. These types
// describe that contract; there is deliberately no test-only production fallback.
interface NativeTarget {
  readonly worker: Worker;
  readonly router: Router;
  readonly routerId: string;
  readonly transportIds: readonly string[] | undefined;
}
type ProofService = MediasoupService & {
  captureNativeTermination(router: Router, transportIds?: readonly string[]): NativeTarget;
  confirmNativeTermination(target: NativeTarget, deadlineMs: number): Promise<void>;
  confirmAllRoutersClosed(deadlineMs: number): Promise<void>;
  close(deadlineMs: number): Promise<void>;
};
type ProofRoomManager = RoomManager & {
  awaitNativeTermination(
    roomId: string,
    userId: string,
    socketId?: string,
    sessionGeneration?: string
  ): Promise<void>;
};

function observe<T>(promise: Promise<T>) {
  const result = { status: 'pending' as 'pending' | 'fulfilled' | 'rejected', promise };
  void promise.then(
    () => { result.status = 'fulfilled'; },
    () => { result.status = 'rejected'; }
  );
  return result;
}

async function flush() {
  await new Promise<void>((resolve) => setImmediate(resolve));
}

function fakeWorker(pid: number) {
  const events = new EventEmitter();
  const nativeRouters = new Map<string, ReturnType<typeof fakeRouter>>();
  const worker = Object.assign(createMockWorker({ pid }), {
    subprocessClosed: false,
    observer: new EventEmitter(),
    on: events.on.bind(events),
    once: events.once.bind(events),
    off: events.off.bind(events),
    dump: vi.fn(async () => ({
      pid,
      webRtcServerIds: [],
      routerIds: [...nativeRouters.keys()],
      channelMessageHandlers: { channelRequestHandlers: [], channelNotificationHandlers: [] },
    })),
    nativeRouters,
    completeSubprocessClose() {
      worker.subprocessClosed = true;
      nativeRouters.clear();
      events.emit('subprocessclose');
    },
  });
  worker.createRouter.mockImplementation(async () => {
    const router = fakeRouter(`router-${pid}-${nativeRouters.size}`, nativeRouters);
    nativeRouters.set(router.id, router);
    return router;
  });
  worker.close.mockImplementation(() => {
    worker.closed = true;
    worker.observer.emit('close');
  });
  return worker;
}

function fakeRouter(id: string, nativeRouters: Map<string, unknown>) {
  const nativeTransportIds = new Set<string>();
  let nextTransport = 0;
  const router = Object.assign(createMockRouter({ id }), {
    observer: new EventEmitter(),
    nativeTransportIds,
    dump: vi.fn(async () => {
      if (!nativeRouters.has(id)) throw new Error('native router absent');
      return {
        id,
        transportIds: [...nativeTransportIds],
        rtpObserverIds: [],
        mapProducerIdConsumerIds: [],
        mapConsumerIdProducerId: [],
        mapProducerIdObserverIds: [],
        mapDataProducerIdDataConsumerIds: [],
        mapDataConsumerIdDataProducerId: [],
      };
    }),
  });
  router.close.mockImplementation(() => {
    router.closed = true;
    router.observer.emit('close');
  });
  router.createWebRtcTransport.mockImplementation(async () => {
    const transport = createMockTransport({ id: `${id}-transport-${nextTransport++}` });
    nativeTransportIds.add(transport.id);
    transport.close.mockImplementation(() => { transport.closed = true; });
    return transport;
  });
  return router;
}

async function fakeService() {
  const workers = [fakeWorker(2001), fakeWorker(2002)];
  workers.forEach((worker) => mockCreateWorker.mockResolvedValueOnce(worker));
  const service = new MediasoupService() as ProofService;
  service.setRoomConsumerCounts(() => new Map());
  await service.init();
  return { service, workers };
}

async function rejectsByDeadline(promise: Promise<void>, elapsedMs = 40) {
  observe(promise);
  await vi.advanceTimersByTimeAsync(elapsedMs);
  await expect(promise).rejects.toThrow();
}

describe('native terminalization with controlled completion', () => {
  beforeEach(() => {
    mockCreateWorker.mockReset();
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'performance'] });
  });
  afterEach(() => { vi.useRealTimers(); });

  it('waits for subprocessclose after Worker.close only marks local closed', async () => {
    const { service, workers } = await fakeService();
    const closing = observe(service.close(performance.now() + 1000));
    await flush();
    expect(workers.every((worker) => worker.closed)).toBe(true);
    expect(workers.every((worker) => !worker.subprocessClosed)).toBe(true);
    expect(closing.status).toBe('pending');
    workers[0].completeSubprocessClose();
    await flush();
    expect(closing.status).toBe('pending');
    workers[1].completeSubprocessClose();
    await closing.promise;
    expect(closing.status).toBe('fulfilled');
  });

  it('accepts an already closed subprocess as the shutdown positive control', async () => {
    const { service, workers } = await fakeService();
    workers.forEach((worker) => worker.completeSubprocessClose());
    await expect(service.close(performance.now() + 25)).resolves.toBeUndefined();
    expect(workers.every((worker) => worker.subprocessClosed)).toBe(true);
  });

  it('rejects shutdown at the shared deadline when subprocess closure is unconfirmed', async () => {
    const { service, workers } = await fakeService();
    await rejectsByDeadline(service.close(performance.now() + 25));
    expect(workers.every((worker) => worker.closed && !worker.subprocessClosed)).toBe(true);
  });

  it('requires native transport absence despite local transport and router closed flags', async () => {
    const { service, workers } = await fakeService();
    const router = await service.getOrCreateRouter('room-target');
    const transport = await router.createWebRtcTransport({ listenIps: [{ ip: '127.0.0.1' }] });
    const target = service.captureNativeTermination(router, [transport.id]);
    transport.close();
    router.close();
    service.removeRouter('room-target');
    await rejectsByDeadline(service.confirmNativeTermination(target, performance.now() + 25));
    expect(transport.closed).toBe(true);
    expect(router.closed).toBe(true);
    expect((await workers[0].dump()).routerIds).toContain(router.id);
    expect((await router.dump()).transportIds).toContain(transport.id);
    workers[0].nativeRouters.delete(router.id);
    await expect(service.confirmNativeTermination(target, performance.now() + 25)).resolves.toBeUndefined();
  });

  it.each(['worker', 'router'] as const)('does not mistake a %s dump rejection for native absence', async (failedDump) => {
    const { service, workers } = await fakeService();
    const router = await service.getOrCreateRouter('room-dump-error');
    const target = service.captureNativeTermination(router, ['old-transport']);
    const nativeRouter = workers[0].nativeRouters.get(router.id)!;
    nativeRouter.nativeTransportIds.add('old-transport');
    if (failedDump === 'worker') workers[0].dump.mockRejectedValue(new Error('worker IPC failed'));
    else nativeRouter.dump.mockRejectedValue(new Error('router IPC failed'));
    await rejectsByDeadline(service.confirmNativeTermination(target, performance.now() + 25));
    expect(workers[0].subprocessClosed).toBe(false);
  });

  it('rechecks worker absence when router dump races native router removal', async () => {
    const { service, workers } = await fakeService();
    const router = await service.getOrCreateRouter('room-race');
    const target = service.captureNativeTermination(router, ['old-transport']);
    const nativeRouter = workers[0].nativeRouters.get(router.id)!;
    nativeRouter.dump.mockImplementation(async () => {
      workers[0].nativeRouters.delete(router.id);
      throw new Error('native router disappeared during dump');
    });
    await expect(service.confirmNativeTermination(target, performance.now() + 25)).resolves.toBeUndefined();
    expect(workers[0].dump.mock.calls.length).toBeGreaterThanOrEqual(2);
    expect((await workers[0].dump()).routerIds).not.toContain(router.id);
  });

  it('accepts an empty transport target without closing its live router', async () => {
    const { service, workers } = await fakeService();
    const router = await service.getOrCreateRouter('room-no-media');
    await expect(service.confirmNativeTermination(service.captureNativeTermination(router, []), performance.now() + 25)).resolves.toBeUndefined();
    expect(router.closed).toBe(false);
    expect(workers[0].closed).toBe(false);
  });

  it('checks every worker for native routers absent from the JavaScript room cache', async () => {
    const { service, workers } = await fakeService();
    const untracked = await workers[1].createRouter();
    await rejectsByDeadline(service.confirmAllRoutersClosed(performance.now() + 25));
    expect(workers[0].dump).toHaveBeenCalled();
    expect(workers[1].dump).toHaveBeenCalled();
    workers[1].nativeRouters.delete(untracked.id);
    await expect(service.confirmAllRoutersClosed(performance.now() + 25)).resolves.toBeUndefined();
  });

  it('blocks RoomManager closeAll when a native router survives outside its empty room map', async () => {
    const { service, workers } = await fakeService();
    const manager = new RoomManager(service);
    const untracked = await workers[1].createRouter();
    await rejectsByDeadline(manager.closeAll(), 40_000);
    expect((await workers[1].dump()).routerIds).toContain(untracked.id);
    workers[1].nativeRouters.delete(untracked.id);
    await expect(manager.closeAll()).resolves.toBeUndefined();
  });

  it('retains failed native proof after local deletion and leaves an exact successor untouched', async () => {
    const { service, workers } = await fakeService();
    const manager = new RoomManager(service) as ProofRoomManager;
    const join = (userId: string, socketId: string, generation: string) => manager.joinRoom(
      'room-exact', userId, socketId, { username: userId }, undefined,
      { mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION, roomContext: { roomKind: 'channel' }, voiceEnforcementSessionGeneration: generation }
    );
    await join('target', 'old-socket', 'old-generation');
    await join('survivor', 'survivor-socket', 'survivor-generation');
    const send = await manager.createTransport('room-exact', 'target', 'send');
    const recv = await manager.createTransport('room-exact', 'target', 'recv');
    const leave = observe(manager.leaveRoom('room-exact', 'target'));
    await flush();
    expect(manager.getParticipant('room-exact', 'target')).toBeUndefined();
    const firstProof = manager.awaitNativeTermination('room-exact', 'target', 'old-socket', 'old-generation');
    await rejectsByDeadline(firstProof, 40_000);
    await join('target', 'new-socket', 'new-generation');
    const successor = await manager.createTransport('room-exact', 'target', 'send');
    await rejectsByDeadline(manager.awaitNativeTermination('room-exact', 'target', 'old-socket', 'old-generation'), 40_000);
    const nativeRouter = workers[0].nativeRouters.values().next().value!;
    expect(nativeRouter.nativeTransportIds).toContain(send.id);
    expect(nativeRouter.nativeTransportIds).toContain(recv.id);
    expect(nativeRouter.nativeTransportIds).toContain(successor.id);
    nativeRouter.nativeTransportIds.delete(send.id);
    nativeRouter.nativeTransportIds.delete(recv.id);
    await expect(manager.awaitNativeTermination('room-exact', 'target', 'old-socket', 'old-generation')).resolves.toBeUndefined();
    expect(manager.getParticipant('room-exact', 'target')?.socketId).toBe('new-socket');
    expect(manager.getParticipant('room-exact', 'survivor')?.socketId).toBe('survivor-socket');
    expect(nativeRouter.nativeTransportIds).toContain(successor.id);
    expect(leave.status).not.toBe('pending');
  });
});

const ownedWorkers: Worker[] = [];
function subprocessClosed(worker: Worker): Promise<void> {
  if (worker.subprocessClosed) return Promise.resolve();
  return new Promise((resolve) => worker.once('subprocessclose', resolve));
}
async function bounded<T>(promise: Promise<T>, timeoutMs: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([promise, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error('owned native worker cleanup deadline')), timeoutMs);
    })]);
  } finally {
    clearTimeout(timer);
  }
}
async function cleanupOwnedWorkers() {
  for (const worker of ownedWorkers.splice(0)) {
    if (worker.subprocessClosed) continue;
    try { process.kill(worker.pid, 'SIGCONT'); } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ESRCH') throw error;
    }
    const closed = subprocessClosed(worker);
    worker.close();
    try { await bounded(closed, 2000); } catch {
      try { process.kill(worker.pid, 'SIGKILL'); } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== 'ESRCH') throw error;
      }
      await bounded(closed, 1000);
    }
  }
}

async function nativeService() {
  const actual = await vi.importActual<typeof import('mediasoup')>('mediasoup');
  const workerBin = process.env.MEDIASOUP_WORKER_BIN ?? actual.workerBin;
  expect(existsSync(workerBin), `required mediasoup worker binary: ${workerBin}`).toBe(true);
  mockCreateWorker.mockImplementation(async (settings) => {
    const worker = await actual.createWorker({ ...settings, workerBin });
    // Cleanup owns exactly the subprocesses created here, registered before any SIGSTOP.
    ownedWorkers.push(worker);
    return worker;
  });
  const service = new MediasoupService() as ProofService;
  service.setRoomConsumerCounts(() => new Map());
  await service.init();
  return { service, workers: [...ownedWorkers] };
}

async function stopOwnedWorker(worker: Worker) {
  expect(ownedWorkers).toContain(worker);
  process.kill(worker.pid, 'SIGSTOP');
  const deadline = performance.now() + 1000;
  while (!/\nState:\s+T/.test(readFileSync(`/proc/${worker.pid}/status`, 'utf8'))) {
    if (performance.now() > deadline) throw new Error('owned worker did not stop');
    await flush();
  }
}

describe('native terminalization with the pinned mediasoup worker', () => {
  beforeEach(() => { mockCreateWorker.mockReset(); });
  afterEach(cleanupOwnedWorkers);

  it('proves target transport absence while a shared-router survivor remains native', async () => {
    const { service, workers } = await nativeService();
    const router = await service.getOrCreateRouter('native-shared');
    const targetTransport = await router.createWebRtcTransport({ listenInfos: [{ protocol: 'udp', ip: '127.0.0.1' }] });
    const survivor = await router.createWebRtcTransport({ listenInfos: [{ protocol: 'udp', ip: '127.0.0.1' }] });
    const target = service.captureNativeTermination(router, [targetTransport.id]);
    expect((await router.dump()).transportIds).toEqual(expect.arrayContaining([targetTransport.id, survivor.id]));
    targetTransport.close();
    await service.confirmNativeTermination(target, performance.now() + 2000);
    expect((await router.dump()).transportIds).not.toContain(targetTransport.id);
    expect((await router.dump()).transportIds).toContain(survivor.id);
    expect((await workers[0].dump()).routerIds).toContain(router.id);
    expect(survivor.closed).toBe(false);
  });

  it('proves native router absence after the last participant leaves during a dump', async () => {
    const { service, workers } = await nativeService();
    const manager = new RoomManager(service) as ProofRoomManager;
    await manager.joinRoom('native-last', 'last-user', 'last-socket', { username: 'last-user' }, undefined, {
      mediaFrameCryptoVersion: SUPPORTED_MEDIA_FRAME_CRYPTO_VERSION,
      roomContext: { roomKind: 'channel' },
      voiceEnforcementSessionGeneration: 'last-generation',
    });
    const router = await service.getOrCreateRouter('native-last');
    const target = service.captureNativeTermination(router);
    const racingDump = router.dump();
    const leave = observe(manager.leaveRoom('native-last', 'last-user'));
    await flush();
    await racingDump.catch(() => undefined);
    expect(manager.getParticipant('native-last', 'last-user')).toBeUndefined();
    expect(router.closed).toBe(true);
    await manager.awaitNativeTermination('native-last', 'last-user', 'last-socket', 'last-generation');
    await service.confirmNativeTermination(target, performance.now() + 2000);
    await leave.promise;
    expect((await workers[0].dump()).routerIds).not.toContain(router.id);
  });

  it('does not complete native shutdown before SIGCONT and actual subprocessclose', async () => {
    const { service, workers } = await nativeService();
    const stopped = workers[0];
    try {
      await stopOwnedWorker(stopped);
      const closing = observe(service.close(performance.now() + 2000));
      await flush();
      expect(stopped.closed).toBe(true);
      expect(stopped.subprocessClosed).toBe(false);
      expect(closing.status).toBe('pending');
      process.kill(stopped.pid, 'SIGCONT');
      await bounded(closing.promise, 2500);
      expect(stopped.subprocessClosed).toBe(true);
      expect(workers.every((worker) => worker.subprocessClosed)).toBe(true);
    } finally {
      if (!stopped.subprocessClosed) process.kill(stopped.pid, 'SIGCONT');
    }
  });

  it('refuses native shutdown proof when a stopped worker reaches the deadline', async () => {
    const { service, workers } = await nativeService();
    const stopped = workers[0];
    try {
      await stopOwnedWorker(stopped);
      await expect(service.close(performance.now() + 25)).rejects.toThrow();
      expect(stopped.closed).toBe(true);
      expect(stopped.subprocessClosed).toBe(false);
    } finally {
      if (!stopped.subprocessClosed) process.kill(stopped.pid, 'SIGCONT');
    }
  });
});
