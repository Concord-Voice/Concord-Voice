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
import { useSubscriptionStore, FREE_ENTITLEMENT } from '@/renderer/stores/auth/subscriptionStore';
import { effectiveCameraSpatialCap } from '@/renderer/utils/policy/videoLimits';

/** Any value above the free ceiling selects the premium cap -- effectiveCameraSpatialCap
 *  compares against FREE_ENTITLEMENT.maxManualBitrateBps rather than matching a tier
 *  string, mirroring maxCameraSpatialLayerForParticipant on the SFU. */
const PREMIUM_MANUAL_BITRATE_BPS = FREE_ENTITLEMENT.maxManualBitrateBps * 2;

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
    const unpressured = svc.computePreferredLayerPayloadForUser('yellow-user', 0);
    expect(setPreferredLayerEmits()[0][1].spatialLayer).toBeLessThan(unpressured.spatialLayer);
  });

  it("pauses on the next red tick once yellow spent the tile's ONLY step", async () => {
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
  // Multi-step pressure. A 1920x1080 focus tile computes to spatial layer 2, so it
  // can absorb TWO steps before a step stops being a step. The single-bit design
  // could only ever take one and then pause, which is why these cases needed a
  // deeper tile than any existing fixture provides.
  function registerDeepTile(svc: any, userId: string, consumerId: string) {
    registerCameraPressureContext(svc, userId, consumerId, {
      role: 'focus',
      cssWidth: 1920,
      cssHeight: 1080,
    });
  }

  it('red steps twice before it pauses anything', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('ms-cam-report', 92);
    const cam = makeVideoConsumer('ms-cam', () => statsMap(cursor), { negotiatedSsrc: 92 });
    const other = makeVideoConsumer('ms-other', () => new Map());
    svc.consumers.set(cam.id, cam);
    svc.consumers.set(other.id, other);
    registerDeepTile(svc, 'ms-user', cam.id);
    const emits = () =>
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

    await svc.profileDecoders(); // baseline
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders(); // RED 1 -> step 1

    expect(emits()).toHaveLength(1);
    expect(emits()[0][1]).toMatchObject({ spatialLayer: 1, pressureStepDown: true });
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);

    addInterval(cursor, elapse(), 40); // blind re-baseline after the sampler reset
    await svc.profileDecoders();
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders(); // RED 2 -> step 2, NOT a pause

    expect(emits()).toHaveLength(2);
    expect(emits()[1][1]).toMatchObject({ spatialLayer: 0, pressureStepDown: true });
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);
    expect(svc.remoteVideoPressureByUser.get('ms-user')).toBe(2);

    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders();
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders(); // RED 3 -> no step left, pause

    expect(emits()).toHaveLength(2);
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(true);
  });

  it('yellow reaches step 1 and stops there, however long it stays yellow', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('yc-cam-report', 94);
    const cam = makeVideoConsumer('yc-cam', () => statsMap(cursor), { negotiatedSsrc: 94 });
    const other = makeVideoConsumer('yc-other', () => new Map());
    svc.consumers.set(cam.id, cam);
    svc.consumers.set(other.id, other);
    registerDeepTile(svc, 'yc-user', cam.id);
    const emits = () =>
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

    await svc.profileDecoders();
    addInterval(cursor, elapse(), 28);
    await svc.profileDecoders(); // YELLOW 1 -> step 1

    expect(emits()).toHaveLength(1);
    expect(emits()[0][1]).toMatchObject({ spatialLayer: 1 });

    addInterval(cursor, elapse(), 28);
    await svc.profileDecoders();
    addInterval(cursor, elapse(), 28);
    await svc.profileDecoders(); // YELLOW 2 -> capped, and yellow never pauses

    expect(emits()).toHaveLength(1);
    expect(svc.remoteVideoPressureByUser.get('yc-user')).toBe(1);
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);
  });

  it('lets red escalate past the step yellow already took', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('yr-cam-report', 96);
    const cam = makeVideoConsumer('yr-cam', () => statsMap(cursor), { negotiatedSsrc: 96 });
    const other = makeVideoConsumer('yr-other', () => new Map());
    svc.consumers.set(cam.id, cam);
    svc.consumers.set(other.id, other);
    registerDeepTile(svc, 'yr-user', cam.id);
    const emits = () =>
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

    await svc.profileDecoders();
    addInterval(cursor, elapse(), 28);
    await svc.profileDecoders(); // YELLOW -> step 1
    expect(emits()).toHaveLength(1);

    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders();
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders(); // RED -> step 2, an escalation rather than a pause

    expect(emits()).toHaveLength(2);
    expect(emits()[1][1]).toMatchObject({ spatialLayer: 0 });
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);
    // Distinguishable (R4): step 2 is a depth yellow structurally cannot reach,
    // so this payload could only have come from red.
    expect(emits()[1][1].spatialLayer).toBeLessThan(emits()[0][1].spatialLayer);
  });

  // Gitar (#3279 review): red-at-cap followed by a YELLOW tick. The other two
  // multi-step cases cover yellow->red and red->red->red; this is the third
  // ordering, and it is the one where yellow's cap could plausibly UNDO red's
  // escalation — `nextSteps = 1` for yellow while currentSteps is already 2, so
  // the `nextSteps <= currentSteps` guard is the only thing stopping a yellow
  // tick from walking the demand back up to layer 1 after red drove it to 0.
  it('does not let a yellow tick walk back a step red already took', async () => {
    const svc = voiceService as any;
    const cursor = makeCursor('ry-cam-report', 104);
    const cam = makeVideoConsumer('ry-cam', () => statsMap(cursor), { negotiatedSsrc: 104 });
    const other = makeVideoConsumer('ry-other', () => new Map());
    svc.consumers.set(cam.id, cam);
    svc.consumers.set(other.id, other);
    registerDeepTile(svc, 'ry-user', cam.id);
    const emits = () =>
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

    await svc.profileDecoders(); // baseline
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders(); // RED 1 -> step 1
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders(); // blind re-baseline
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders(); // RED 2 -> step 2 (the cap for this tile)

    expect(emits()).toHaveLength(2);
    expect(svc.remoteVideoPressureByUser.get('ry-user')).toBe(2);

    addInterval(cursor, elapse(), 28);
    await svc.profileDecoders(); // blind
    addInterval(cursor, elapse(), 28);
    await svc.profileDecoders(); // YELLOW at cap -> must be a no-op

    // No third emit, the depth is unchanged, and yellow still never pauses.
    expect(emits()).toHaveLength(2);
    expect(emits()[1][1].spatialLayer).toBe(0);
    expect(svc.remoteVideoPressureByUser.get('ry-user')).toBe(2);
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);
  });

  // The entitlement cap, at the INTEGRATION seam rather than in the policy.
  //
  // Every other test in this file leaves the subscription store unhydrated, so
  // `shouldEnforceForSubscription` fails open and the cap is 2 — which is why
  // they all passed before and after the fix, and why this case has to set the
  // store explicitly. Without it the production change is untested.
  //
  // Free viewers are capped at spatial layer 1 by the SFU
  // (`maxCameraSpatialLayerForParticipant`). Before #3279 the client computed
  // its base from the unclamped ladder (2), so the first pressure step asked for
  // 1 — the layer the server was already forwarding — and IGNIS recorded a step
  // that reduced nothing (Codex, #3279).
  it('lands a free viewer first pressure step on a layer the SFU was not already sending', async () => {
    useSubscriptionStore.setState({
      entitlement: { ...FREE_ENTITLEMENT },
      hydrated: true,
      degraded: false,
    });

    const svc = voiceService as any;
    const cursor = makeCursor('cap-cam-report', 96);
    const cam = makeVideoConsumer('cap-cam', () => statsMap(cursor), { negotiatedSsrc: 96 });
    const other = makeVideoConsumer('cap-other', () => new Map());
    svc.consumers.set(cam.id, cam);
    svc.consumers.set(other.id, other);
    registerCameraPressureContext(svc, 'cap-user', cam.id, {
      role: 'focus',
      cssWidth: 1920,
      cssHeight: 1080,
    });
    const emits = () =>
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

    // The unpressured base is already the CAP, not the ladder value.
    expect(svc.computePreferredLayerPayloadForUser('cap-user', 0).spatialLayer).toBe(1);

    await svc.profileDecoders(); // baseline
    addInterval(cursor, elapse(), 40);
    await svc.profileDecoders(); // RED

    // One step, and it goes BELOW what the SFU was forwarding — the whole point.
    expect(emits()).toHaveLength(1);
    expect(emits()[0][1].spatialLayer).toBe(0);
    expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);
  });

  // The other half of that cap: it is CAMERA-only, and the same helper serves
  // both screen paths.
  //
  // `validateAndClampLayerDemand` (roomManager.ts:2804) reads
  // `source === 'screen' ? 2 : maxCameraSpatialLayerForParticipant(...)`, so the
  // SFU hands screen consumers the full range no matter what the viewer's camera
  // entitlement is. Applying the camera cap to a screen demand therefore asks for
  // LESS than the server was offering — the free viewer's full-stage and PiP
  // screen shares pinned one layer below what was on the wire. That regression
  // was introduced by the fix above and caught by Codex on #3279.
  //
  // The camera assertion at the end is not decoration: with the store failing
  // open this whole case passes for the wrong reason, since the cap would be 2
  // everywhere. It is what proves the store is genuinely hydrated-free.
  it('keeps the camera cap off screen demand, on both the full-stage and PiP paths', () => {
    useSubscriptionStore.setState({
      entitlement: { ...FREE_ENTITLEMENT },
      hydrated: true,
      degraded: false,
    });

    const svc = voiceService as any;
    const largeTile = {
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      role: 'focus' as const,
      focusedWindow: true,
    };

    // Full-stage screen — computePreferredLayerPayloadForUser(..., 'screen').
    svc.remoteScreenRenderStateByUser.set('scr-user', new Map([['stage', largeTile]]));
    expect(svc.computePreferredLayerPayloadForUser('scr-user', undefined, 'screen')).toMatchObject({
      spatialLayer: 2,
      pressureStepDown: false,
    });

    // PiP screen — the explicit-consumer path, which never sees a userId at all.
    svc.emitPreferredLayersForConsumer('pip-consumer', largeTile);
    const emits = mockSocket.emit.mock.calls.filter(
      ([event]: [string]) => event === 'set-preferred-layers'
    );
    expect(emits).toHaveLength(1);
    expect(emits[0][1]).toMatchObject({ consumerId: 'pip-consumer', spatialLayer: 2 });

    // Same store, same tile geometry, CAMERA source: still capped at 1.
    registerCameraPressureContext(svc, 'scr-user', 'scr-cam', {
      role: 'focus',
      cssWidth: 1920,
      cssHeight: 1080,
    });
    expect(svc.computePreferredLayerPayloadForUser('scr-user', 0).spatialLayer).toBe(1);
  });

  // One user, TWO camera consumers. This is not contrived: a PiP window consumes
  // the same producer on a second recv transport (media-plane.md — `consume` is
  // idempotent per (participant, producerId, recvTransportId), three parts
  // precisely so this case is legal), and profileDecoders classifies every video
  // consumer in turn. The pressure LEDGER is keyed by producing user, so without
  // a per-pass budget two red consumers spend two steps in a single pass and a
  // third reads "no step left" and pauses someone who was just stepped twice
  // (CodeRabbit, #3279).
  it('spends at most one step per producing user per pass, and does not pause the rest', async () => {
    const svc = voiceService as any;
    const cursorA = makeCursor('dup-a-report', 94);
    const cursorB = makeCursor('dup-b-report', 95);
    // Both consumers carry the SAME producerUserId — registerCameraPressureContext
    // writes consumerMeta per consumer id, so calling it twice models one user
    // rendered through two consumers rather than two users.
    const camA = makeVideoConsumer('dup-a', () => statsMap(cursorA), { negotiatedSsrc: 94 });
    const camB = makeVideoConsumer('dup-b', () => statsMap(cursorB), { negotiatedSsrc: 95 });
    svc.consumers.set(camA.id, camA);
    svc.consumers.set(camB.id, camB);
    registerCameraPressureContext(svc, 'dup-user', camA.id, {
      role: 'focus',
      cssWidth: 1920,
      cssHeight: 1080,
    });
    registerCameraPressureContext(svc, 'dup-user', camB.id, {
      role: 'focus',
      cssWidth: 1920,
      cssHeight: 1080,
    });
    const emits = () =>
      mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

    await svc.profileDecoders(); // baseline for both
    addInterval(cursorA, elapse(), 40);
    addInterval(cursorB, rtcNow(), 40);
    await svc.profileDecoders(); // BOTH classify RED in one pass

    // Exactly one step in the LEDGER -- but BOTH consumers addressed. The budget
    // bounds how much quality is given up, not how many consumers hear about it:
    // SFU layer demand is consumer-scoped, so a sibling left unaddressed keeps
    // decoding at full quality and the step buys no relief (Codex, #3279).
    expect(svc.remoteVideoPressureByUser.get('dup-user')).toBe(1);
    expect(emits()).toHaveLength(2);
    expect(new Set(emits().map(([, p]: [string, any]) => p.consumerId))).toEqual(
      new Set([camA.id, camB.id])
    );
    // And the second consumer is NOT treated as a pressure failure: nothing is
    // paused, because this user's relief is already in flight.
    expect(svc.pauseCoordinator.hasReason(camA.id, 'ignis')).toBe(false);
    expect(svc.pauseCoordinator.hasReason(camB.id, 'ignis')).toBe(false);

    // The NEXT pass may step again -- the budget bounds one pass, not the session --
    // and again addresses both consumers.
    addInterval(cursorA, elapse(), 40);
    addInterval(cursorB, rtcNow(), 40);
    await svc.profileDecoders();
    expect(svc.remoteVideoPressureByUser.get('dup-user')).toBe(2);
    expect(emits()).toHaveLength(4);
  });

  // The sibling's sample segment is dropped too, not just the triggering one.
  //
  // `handleRedZone` has always cleared the consumer that classified red. The
  // DEFERRED sibling was left holding the history it accumulated BEFORE the step,
  // so the ladder could advance again on evidence gathered before the reduction it
  // is supposed to be measuring (Codex, #3279).
  //
  // The discriminator has to be GREEN post-step evidence, and finding that took a
  // surviving mutant: an earlier version asserted the triggering consumer was
  // absent from the sampler's state map, which `handleRedZone` already guaranteed
  // on its own -- it passed with the per-consumer reset removed. p95 is nearest-rank
  // over retained intervals, so a sibling that kept its pre-step 40 ms samples still
  // reads red however quiet the link has since become.
  it('drops the deferred sibling segment too, so quiet post-step evidence reads green', async () => {
    const svc = voiceService as any;
    const cursorA = makeCursor('ev-a-report', 92);
    const cursorB = makeCursor('ev-b-report', 93);
    const camA = makeVideoConsumer('ev-a', () => statsMap(cursorA), { negotiatedSsrc: 92 });
    const camB = makeVideoConsumer('ev-b', () => statsMap(cursorB), { negotiatedSsrc: 93 });
    svc.consumers.set(camA.id, camA);
    svc.consumers.set(camB.id, camB);
    for (const id of [camA.id, camB.id]) {
      registerCameraPressureContext(svc, 'ev-user', id, {
        role: 'focus',
        cssWidth: 1920,
        cssHeight: 1080,
      });
    }

    await svc.profileDecoders(); // baseline
    addInterval(cursorA, elapse(), 40);
    addInterval(cursorB, rtcNow(), 40);
    await svc.profileDecoders(); // RED -> step 1, both addressed, both segments dropped
    expect(svc.remoteVideoPressureByUser.get('ev-user')).toBe(1);

    // The link is quiet now. Every interval from here is comfortably green, so
    // nothing should climb the ladder -- unless a stale 40 ms sample survived.
    for (let i = 0; i < 3; i++) {
      addInterval(cursorA, elapse(), 4);
      addInterval(cursorB, rtcNow(), 4);
      await svc.profileDecoders();
    }
    // Three green cycles is the recovery threshold, so the ledger is CLEARED rather
    // than merely held -- quiet evidence all the way back to zero. With the sibling
    // still holding its 40 ms samples there is no green cycle at all: it reads red
    // and the ladder climbs to 2 instead.
    expect(svc.remoteVideoPressureByUser.get('ev-user') ?? 0).toBe(0);
    expect(svc.pauseCoordinator.hasReason(camB.id, 'ignis')).toBe(false);
  });

  // Step down as a group, restore as a group.
  //
  // The step-down addresses every camera consumer a user has, because SFU layer
  // demand is consumer-scoped. The RESTORE ran through the singular
  // `findCameraConsumerIdForUser` and therefore released only the first, leaving
  // every sibling pinned at the stepped-down layer for the rest of the session --
  // with the pressure ledger reading zero, so nothing would ever try again
  // (Codex, #3279). The asymmetry is the bug; neither half is wrong alone.
  it('restores every camera consumer it stepped down, not just the first', () => {
    const svc = voiceService as any;
    for (const id of ['res-a', 'res-b']) {
      registerCameraPressureContext(svc, 'res-user', id, {
        role: 'focus',
        cssWidth: 1920,
        cssHeight: 1080,
      });
    }
    // Two steps deep, as a red segment would have left it.
    svc.remoteVideoPressureByUser.set('res-user', 2);

    svc.clearRemoteVideoPressureAndEmit();

    const emits = mockSocket.emit.mock.calls.filter(
      ([event]: [string]) => event === 'set-preferred-layers'
    );
    expect(emits).toHaveLength(2);
    expect(new Set(emits.map(([, p]: [string, any]) => p.consumerId))).toEqual(
      new Set(['res-a', 'res-b'])
    );
    // Restored, not merely re-addressed: the ledger is cleared and the payloads
    // carry no pressure.
    expect(svc.remoteVideoPressureByUser.has('res-user')).toBe(false);
    for (const [, payload] of emits) {
      expect(payload.pressureStepDown).toBe(false);
    }
  });

  // A screen RELEASE must not carry camera pressure.
  //
  // `remoteVideoPressureByUser` is keyed by producing USER, not by source, so the
  // default `pressureSteps` parameter of layerPayloadForTileState resolves a
  // camera step count for a screen payload. Every other screen call site passes 0
  // explicitly; the last-unmount release passed `undefined` and therefore stepped
  // the release payload down by however many camera steps that user was under,
  // persisting a skewed storedDemand on the SFU (Gitar, #3279).
  it('does not let camera pressure steps leak into a screen release payload', () => {
    const svc = voiceService as any;
    svc.consumerMeta.set('scr-consumer', {
      source: 'screen',
      producerUserId: 'leak-user',
      producerId: 'producer-scr',
    });
    // This user is two camera steps deep.
    svc.remoteVideoPressureByUser.set('leak-user', 2);

    svc.releaseScreenDemandOnLastUnmount('leak-user', {
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      role: 'focus',
      focusedWindow: true,
    });

    const emits = mockSocket.emit.mock.calls.filter(
      ([event]: [string]) => event === 'set-preferred-layers'
    );
    expect(emits).toHaveLength(1);
    // visible:false is the release itself; pressureStepDown:false is the property
    // under test -- with the default parameter it arrived true.
    expect(emits[0][1]).toMatchObject({
      consumerId: 'scr-consumer',
      visible: false,
      pressureStepDown: false,
    });
  });

  // The cap belongs to the MEDIA SESSION, not to the live store.
  //
  // `Participant.tier` is written once at join (roomManager.ts:1412-1413) and
  // `tierchange.go` deliberately leaves the SFU session alone on a tier change --
  // it broadcasts the new entitlement to every connected client and says in its
  // own comment that media-plane enforcement is deferred. So an upgrade mid-call
  // raises a live-store read to 2 while the SFU still clamps to 1, and the first
  // red step asks for the layer already being forwarded: the exact defect the cap
  // was added to close, reached through the upgrade path (Codex, #3279).
  it('holds the session cap across a mid-call upgrade, and falls back before a session', () => {
    const svc = voiceService as any;
    const tile = {
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      role: 'focus' as const,
      focusedWindow: true,
    };
    svc.remoteVideoRenderStateByUser.set('pin-user', new Map([['tile-1', tile]]));

    // Admitted while free. establishMediaSession pins this from the SERVER's own
    // join payload (`cameraSpatialCapFromJoin`, covered in videoLimits.test.ts),
    // falling back to the store only when the field is absent; what this case
    // exercises is the READ side -- that a pinned value outranks the live store
    // however the pin was obtained.
    useSubscriptionStore.setState({
      entitlement: { ...FREE_ENTITLEMENT },
      hydrated: true,
      degraded: false,
    });
    svc.cameraSpatialCapForSession = 1;

    // Upgrade lands mid-call: the store flips, the SFU session does not.
    useSubscriptionStore.setState({
      entitlement: { ...FREE_ENTITLEMENT, maxManualBitrateBps: PREMIUM_MANUAL_BITRATE_BPS },
      hydrated: true,
      degraded: false,
    });
    expect(effectiveCameraSpatialCap(useSubscriptionStore.getState())).toBe(2); // store DID move
    expect(svc.computePreferredLayerPayloadForUser('pin-user', 0).spatialLayer).toBe(1);

    // With no session pinned the live store is authoritative again, so joining
    // fresh as premium is not stuck at the old value.
    svc.cameraSpatialCapForSession = null;
    expect(svc.computePreferredLayerPayloadForUser('pin-user', 0).spatialLayer).toBe(2);
  });

  // Multi-tile pressure. registerCameraPressureContext models ONE tile, but
  // production renders the same participant in several at once (grid + focus,
  // thumbnail strip + stage), and computePreferredLayerPayloadForUser picks the
  // best VISIBLE tile across them. So a tile with no step left must not be able
  // to speak for the participant: the L2 guard asks whether the POLICY has a step,
  // and the answer belongs to the winning tile.
  // BOTH insertion orders, and the second case is the one with teeth. With the
  // focus tile added last, "pick the last visible tile" passes this test exactly
  // as "pick the best visible tile" does — so the original fixture could not
  // tell the property it names from a trivially wrong implementation that
  // happens to agree on one ordering (CodeRabbit, #3277). Map iteration order is
  // insertion order, so reversing it is the whole control.
  it.each([
    ['thumbnail inserted first', false],
    ['focus inserted first', true],
  ])(
    'takes the step the best visible tile still has, not the one a thumbnail lacks (%s)',
    async (_label, focusFirst) => {
      const svc = voiceService as any;
      const cursor = makeCursor('mt-cam-report', 90);
      const cam = makeVideoConsumer('mt-cam', () => statsMap(cursor), { negotiatedSsrc: 90 });
      const other = makeVideoConsumer('mt-other', () => new Map());
      svc.consumers.set(cam.id, cam);
      svc.consumers.set(other.id, other);
      // One tile at thumbnail (forced to spatial 0 — no step), one at focus (has one).
      registerCameraPressureContext(svc, 'mt-user', cam.id, { role: 'thumbnail' });
      const thumbnail = svc.remoteVideoRenderStateByUser.get('mt-user').get('tile-1');
      const focus = {
        visible: true,
        cssWidth: 1920,
        cssHeight: 1080,
        role: 'focus' as const,
        focusedWindow: true,
      };
      svc.remoteVideoRenderStateByUser.set(
        'mt-user',
        focusFirst
          ? new Map([
              ['tile-focus', focus],
              ['tile-1', thumbnail],
            ])
          : new Map([
              ['tile-1', thumbnail],
              ['tile-focus', focus],
            ])
      );
      const setPreferredLayerEmits = () =>
        mockSocket.emit.mock.calls.filter(([event]: [string]) => event === 'set-preferred-layers');

      await svc.profileDecoders(); // baseline
      addInterval(cursor, elapse(), 40);
      await svc.profileDecoders(); // classified RED

      // L2 must NOT fire: the focus tile still has a step, so this is an emit, not a pause.
      expect(setPreferredLayerEmits()).toHaveLength(1);
      expect(setPreferredLayerEmits()[0][1]).toMatchObject({ pressureStepDown: true });
      expect(svc.pauseCoordinator.hasReason(cam.id, 'ignis')).toBe(false);
      // And the demand is the FOCUS tile's pressured layer, strictly below its own
      // unpressured value — the thumbnail's 0 never becomes the participant's answer.
      const unpressured = svc.computePreferredLayerPayloadForUser('mt-user', 0);
      expect(setPreferredLayerEmits()[0][1].spatialLayer).toBeLessThan(unpressured.spatialLayer);
      expect(setPreferredLayerEmits()[0][1].spatialLayer).toBeGreaterThan(0);
    }
  );
});
