import * as mediasoup from 'mediasoup';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import {
  NoWorkersAvailableError,
  RoomConsumerCountsUnavailableError,
  selectLeastLoadedWorkerIndex,
} from './workerSelection.js';
import type {
  Worker,
  Router,
  RouterRtpCodecCapability,
  WebRtcServer,
  TransportListenInfo,
} from 'mediasoup/types';
import type { EmitSecurityEvent } from './securityEvent.js';

/** Live consumer counts keyed by roomId, supplied by RoomManager (#3149). */
export type RoomConsumerCountsProvider = () => ReadonlyMap<string, number>;

/** Startup has no command owner yet; retain the existing native teardown bound. */
const STARTUP_NATIVE_CLEANUP_TIMEOUT_MS = 35_000;

/** A room's router plus the worker hosting it. */
type RouterAssignment = { router: Router; worker: Worker };

export interface NativeTerminationTarget {
  readonly worker: Worker;
  readonly router: Router;
  readonly routerId: string;
  readonly transportIds: readonly string[] | undefined;
}

interface NativeWorkerClosure {
  observed: boolean;
  closed: Promise<void>;
}

function nativeTerminationUnavailable(): Error {
  return new Error('Native media termination unavailable');
}

export async function awaitNativeDeadline<T>(
  operation: () => Promise<T>,
  deadlineMs: number
): Promise<T> {
  const remaining = deadlineMs - performance.now();
  if (!Number.isFinite(remaining) || remaining <= 0) throw nativeTerminationUnavailable();
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const deadline = new Promise<never>((_resolve, reject) => {
      timer = setTimeout(() => reject(nativeTerminationUnavailable()), remaining);
    });
    const result = await Promise.race([operation(), deadline]);
    if (performance.now() >= deadlineMs) throw nativeTerminationUnavailable();
    return result;
  } catch {
    throw nativeTerminationUnavailable();
  } finally {
    if (timer) clearTimeout(timer);
  }
}

export class MediasoupService {
  private readonly routerWorkers = new WeakMap<Router, Worker>();
  private readonly nativeWorkers = new Map<Worker, NativeWorkerClosure>();
  private readonly webRtcServers = new Map<Worker, WebRtcServer>();
  private initialized = false;
  private listenerFailed = false;
  private routerCreations = 0;
  private routerCreationVersion = 0;
  private closing = false;

  private trackNativeWorker(worker: Worker): NativeWorkerClosure {
    const existing = this.nativeWorkers.get(worker);
    if (existing) return existing;
    let resolve: () => void = () => undefined;
    const closure: NativeWorkerClosure = {
      observed: false,
      closed: new Promise<void>((complete) => {
        resolve = complete;
      }),
    };
    this.nativeWorkers.set(worker, closure);
    const closed = () => {
      closure.observed = true;
      resolve();
    };
    worker.once('subprocessclose', closed);
    if (worker.subprocessClosed) closed();
    return closure;
  }

  private workerStopped(worker: Worker): boolean {
    const closure = this.nativeWorkers.get(worker);
    if (!closure) throw nativeTerminationUnavailable();
    return closure.observed || worker.subprocessClosed;
  }

  captureNativeTermination(
    router: Router,
    transportIds?: readonly string[]
  ): NativeTerminationTarget {
    const worker = this.routerWorkers.get(router);
    if (
      !worker ||
      !this.nativeWorkers.has(worker) ||
      !router.id ||
      transportIds?.some((id) => typeof id !== 'string' || !id)
    )
      throw nativeTerminationUnavailable();
    return Object.freeze({
      worker,
      router,
      routerId: router.id,
      transportIds:
        transportIds === undefined ? undefined : Object.freeze([...new Set(transportIds)]),
    });
  }

  private async nativeRouterAbsent(
    target: NativeTerminationTarget,
    deadlineMs: number
  ): Promise<boolean> {
    if (this.workerStopped(target.worker)) return true;
    const dump = await awaitNativeDeadline(() => target.worker.dump(), deadlineMs);
    if (this.workerStopped(target.worker)) return true;
    if (dump.pid !== target.worker.pid || !Array.isArray(dump.routerIds))
      throw nativeTerminationUnavailable();
    return !dump.routerIds.includes(target.routerId);
  }

