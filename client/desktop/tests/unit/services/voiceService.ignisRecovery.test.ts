/** IGNIS interval decoder-budget, overload intervention, and recovery regressions. */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';

// ---------------------------------------------------------------------------
// Mock external dependencies BEFORE importing voiceService
// ---------------------------------------------------------------------------

// --- mediasoup-client ---
const mockDeviceLoad = vi.fn().mockResolvedValue(undefined);
const mockDeviceRtpCapabilities = {
  codecs: [
    { mimeType: 'audio/opus', kind: 'audio', clockRate: 48000, channels: 2, parameters: {} },
    { mimeType: 'video/VP8', kind: 'video', clockRate: 90000, parameters: {} },
  ],
};

const mockCreateSendTransport = vi.fn();
const mockCreateRecvTransport = vi.fn();

vi.mock('mediasoup-client', () => ({
  Device: class MockDevice {
    load = mockDeviceLoad;
    rtpCapabilities = mockDeviceRtpCapabilities;
    createSendTransport = mockCreateSendTransport;
    createRecvTransport = mockCreateRecvTransport;
    loaded = true;
  },
  types: {},
}));

// --- socket.io-client ---
const mockSocket = {
  connected: false,
  emit: vi.fn(),
  on: vi.fn(),
  once: vi.fn(),
  disconnect: vi.fn(),
  io: { on: vi.fn() },
};

vi.mock('socket.io-client', () => ({
  io: vi.fn().mockReturnValue(mockSocket),
}));

// --- apiClient ---
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

// --- e2eeService ---
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    getChannelKey: vi.fn().mockResolvedValue(null),
    getChannelKeyMaterial: vi.fn().mockResolvedValue({ channelKey: null, keyVersion: 0 }),
    invalidateChannelKey: vi.fn(),
    // #1878: version binding + sender re-base surface.
    getChannelKeyVersion: vi.fn().mockReturnValue(0),
    getChannelKeyByVersion: vi.fn().mockResolvedValue(null),
    onKeyRotation: vi.fn().mockReturnValue(() => {}),
  },
}));

// --- mediaEncryption ---
vi.mock('@/renderer/services/mediaEncryption', () => ({
  // Mirror the live wire version so this focused suite cannot silently pin stale crypto.
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 5,
  MediaEncryption: class MockMediaEncryption {
    init = vi.fn().mockResolvedValue(undefined);
    initFromKey = vi.fn();
    destroy = vi.fn();
    getCurrentKeyId = vi.fn().mockReturnValue(0);
    setCurrentKeyId = vi.fn();
    // #1878: encrypt-version binding.
    setKeyVersion = vi.fn();
    getKeyVersion = vi.fn().mockReturnValue(0);
    encryptFrame = vi.fn().mockResolvedValue(undefined);
    decryptFrame = vi.fn().mockResolvedValue(undefined);
    addDecryptKey = vi.fn().mockResolvedValue(undefined);
    addDecryptKeyAtEpoch = vi.fn().mockResolvedValue({} as CryptoKey);
    addDecryptKeyAtVersion = vi.fn().mockResolvedValue({} as CryptoKey);
    addDecryptKeyDirect = vi.fn();
    addDecryptKeyDirectV3 = vi.fn();
    debouncedRotateKeys = vi.fn();
    catchUpToEpoch = vi.fn().mockResolvedValue(undefined);
  },
  deriveFrameKey: vi.fn().mockResolvedValue({} as CryptoKey),
  ratchetKey: vi.fn().mockResolvedValue({} as CryptoKey),
}));

// --- osPermissionStore ---
vi.mock('@/renderer/stores/osPermissionStore', () => ({
  useOsPermissionStore: {
    getState: vi.fn().mockReturnValue({
      checkOne: vi.fn().mockResolvedValue('granted'),
      openSettings: vi.fn(),
    }),
    subscribe: vi.fn().mockReturnValue(() => {}),
  },
  ensureOsPermission: vi.fn().mockResolvedValue('granted'),
}));

