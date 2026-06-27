/**
 * Regression: multi-camera voice-call crash via the camera-layering gate re-produce.
 *
 * When a 2nd webcam joins a room, the SFU flips the `camera-layering-gate` and
 * every client runs fastReproduceCamera() to swap encodings. That path closes
 * the camera producer and re-produces REUSING the same MediaStreamTrack. Because
 * the producer was created with mediasoup-client's default `stopTracks: true`,
 * producer.close() STOPS the reused track (Producer.js destroyTrack →
 * `if (this._stopTracks) this._track.stop()`), so the subsequent
 * sendTransport.produce({ track }) throws InvalidStateError('track ended')
 * (Transport.js: `if (track.readyState === 'ended') throw ...`). The producer is
 * gone (locally + server via close-producer) and the re-produce never replaces
 * it — so EVERY camera in the room dies ("only one camera per call").
 *
 * Fix: create the camera/screen/screen-audio producers with `stopTracks: false`
 * — the track lifecycle is owned by localCameraStream/localScreenStream, which
 * every teardown path stops explicitly. The reused track then survives close().
 *
 * Companion (security): with stopTracks:false, producer.close() no longer stops
 * the capture track, so failClosedEncryptTransform() must stop the owning capture
 * stream itself — otherwise a fail-closed E2EE encrypt-transform path leaves the
 * camera/mic hardware capture light ON (CWE-212 / privacy).
 *
 * The shared voiceService.test.ts harness CANNOT catch this: its createMockProducer
 * close() never stops the track and its makeSendTransport produce() resolves
 * regardless of track.readyState. This suite uses mediasoup-FAITHFUL mocks.
 */
import { vi, describe, it, expect, beforeEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';

// ---------------------------------------------------------------------------
// Mock external dependencies BEFORE importing voiceService
// ---------------------------------------------------------------------------

const mockDeviceRtpCapabilities = {
  codecs: [
    { mimeType: 'audio/opus', kind: 'audio', clockRate: 48000, channels: 2, parameters: {} },
    { mimeType: 'video/VP8', kind: 'video', clockRate: 90000, parameters: {} },
  ],
};

vi.mock('mediasoup-client', () => ({
  Device: class MockDevice {
    load = vi.fn().mockResolvedValue(undefined);
    rtpCapabilities = mockDeviceRtpCapabilities;
    createSendTransport = vi.fn();
    createRecvTransport = vi.fn();
    loaded = true;
  },
  types: {},
}));

const mockSocket = {
  connected: true,
  emit: vi.fn(),
  on: vi.fn(),
  once: vi.fn(),
  disconnect: vi.fn(),
  io: { on: vi.fn() },
};
vi.mock('socket.io-client', () => ({
  io: vi.fn().mockReturnValue(mockSocket),
}));

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    getChannelKey: vi.fn().mockResolvedValue(null),
    invalidateChannelKey: vi.fn(),
    getChannelKeyVersion: vi.fn().mockReturnValue(0),
    getChannelKeyByVersion: vi.fn().mockResolvedValue(null),
    onKeyRotation: vi.fn().mockReturnValue(() => {}),
  },
}));

