/**
 * regression for #1540
 *
 * IGNIS decoder-overload recovery path: once a consumer is paused by
 * pauseLowestPriorityConsumer, it must be resumed after IGNIS_RECOVERY_GREEN_INTERVALS
 * consecutive green profileDecoders() cycles.  Before the fix, there was NO recovery
 * arm — paused consumers stayed paused forever.
 */
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
    invalidateChannelKey: vi.fn(),
  },
}));

// --- mediaEncryption ---
vi.mock('@/renderer/services/mediaEncryption', () => ({
  MediaEncryption: class MockMediaEncryption {
    init = vi.fn().mockResolvedValue(undefined);
    initFromKey = vi.fn();
    destroy = vi.fn();
    getCurrentKeyId = vi.fn().mockReturnValue(0);
    setCurrentKeyId = vi.fn();
    encryptFrame = vi.fn().mockResolvedValue(undefined);
    decryptFrame = vi.fn().mockResolvedValue(undefined);
    addDecryptKey = vi.fn().mockResolvedValue(undefined);
    addDecryptKeyDirect = vi.fn();
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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** RED zone stats: totalDecodeTime=2.06, framesDecoded=100, fps=30 → rho ≈ 0.927 (>= 0.925) */
function makeRedStatsMap(): Map<string, unknown> {
  const m = new Map<string, unknown>();
  m.set('inbound', {
    type: 'inbound-rtp',
    kind: 'video',
    totalDecodeTime: 2.06,
    framesDecoded: 100,
    framesPerSecond: 30,
  });
  return m;
}

/** GREEN zone stats: totalDecodeTime=1.0, framesDecoded=100, fps=30 → rho = 0.45 (< 0.67) */
function makeGreenStatsMap(): Map<string, unknown> {
  const m = new Map<string, unknown>();
  m.set('inbound', {
    type: 'inbound-rtp',
    kind: 'video',
    totalDecodeTime: 1.0,
    framesDecoded: 100,
    framesPerSecond: 30,
  });
  return m;
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
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('resumes a paused consumer after IGNIS_RECOVERY_GREEN_INTERVALS consecutive green cycles', async () => {
    const svc = voiceService as any;

    // Build a video consumer whose pause/resume spies also flip the paused field.
    // We use a state object captured by closure so the vi.fn() spies remain intact.
    const state = { paused: false };
    const consumer = {
      id: 'cons-ignis',
      kind: 'video' as const,
      get paused() {
        return state.paused;
      },
      closed: false,
      producerId: 'prod-ignis',
      track: { id: 'track-ignis', kind: 'video', readyState: 'live', enabled: true, stop: vi.fn() },
      close: vi.fn(),
      pause: vi.fn().mockImplementation(() => {
        state.paused = true;
      }),
      resume: vi.fn().mockImplementation(() => {
        state.paused = false;
      }),
      on: vi.fn(),
      getStats: vi.fn().mockResolvedValue(makeRedStatsMap()),
      rtpReceiver: { transform: null },
      // single-layer stream: currentLayers is undefined — RED falls straight to pause
      currentLayers: undefined,
      setPreferredLayers: vi.fn(),
    };

    svc.consumers.set('cons-ignis', consumer);

    // ── Phase A: trigger RED zone → consumer must be paused ──────────────
    await svc.profileDecoders();

    expect(consumer.pause).toHaveBeenCalledTimes(1);
    expect(consumer.paused).toBe(true);

    // ── Phase B: switch to GREEN and drive 3 consecutive cycles ──────────
    // (IGNIS_RECOVERY_GREEN_INTERVALS = 3)
    consumer.getStats.mockResolvedValue(makeGreenStatsMap());

    await svc.profileDecoders(); // green cycle 1
    await svc.profileDecoders(); // green cycle 2
    await svc.profileDecoders(); // green cycle 3 — must trigger recovery

    // The recovery arm must have called resume() exactly once
    expect(consumer.resume).toHaveBeenCalledTimes(1);
  });
});