// --- browser APIs ---
class MockMediaStream {
  private _tracks: unknown[];
  constructor(tracks?: unknown[]) {
    this._tracks = tracks || [];
  }
  getTracks() {
    return this._tracks;
  }
  getAudioTracks() {
    return this._tracks.filter((t: any) => t.kind === 'audio');
  }
  getVideoTracks() {
    return this._tracks.filter((t: any) => t.kind === 'video');
  }
  addTrack(t: unknown) {
    this._tracks.push(t);
  }
}
Object.defineProperty(globalThis, 'MediaStream', {
  value: MockMediaStream,
  writable: true,
  configurable: true,
});

function MockRTCRtpSender() {}
Object.defineProperty(globalThis, 'RTCRtpSender', {
  value: MockRTCRtpSender,
  writable: true,
  configurable: true,
});

if ('RTCRtpScriptTransform' in globalThis) {
  delete (globalThis as Record<string, unknown>)['RTCRtpScriptTransform'];
}

class MockAudioContext {
  state = 'running';
  currentTime = 0;
  sampleRate = 48000;
  createMediaStreamSource = vi.fn().mockReturnValue({ connect: vi.fn(), disconnect: vi.fn() });
  createAnalyser = vi.fn().mockReturnValue({
    fftSize: 0,
    smoothingTimeConstant: 0,
    frequencyBinCount: 128,
    getByteFrequencyData: vi.fn(),
    getByteTimeDomainData: vi.fn(),
    connect: vi.fn(),
    disconnect: vi.fn(),
  });
  createGain = vi.fn().mockReturnValue({
    gain: { value: 1, setTargetAtTime: vi.fn() },
    connect: vi.fn(),
    disconnect: vi.fn(),
  });
  createMediaStreamDestination = vi.fn().mockReturnValue({
    stream: {
      getAudioTracks: vi.fn().mockReturnValue([
        {
          id: 'processed-track',
          kind: 'audio',
          readyState: 'live',
          enabled: true,
          stop: vi.fn(),
          getSettings: vi.fn().mockReturnValue({}),
        },
      ]),
    },
  });
  close = vi.fn().mockResolvedValue(undefined);
}
Object.defineProperty(globalThis, 'AudioContext', {
  value: MockAudioContext,
  writable: true,
  configurable: true,
});

Object.defineProperty(navigator, 'mediaDevices', {
  value: {
    getUserMedia: vi.fn(),
    getDisplayMedia: vi.fn(),
    enumerateDevices: vi.fn().mockResolvedValue([]),
  },
  writable: true,
  configurable: true,
});

// ---------------------------------------------------------------------------
// Import voiceService AFTER all mocks
// ---------------------------------------------------------------------------
const { voiceService } = await import('@/renderer/services/voiceService');
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

interface DecoderCursor {
  reportId: string;
  ssrc: number;
  totalDecodeTime: number;
  framesDecoded: number;
  timestamp: number;
  framesPerSecond: number;
  active?: boolean;
}

function rtcNow(): number {
  return performance.timeOrigin + performance.now();
}

function makeCursor(reportId: string, ssrc: number, framesPerSecond = 30): DecoderCursor {
  return {
    reportId,
    ssrc,
    totalDecodeTime: 0,
    framesDecoded: 0,
    timestamp: rtcNow(),
    framesPerSecond,
    active: true,
  };
}

function addInterval(
  cursor: DecoderCursor,
  timestamp: number,
  decodeMsPerFrame: number,
  frames = 30
): void {
  cursor.totalDecodeTime += (decodeMsPerFrame * frames) / 1_000;
  cursor.framesDecoded += frames;
  cursor.timestamp = timestamp;
}

function elapse(ms = 1_000): number {
  vi.advanceTimersByTime(ms);
  return rtcNow();
}