vi.mock('@/renderer/services/mediaEncryption', () => ({
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 3,
  MediaEncryption: class MockMediaEncryption {
    init = vi.fn().mockResolvedValue(undefined);
    destroy = vi.fn();
  },
  deriveFrameKey: vi.fn().mockResolvedValue({} as CryptoKey),
  ratchetKey: vi.fn().mockResolvedValue({} as CryptoKey),
}));

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
  private _tracks: any[];
  constructor(tracks?: any[]) {
    this._tracks = tracks || [];
  }
  getTracks() {
    return this._tracks;
  }
  getAudioTracks() {
    return this._tracks.filter((t) => t.kind === 'audio');
  }
  getVideoTracks() {
    return this._tracks.filter((t) => t.kind === 'video');
  }
  addTrack(t: any) {
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

// ---------------------------------------------------------------------------
// mediasoup-FAITHFUL mock factories
// ---------------------------------------------------------------------------

let nextProducerId = 0;
// Toggled by the fail-closed test to make the NEXT produced producer lack rtpSender.
let produceWithRtpSender = true;

function makeVideoTrack(id = 'cam-track'): any {
  const t: any = {
    id,
    kind: 'video',
    readyState: 'live',
    enabled: true,
    contentHint: '',
    onended: null,
    getSettings: () => ({ width: 1280, height: 720, frameRate: 30 }),
    applyConstraints: vi.fn().mockResolvedValue(undefined),
    stop: vi.fn(() => {
      // A real MediaStreamTrack.stop() flips readyState to 'ended'.
      t.readyState = 'ended';
    }),
  };
  return t;
}

function makeAudioTrack(id = 'mic-track'): any {
  const t: any = {
    id,
    kind: 'audio',
    readyState: 'live',
    enabled: true,
    getSettings: () => ({}),
    stop: vi.fn(() => {
      t.readyState = 'ended';
    }),
  };
  return t;
}

/**
 * Faithful to mediasoup-client Producer: close() stops the underlying track
 * UNLESS the producer was created with `stopTracks: false`. This is the exact
 * semantic the bug + fix hinge on.
 */
function makeMockProducer(opts: {
  track?: any;
  stopTracks?: boolean;
  source: string;
  withRtpSender?: boolean;
}): any {
  const { track, stopTracks, source, withRtpSender = true } = opts;
  const producer: any = {
    id: `producer-${nextProducerId++}`,
    closed: false,
    paused: false,
    appData: { source },
    rtpParameters: { codecs: [{ mimeType: 'video/VP8' }] },
    rtpSender: withRtpSender
      ? {
          getParameters: () => ({ encodings: [{}], codecs: [{ mimeType: 'video/VP8' }] }),
          setParameters: vi.fn().mockResolvedValue(undefined),
        }
      : undefined,
    on: vi.fn(),
    pause: vi.fn(),
    resume: vi.fn(),
    replaceTrack: vi.fn().mockResolvedValue(undefined),
    close: vi.fn(() => {
      producer.closed = true;
      if (stopTracks !== false && track && typeof track.stop === 'function') {
        track.stop();
      }
    }),
  };
  return producer;
}

/** Faithful to Transport.produce: rejects an already-ended track. */
function makeSendTransport(): any {
  return {
    produce: vi.fn(async (o: any) => {
      if (o?.track && o.track.readyState === 'ended') {
        throw new Error('track ended');
      }
      return makeMockProducer({
        track: o?.track,
        stopTracks: o?.stopTracks,
        source: o?.appData?.source ?? 'unknown',
        withRtpSender: produceWithRtpSender,
      });
    }),
    on: vi.fn(),
    close: vi.fn(),
  };
}

// ---------------------------------------------------------------------------
// Per-test reset of the singleton service
// ---------------------------------------------------------------------------
function resetService(svc: any): void {
  svc.producers = new Map();
  svc.localCameraStream = null;
  svc.localScreenStream = null;
  svc.localMicStream = null;
  svc.sendTransport = makeSendTransport();
  svc.device = { rtpCapabilities: mockDeviceRtpCapabilities, loaded: true };
  svc.socket = mockSocket;
  svc.mediaEncryption = null;
  // Stub helpers that are incidental to the track-lifecycle bug.
  svc.ensureOsPermission = vi.fn().mockResolvedValue('granted');
  svc.applyDegradationPreference = vi.fn();
  svc.drainSendTransportQueue = vi.fn().mockResolvedValue(undefined);
  svc.buildCameraFallbackChain = vi.fn().mockReturnValue([]);
  svc.pickCameraCodec = vi
    .fn()
    .mockReturnValue({ codec: undefined, encodings: [{ maxBitrate: 1_000_000 }] });
  svc.pickScreenCodec = vi.fn().mockReturnValue({
    codec: undefined,
    encodings: [{ maxBitrate: 1_500_000 }],
    effectiveBitrate: 1_500_000,
  });
}

describe('voiceService camera/screen re-produce track lifecycle', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    nextProducerId = 0;
    produceWithRtpSender = true;
  });

  it('camera-layering re-produce keeps the camera alive (reused track survives close)', async () => {
    const svc = voiceService as any;
    resetService(svc);

    const camTrack = makeVideoTrack('cam-track');
    svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([camTrack]));

    await svc.produceVideo();
    const original = svc.producers.get('camera');
    expect(original, 'camera producer should exist after produceVideo').toBeDefined();
    expect(camTrack.readyState).toBe('live');

    // SFU flips camera-layering-gate (2nd webcam) -> client re-produces.
    await svc.fastReproduceCamera();

    const reproduced = svc.producers.get('camera');
    expect(reproduced, 'camera producer must survive the gate-change re-produce').toBeDefined();
    expect(reproduced.id).not.toBe(original.id);
    expect(camTrack.readyState, 'reused camera track must stay live').toBe('live');
  });

  it('codec-floor screen re-produce keeps the screen alive (reused track survives close)', async () => {
    const svc = voiceService as any;
    resetService(svc);

    const screenTrack = makeVideoTrack('screen-track');
    const screenStream = new MockMediaStream([screenTrack]);
    svc.captureScreen = vi.fn().mockResolvedValue(screenStream);

    await svc.produceScreen('window:1:0');
    const original = svc.producers.get('screen');
    expect(original, 'screen producer should exist after produceScreen').toBeDefined();
    expect(screenTrack.readyState).toBe('live');

    await svc.fastReproduceScreen();

    const reproduced = svc.producers.get('screen');
    expect(reproduced, 'screen producer must survive the codec-floor re-produce').toBeDefined();
    expect(reproduced.id).not.toBe(original.id);
    expect(screenTrack.readyState, 'reused screen track must stay live').toBe('live');
  });

  // failClosedEncryptTransform must stop the OWNING capture stream itself, since
  // (with stopTracks:false) producer.close() no longer stops the track — otherwise
  // an E2EE encrypt-transform failure leaks the camera/mic capture light (CWE-212).
  // Parametrized over every source so each branch of the source→cleanup dispatch
  // (camera/mic/screen/screen-audio) is exercised. A producer with no rtpSender
  // deterministically forces failClosedEncryptTransform('no rtpSender'); close()
  // here mirrors a stopTracks:false producer (does NOT stop the track), so only the
  // companion fix can stop the capture stream.
  const failClosedCases = [
    { source: 'camera', kind: 'video', field: 'localCameraStream' },
    { source: 'mic', kind: 'audio', field: 'localMicStream' },
    { source: 'screen', kind: 'video', field: 'localScreenStream' },
    { source: 'screen-audio', kind: 'audio', field: 'localScreenStream' },
  ] as const;

  it.each(failClosedCases)(
    'fail-closed encrypt transform stops $source capture via $field (no leaked light)',
    ({ source, kind, field }) => {
      const svc = voiceService as any;
      resetService(svc);

      const track =
        kind === 'video' ? makeVideoTrack(`${source}-track`) : makeAudioTrack(`${source}-track`);
      svc[field] = new MockMediaStream([track]);

      const producer = makeMockProducer({
        track,
        stopTracks: false,
        source,
        withRtpSender: false,
      });

      expect(() => svc.applyEncryptTransform(producer)).toThrow(/encrypt transform/);

      expect(track.readyState, `${source} capture must be stopped on fail-closed`).toBe('ended');
      expect(svc[field], `${field} must be released on fail-closed`).toBeNull();
    }
  );

  // The screen/screen-audio branch floats async cleanupScreenState() with `void`;
  // a rejection (e.g. the awaited transport-queue drain fails) must be caught/logged,
  // never surface as an unhandled rejection. (Gitar review on PR #1903.)
  it('fail-closed screen cleanup rejection is caught and logged (no unhandled rejection)', async () => {
    const svc = voiceService as any;
    resetService(svc);
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    const screenTrack = makeVideoTrack('screen-track');
    svc.localScreenStream = new MockMediaStream([screenTrack]);
    // A screen-audio producer in the map makes cleanupScreenState take its await path.
    svc.producers.set(
      'screen-audio',
      makeMockProducer({
        track: makeAudioTrack('sa-track'),
        stopTracks: false,
        source: 'screen-audio',
      })
    );
    // Force the awaited drain to reject so cleanupScreenState() rejects.
    svc.drainSendTransportQueue = vi.fn().mockRejectedValue(new Error('drain failed'));

    const producer = makeMockProducer({
      track: screenTrack,
      stopTracks: false,
      source: 'screen',
      withRtpSender: false,
    });

    // failClosed throws synchronously; the floated .catch() fires on a later microtask.
    expect(() => svc.applyEncryptTransform(producer)).toThrow(/encrypt transform/);
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(errSpy).toHaveBeenCalledWith(
      'E2EE: fail-closed screen cleanup failed:',
      expect.stringContaining('drain failed')
    );
    errSpy.mockRestore();
  });
});