  async confirmNativeTermination(
    target: NativeTerminationTarget,
    deadlineMs: number
  ): Promise<void> {
    if (
      this.routerWorkers.get(target.router) !== target.worker ||
      target.router.id !== target.routerId
    ) {
      throw nativeTerminationUnavailable();
    }
    if (await this.nativeRouterAbsent(target, deadlineMs)) return;
    if (target.transportIds === undefined) throw nativeTerminationUnavailable();
    if (target.transportIds.length === 0) return;
    let dump;
    try {
      // Pinned Router.dump still issues a Worker-channel request after JS close.
      dump = await awaitNativeDeadline(() => target.router.dump(), deadlineMs);
    } catch {
      // A concurrent native router removal may reject its dump. Rejection alone
      // is never absence: obtain a fresh, successful worker proof instead.
      if (await this.nativeRouterAbsent(target, deadlineMs)) return;
      throw nativeTerminationUnavailable();
    }
    if (this.workerStopped(target.worker)) return;
    if (
      dump.id !== target.routerId ||
      !Array.isArray(dump.transportIds) ||
      target.transportIds.some((id) => dump.transportIds.includes(id))
    )
      throw nativeTerminationUnavailable();
  }

  async confirmAllRoutersClosed(deadlineMs: number): Promise<void> {
    const version = this.routerCreationVersion;
    if (this.routerCreations !== 0) throw nativeTerminationUnavailable();
    await Promise.all(
      this.workers.map(async (worker) => {
        if (this.workerStopped(worker)) return;
        const dump = await awaitNativeDeadline(() => worker.dump(), deadlineMs);
        if (this.workerStopped(worker)) return;
        if (
          dump.pid !== worker.pid ||
          !Array.isArray(dump.routerIds) ||
          dump.routerIds.length !== 0
        ) {
          throw nativeTerminationUnavailable();
        }
      })
    );
    // Catch a router created on a worker whose earlier dump was already empty.
    if (this.routerCreations !== 0 || this.routerCreationVersion !== version)
      throw nativeTerminationUnavailable();
  }

  async close(deadlineMs: number): Promise<void> {
    this.closing = true;
    this.initialized = false;
    await this.enqueueWorkerOperation(async () => {
      const workers = [...this.workers];
      const closures = workers.map((worker) => this.trackNativeWorker(worker));
      logger.info('Closing mediasoup service');
      // Every subprocess listener is installed before the first close request.
      for (const server of this.webRtcServers.values()) {
        try {
          if (!server.closed) server.close();
        } catch {
          logger.error('Mediasoup shared listener close failed');
        }
      }
      for (const worker of workers) {
        try {
          if (!worker.closed) worker.close();
        } catch {
          logger.error('Mediasoup worker close failed');
        }
      }
      await awaitNativeDeadline(
        () => Promise.all(closures.map((closure) => closure.closed)),
        deadlineMs
      );
      if (closures.some((closure) => !closure.observed)) throw nativeTerminationUnavailable();
      this.workers = [];
      this.webRtcServers.clear();
      this.workerRouters.clear();
      this.workerPorts.clear();
      this.routers.clear();
    });
  }

  private workers: Worker[] = [];
  // Keep ports allocated until subprocessclose, after the old listener is gone.
  private readonly workerPorts = new Map<Worker, number>();
  private readonly workerRouters = new Map<Worker, Set<Router>>();
  private workerOperations: Promise<void> = Promise.resolve();
  private readonly routers: Map<string, RouterAssignment> = new Map();
  private roomConsumerCounts: RoomConsumerCountsProvider | null = null;
  private emitSecurityEvent: EmitSecurityEvent | undefined;
  private flushSecurityEvents: (() => void) | undefined;

  setSecurityEventEmitter(emit: EmitSecurityEvent | undefined, flush?: () => void): void {
    this.emitSecurityEvent = emit;
    this.flushSecurityEvents = flush;
  }

  async init() {
    if (this.closing || this.initialized || this.workers.length !== 0) {
      throw nativeTerminationUnavailable();
    }
    const initialWorkers = config.mediasoup.initialWorkers ?? config.mediasoup.numWorkers;
    logger.info('Initializing mediasoup workers', {
      initialWorkers,
      maxWorkers: config.mediasoup.numWorkers,
    });

    try {
      for (let i = 0; i < initialWorkers; i++) await this.addWorkerInternal();
      this.initialized = true;
      if (!this.isReady()) throw nativeTerminationUnavailable();
    } catch (error) {
      await this.close(performance.now() + STARTUP_NATIVE_CLEANUP_TIMEOUT_MS);
      throw error;
    }
  }

