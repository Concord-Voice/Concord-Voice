import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const { mockDecryptFrame } = vi.hoisted(() => ({
  mockDecryptFrame: vi.fn(),
}));

vi.mock('@/renderer/services/mediaEncryption', () => ({
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 2,
  MediaEncryption: class {
    initFromKey = vi.fn();
    addDecryptKeyDirect = vi.fn();
    rotateKeys = vi.fn().mockResolvedValue(undefined);
    catchUpToEpoch = vi.fn().mockResolvedValue(undefined);
    getCurrentKeyId = vi.fn().mockReturnValue(0);
    encryptFrame = vi.fn().mockResolvedValue(undefined);
    decryptFrame = mockDecryptFrame;
    destroy = vi.fn();
  },
}));

describe('e2eeWorker keyframe recovery', () => {
  let postMessage: ReturnType<typeof vi.fn>;
  let rtctransformListener: ((event: { transformer: unknown }) => void) | undefined;

  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    postMessage = vi.fn();
    rtctransformListener = undefined;
    vi.stubGlobal('self', {
      postMessage,
      onmessage: null,
      addEventListener: vi.fn((event: string, listener: EventListener) => {
        if (event === 'rtctransform') {
          rtctransformListener = listener as unknown as (event: { transformer: unknown }) => void;
        }
      }),
    });
    await import('@/renderer/workers/e2eeWorker');
  });

  it('posts requestKeyframe when video decrypt recovers after dropped frames', async () => {
    const dropped = { type: 'key', data: new ArrayBuffer(8) } as RTCEncodedVideoFrame;
    const recovered = { type: 'delta', data: new ArrayBuffer(8) } as RTCEncodedVideoFrame;
    const written: unknown[] = [];

    mockDecryptFrame
      .mockRejectedValueOnce(new DOMException('', 'OperationError'))
      .mockResolvedValueOnce(undefined);

    const readable = new ReadableStream<RTCEncodedVideoFrame>({
      start(controller) {
        controller.enqueue(dropped);
        controller.enqueue(recovered);
        controller.close();
      },
    });
    const writable = new WritableStream<RTCEncodedVideoFrame>({
      write(chunk) {
        written.push(chunk);
      },
    });

    expect(rtctransformListener).toBeTypeOf('function');
    rtctransformListener!({
      transformer: {
        options: { role: 'decrypt', senderUserId: 'sender-1' },
        readable,
        writable,
      },
    });

    await vi.waitFor(() => {
      expect(postMessage).toHaveBeenCalledWith({
        type: 'requestKeyframe',
        senderUserId: 'sender-1',
      });
    });
    expect(written).toEqual([recovered]);
  });
});

describe('e2eeWorker requestFrameKey retry policy (#1885)', () => {
  let postMessage: ReturnType<typeof vi.fn>;
  let requestFrameKeyOnce: (s: string, v: number, k: number) => void;
  let frameKeyRequests: Map<string, { lastAttempt: number; attempts: number }>;

  beforeEach(async () => {
    vi.resetModules();
    vi.clearAllMocks();
    vi.useFakeTimers();
    vi.setSystemTime(0);
    postMessage = vi.fn();
    vi.stubGlobal('self', {
      postMessage,
      onmessage: null,
      addEventListener: vi.fn(),
    });
    const mod = await import('@/renderer/workers/e2eeWorker');
    requestFrameKeyOnce = mod.requestFrameKeyOnce;
    frameKeyRequests = mod.frameKeyRequests;
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const countRequests = () =>
    postMessage.mock.calls.filter((c) => (c[0] as { type?: string }).type === 'requestFrameKey')
      .length;

  it('retries in a bounded burst then pauses (never loops forever)', () => {
    // 20 frames arrive for the same missing key, each spaced past the backoff.
    for (let i = 0; i < 20; i++) {
      requestFrameKeyOnce('sender-1', 3, 0);
      vi.setSystemTime((i + 1) * 400); // > FRAME_KEY_BACKOFF_MS (350) each step
    }
    expect(countRequests()).toBe(8); // capped at FRAME_KEY_BURST_CAP — no infinite loop
  });

  it('resumes after the idle-reset window so a slow-published key recovers (Gitar #3)', () => {
    for (let i = 0; i < 10; i++) {
      requestFrameKeyOnce('sender-1', 3, 0);
      vi.setSystemTime((i + 1) * 400);
    }
    expect(countRequests()).toBe(8); // burst exhausted, paused

    // A pending-404 key published slower than the burst window: after the idle
    // reset, the next frame resumes requesting instead of staying black forever.
    vi.setSystemTime(10 * 400 + 16_000); // > FRAME_KEY_RETRY_RESET_MS (15000) since last request
    requestFrameKeyOnce('sender-1', 3, 0);
    expect(countRequests()).toBe(9); // recovered: a fresh request went out
  });

  it('suppresses repeated misses inside the backoff window (no request spam)', () => {
    requestFrameKeyOnce('sender-1', 3, 0); // 1 request
    vi.setSystemTime(100); // < 350ms backoff
    requestFrameKeyOnce('sender-1', 3, 0); // suppressed
    requestFrameKeyOnce('sender-1', 3, 0); // suppressed
    expect(countRequests()).toBe(1);
  });

  it('caps the tracking map against a sender churning unique keys (DoS bound)', () => {
    // Each unique (keyVersion,keyId) is a distinct tracked entry; far exceed the cap.
    for (let i = 0; i < 600; i++) {
      requestFrameKeyOnce('sender-1', i, 0); // unique keyVersion each call
      vi.setSystemTime(i + 1); // distinct timestamps
    }
    expect(frameKeyRequests.size).toBeLessThanOrEqual(512); // FRAME_KEY_MAX_TRACKED
  });

  it('LRU eviction keeps an actively-touched key while churn evicts stale ones (Gitar #1885)', () => {
    const keepAlive = 'sender-1:9999:0';
    // Insert keep-alive key A first — under the old FIFO it would be evicted earliest.
    requestFrameKeyOnce('sender-1', 9999, 0); // A, at t=0
    let t = 1;
    for (let i = 0; i < 510; i++) {
      requestFrameKeyOnce('sender-1', i, 0); // churn unique keys (map → 511, under cap)
      vi.setSystemTime(t++);
    }
    expect(frameKeyRequests.has(keepAlive)).toBe(true);

    // Re-touch A past the backoff (and within the reset window) so LRU moves it
    // to the most-recent position — the move FIFO would not make.
    vi.setSystemTime(1000);
    requestFrameKeyOnce('sender-1', 9999, 0);

    // Churn 200 more unique keys — each evicts the least-recently-touched entry.
    t = 1001;
    for (let i = 510; i < 710; i++) {
      requestFrameKeyOnce('sender-1', i, 0);
      vi.setSystemTime(t++);
    }

    // Under LRU the recently-touched A survives; under the retired FIFO (evict
    // oldest-inserted) A would have been evicted. Size stays bounded either way.
    expect(frameKeyRequests.has(keepAlive)).toBe(true);
    expect(frameKeyRequests.size).toBeLessThanOrEqual(512);
  });
});
