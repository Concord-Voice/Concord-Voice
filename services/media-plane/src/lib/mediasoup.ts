import * as mediasoup from 'mediasoup';
import { config } from '../config/index.js';
import { logger } from './logger.js';
import type { Worker, Router, RouterRtpCodecCapability } from 'mediasoup/types';

export class MediasoupService {
  private workers: Worker[] = [];
  private nextWorkerIdx = 0;
  private readonly routers: Map<string, Router> = new Map();

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
        logger.error('Mediasoup worker died', { pid: worker.pid });
        process.exit(1);
      });

      this.workers.push(worker);

      logger.debug('Mediasoup worker created', {
        pid: worker.pid,
        index: i,
      });
    }
  }

  private getNextWorker(): Worker {
    const worker = this.workers[this.nextWorkerIdx];
    this.nextWorkerIdx = (this.nextWorkerIdx + 1) % this.workers.length;
    return worker;
  }

  async getOrCreateRouter(roomId: string): Promise<Router> {
    let router = this.routers.get(roomId);

    // Evict stale closed routers (room was destroyed and user is rejoining)
    if (router?.closed) {
      this.routers.delete(roomId);
      router = undefined;
    }

    if (!router) {
      const worker = this.getNextWorker();
      router = await worker.createRouter({
        mediaCodecs: config.mediasoup.router.mediaCodecs satisfies RouterRtpCodecCapability[],
      });

      this.routers.set(roomId, router);

      logger.info('Created router for room', { roomId, workerId: worker.pid });
    }

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
