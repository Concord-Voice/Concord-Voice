import { describe, it, expect, vi, beforeEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
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
      numWorkers: 3,
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
import {
  NoWorkersAvailableError,
  RoomConsumerCountsUnavailableError,
} from '../src/lib/workerSelection.js';

describe('MediasoupService', () => {
  let service: MediasoupService;

  beforeEach(() => {
    service = new MediasoupService();
    mockCreateWorker.mockReset();
    // Default so every describe can init() regardless of how many workers it
    // names explicitly; mockResolvedValueOnce still takes precedence.
    mockCreateWorker.mockImplementation(() => Promise.resolve(createMockWorker()));
    // Placement reads live consumer counts (#3149). Default to an idle room
    // set; tests that care about load override with withCounts(...).
    service.setRoomConsumerCounts(() => new Map());
  });

  describe('init', () => {
    it('creates the configured number of workers', async () => {
      const worker1 = createMockWorker();
      const worker2 = createMockWorker();
      mockCreateWorker.mockResolvedValueOnce(worker1).mockResolvedValueOnce(worker2);

      await service.init();

      expect(mockCreateWorker).toHaveBeenCalledTimes(3);
      expect(service.getWorkerCount()).toBe(3);
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

    it('flushes an injected security writer before a worker-death exit', async () => {
      const worker = createMockWorker();
      const emit = vi.fn();
      const flush = vi.fn();
      service.setSecurityEventEmitter(emit, flush);
      mockCreateWorker.mockResolvedValueOnce(worker).mockResolvedValueOnce(createMockWorker());
      await service.init();
      worker._emit('died');
      expect(emit).toHaveBeenCalledWith({
        eventType: 'dependency',
        outcome: 'failure',
        severity: 'critical',
        reasonCode: 'dependency_unavailable',
      });
      expect(flush).toHaveBeenCalledOnce();
    });

    it('orders emit, synchronous flush, and exit even when an observer throws', async () => {
      const worker = createMockWorker();
      const order: string[] = [];
      const exit = vi.mocked(process.exit).mockImplementationOnce((() => {
        order.push('exit');
        return undefined as never;
      }) as never);
      service.setSecurityEventEmitter(
        () => {
          order.push('emit');
          throw new Error('observer failure');
        },
        () => order.push('flush')
      );
      mockCreateWorker.mockResolvedValueOnce(worker).mockResolvedValueOnce(createMockWorker());
      await service.init();
      expect(() => worker._emit('died')).not.toThrow();
      expect(order).toEqual(['emit', 'flush', 'exit']);
      expect(exit).toHaveBeenCalledWith(1);
    });

    it('still exits when the synchronous flush observer throws', async () => {
      const worker = createMockWorker();
      const exit = vi
        .mocked(process.exit)
        .mockImplementationOnce((() => undefined as never) as never);
      service.setSecurityEventEmitter(vi.fn(), () => {
        throw new Error('flush failure');
      });
      mockCreateWorker.mockResolvedValueOnce(worker).mockResolvedValueOnce(createMockWorker());
      await service.init();
      expect(() => worker._emit('died')).not.toThrow();
      expect(exit).toHaveBeenCalledWith(1);
    });
  });

  describe('getOrCreateRouter', () => {
    let w: ReturnType<typeof createMockWorker>[];

    beforeEach(async () => {
      // THREE workers on purpose. With two, round-robin's next index IS the
      // other worker, so every two-room scenario is indistinguishable from
      // least-loaded and the placement tests cannot discriminate (#3157 review).
      w = [createMockWorker(), createMockWorker(), createMockWorker()];
      w.forEach((worker) => mockCreateWorker.mockResolvedValueOnce(worker));
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

    // Placement helper: declare what the rooms currently cost.
    const withCounts = (counts: Record<string, number>) =>
      service.setRoomConsumerCounts(() => new Map(Object.entries(counts)));

    it('places a new room on the worker with the fewest consumers', async () => {
      // Fill three workers one room each (the rooms tie-break spreads them),
      // then make worker 2's room the lightest. Round-robin would send room-d
      // to worker 0; least-loaded must send it to worker 2.
      await service.getOrCreateRouter('room-a');
      await service.getOrCreateRouter('room-b');
      await service.getOrCreateRouter('room-c');
      withCounts({ 'room-a': 5, 'room-b': 5, 'room-c': 1 });

      await service.getOrCreateRouter('room-d');

      expect(w[2].createRouter).toHaveBeenCalledTimes(2);
      expect(w[0].createRouter).toHaveBeenCalledTimes(1);
    });

    it('spreads idle rooms across workers — the fill-window fix (#3157)', async () => {
      // Every room reads 0 consumers until a second participant joins AND
      // consumes. Before the rooms tie-break, all three landed on worker 0 and
      // stayed there for life. Load carries no information here, so placement
      // must fall through to room count.
      await service.getOrCreateRouter('room-a');
      await service.getOrCreateRouter('room-b');
      await service.getOrCreateRouter('room-c');

      expect(w[0].createRouter).toHaveBeenCalledTimes(1);
      expect(w[1].createRouter).toHaveBeenCalledTimes(1);
      expect(w[2].createRouter).toHaveBeenCalledTimes(1);
    });

    it('sums the consumer counts of every room sharing a worker', async () => {
      // Two rooms on worker 0 must ADD. With `=` instead of `+=`, worker 0
      // reports only its last-iterated room (4) and wrongly wins against
      // worker 1's single room of 5.
      await service.getOrCreateRouter('room-a'); // w0
      await service.getOrCreateRouter('room-b'); // w1
      await service.getOrCreateRouter('room-c'); // w2
      withCounts({ 'room-a': 3, 'room-b': 5, 'room-c': 5 });
      await service.getOrCreateRouter('room-d'); // w0 — lightest at 3
      withCounts({ 'room-a': 3, 'room-d': 4, 'room-b': 5, 'room-c': 5 });

      await service.getOrCreateRouter('room-e');

      // Summed: w0 = 7 > w1 = 5, so room-e goes to worker 1.
      expect(w[1].createRouter).toHaveBeenCalledTimes(2);
      expect(w[0].createRouter).toHaveBeenCalledTimes(2);
    });

    it('treats a room the provider no longer reports as zero, not NaN', async () => {
      // roomManager deletes a room with a closed router from `rooms` WITHOUT
      // calling removeRouter, so this map legitimately holds unreported ids.
      // Without `?? 0` that yields NaN at index 0 — an absorbing state, since
      // every `NaN < x` is false, silently pinning every later room to
      // worker 0.
      await service.getOrCreateRouter('room-a'); // w0
      withCounts({}); // room-a is no longer reported at all

      await service.getOrCreateRouter('room-b');

      expect(w[1].createRouter).toHaveBeenCalledTimes(1);
      expect(w[0].createRouter).toHaveBeenCalledTimes(1);
    });

    it('throws a typed error when the worker pool is empty', async () => {
      await service.close();

      await expect(service.getOrCreateRouter('room-a')).rejects.toThrow(NoWorkersAvailableError);
    });

    it('stops counting a room once its router is removed', async () => {
      await service.getOrCreateRouter('room-a'); // w0
      await service.getOrCreateRouter('room-b'); // w1
      await service.getOrCreateRouter('room-c'); // w2
      withCounts({ 'room-a': 100, 'room-b': 1, 'room-c': 1 });

      service.removeRouter('room-a'); // w0 drops to 0 consumers, 0 rooms
      await service.getOrCreateRouter('room-d');

      expect(w[0].createRouter).toHaveBeenCalledTimes(2);
    });

    it('stops counting a room once its router is evicted as closed', async () => {
      const first = await service.getOrCreateRouter('room-a'); // w0
      await service.getOrCreateRouter('room-b'); // w1
      await service.getOrCreateRouter('room-c'); // w2
      withCounts({ 'room-a': 100, 'room-b': 1, 'room-c': 1 });

      (first as any).closed = true;
      await service.getOrCreateRouter('room-a'); // re-placed; w0 now empty

      expect(w[0].createRouter).toHaveBeenCalledTimes(2);
    });

    it('co-locates rooms created in the SAME TICK — accepted residual (#3149)', async () => {
      // The residual after the #3157 fix, and now genuinely one `await` wide:
      // neither router is registered when the other selects, so the rooms
      // tie-break sees [0,0,0] for both.
      //
      // Discriminating: the sequential sibling above spreads these across
      // three workers. Only the concurrency window co-locates them, so this
      // test fails if placement is ever serialized OR given a reservation.
      await Promise.all([
        service.getOrCreateRouter('room-a'),
        service.getOrCreateRouter('room-b'),
      ]);

      expect(w[0].createRouter).toHaveBeenCalledTimes(2);
      expect(w[1].createRouter).not.toHaveBeenCalled();
    });

    it('passes the configured mediaCodecs array to worker.createRouter', async () => {
      await service.getOrCreateRouter('room-codec-test');

      // Parity with the worker-config test above: asserts the codec config
      // is forwarded verbatim to mediasoup. Catches accidental future
      // refactors that filter, transform, or swap the codec list at the
      // call site. (Type-level concerns are caught by tsc; this is the
      // runtime wiring guard.)
      expect(w[0].createRouter).toHaveBeenCalledWith({
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

  describe('worker placement without a provider', () => {
    it('throws a typed error when no consumer-count provider is installed', async () => {
      // A service that was never wired at index.ts. Degrading to all-zero load
      // would silently pin every room to worker 0, so this fails closed.
      const bare = new MediasoupService();
      mockCreateWorker
        .mockResolvedValueOnce(createMockWorker())
        .mockResolvedValueOnce(createMockWorker());
      await bare.init();

      await expect(bare.getOrCreateRouter('room-1')).rejects.toThrow(
        RoomConsumerCountsUnavailableError
      );
    });
  });

  /**
   * The provider is INSTALLED in index.ts and CONSUMED here. Every behavioural
   * test above supplies its own provider, so all of them keep passing if
   * index.ts stops installing one — and index.ts is in vitest.config.ts's
   * coverage `exclude`, so nothing else looks at it either.
   *
   * The consequence of that gap is not subtle: placement fails closed, so a
   * missing wiring line means EVERY voice join throws
   * RoomConsumerCountsUnavailableError in production, with a green suite.
   *
   * So this scans the real source, the same way producerSupersession.test.ts
   * enforces its two-step contract (see [internal]rules/tests.md § "Test the
   * consumer, not the handshake").
   *
   * KNOWN BOUNDS, so a future failure is diagnosable rather than baffling:
   *   - It matches source TEXT, so reformatting the wiring statement across
   *     lines reds it even though the code is correct. The regex tolerates
   *     internal whitespace; it does not tolerate arbitrary restructuring.
   *   - It proves the call EXISTS, not that it is reachable. Moving it into
   *     dead code would still pass.
   *   - codeOf() strips `//` anywhere, including inside a string literal, so
   *     a URL on the wiring line would truncate it. Harmless today: that can
   *     only cause a false failure, never a false pass.
   */
  describe('worker-placement wiring contract (#3149)', () => {
    /** Source with comments stripped, so prose mentioning a call is not a call. */
    const codeOf = (relativePath: string): string =>
      readFileSync(join(__dirname, '..', 'src', relativePath), 'utf8')
        .replace(/\/\*[\s\S]*?\*\//g, '')
        .replace(/\/\/[^\n]*/g, '');

    it('index.ts installs the room-consumer-count provider', () => {
      const index = codeOf('index.ts');

      // ONE regex over the whole call expression. Two independent toContain
      // assertions both passed if the halves appeared in unrelated statements
      // — e.g. rewired to `setRoomConsumerCounts(() => new Map())` with a
      // stray `roomManager.getRoomConsumerCounts()` surviving elsewhere.
      expect(index).toMatch(
        /mediasoupService\.setRoomConsumerCounts\(\s*\(\)\s*=>\s*roomManager\.getRoomConsumerCounts\(\)\s*\)/
      );
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
