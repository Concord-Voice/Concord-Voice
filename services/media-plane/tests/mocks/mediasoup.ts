import { vi } from 'vitest';

// ---------------------------------------------------------------------------
// Mock factory functions for mediasoup objects.
//
// The mediasoup deep imports (mediasoup/node/lib/types.js, etc.) are handled
// by vitest aliases in vitest.config.ts → mediasoup-types-stub.ts.
// Individual test files must still vi.mock('mediasoup') to prevent native
// C++ binding loading.
// ---------------------------------------------------------------------------

type EventMap = Map<string, ((...args: any[]) => void)[]>;

function createEventEmitter() {
  const events: EventMap = new Map();
  return {
    _events: events,
    on: vi.fn((event: string, handler: (...args: any[]) => void) => {
      if (!events.has(event)) events.set(event, []);
      events.get(event)!.push(handler);
    }),
    once: vi.fn((event: string, handler: (...args: any[]) => void) => {
      const once = (...args: any[]) => {
        events.set(
          event,
          (events.get(event) ?? []).filter((candidate) => candidate !== once)
        );
        handler(...args);
      };
      if (!events.has(event)) events.set(event, []);
      events.get(event)!.push(once);
    }),
    emit(event: string, ...args: any[]) {
      const handlers = events.get(event);
      if (handlers) handlers.forEach((h) => h(...args));
    },
  };
}

export function createMockProducer(overrides: Record<string, unknown> = {}) {
  const emitter = createEventEmitter();
  return {
    id: `producer-${Math.random().toString(36).slice(2, 8)}`,
    kind: 'audio' as const,
    rtpParameters: {},
    appData: {},
    closed: false,
    pause: vi.fn(),
    resume: vi.fn(),
    close: vi.fn(),
    on: emitter.on,
    _emit: emitter.emit,
    ...overrides,
  };
}

export function createMockConsumer(overrides: Record<string, unknown> = {}) {
  const emitter = createEventEmitter();
  return {
    id: `consumer-${Math.random().toString(36).slice(2, 8)}`,
    kind: 'audio' as const,
    rtpParameters: {},
    producerId: '',
    appData: {},
    closed: false,
    // pause/resume return resolved promises (mediasoup's real API is async — the
    // last-N applyLastNDelta path calls `.catch()` on the returned promise).
    pause: vi.fn(() => Promise.resolve()),
    resume: vi.fn(() => Promise.resolve()),
    setPreferredLayers: vi.fn(() => Promise.resolve()),
    setPriority: vi.fn(() => Promise.resolve()),
    close: vi.fn(),
    on: emitter.on,
    _emit: emitter.emit,
    ...overrides,
  };
}

export function createMockTransport(overrides: Record<string, unknown> = {}) {
  const emitter = createEventEmitter();
  const id = `transport-${Math.random().toString(36).slice(2, 8)}`;
  return {
    id,
    iceParameters: { usernameFragment: 'test', ice: 'mock-credential' },
    iceCandidates: [
      { foundation: '1', priority: 1, ip: '127.0.0.1', port: 5000, type: 'host', protocol: 'udp' },
    ],
    dtlsParameters: { fingerprints: [{ algorithm: 'sha-256', value: 'AA:BB:CC' }], role: 'auto' },
    iceState: 'new',
    dtlsState: 'new',
    closed: false,
    connect: vi.fn(),
    produce: vi.fn(),
    consume: vi.fn(),
    close: vi.fn(),
    setMaxIncomingBitrate: vi.fn(),
    on: emitter.on,
    _emit: emitter.emit,
    ...overrides,
  };
}

export function createMockAudioLevelObserver(overrides: Record<string, unknown> = {}) {
  const emitter = createEventEmitter();
  return {
    closed: false,
    close: vi.fn(),
    addProducer: vi.fn(),
    on: emitter.on,
    _emit: emitter.emit,
    ...overrides,
  };
}

export function createMockRouter(overrides: Record<string, unknown> = {}) {
  const emitter = createEventEmitter();
  const audioLevelObserver = createMockAudioLevelObserver();
  return {
    id: `router-${Math.random().toString(36).slice(2, 8)}`,
    closed: false,
    rtpCapabilities: {
      codecs: [
        { kind: 'audio', mimeType: 'audio/opus', clockRate: 48000, channels: 2 },
        { kind: 'video', mimeType: 'video/VP8', clockRate: 90000 },
      ],
      headerExtensions: [],
    },
    createWebRtcTransport: vi.fn(() => Promise.resolve(createMockTransport())),
    createAudioLevelObserver: vi.fn(() => Promise.resolve(audioLevelObserver)),
    canConsume: vi.fn(() => true),
    close: vi.fn(),
    on: emitter.on,
    _emit: emitter.emit,
    _audioLevelObserver: audioLevelObserver,
    ...overrides,
  };
}