  private enqueueWorkerOperation<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.workerOperations.then(operation);
    this.workerOperations = result.then(
      () => undefined,
      () => undefined
    );
    return result;
  }

  private nextRtcPort(): number {
    const used = new Set(this.workerPorts.values());
    for (let port = config.rtc.minPort; port <= config.rtc.maxPort; port++) {
      if (!used.has(port)) return port;
    }
    throw new Error('No free mediasoup WebRtcServer ports remain');
  }

  private async addWorkerInternal(): Promise<void> {
    if (this.closing || this.listenerFailed) throw nativeTerminationUnavailable();
    if (this.workers.length >= config.mediasoup.numWorkers)
      throw new Error('Mediasoup worker pool is already at its configured maximum');
    const startup = !this.initialized;
    const port = config.mediaInstanceColor ? this.nextRtcPort() : undefined;
    const worker = await mediasoup.createWorker({
      logLevel: config.mediasoup.worker.logLevel,
      logTags: config.mediasoup.worker.logTags,
      rtcMinPort: config.mediasoup.worker.rtcMinPort,
      rtcMaxPort: config.mediasoup.worker.rtcMaxPort,
    });
    const closure = this.trackNativeWorker(worker);
    if (port !== undefined) {
      this.workerPorts.set(worker, port);
      worker.once('subprocessclose', () => this.workerPorts.delete(worker));
    }
    this.installWorkerDeathHandler(worker);

    try {
      if (config.mediaInstanceColor || config.mediasoup.webRtcServer.enabled) {
        await this.createWebRtcServer(worker, this.workers.length, port);
      }
      if (this.closing || this.listenerFailed) throw nativeTerminationUnavailable();
      logger.debug('Mediasoup worker created', {
        pid: worker.pid,
        index: this.workers.length,
      });
      this.workers.push(worker);
      this.workerRouters.set(worker, new Set());
    } catch (error) {
      await this.cleanupFailedWorkerSetup(worker, closure, startup);
      throw error;
    }
  }

  private installWorkerDeathHandler(worker: Worker): void {
    worker.on('died', () => {
      this.listenerFailed = true;
      try {
        this.emitSecurityEvent?.({
          eventType: 'dependency',
          outcome: 'failure',
          severity: 'critical',
          reasonCode: 'dependency_unavailable',
        });
      } catch {
        // An observer must not change the mandatory worker-death exit path.
      }
      try {
        this.flushSecurityEvents?.();
      } catch {
        // Best effort; exit remains authoritative after a worker death.
      }
      logger.error('Mediasoup worker died');
      process.exit(1);
    });
  }

  private async cleanupFailedWorkerSetup(
    worker: Worker,
    closure: NativeWorkerClosure,
    startup: boolean
  ): Promise<void> {
    const server = this.webRtcServers.get(worker);
    this.webRtcServers.delete(worker);
    try {
      if (server && !server.closed) server.close();
    } catch {
      logger.error('Mediasoup shared listener close failed after setup error');
    }
    try {
      if (!worker.closed) worker.close();
    } catch {
      logger.error('Mediasoup worker close failed after listener setup error');
    }
    try {
      await awaitNativeDeadline(
        () => closure.closed,
        performance.now() + (startup ? STARTUP_NATIVE_CLEANUP_TIMEOUT_MS : 5_000)
      );
      if (!closure.observed) throw nativeTerminationUnavailable();
      this.nativeWorkers.delete(worker);
    } catch {
      this.listenerFailed = true;
      throw nativeTerminationUnavailable();
    }
  }

  addWorker(): Promise<void> {
    return this.enqueueWorkerOperation(() => this.addWorkerInternal());
  }

  removeWorker(worker: Worker): Promise<void> {
    return this.enqueueWorkerOperation(async () => {
      if (this.closing || this.routerCreations !== 0) throw nativeTerminationUnavailable();
      if (this.workers.length <= config.mediasoup.initialWorkers)
        throw new Error('Cannot demote the mediasoup worker pool below its initial count');
      const index = this.workers.indexOf(worker);
      const routers = this.workerRouters.get(worker);
      if (index < 0 || !routers || [...routers].some((router) => !router.closed))
        throw new Error('Cannot demote a worker while it owns a live router');

      for (const router of routers) {
        for (const [roomId, assignment] of this.routers) {
          if (assignment.router === router) this.routers.delete(roomId);
        }
      }
      const closure = this.trackNativeWorker(worker);
      const server = this.webRtcServers.get(worker);
      // This listener close is intentional; only an unexpected close latches failure.
      this.webRtcServers.delete(worker);
      // Retire the idle worker from admission before native shutdown can block.
      this.workers.splice(index, 1);
      this.workerRouters.delete(worker);
      await this.closeDemotedWorker(worker, server, closure);
    });
  }

  private async closeDemotedWorker(
    worker: Worker,
    server: WebRtcServer | undefined,
    closure: NativeWorkerClosure
  ): Promise<void> {
    try {
      if (server && !server.closed) server.close();
    } catch {
      logger.error('Mediasoup shared listener close failed during worker demotion');
    }
    try {
      if (!worker.closed) worker.close();
    } catch {
      logger.error('Mediasoup worker close failed during demotion');
    }
    try {
      await awaitNativeDeadline(() => closure.closed, performance.now() + 5_000);
      if (!closure.observed) throw nativeTerminationUnavailable();
    } catch (error) {
      this.listenerFailed = true;
      throw error;
    }
  }

  /** Drain idle workers without touching any worker that still owns a room. */
  async demoteIdleWorkersToInitial(): Promise<number> {
    let removed = 0;
    for (const worker of [...this.workers].reverse()) {
      if (this.workers.length <= config.mediasoup.initialWorkers) break;
      const routers = this.workerRouters.get(worker);
      if (!routers || [...routers].some((router) => !router.closed)) continue;
      try {
        await this.removeWorker(worker);
        removed += 1;
      } catch {
        // A router creation or worker closure may race the snapshot. The next
        // drain heartbeat retries; removal itself rechecks both invariants.
        break;
      }
    }
    return removed;
  }

  private promoteIfSaturated(): Promise<void> {
    return this.enqueueWorkerOperation(async () => {
      if (
        this.closing ||
        this.workers.length >= config.mediasoup.numWorkers ||
        this.workers.length === 0 ||
        this.workers.some((worker) => {
          const routers = this.workerRouters.get(worker);
          return !routers || ![...routers].some((router) => !router.closed);
        })
      )
        return;
      await this.addWorkerInternal();
    });
  }

  private async createWebRtcServer(
    worker: Worker,
    index: number,
    colorPort?: number
  ): Promise<void> {
    const shared = config.mediasoup.webRtcServer;
    const port = config.mediaInstanceColor ? colorPort : shared.ports[index];
    if (port === undefined || !Number.isInteger(port)) throw nativeTerminationUnavailable();
    const ip = config.mediaInstanceColor ? '0.0.0.0' : shared.listenIp;
    const announcedAddress = config.mediaInstanceColor
      ? config.rtc.announcedIp
      : shared.announcedAddress;
    const listenInfos: TransportListenInfo[] = [];
    if (!config.mediaInstanceColor || config.mediasoup.webRtcTransport.enableUdp) {
      listenInfos.push({ protocol: 'udp', ip, announcedAddress, port });
    }
    if (config.mediasoup.webRtcTransport.enableTcp) {
      listenInfos.push({ protocol: 'tcp', ip, announcedAddress, port });
    }
    if (listenInfos.length === 0)
      throw new Error('At least one WebRTC listener protocol is required');
    const server = await worker.createWebRtcServer({ listenInfos });
    this.webRtcServers.set(worker, server);
    server.observer.once('close', () => {
      if (this.closing || this.webRtcServers.get(worker) !== server) return;
      this.listenerFailed = true;
      logger.error('Mediasoup shared listener closed');
    });
    if (this.closing || this.listenerFailed || worker.closed || server.closed) {
      throw nativeTerminationUnavailable();
    }
  }

  isReady(): boolean {
    const initialWorkers = config.mediasoup.initialWorkers ?? config.mediasoup.numWorkers;
    if (
      !this.initialized ||
      this.closing ||
      this.listenerFailed ||
      this.workers.length < initialWorkers ||
      this.workers.length > config.mediasoup.numWorkers
    )
      return false;
    return this.workers.every((worker) => {
      if (worker.closed || this.workerStopped(worker)) return false;
      return (
        !(config.mediaInstanceColor || config.mediasoup.webRtcServer.enabled) ||
        this.webRtcServers.get(worker)?.closed === false
      );
    });
  }

  /** Resolve by the captured router owner, independently of the room cache. */
  getWebRtcServer(router: Router): WebRtcServer | undefined {
    if (!(config.mediaInstanceColor || config.mediasoup.webRtcServer.enabled)) return undefined;
    const worker = this.routerWorkers.get(router);
    const server = worker && this.webRtcServers.get(worker);
    if (!this.isReady() || router.closed || !server || server.closed) {
      throw nativeTerminationUnavailable();
    }
    return server;
  }

  /**
   * Install the per-room consumer-count provider (#3149).
   *
   * Wired after construction rather than injected: index.ts builds
   * MediasoupService before RoomManager, and roomManager.ts already imports
   * this module, so an import in the other direction would be a cycle.
   */
  setRoomConsumerCounts(provider: RoomConsumerCountsProvider): void {
    this.roomConsumerCounts = provider;
  }

  /**
   * Current consumer load, and assigned-router count, per worker index.
   *
   * Returned together because both are one pass over `this.routers` and the
   * selector needs both: load is the primary key, rooms break its ties.
   *
   * `?? 0` is load-bearing rather than defensive: `roomManager.ts`
   * deletes a room with a closed router from `this.rooms` WITHOUT calling
   * `removeRouter`, so this map legitimately holds roomIds the provider no
   * longer reports.
   */
  private loadByWorkerIndex(): { load: number[]; rooms: number[] } {
    if (!this.roomConsumerCounts) {
      throw new RoomConsumerCountsUnavailableError();
    }

    const perRoom = this.roomConsumerCounts();
    const load = new Array<number>(this.workers.length).fill(0);
    const rooms = new Array<number>(this.workers.length).fill(0);

    for (const [roomId, assignment] of this.routers) {
      const workerIndex = this.workers.indexOf(assignment.worker);
      if (workerIndex < 0 || assignment.router.closed) continue;
      load[workerIndex] += perRoom.get(roomId) ?? 0;
      rooms[workerIndex] += 1;
    }

    return { load, rooms };
  }

  async getOrCreateRouter(roomId: string): Promise<Router> {
    if (this.workers.length === 0) throw new NoWorkersAvailableError();
    if (!this.isReady()) {
      throw nativeTerminationUnavailable();
    }
    const existing = this.routers.get(roomId);

    if (existing && !existing.router.closed) {
      return existing.router;
    }

    this.routerCreations++;
    this.routerCreationVersion++;
    try {
      if (existing) this.removeRouter(roomId);
      if (
        this.workers.length > 0 &&
        this.workers.every((worker) => {
          const routers = this.workerRouters.get(worker);
          return Boolean(routers && [...routers].some((router) => !router.closed));
        })
      )
        await this.promoteIfSaturated();
      const { load, rooms } = this.loadByWorkerIndex();
      const workerIndex = selectLeastLoadedWorkerIndex(this.workers.length, load, rooms);
      if (!this.isReady()) throw nativeTerminationUnavailable();
      const worker = this.workers[workerIndex];
      const mediaCodecs: RouterRtpCodecCapability[] = config.mediasoup.router.mediaCodecs;
      const router = await worker.createRouter({
        mediaCodecs,
      });
      this.routerWorkers.set(router, worker);
      if (!this.isReady()) {
        router.close();
        throw nativeTerminationUnavailable();
      }
      this.routers.set(roomId, { router, worker });
      this.workerRouters.get(worker)?.add(router);
      logger.info('Created media router', { workerIndex });
      return router;
    } finally {
      this.routerCreations--;
      this.routerCreationVersion++;
    }
  }

  /** Remove a cached router (called when a room is destroyed) */
  removeRouter(roomId: string): void {
    const assignment = this.routers.get(roomId);
    if (!assignment) return;
    this.routers.delete(roomId);
    this.workerRouters.get(assignment.worker)?.delete(assignment.router);
  }

  getWorkers(): readonly Worker[] {
    return [...this.workers];
  }

  getWorkerCount(): number {
    return this.workers.length;
  }
}
