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
  const router = createMockRouter();
  return {
    pid: Math.floor(Math.random() * 100000),
    closed: false,
    createRouter: vi.fn(() => Promise.resolve(router)),
    close: vi.fn(),
    on: emitter.on,
    _emit: emitter.emit,
    _router: router,
    ...overrides,
  };
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

/**
 * Build mock RtpParameters for an opus audio producer. By default it omits opus
 * fmtp parameters (the audio-tier gate is a no-op without them — mediasoup
 * defaults apply). Pass `opusParameters` to simulate a client-declared opus
 * ptime / maxaveragebitrate for the #1300 tier-gate tests.
 */
export function createRtpParameters(opusParameters?: Record<string, unknown>) {
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
    encodings: [{ ssrc: 12345 }],
    rtcp: { cname: 'test', reducedSize: true },
  };
}
