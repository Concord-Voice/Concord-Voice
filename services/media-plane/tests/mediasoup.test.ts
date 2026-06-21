import { describe, it, expect, vi, beforeEach } from 'vitest';
import { createMockWorker } from './mocks/mediasoup.js';
import './mocks/logger.js';

// Mock mediasoup native module
const mockCreateWorker = vi.fn();
vi.mock('mediasoup', () => ({
  createWorker: (...args: any[]) => mockCreateWorker(...args),
}));

// Mock config
vi.mock('@/config/index.js', () => ({
  config: {
    mediasoup: {
      numWorkers: 2,
      worker: {
        logLevel: 'warn',
        logTags: ['info'],
        rtcMinPort: 40000,
        rtcMaxPort: 49999,
      },
      router: {
        mediaCodecs: [{ kind: 'audio', mimeType: 'audio/opus', clockRate: 48000, channels: 2 }],
      },
    },
  },
}));

import { MediasoupService } from '../src/lib/mediasoup.js';

describe('MediasoupService', () => {
  let service: MediasoupService;

  beforeEach(() => {
    service = new MediasoupService();
    mockCreateWorker.mockReset();
  });

  describe('init', () => {
    it('creates the configured number of workers', async () => {
      const worker1 = createMockWorker();
      const worker2 = createMockWorker();
      mockCreateWorker.mockResolvedValueOnce(worker1).mockResolvedValueOnce(worker2);

      await service.init();

      expect(mockCreateWorker).toHaveBeenCalledTimes(2);
      expect(service.getWorkerCount()).toBe(2);
    });

    it('passes worker config to createWorker', async () => {
      mockCreateWorker
        .mockResolvedValueOnce(createMockWorker())
        .mockResolvedValueOnce(createMockWorker());

      await service.init();

      expect(mockCreateWorker).toHaveBeenCalledWith({
        logLevel: 'warn',
        logTags: ['info'],
        rtcMinPort: 40000,
        rtcMaxPort: 49999,
      });
    });

    it('calls process.exit(1) when a worker dies', async () => {
      const worker = createMockWorker();
      mockCreateWorker.mockResolvedValueOnce(worker).mockResolvedValueOnce(createMockWorker());

      await service.init();

      worker._emit('died');

      await vi.waitFor(() => {
        expect(process.exit).toHaveBeenCalledWith(1);
      });
    });
  });

  describe('getOrCreateRouter', () => {
    let worker1: ReturnType<typeof createMockWorker>;
    let worker2: ReturnType<typeof createMockWorker>;

    beforeEach(async () => {
      worker1 = createMockWorker();
      worker2 = createMockWorker();
      mockCreateWorker.mockResolvedValueOnce(worker1).mockResolvedValueOnce(worker2);
      await service.init();
    });

    it('creates a router on first call for a roomId', async () => {
      const router = await service.getOrCreateRouter('room-1');
      expect(router).toBeDefined();
      expect(router.rtpCapabilities).toBeDefined();
    });

    it('returns cached router on second call', async () => {
      const first = await service.getOrCreateRouter('room-1');
      const second = await service.getOrCreateRouter('room-1');
      expect(second).toBe(first);
    });

    it('evicts closed router and creates a new one', async () => {
      const first = await service.getOrCreateRouter('room-1');
      (first as any).closed = true;

      const second = await service.getOrCreateRouter('room-1');
      expect(second).not.toBe(first);
    });

    it('round-robins workers across rooms', async () => {
      // Two workers: room-a → worker 0, room-b → worker 1, room-c → worker 0
      await service.getOrCreateRouter('room-a');
      await service.getOrCreateRouter('room-b');
      await service.getOrCreateRouter('room-c');

      expect(worker1.createRouter).toHaveBeenCalledTimes(2); // room-a, room-c
      expect(worker2.createRouter).toHaveBeenCalledTimes(1); // room-b
    });

    it('passes the configured mediaCodecs array to worker.createRouter', async () => {
      await service.getOrCreateRouter('room-codec-test');

      // Parity with the worker-config test above: asserts the codec config
      // is forwarded verbatim to mediasoup. Catches accidental future
      // refactors that filter, transform, or swap the codec list at the
      // call site. (Type-level concerns are caught by tsc; this is the
      // runtime wiring guard.)
      expect(worker1.createRouter).toHaveBeenCalledWith({
        mediaCodecs: [{ kind: 'audio', mimeType: 'audio/opus', clockRate: 48000, channels: 2 }],
      });
    });
  });

  describe('removeRouter', () => {
    it('deletes router from cache so next call creates fresh', async () => {
      const worker = createMockWorker();
      mockCreateWorker.mockResolvedValueOnce(worker).mockResolvedValueOnce(createMockWorker());
      await service.init();

      const first = await service.getOrCreateRouter('room-1');
      service.removeRouter('room-1');
      const second = await service.getOrCreateRouter('room-1');

      expect(second).not.toBe(first);
    });
  });

  describe('close', () => {
    it('closes all workers and clears state', async () => {
      const worker1 = createMockWorker();
      const worker2 = createMockWorker();
      mockCreateWorker.mockResolvedValueOnce(worker1).mockResolvedValueOnce(worker2);
      await service.init();

      await service.close();

      expect(worker1.close).toHaveBeenCalled();
      expect(worker2.close).toHaveBeenCalled();
      expect(service.getWorkerCount()).toBe(0);
    });
  });
});