function statsMap(...cursors: DecoderCursor[]): Map<string, unknown> {
  return new Map(
    cursors.map((cursor) => [
      cursor.reportId,
      {
        id: cursor.reportId,
        type: 'inbound-rtp',
        kind: 'video',
        ssrc: cursor.ssrc,
        totalDecodeTime: cursor.totalDecodeTime,
        framesDecoded: cursor.framesDecoded,
        framesPerSecond: cursor.framesPerSecond,
        timestamp: cursor.timestamp,
        active: cursor.active,
      },
    ])
  );
}

function makeVideoConsumer(
  id: string,
  getStats: () => Map<string, unknown>,
  options: {
    producerId?: string;
    negotiatedSsrc?: number;
    currentLayers?: { spatialLayer: number; temporalLayer: number };
  } = {}
) {
  const state = { paused: false };
  return {
    id,
    kind: 'video' as const,
    get paused() {
      return state.paused;
    },
    closed: false,
    producerId: options.producerId ?? `producer-${id}`,
    track: { id: `track-${id}`, kind: 'video', readyState: 'live', enabled: true, stop: vi.fn() },
    close: vi.fn(),
    pause: vi.fn().mockImplementation(() => {
      state.paused = true;
    }),
    resume: vi.fn().mockImplementation(() => {
      state.paused = false;
    }),
    on: vi.fn(),
    getStats: vi.fn().mockImplementation(async () => getStats()),
    rtpReceiver: { transform: null },
    rtpParameters: {
      encodings: options.negotiatedSsrc === undefined ? [] : [{ ssrc: options.negotiatedSsrc }],
    },
    currentLayers: options.currentLayers,
    setPreferredLayers: vi.fn(),
  };
}