export function createMockWorker(overrides: Record<string, unknown> = {}) {
  const emitter = createEventEmitter();
  const routers = new Map<string, ReturnType<typeof createMockRouter>>();
  const pid =
    typeof overrides.pid === 'number' ? overrides.pid : Math.floor(Math.random() * 100000);
  const worker = {
    pid,
    closed: false,
    subprocessClosed: false,
    // Real mediasoup mints a NEW Router per createRouter() call. This used to
    // return one shared instance, which made "created a fresh router"
    // assertions pass only when placement happened to move the room to a
    // DIFFERENT worker — so they were really asserting worker rotation. Least
    // -loaded placement (#3149) can legitimately re-place a room on the same
    // worker, which exposed it.
    createRouter: vi.fn(async () => {
      const router = createMockRouter();
      routers.set(router.id, router);
      router.close.mockImplementation(() => {
        router.closed = true;
        routers.delete(router.id);
      });
      return router;
    }),
    dump: vi.fn(async () => ({
      pid,
      webRtcServerIds: [],
      routerIds: [...routers.values()]
        .filter((router) => !router.closed)
        .map((router) => router.id),
      channelMessageHandlers: { channelRequestHandlers: [], channelNotificationHandlers: [] },
    })),
    close: vi.fn((): void => {
      worker.closed = true;
      worker.subprocessClosed = true;
      routers.clear();
      emitter.emit('subprocessclose');
    }),
    on: emitter.on,
    once: emitter.once,
    _emit: emitter.emit,
    ...overrides,
  };
  return worker;
}

export function createRtpCapabilities(videoCodecs: string[] = ['video/VP8', 'video/VP9']) {
  return {
    codecs: [
      { kind: 'audio' as const, mimeType: 'audio/opus', clockRate: 48000, channels: 2 },
      ...videoCodecs.map((mimeType) => ({
        kind: 'video' as const,
        mimeType,
        clockRate: 90000,
      })),
    ],
    headerExtensions: [],
  };
}

/** What a stock free-tier client declares for its mic (desktop default `standard` tier). */
export const STOCK_FREE_MIC_PTIME_MS = 20;
export const STOCK_FREE_MIC_MAX_BITRATE_BPS = 96_000;

/**
 * Build mock RtpParameters for an opus audio producer.
 *
 * The audio-tier gate refuses a mic producer that declares no ptime or no bitrate
 * (#3858), so the defaults are what a stock free client declares: opus `ptime`
 * 20 ms and one encoding with `maxBitrate` 96 kbps. That keeps every test that
 * only needs a mic producer to EXIST admitted by the gate.
 *
 * - `opusParameters` undefined → `{ ptime: 20 }`; `null` → no opus parameters at
 *   all; an object → exactly that object (no merge), so a gate test states every
 *   field it depends on.
 * - `encodings` undefined → `[{ ssrc, maxBitrate: 96_000 }]`; an array → exactly
 *   that array (`[]` simulates a client that sends no encodings).
 */
export function createRtpParameters(
  opusParameters: Record<string, unknown> | null = { ptime: STOCK_FREE_MIC_PTIME_MS },
  encodings: Array<Record<string, unknown>> = [
    { ssrc: 12345, maxBitrate: STOCK_FREE_MIC_MAX_BITRATE_BPS },
  ]
) {
  return {
    codecs: [
      {
        mimeType: 'audio/opus',
        payloadType: 111,
        clockRate: 48000,
        channels: 2,
        ...(opusParameters ? { parameters: opusParameters } : {}),
      },
    ],
    headerExtensions: [],
    encodings,
    rtcp: { cname: 'test', reducedSize: true },
  };
}

/**
 * A camera/screen producer's rtpParameters: every codec `video/*`, as
 * `assertProducerKindMatches` requires of a video producer (#1308). Encodings
 * default to the same single entry `createRtpParameters` uses, so swapping
 * one helper for the other changes the codec and nothing else.
 */
export function createVideoRtpParameters(
  encodings: Array<Record<string, unknown>> = [
    { ssrc: 12345, maxBitrate: STOCK_FREE_MIC_MAX_BITRATE_BPS },
  ]
) {
  return {
    codecs: [{ mimeType: 'video/VP8', payloadType: 96, clockRate: 90000 }],
    headerExtensions: [],
    encodings,
    rtcp: { cname: 'test', reducedSize: true },
  };
}
