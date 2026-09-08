import * as mediasoup from 'mediasoup';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import {
  RoomConsumerCountsUnavailableError,
  selectLeastLoadedWorkerIndex,
} from './workerSelection.js';
import type { Worker, Router, RouterRtpCodecCapability } from 'mediasoup/types';
import type { EmitSecurityEvent } from './securityEvent.js';

/** Live consumer counts keyed by roomId, supplied by RoomManager (#3149). */
export type RoomConsumerCountsProvider = () => ReadonlyMap<string, number>;

/** A room's router plus the index of the worker hosting it. */
type RouterAssignment = { router: Router; workerIndex: number };

export class MediasoupService {
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
    logger.info('Initializing mediasoup workers', {
      numWorkers: config.mediasoup.numWorkers,
    });

    for (let i = 0; i < config.mediasoup.numWorkers; i++) {
      const worker = await mediasoup.createWorker({
        logLevel: config.mediasoup.worker.logLevel,
        logTags: config.mediasoup.worker.logTags satisfies mediasoup.types.WorkerLogTag[],
        rtcMinPort: config.mediasoup.worker.rtcMinPort,
        rtcMaxPort: config.mediasoup.worker.rtcMaxPort,
      });

      worker.on('died', () => {
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

      this.workers.push(worker);

      logger.debug('Mediasoup worker created', {
        pid: worker.pid,
        index: i,
      });
    }
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

    const router = await worker.createRouter({
      mediaCodecs: config.mediasoup.router.mediaCodecs satisfies RouterRtpCodecCapability[],
    });

    this.routers.set(roomId, { router, workerIndex });

    logger.info('Created router for room', { roomId, workerId: worker.pid, workerIndex });

    return router;
  }

  /** Remove a cached router (called when a room is destroyed) */
  removeRouter(roomId: string): void {
    this.routers.delete(roomId);
  }

  getWorkerCount(): number {
    return this.workers.length;
  }

  async close() {
    logger.info('Closing mediasoup service');

    for (const worker of this.workers) {
      worker.close();
    }

    this.workers = [];
    this.routers.clear();
  }
}