function setupAuth() {
  useAuthStore.getState().setAccessToken('test-token');
  useUserStore.setState({
    user: {
      id: 'user-1',
      username: 'testuser',
      display_name: 'Test User',
      avatar_url: null,
      email: 'test@test.com',
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
    },
  });
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('IGNIS decoder recovery (#1540)', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetAllStores();
    vi.clearAllMocks();
    setupAuth();
    const svc = voiceService as any;
    svc.consumers.clear();
    svc.consumerMeta.clear();
    svc.pauseCoordinator.reset();
    svc.decoderBudgetSampler?.clear();
    if (svc.decoderProfilingTimer) clearInterval(svc.decoderProfilingTimer);
    svc.decoderProfilingTimer = null;
    svc.decoderProfilingInFlight = false;
    svc.consecutiveGreenIntervals = 0;
  });

  afterEach(() => {
    const svc = voiceService as any;
    svc.consumers.clear();
    svc.consumerMeta.clear();
    svc.pauseCoordinator.reset();
    svc.decoderBudgetSampler?.clear();
    if (svc.decoderProfilingTimer) clearInterval(svc.decoderProfilingTimer);
    svc.decoderProfilingTimer = null;
    svc.decoderProfilingInFlight = false;
    svc.consecutiveGreenIntervals = 0;
    vi.useRealTimers();
  });

  it('observes paused video synchronously without yielding the consumer pass', async () => {
    const svc = voiceService as any;
    const paused = makeVideoConsumer('paused-camera', () => new Map());
    paused.pause();
    svc.consumers.set(paused.id, paused);
    const observeSpy = vi.spyOn(svc.decoderBudgetSampler, 'observe');

    const profiling = svc.profileDecoders();

    expect(observeSpy).toHaveBeenCalledTimes(1);
    expect(paused.getStats).not.toHaveBeenCalled();
    await profiling;
  });

  it('uses another active green consumer to recover an IGNIS-paused consumer', async () => {
    const svc = voiceService as any;
    const pausedCursor = makeCursor('paused-report', 10);
    const greenCursor = makeCursor('green-report', 20);
    const paused = makeVideoConsumer('paused-camera', () => statsMap(pausedCursor), {
      negotiatedSsrc: 10,
    });
    const active = makeVideoConsumer('active-camera', () => statsMap(greenCursor), {
      negotiatedSsrc: 20,
    });
    svc.consumers.set(paused.id, paused);
    svc.consumers.set(active.id, active);
    svc.pauseLowestPriorityConsumer(paused);

    await svc.profileDecoders(); // active consumer baseline; paused consumer is unknown
    expect(paused.paused).toBe(true);
    expect(svc.consecutiveGreenIntervals).toBe(0);

    for (let greenCycle = 0; greenCycle < 3; greenCycle++) {
      addInterval(greenCursor, elapse(), 5);
      await svc.profileDecoders();
    }

    expect(active.pause).not.toHaveBeenCalled();
    expect(paused.resume).toHaveBeenCalledTimes(1);
    expect(paused.paused).toBe(false);
  });

  it('keeps the sole active video consumer decoding when RED at its lowest layers', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('sole-red-report', 25);
    const consumer = makeVideoConsumer('sole-camera', () => statsMap(cursor), {
      negotiatedSsrc: 25,
      currentLayers: { spatialLayer: 0, temporalLayer: 0 },
    });
    svc.consumers.set(consumer.id, consumer);

    await svc.profileDecoders();
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders();

    expect(useVoiceStore.getState().decoderHealth).toBe('red');
    expect(consumer.pause).not.toHaveBeenCalled();
    expect(consumer.paused).toBe(false);
    expect(svc.pauseCoordinator.hasReason(consumer.id, 'ignis')).toBe(false);
  });

  it('runs only one interval-triggered decoder profile while getStats is pending', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('deferred-report', 26);
    let releaseStats!: () => void;
    const statsPending = new Promise<void>((resolve) => {
      releaseStats = resolve;
    });
    const consumer = makeVideoConsumer('deferred-camera', () => statsMap(cursor), {
      negotiatedSsrc: 26,
    });
    consumer.getStats.mockImplementation(async () => {
      await statsPending;
      return statsMap(cursor);
    });
    svc.consumers.set(consumer.id, consumer);

    try {
      svc.startDecoderBudgetProfiling();
      vi.advanceTimersByTime(5_000);
      await Promise.resolve();
      expect(consumer.getStats).toHaveBeenCalledTimes(1);

      vi.advanceTimersByTime(5_000);
      await Promise.resolve();
      expect(consumer.getStats).toHaveBeenCalledTimes(1);

      releaseStats();
      for (let turn = 0; turn < 5; turn++) await Promise.resolve();
      expect(svc.decoderProfilingInFlight).toBe(false);

      vi.advanceTimersByTime(5_000);
      await Promise.resolve();
      expect(consumer.getStats).toHaveBeenCalledTimes(2);
    } finally {
      releaseStats();
      if (svc.decoderProfilingTimer) clearInterval(svc.decoderProfilingTimer);
      svc.decoderProfilingTimer = null;
    }
  });

  it('does not report green or advance recovery when every video sample is unknown', async () => {
    const svc = voiceService as any;
    const malformed = makeVideoConsumer(
      'malformed-camera',
      () =>
        new Map([
          [
            'malformed-active',
            {
              id: 'malformed-active',
              type: 'inbound-rtp',
              kind: 'video',
              active: true,
              ssrc: 30,
              totalDecodeTime: Number.NaN,
              framesDecoded: 10,
              framesPerSecond: 30,
              timestamp: rtcNow(),
            },
          ],
          [
            'valid-inactive',
            {
              id: 'valid-inactive',
              type: 'inbound-rtp',
              kind: 'video',
              active: false,
              ssrc: 30,
              totalDecodeTime: 1,
              framesDecoded: 100,
              framesPerSecond: 30,
              timestamp: rtcNow(),
            },
          ],
        ]),
      { negotiatedSsrc: 30 }
    );
    svc.consumers.set(malformed.id, malformed);
    useVoiceStore.getState().setDecoderHealth('red');
    svc.consecutiveGreenIntervals = 2;

    await svc.profileDecoders();

    expect(useVoiceStore.getState().decoderHealth).toBe('red');
    expect(svc.consecutiveGreenIntervals).toBe(2);
  });

  it('selects one active report by negotiated SSRC before FPS, timestamp, and stable id', async () => {
    const svc = voiceService as any;
    const staleRed = makeCursor('a-stale-red', 41);
    const selectedGreen = makeCursor('z-negotiated-green', 42);
    staleRed.totalDecodeTime = 4;
    staleRed.framesDecoded = 100;
    staleRed.timestamp += 1;
    const consumer = makeVideoConsumer('rollover-camera', () => statsMap(staleRed, selectedGreen), {
      negotiatedSsrc: 42,
    });
    svc.consumers.set(consumer.id, consumer);

    await svc.profileDecoders();
    const timestamp = elapse();
    addInterval(staleRed, timestamp, 40);
    addInterval(selectedGreen, timestamp, 5);
    await svc.profileDecoders();

    expect(consumer.pause).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().decoderHealth).toBe('green');
  });

  it('aggregates screen and camera consumers using the worst usable interval', async () => {
    const svc = voiceService as any;
    const screenCursor = makeCursor('screen-report', 51);
    const cameraCursor = makeCursor('camera-report', 52);
    const screen = makeVideoConsumer('screen-consumer', () => statsMap(screenCursor), {
      producerId: 'screen-producer',
      negotiatedSsrc: 51,
    });
    const camera = makeVideoConsumer('camera-consumer', () => statsMap(cameraCursor), {
      negotiatedSsrc: 52,
    });
    svc.consumers.set(screen.id, screen);
    svc.consumers.set(camera.id, camera);
    useVoiceStore.getState().tuneIn('screen-producer', screen.id);

    await svc.profileDecoders();
    const timestamp = elapse();
    addInterval(screenCursor, timestamp, 5);
    addInterval(cameraCursor, timestamp, 40);
    await svc.profileDecoders();

    expect(useVoiceStore.getState().decoderHealth).toBe('red');
    expect(camera.pause).toHaveBeenCalledTimes(1);
    expect(screen.pause).not.toHaveBeenCalled();
  });

  it('starts a fresh sampling segment after a layer intervention', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('layered-report', 60);
    const consumer = makeVideoConsumer('layered-camera', () => statsMap(cursor), {
      negotiatedSsrc: 60,
      currentLayers: { spatialLayer: 1, temporalLayer: 0 },
    });
    svc.consumers.set(consumer.id, consumer);

    await svc.profileDecoders();
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders();
    expect(consumer.setPreferredLayers).toHaveBeenCalledTimes(1);

    addInterval(cursor, elapse(), 5);
    await svc.profileDecoders(); // fresh baseline: stale RED history must not fire again
    expect(consumer.setPreferredLayers).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().decoderHealth).toBe('red');

    addInterval(cursor, elapse(), 5);
    await svc.profileDecoders();
    expect(consumer.setPreferredLayers).toHaveBeenCalledTimes(1);
    expect(useVoiceStore.getState().decoderHealth).toBe('green');
  });

  it('clears sampler and single-flight state during session cleanup', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('cleanup-report', 70);
    const consumer = makeVideoConsumer('cleanup-camera', () => statsMap(cursor), {
      negotiatedSsrc: 70,
    });
    svc.consumers.set(consumer.id, consumer);
    const deleteSpy = vi.spyOn(svc.decoderBudgetSampler, 'deleteConsumer');
    const clearSpy = vi.spyOn(svc.decoderBudgetSampler, 'clear');

    svc.closeConsumerAndNotify(consumer.id);
    expect(deleteSpy).toHaveBeenCalledWith(consumer.id);

    svc.decoderProfilingInFlight = true;
    svc.cleanupTimersAndE2EE();
    expect(svc.decoderProfilingInFlight).toBe(false);

    svc.decoderProfilingInFlight = true;
    await svc.cleanup();
    expect(clearSpy).toHaveBeenCalled();
    expect(svc.decoderProfilingInFlight).toBe(false);
  });
});
