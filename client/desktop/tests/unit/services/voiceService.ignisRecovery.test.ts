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
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

// --- e2eeService ---
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
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
vi.mock('@/renderer/services/e2ee/mediaEncryption', () => ({
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
vi.mock('@/renderer/stores/voice/osPermissionStore', () => ({
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
const { voiceService } = await import('@/renderer/services/voice/voiceService');
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';

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
  };
}

/** Opt-in: give a consumer the production context camera pressure needs.
 *  Without this call, tryEmitCameraPressureLayerRequest bails at its first guard —
 *  which is what makes a vacuous test visible at the call site. */
function registerCameraPressureContext(
  svc: any,
  userId: string,
  consumerId: string,
  opts: {
    gateEnabled?: boolean;
    role?: 'thumbnail' | 'grid' | 'focus';
    cssWidth?: number;
    cssHeight?: number;
  } = {}
) {
  // consumerMeta's value shape is exactly these three fields (voiceService.ts:542-545).
  svc.consumerMeta.set(consumerId, {
    source: 'camera',
    producerUserId: userId,
    producerId: `producer-${consumerId}`,
  });
  // RemoteVideoTileRenderState is exactly these FIVE fields. devicePixelRatio is NOT
  // one of them — layerPayloadForTileState supplies it from remoteVideoDevicePixelRatio()
  // at call time. Adding it here would model a shape production never stores.
  svc.remoteVideoRenderStateByUser.set(
    userId,
    new Map([
      [
        'tile-1',
        {
          visible: true,
          cssWidth: opts.cssWidth ?? 640,
          cssHeight: opts.cssHeight ?? 360,
          role: opts.role ?? 'grid',
          focusedWindow: true,
        },
      ],
    ])
  );
  svc.cameraLayeringEnabled = opts.gateEnabled ?? true;
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
    // IGNIS camera-pressure context (#3094): a real socket spy so
    // tryEmitCameraPressureLayerRequest's `!this.socket` guard doesn't fallback
    // vacuously, plus a clean slate for the per-user pressure/render-state/gate
    // state registerCameraPressureContext writes into.
    svc.socket = mockSocket;
    svc.remoteVideoPressureByUser?.clear();
    svc.remoteVideoRenderStateByUser?.clear();
    svc.lastPreferredLayerKeyByConsumer?.clear();
    svc.cameraLayeringEnabled = false;
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
    svc.socket = null;
    svc.remoteVideoPressureByUser?.clear();
    svc.remoteVideoRenderStateByUser?.clear();
    svc.lastPreferredLayerKeyByConsumer?.clear();
    svc.cameraLayeringEnabled = false;
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
    });
    svc.consumers.set(consumer.id, consumer);
    // Thumbnail role forces spatial 0 unconditionally (L2: no step left), so the
    // render-demand path falls back and pauseLowestPriorityConsumer is genuinely
    // reached — not vacuously, via a missing-consumerMeta bail at tryEmit's first guard.
    registerCameraPressureContext(svc, 'sole-user', consumer.id, { role: 'thumbnail' });

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
    });
    svc.consumers.set(consumer.id, consumer);
    // Headroom available (default grid role, 640x360): the render-demand path emits
    // instead of the deleted client-side setPreferredLayers clamp this test used to
    // witness. deleteConsumer(consumer.id) on that emit is what resets the sampler
    // segment — the reset is witnessed below through the tick-3 decoderHealth
    // transition (was already asserted pre-#3094) plus the absence of a repeat emit.
    registerCameraPressureContext(svc, 'layered-user', consumer.id);
    const setPreferredLayerEmits = () =>
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

    await svc.profileDecoders();
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders();
    expect(setPreferredLayerEmits()).toHaveLength(1);

    addInterval(cursor, elapse(), 5);
    await svc.profileDecoders(); // fresh baseline (blind tick): stale RED history must not fire again
    expect(setPreferredLayerEmits()).toHaveLength(1);
    expect(useVoiceStore.getState().decoderHealth).toBe('red');

    addInterval(cursor, elapse(), 5);
    await svc.profileDecoders();
    expect(setPreferredLayerEmits()).toHaveLength(1);
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

  // #3094 — camera decoder pressure now runs through the server-authoritative
  // render-demand path instead of the dead browser-side layer cast. Each case pairs
  // the value PASSED (the emit) with the value OBEYED (pauseCoordinator.hasReason),
  // per [internal]rules/tests.md's founding rule.
  it('emits one pressure request, then escalates to pause on the next classified red tick', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('esc-cam-report', 80);
    const cam = makeVideoConsumer('esc-cam', () => statsMap(cursor), { negotiatedSsrc: 80 });
    const other = makeVideoConsumer('esc-other', () => new Map());
    svc.consumers.set(cam.id, cam);
    svc.consumers.set(other.id, other);
    registerCameraPressureContext(svc, 'esc-user', cam.id);
    const setPreferredLayerEmits = () =>
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

    await svc.profileDecoders(); // baseline — unknown

    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders(); // classified RED, tick 1 — headroom available, emits

    expect(setPreferredLayerEmits()).toHaveLength(1);
    expect(setPreferredLayerEmits()[0][1]).toMatchObject({ pressureStepDown: true });
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);

    addInterval(cursor, elapse(), 40); // blind tick — sampler was reset by the emit
    await svc.profileDecoders();

    expect(setPreferredLayerEmits()).toHaveLength(1);
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);

    addInterval(cursor, elapse(), 40); // next classified RED tick — pressure already spent (O2)
    await svc.profileDecoders();

    expect(setPreferredLayerEmits()).toHaveLength(1);
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(true);
  });

  it('pauses on the FIRST red tick when the room camera-layering gate is off (L1)', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('l1-cam-report', 82);
    const cam = makeVideoConsumer('l1-cam', () => statsMap(cursor), { negotiatedSsrc: 82 });
    const other = makeVideoConsumer('l1-other', () => new Map());
    svc.consumers.set(cam.id, cam);
    svc.consumers.set(other.id, other);
    registerCameraPressureContext(svc, 'l1-user', cam.id, { gateEnabled: false });

    await svc.profileDecoders();
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders();

    expect(
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers')
    ).toHaveLength(0);
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(true);
  });

  it('pauses on the FIRST red tick when policy has no step left (L2)', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('l2-cam-report', 84);
    const cam = makeVideoConsumer('l2-cam', () => statsMap(cursor), { negotiatedSsrc: 84 });
    const other = makeVideoConsumer('l2-other', () => new Map());
    svc.consumers.set(cam.id, cam);
    svc.consumers.set(other.id, other);
    // Thumbnail role forces spatial 0 unconditionally: pressured === unpressured by
    // construction, independent of size or device-pixel ratio.
    registerCameraPressureContext(svc, 'l2-user', cam.id, { role: 'thumbnail' });

    await svc.profileDecoders();
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders();

    expect(
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers')
    ).toHaveLength(0);
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(true);
  });

  // #3094 YELLOW — the arm this change actually alters. Pre-#3094 handleYellowZone was a
  // guaranteed no-op (it called the dead clampRemoteVideoLayer path and returned); it now
  // emits a real demand AND spends the same one-step-per-user budget red's escalation reads.
  // Nothing in the repo drove rho into [0.80, 0.925) before these two, so reverting the body
  // to `return mergeDecoderZones(worstZone, 'yellow')` passed the whole suite.
  // 28 ms/frame at 30 fps ⇒ rho = 28 × 30 / 1000 = 0.84 — inside the yellow band.
  it('steps down without pausing on a yellow tick', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('yellow-cam-report', 86);
    const cam = makeVideoConsumer('yellow-cam', () => statsMap(cursor), { negotiatedSsrc: 86 });
    const other = makeVideoConsumer('yellow-other', () => new Map());
    svc.consumers.set(cam.id, cam);
    svc.consumers.set(other.id, other);
    registerCameraPressureContext(svc, 'yellow-user', cam.id, { role: 'focus' });
    const setPreferredLayerEmits = () =>
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

    await svc.profileDecoders(); // baseline — unknown

    addInterval(cursor, elapse(), 28);
    await svc.profileDecoders(); // classified YELLOW

    // Value PASSED: exactly one demand, carrying the pressure flag.
    expect(setPreferredLayerEmits()).toHaveLength(1);
    expect(setPreferredLayerEmits()[0][1]).toMatchObject({ pressureStepDown: true });
    // Value OBEYED: yellow steps down and never pauses — that distinction is the whole arm.
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);
    expect(useVoiceStore.getState().decoderHealth).toBe('yellow');
    // Monotone: the pressured demand may only ask for a SMALLER layer, never a larger one.
    const unpressured = svc.computePreferredLayerPayloadForUser('yellow-user', false);
    expect(setPreferredLayerEmits()[0][1].spatialLayer).toBeLessThan(unpressured.spatialLayer);
  });

  it('spends the pressure budget on yellow, so the next classified red tick pauses at once', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('y2r-cam-report', 88);
    const cam = makeVideoConsumer('y2r-cam', () => statsMap(cursor), { negotiatedSsrc: 88 });
    const other = makeVideoConsumer('y2r-other', () => new Map());
    svc.consumers.set(cam.id, cam);
    svc.consumers.set(other.id, other);
    registerCameraPressureContext(svc, 'y2r-user', cam.id, { role: 'focus' });
    const setPreferredLayerEmits = () =>
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

    await svc.profileDecoders(); // baseline — unknown

    addInterval(cursor, elapse(), 28);
    await svc.profileDecoders(); // classified YELLOW — emits, spends O2, resets the sampler

    expect(setPreferredLayerEmits()).toHaveLength(1);
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);

    addInterval(cursor, elapse(), 40); // blind tick — re-baselines after the reset
    await svc.profileDecoders();

    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders(); // FIRST classified RED since yellow — no step left to take

    // No second emit, and the pause lands on that first red tick rather than a later one.
    expect(setPreferredLayerEmits()).toHaveLength(1);
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(true);
  });
});
