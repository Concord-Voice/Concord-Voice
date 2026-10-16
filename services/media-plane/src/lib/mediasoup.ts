import * as mediasoup from 'mediasoup';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import {
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

/** A room's router plus the index of the worker hosting it. */
type RouterAssignment = { router: Router; workerIndex: number };

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
    const workers = [...this.workers];
    const closures = workers.map((worker) => this.trackNativeWorker(worker));
    logger.info('Closing mediasoup service');
    // Every subprocess listener is installed before the first close request.
    for (const server of this.webRtcServers.values()) {
      try {
        if (!server.closed) server.close();
      } catch {
        // Worker closure still has to run for every captured subprocess.
        logger.error('Mediasoup shared listener close failed');
      }
    }
    for (const worker of workers) {
      try {
        worker.close();
      } catch {
        // A thrown close request is not proof of native termination.
        logger.error('Mediasoup worker close failed');
      }
    }
    await awaitNativeDeadline(
      () => Promise.all(closures.map((closure) => closure.closed)),
      deadlineMs
    );
    if (closures.some((closure) => !closure.observed)) throw nativeTerminationUnavailable();
    this.workers = [];
    this.routers.clear();
    this.webRtcServers.clear();
  }

  private workers: Worker[] = [];
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
    logger.info('Initializing mediasoup workers', {
      numWorkers: config.mediasoup.numWorkers,
    });

    try {
      for (let i = 0; i < config.mediasoup.numWorkers; i++) {
        const logTags: mediasoup.types.WorkerLogTag[] = config.mediasoup.worker.logTags;
        const worker = await mediasoup.createWorker({
          logLevel: config.mediasoup.worker.logLevel,
          logTags,
          rtcMinPort: config.mediasoup.worker.rtcMinPort,
          rtcMaxPort: config.mediasoup.worker.rtcMaxPort,
        });

        this.trackNativeWorker(worker);
        // Retain this worker before listener creation can fail or yield.
        this.workers.push(worker);
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
            // Best effort: exit remains authoritative after a worker death.
          }
          logger.error('Mediasoup worker died');
          process.exit(1);
        });

        if (this.closing) throw nativeTerminationUnavailable();
        if (config.mediasoup.webRtcServer.enabled) {
          await this.createWebRtcServer(worker, i);
        }

        logger.debug('Mediasoup worker created', {
          pid: worker.pid,
          index: i,
        });
      }
      this.initialized = true;
      if (!this.isReady()) throw nativeTerminationUnavailable();
    } catch (error) {
      await this.close(performance.now() + STARTUP_NATIVE_CLEANUP_TIMEOUT_MS);
      throw error;
    }
  }

  private async createWebRtcServer(worker: Worker, index: number): Promise<void> {
    const shared = config.mediasoup.webRtcServer;
    const listenInfos: TransportListenInfo[] = [
      {
        protocol: 'udp',
        ip: shared.listenIp,
        announcedAddress: shared.announcedAddress,
        port: shared.ports[index],
      },
    ];
    if (config.mediasoup.webRtcTransport.enableTcp) {
      listenInfos.push({ ...listenInfos[0], protocol: 'tcp' });
    }
    const server = await worker.createWebRtcServer({ listenInfos });
    this.webRtcServers.set(worker, server);
    server.observer.once('close', () => {
      if (this.closing) return;
      this.listenerFailed = true;
      logger.error('Mediasoup shared listener closed');
    });
    if (this.closing || this.listenerFailed || worker.closed || server.closed) {
      throw nativeTerminationUnavailable();
    }
  }

  isReady(): boolean {
    if (
      !this.initialized ||
      this.closing ||
      this.listenerFailed ||
      this.workers.length !== config.mediasoup.numWorkers
    ) {
      return false;
    }
    return this.workers.every((worker) => {
      if (worker.closed || this.workerStopped(worker)) return false;
      return (
        !config.mediasoup.webRtcServer.enabled ||
        this.webRtcServers.get(worker)?.closed === false
      );
    });
  }

  /** Resolve by the captured router owner, independently of the room cache. */
  getWebRtcServer(router: Router): WebRtcServer | undefined {
    if (!config.mediasoup.webRtcServer.enabled) return undefined;
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
   * The `workerIndex < length` guard makes the bounds safety LOCAL. It was
   * previously contingent on the callee only reading [0, workerCount) — true,
   * but invisible from this file, and an out-of-range `+=` would write
   * `undefined + n` = NaN into a lengthened array. A NaN that ever landed in
   * range is an absorbing state: every `NaN < best` is false, so worker 0
   * would win every placement thereafter, silently (#3157 review).
   *
   * `?? 0` is likewise load-bearing rather than defensive: `roomManager.ts`
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
      if (assignment.workerIndex >= load.length) continue;
      load[assignment.workerIndex] += perRoom.get(roomId) ?? 0;
      rooms[assignment.workerIndex] += 1;
    }

    return { load, rooms };
  }

  async getOrCreateRouter(roomId: string): Promise<Router> {
    if (config.mediasoup.webRtcServer.enabled && !this.isReady()) {
      throw nativeTerminationUnavailable();
    }
    const existing = this.routers.get(roomId);

    if (existing && !existing.router.closed) {
      return existing.router;
    }

    // Evict a stale closed router (room was destroyed and a user is rejoining).
    // Dropping the whole entry drops its worker assignment with it — the two
    // cannot desync because they are one value.
    if (existing) {
      this.routers.delete(roomId);
    }

    const { load, rooms } = this.loadByWorkerIndex();
    const workerIndex = selectLeastLoadedWorkerIndex(this.workers.length, load, rooms);
    const worker = this.workers[workerIndex];

    if (this.closing) throw nativeTerminationUnavailable();
    this.routerCreations++;
    this.routerCreationVersion++;
    try {
      const mediaCodecs: RouterRtpCodecCapability[] = config.mediasoup.router.mediaCodecs;
      const router = await worker.createRouter({
        mediaCodecs,
      });
      this.routerWorkers.set(router, worker);
      if (this.closing || (config.mediasoup.webRtcServer.enabled && !this.isReady())) {
        router.close();
        throw nativeTerminationUnavailable();
      }
      this.routers.set(roomId, { router, workerIndex });
      logger.info('Created media router', { workerIndex });
      return router;
    } finally {
      this.routerCreations--;
      this.routerCreationVersion++;
    }
  }

  /** Remove a cached router (called when a room is destroyed) */
  removeRouter(roomId: string): void {
    this.routers.delete(roomId);
  }

  getWorkerCount(): number {
    return this.workers.length;
  }
}
