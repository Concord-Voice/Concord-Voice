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
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';

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
    getChannelKeyMaterial: vi.fn().mockResolvedValue({ channelKey: null, keyVersion: 0 }),
    invalidateChannelKey: vi.fn(),
    getChannelKeyVersion: vi.fn().mockReturnValue(0),
    getChannelKeyByVersion: vi.fn().mockResolvedValue(null),
    onKeyRotation: vi.fn().mockReturnValue(() => {}),
  },
}));

vi.mock('@/renderer/services/mediaEncryption', () => ({
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 5,
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
Object.defineProperty(MockRTCRtpSender.prototype, 'createEncodedStreams', { value: vi.fn() });
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
let produceWithRtpParameters = true;
let requirePrePublishTransform = false;
let pendingSenderSupportsTransform = true;
let prePublishTransformSources: string[] = [];
let publishedSources: string[] = [];
let negotiatedVideoCodec: { mimeType: string; sdpFmtpLine?: string } = {
  mimeType: 'video/VP8',
};
let senderVideoCodec: { mimeType: string; sdpFmtpLine?: string } | null = null;

const requestedVp8Codec = {
  mimeType: 'video/VP8',
  kind: 'video',
  clockRate: 90000,
  parameters: {},
};

const requestedH264Codec = {
  mimeType: 'video/H264',
  kind: 'video',
  clockRate: 90000,
  parameters: {},
};

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
    rtpParameters: { codecs: produceWithRtpParameters ? [negotiatedVideoCodec] : [] },
    rtpSender: withRtpSender
      ? {
          getParameters: () => ({
            encodings: [{}],
            codecs: [senderVideoCodec ?? negotiatedVideoCodec],
          }),
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
      const source = o?.appData?.source ?? 'unknown';
      if (requirePrePublishTransform) {
        if (typeof o?.onRtpSender !== 'function') {
          throw new Error(`plaintext publication attempted for ${source}`);
        }
        const createEncodedStreams = pendingSenderSupportsTransform
          ? vi.fn(() => ({
              readable: new ReadableStream({
                start(controller) {
                  controller.close();
                },
              }),
              writable: new WritableStream(),
            }))
          : undefined;
        const sender = { createEncodedStreams, replaceTrack: vi.fn().mockResolvedValue(undefined) };
        o.onRtpSender(sender);
        if (!createEncodedStreams?.mock.calls.length) {
          throw new Error(`sender transform missing before publication for ${source}`);
        }
        prePublishTransformSources.push(source);
      }
      publishedSources.push(source);
      return makeMockProducer({
        track: o?.track,
        stopTracks: o?.stopTracks,
        source,
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
  // The layering re-produce serializer flags live on the singleton and are NOT
  // reset by leaving/rejoining; clear them so an in-flight drain from a prior
  // test never queues (pending) the next test's schedule.
  svc.cameraLayeringReproduceInFlight = false;
  svc.cameraLayeringReproducePending = false;
  svc.screenLayeringReproduceInFlight = false;
  svc.screenLayeringReproducePending = false;
  svc.videoReproduceSessionActive = true;
  svc.videoReproduceQueues.camera = Promise.resolve();
  svc.videoReproduceQueues.screen = Promise.resolve();
  svc.videoReproduceSourceGenerations.camera = 0;
  svc.videoReproduceSourceGenerations.screen = 0;
  // Stub helpers that are incidental to the track-lifecycle bug.
  svc.ensureOsPermission = vi.fn().mockResolvedValue('granted');
  svc.applyDegradationPreference = vi.fn();
  svc.drainSendTransportQueue = vi.fn().mockResolvedValue(undefined);
  svc.buildCameraFallbackChain = vi.fn().mockReturnValue([]);
  svc.pickCameraCodec = vi
    .fn()
    .mockReturnValue({ codec: requestedVp8Codec, encodings: [{ maxBitrate: 1_000_000 }] });
  svc.pickScreenCodec = vi.fn().mockReturnValue({
    codec: requestedVp8Codec,
    encodings: [{ maxBitrate: 1_500_000 }],
    effectiveBitrate: 1_500_000,
  });
}

/**
 * Register the real socket handlers on the mock socket and return the callback
 * bound to `event`. setupSocketListeners only registers callbacks (never invokes
 * them), so it is safe against the mock socket.
 */
function getSocketHandler(svc: any, event: string): (payload: unknown) => void {
  svc.setupSocketListeners();
  const call = mockSocket.on.mock.calls.find((c: unknown[]) => c[0] === event);
  if (!call) throw new Error(`no handler registered for ${event}`);
  return call[1] as (payload: unknown) => void;
}

describe('voiceService camera/screen re-produce track lifecycle', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    nextProducerId = 0;
    produceWithRtpSender = true;
    produceWithRtpParameters = true;
    requirePrePublishTransform = false;
    pendingSenderSupportsTransform = true;
    prePublishTransformSources = [];
    publishedSources = [];
    negotiatedVideoCodec = { mimeType: 'video/VP8' };
    senderVideoCodec = null;
  });

  it('installs E2EE before all seven audio/video producer publication paths', async () => {
    const svc = voiceService as any;
    resetService(svc);
    requirePrePublishTransform = true;
    svc.mediaEncryption = { encryptFrame: vi.fn().mockResolvedValue(undefined) };
    svc.startPacketLossMonitor = vi.fn();
    svc.startLocalVAD = vi.fn();
    svc.applyNoiseGate = vi.fn((stream: MockMediaStream) => stream.getAudioTracks()[0]);
    svc.applyInputVolume = vi.fn((track: unknown) => track);

    const cameraTrack = makeVideoTrack('pre-publish-camera');
    svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([cameraTrack]));
    await svc.produceVideo();
    await svc.fastReproduceCamera();

    const screenTrack = makeVideoTrack('pre-publish-screen');
    const screenAudioTrack = makeAudioTrack('pre-publish-screen-audio');
    svc.captureScreen = vi
      .fn()
      .mockResolvedValue(new MockMediaStream([screenTrack, screenAudioTrack]));
    await svc.produceScreen('window:pre-publish:0');
    await svc.fastReproduceScreen();

    const micTrack = makeAudioTrack('pre-publish-mic');
    const micStream = new MockMediaStream([micTrack]);
    svc.resolveAudioStream = vi.fn().mockResolvedValue(micStream);
    await svc.produceAudio(undefined, micStream);

    const expectedSources = [
      'camera',
      'camera',
      'screen',
      'screen-audio',
      'screen',
      'screen-audio',
      'mic',
    ];
    expect(prePublishTransformSources).toEqual(expectedSources);
    expect(publishedSources).toEqual(expectedSources);
  });

  it('fails closed before publication when the sender transform cannot attach', async () => {
    const svc = voiceService as any;
    resetService(svc);
    requirePrePublishTransform = true;
    pendingSenderSupportsTransform = false;
    svc.mediaEncryption = { encryptFrame: vi.fn().mockResolvedValue(undefined) };
    svc.applyInputVolume = vi.fn((track: unknown) => track);

    const micTrack = makeAudioTrack('fail-closed-pre-publish-mic');
    const micStream = new MockMediaStream([micTrack]);
    svc.resolveAudioStream = vi.fn().mockResolvedValue(micStream);

    await expect(svc.produceAudio(undefined, micStream)).rejects.toThrow(/encrypt transform/);
    expect(publishedSources).toEqual([]);
    expect(micTrack.readyState).toBe('ended');
    expect(svc.localMicStream).toBeNull();
    expect(svc.producers.has('mic')).toBe(false);
    expect(svc.sendTransport.close).toHaveBeenCalledOnce();
  });

  it('records the exact negotiated H264 profile for camera produce and re-produce (#2242)', async () => {
    const svc = voiceService as any;
    resetService(svc);
    negotiatedVideoCodec = {
      mimeType: 'video/H264',
      sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640034',
    };
    svc.pickCameraCodec.mockReturnValue({
      codec: requestedH264Codec,
      encodings: [{ maxBitrate: 1_000_000 }],
    });

    const camTrack = makeVideoTrack('cam-profile-track');
    svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([camTrack]));

    await svc.produceVideo();
    expect(useVoiceStore.getState().activeCameraCodec).toBe('video/h264:640034');

    await svc.fastReproduceCamera();
    expect(useVoiceStore.getState().activeCameraCodec).toBe('video/h264:640034');
  });

  it('records the exact negotiated H264 profile for screen produce and re-produce (#2242)', async () => {
    const svc = voiceService as any;
    resetService(svc);
    negotiatedVideoCodec = {
      mimeType: 'video/H264',
      sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640034',
    };
    svc.pickScreenCodec.mockReturnValue({
      codec: requestedH264Codec,
      encodings: [{ maxBitrate: 1_500_000 }],
      effectiveBitrate: 1_500_000,
    });

    const screenTrack = makeVideoTrack('screen-profile-track');
    svc.captureScreen = vi.fn().mockResolvedValue(new MockMediaStream([screenTrack]));

    await svc.produceScreen('window:1:0');
    expect(useVoiceStore.getState().activeScreenCodec).toBe('video/h264:640034');

    await svc.fastReproduceScreen();
    expect(useVoiceStore.getState().activeScreenCodec).toBe('video/h264:640034');
  });

  it('uses authoritative producer RTP parameters when sender parameters are unavailable (#2242)', async () => {
    const svc = voiceService as any;
    resetService(svc);
    produceWithRtpSender = false;
    svc.pickCameraCodec.mockReturnValue({
      codec: requestedH264Codec,
      encodings: [{ maxBitrate: 1_000_000 }],
    });

    const camTrack = makeVideoTrack('cam-fallback-track');
    svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([camTrack]));

    await svc.produceVideo();
    expect(useVoiceStore.getState().activeCameraCodec).toBe('video/vp8');
  });

  it('prefers authoritative producer RTP parameters over the sender codec list (#2242)', async () => {
    const svc = voiceService as any;
    resetService(svc);
    negotiatedVideoCodec = {
      mimeType: 'video/H264',
      sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f',
    };
    senderVideoCodec = { mimeType: 'video/VP8' };
    svc.pickCameraCodec.mockReturnValue({
      codec: requestedH264Codec,
      encodings: [{ maxBitrate: 1_000_000 }],
    });

    const camTrack = makeVideoTrack('cam-authoritative-producer-track');
    svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([camTrack]));

    await svc.produceVideo();
    expect(useVoiceStore.getState().activeCameraCodec).toBe('video/h264:64001f');
  });

  it('falls back to the requested MIME only when producer and sender parameters are unavailable', async () => {
    const svc = voiceService as any;
    resetService(svc);
    produceWithRtpParameters = false;
    produceWithRtpSender = false;
    svc.pickCameraCodec.mockReturnValue({
      codec: requestedH264Codec,
      encodings: [{ maxBitrate: 1_000_000 }],
    });

    const camTrack = makeVideoTrack('cam-requested-fallback-track');
    svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([camTrack]));

    await svc.produceVideo();
    expect(useVoiceStore.getState().activeCameraCodec).toBe('video/h264');
  });

  it('preserves the selected profile when producer and sender parameters are unavailable', async () => {
    const svc = voiceService as any;
    resetService(svc);
    produceWithRtpParameters = false;
    produceWithRtpSender = false;
    svc.pickCameraCodec.mockReturnValue({
      codec: {
        mimeType: 'video/VP9',
        kind: 'video',
        clockRate: 90000,
        parameters: { 'profile-id': 2 },
      },
      encodings: [{ maxBitrate: 1_000_000 }],
    });

    const camTrack = makeVideoTrack('cam-selected-profile-fallback-track');
    svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([camTrack]));

    await svc.produceVideo();
    expect(useVoiceStore.getState().activeCameraCodec).toBe('video/vp9:2');
  });

  it('normalizes an fmtp-less VP9 producer to Profile 0', async () => {
    const svc = voiceService as any;
    resetService(svc);
    negotiatedVideoCodec = { mimeType: 'video/VP9' };

    const camTrack = makeVideoTrack('cam-vp9-profile-zero-track');
    svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([camTrack]));

    await svc.produceVideo();
    expect(useVoiceStore.getState().activeCameraCodec).toBe('video/vp9:0');
  });

  it.each(['camera', 'screen'] as const)(
    'fails closed without invoking mediasoup when $source has no eligible codec',
    async (source) => {
      const svc = voiceService as any;
      resetService(svc);
      const track = makeVideoTrack(`${source}-no-codec-track`);

      if (source === 'camera') {
        svc.pickCameraCodec.mockReturnValue({
          codec: undefined,
          encodings: [{ maxBitrate: 1_000_000 }],
        });
        svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([track]));
        await svc.produceVideo();
      } else {
        svc.pickScreenCodec.mockReturnValue({
          codec: undefined,
          encodings: [{ maxBitrate: 1_500_000 }],
          effectiveBitrate: 1_500_000,
        });
        svc.captureScreen = vi.fn().mockResolvedValue(new MockMediaStream([track]));
        await expect(svc.produceScreen('window:no-codec:0')).rejects.toThrow(
          /eligible screen codec/i
        );
      }

      expect(svc.sendTransport.produce).not.toHaveBeenCalled();
      expect(svc.producers.has(source)).toBe(false);
      expect(track.readyState).toBe('ended');
      expect(source === 'camera' ? svc.localCameraStream : svc.localScreenStream).toBeNull();
      expect(
        source === 'camera'
          ? useVoiceStore.getState().activeCameraCodec
          : useVoiceStore.getState().activeScreenCodec
      ).toBeNull();
    }
  );

  it.each(['camera', 'screen'] as const)(
    'fails closed before mediasoup when fast $source re-produce has no eligible codec',
    async (source) => {
      const svc = voiceService as any;
      resetService(svc);
      const track = makeVideoTrack(`${source}-fast-no-codec-track`);

      if (source === 'camera') {
        svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([track]));
        await svc.produceVideo();
        svc.pickCameraCodec.mockReturnValue({
          codec: undefined,
          encodings: [{ maxBitrate: 1_000_000 }],
        });
      } else {
        svc.captureScreen = vi.fn().mockResolvedValue(new MockMediaStream([track]));
        await svc.produceScreen('window:fast-no-codec:0');
        svc.pickScreenCodec.mockReturnValue({
          codec: undefined,
          encodings: [{ maxBitrate: 1_500_000 }],
          effectiveBitrate: 1_500_000,
        });
      }
      svc.sendTransport.produce.mockClear();

      await expect(
        source === 'camera' ? svc.fastReproduceCamera() : svc.fastReproduceScreen()
      ).rejects.toThrow(new RegExp(`eligible ${source} codec`, 'i'));

      expect(svc.sendTransport.produce).not.toHaveBeenCalled();
      expect(svc.producers.has(source)).toBe(false);
      expect(track.readyState).toBe('ended');
      expect(source === 'camera' ? svc.localCameraStream : svc.localScreenStream).toBeNull();
    }
  );

  it.each(['camera', 'screen'] as const)(
    'cleans up $source state and capture when fast re-produce rejects',
    async (source) => {
      const svc = voiceService as any;
      resetService(svc);
      const track = makeVideoTrack(`${source}-reproduce-reject-track`);

      if (source === 'camera') {
        svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([track]));
        await svc.produceVideo();
      } else {
        svc.captureScreen = vi.fn().mockResolvedValue(new MockMediaStream([track]));
        await svc.produceScreen('window:reproduce-reject:0');
      }

      svc.sendTransport.produce.mockRejectedValueOnce(new Error('replacement produce failed'));

      await expect(
        source === 'camera' ? svc.fastReproduceCamera() : svc.fastReproduceScreen()
      ).rejects.toThrow('replacement produce failed');

      expect(svc.producers.has(source)).toBe(false);
      expect(track.readyState).toBe('ended');
      expect(source === 'camera' ? svc.localCameraStream : svc.localScreenStream).toBeNull();
      const state = useVoiceStore.getState();
      expect(source === 'camera' ? state.activeCameraCodec : state.activeScreenCodec).toBeNull();
      expect(source === 'camera' ? state.isVideoOn : state.isScreenSharing).toBe(false);
    }
  );

  it.each([
    { source: 'camera', reproduce: false },
    { source: 'camera', reproduce: true },
    { source: 'screen', reproduce: false },
    { source: 'screen', reproduce: true },
  ] as const)(
    'clears $source In Use state when its $reproduce transport closes',
    async ({ source, reproduce }) => {
      const svc = voiceService as any;
      resetService(svc);

      const track = makeVideoTrack(`${source}-transport-close`);
      if (source === 'camera') {
        svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([track]));
        await svc.produceVideo();
        if (reproduce) await svc.fastReproduceCamera();
      } else {
        svc.captureScreen = vi.fn().mockResolvedValue(new MockMediaStream([track]));
        await svc.produceScreen('window:transport-close:0');
        if (reproduce) await svc.fastReproduceScreen();
      }

      const producer = svc.producers.get(source);
      const closeHandler = producer.on.mock.calls.find(
        (call: unknown[]) => call[0] === 'transportclose'
      )?.[1] as (() => void) | undefined;
      expect(closeHandler).toBeTypeOf('function');
      expect(
        source === 'camera'
          ? useVoiceStore.getState().activeCameraCodec
          : useVoiceStore.getState().activeScreenCodec
      ).not.toBeNull();

      closeHandler?.();

      expect(
        source === 'camera'
          ? useVoiceStore.getState().activeCameraCodec
          : useVoiceStore.getState().activeScreenCodec
      ).toBeNull();
    }
  );

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

  it('screen track-ended cleanup ignores stale swaps and catches current close failures', async () => {
    const svc = voiceService as any;
    resetService(svc);

    const screenTrack = makeVideoTrack('screen-track-ended');
    const screenStream = new MockMediaStream([screenTrack]);
    svc.captureScreen = vi.fn().mockResolvedValue(screenStream);

    await svc.produceScreen('window:1:0');
    await svc.fastReproduceScreen();
    const reproduced = svc.producers.get('screen');
    expect(screenTrack.onended).toEqual(expect.any(Function));

    const closeProducer = vi.fn().mockRejectedValue(new Error('close failed'));
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    svc.closeProducer = closeProducer;
    try {
      svc.producers.set(
        'screen',
        makeMockProducer({ source: 'screen', track: screenTrack, stopTracks: false })
      );
      screenTrack.onended();
      expect(closeProducer).not.toHaveBeenCalled();

      svc.producers.set('screen', reproduced);
      screenTrack.onended();
      await vi.waitFor(() => {
        expect(warn).toHaveBeenCalledWith(
          '[screen-share] Track-ended cleanup failed:',
          'close failed'
        );
      });
      expect(closeProducer).toHaveBeenCalledWith('screen');
    } finally {
      delete svc.closeProducer;
      warn.mockRestore();
    }
  });

  it('camera transport close only cleans up the producer that owns the callback', async () => {
    const svc = voiceService as any;
    resetService(svc);
    useUserStore.setState({ user: { id: 'local-user', username: 'me' } } as never);
    useVoiceStore.getState().upsertParticipant('local-user', { username: 'me', isVideoOn: true });

    const cameraTrack = makeVideoTrack('camera-transport-close');
    svc.acquireCameraWithFallback = vi.fn().mockResolvedValue(new MockMediaStream([cameraTrack]));

    await svc.produceVideo();
    const producer = svc.producers.get('camera');
    const transportClose = producer.on.mock.calls.find(
      ([event]: [string]) => event === 'transportclose'
    )?.[1];
    expect(transportClose).toEqual(expect.any(Function));

    const successor = makeMockProducer({
      source: 'camera',
      track: cameraTrack,
      stopTracks: false,
    });
    svc.producers.set('camera', successor);
    transportClose();
    expect(svc.producers.get('camera')).toBe(successor);
    expect(cameraTrack.readyState).toBe('live');

    svc.producers.set('camera', producer);
    transportClose();
    expect(svc.producers.has('camera')).toBe(false);
    expect(svc.localCameraStream).toBeNull();
    expect(cameraTrack.readyState).toBe('ended');
    expect(useVoiceStore.getState().isVideoOn).toBe(false);
    expect(useVoiceStore.getState().participants['local-user']?.isVideoOn).toBe(false);
  });

  it('screen re-produce carries the activeScreenShares metadata to the new producerId (#2088)', async () => {
    const svc = voiceService as any;
    resetService(svc);
    // updateStoreForScreenShare keys the local registration on the auth user
    useUserStore.setState({ user: { id: 'local-user', username: 'me' } } as never);

    const screenTrack = makeVideoTrack('screen-track');
    const screenStream = new MockMediaStream([screenTrack]);
    svc.captureScreen = vi.fn().mockResolvedValue(screenStream);

    await svc.produceScreen('window:1:0');
    const original = svc.producers.get('screen');
    const store = useVoiceStore.getState();
    // updateStoreForScreenShare registered the local share under the old id
    expect(store.activeScreenShares[original.id]?.isLocal).toBe(true);

    // Simulate the producer-closed self-echo pruning state MID-swap by
    // removing the old entries before the swap block runs — the snapshot
    // taken before the first await must still restore the new state.
    await svc.fastReproduceScreen();

    const reproduced = svc.producers.get('screen');
    const after = useVoiceStore.getState();
    expect(after.activeScreenShares[original.id]).toBeUndefined();
    expect(after.activeScreenShares[reproduced.id]?.isLocal).toBe(true);
    expect(after.tunedInScreenShares[reproduced.id]).toBe('local-screen');
    expect(after.tunedInScreenShares[original.id]).toBeUndefined();
    expect(after.dominantScreenShareId).toBe(reproduced.id);
  });

  // A pre-publication transform failure has no Producer to close. It must still
  // stop the owning capture stream so camera/mic hardware is never left active.
  // Parametrize every source through the source→cleanup dispatch.
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

      expect(() => svc.applyEncryptTransform({}, undefined, source)).toThrow(/encrypt transform/);

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

    // failClosed throws synchronously; the floated .catch() fires on a later microtask.
    expect(() => svc.applyEncryptTransform({}, undefined, 'screen')).toThrow(/encrypt transform/);
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(errSpy).toHaveBeenCalledWith(
      'E2EE: fail-closed screen cleanup failed:',
      expect.stringContaining('drain failed')
    );
    errSpy.mockRestore();
  });

  // ── #1924 server-gated simulcast screenshare ────────────────────────────────
  // The media-plane's screen-layering-gate flips this.screenLayeringEnabled, which
  // reproduces the local screen with new simulcast eligibility via fastReproduceScreen
  // (stopTracks:false). A client can NEVER publish simulcast screen unilaterally.

  it('screen-layering-gate {enabled:true} reproduces screen, keeping the reused track alive', async () => {
    const svc = voiceService as any;
    resetService(svc);

    // The service is a singleton; pin the starting gate state (resetService does not).
    svc.screenLayeringEnabled = false;

    const screenTrack = makeVideoTrack('screen-track');
    svc.captureScreen = vi.fn().mockResolvedValue(new MockMediaStream([screenTrack]));
    await svc.produceScreen('window:1:0');
    const original = svc.producers.get('screen');
    expect(svc.screenLayeringEnabled).toBe(false);

    const handler = getSocketHandler(svc, 'screen-layering-gate');
    const reproSpy = vi.spyOn(svc, 'fastReproduceScreen');

    handler({ enabled: true });
    expect(svc.screenLayeringEnabled, 'gate flips the client flag on').toBe(true);
    expect(reproSpy).toHaveBeenCalledTimes(1);
    await reproSpy.mock.results[0].value;

    const reproduced = svc.producers.get('screen');
    expect(reproduced.id, 'screen producer swapped on gate flip').not.toBe(original.id);
    expect(screenTrack.readyState, 'reused screen track survives (stopTracks:false)').toBe('live');

    // Idempotent: a same-value gate event does NOT reproduce again.
    handler({ enabled: true });
    expect(reproSpy).toHaveBeenCalledTimes(1);
    reproSpy.mockRestore();
  });

  it('screen-layering-gate {enabled:false} reproduces screen back toward a single encode', async () => {
    const svc = voiceService as any;
    resetService(svc);

    const screenTrack = makeVideoTrack('screen-track');
    svc.captureScreen = vi.fn().mockResolvedValue(new MockMediaStream([screenTrack]));
    await svc.produceScreen('window:1:0');
    const original = svc.producers.get('screen');
    // Simulcast screen currently live (gate was on).
    svc.screenLayeringEnabled = true;

    const handler = getSocketHandler(svc, 'screen-layering-gate');
    const reproSpy = vi.spyOn(svc, 'fastReproduceScreen');

    handler({ enabled: false });
    expect(svc.screenLayeringEnabled, 'gate-off clears the client flag').toBe(false);
    expect(reproSpy).toHaveBeenCalledTimes(1);
    await reproSpy.mock.results[0].value;

    const reproduced = svc.producers.get('screen');
    expect(reproduced.id).not.toBe(original.id);
    expect(screenTrack.readyState, 'reused screen track survives').toBe('live');
    reproSpy.mockRestore();
  });

  it('supportSimulcast toggle reproduces screen ONLY when the gate is enabled', () => {
    const svc = voiceService as any;
    resetService(svc);
    svc.producers.set(
      'screen',
      makeMockProducer({ track: makeVideoTrack('s'), stopTracks: false, source: 'screen' })
    );
    const reproSpy = vi.spyOn(svc, 'fastReproduceScreen').mockResolvedValue(undefined);

    const prev = {
      screenSharePriority: 'off',
      screenShareBitrate: 0,
      supportSvc: true,
      supportSimulcast: false,
    };
    const next = { ...prev, supportSimulcast: true };

    // Gate OFF → a supportSimulcast flip is a no-op for screen (no wasteful reproduce).
    svc.screenLayeringEnabled = false;
    svc.applyScreenShareSettingsChange(next, prev);
    expect(reproSpy).not.toHaveBeenCalled();

    // Gate ON → the supportSimulcast flip now governs screen and reproduces it.
    svc.screenLayeringEnabled = true;
    svc.applyScreenShareSettingsChange(next, prev);
    expect(reproSpy).toHaveBeenCalledTimes(1);
    reproSpy.mockRestore();
  });

  // The gate-flip reproduce goes through fastReproduceScreen (stopTracks:false), so the
  // reproduced screen producer's close() no longer stops the capture track. An E2EE
  // encrypt-transform failure on that producer MUST stop the owning screen capture
  // stream itself (CWE-212), same as the parametrized failClosedCases('screen') above.
  it('fail-closed E2EE on the gate-reproduced screen producer stops the screen capture (CWE-212)', async () => {
    const svc = voiceService as any;
    resetService(svc);
    // The service is a singleton; pin the starting gate state (resetService does not).
    svc.screenLayeringEnabled = false;

    const screenTrack = makeVideoTrack('screen-track');
    svc.captureScreen = vi.fn().mockResolvedValue(new MockMediaStream([screenTrack]));
    await svc.produceScreen('window:1:0');

    const handler = getSocketHandler(svc, 'screen-layering-gate');
    const reproSpy = vi.spyOn(svc, 'fastReproduceScreen');
    handler({ enabled: true });
    await reproSpy.mock.results[0].value;
    reproSpy.mockRestore();
    expect(svc.localScreenStream, 'capture stream survives the reproduce').not.toBeNull();

    expect(() => svc.applyEncryptTransform({}, undefined, 'screen')).toThrow(/encrypt transform/);
    expect(screenTrack.readyState, 'screen capture must be stopped on fail-closed').toBe('ended');
    expect(svc.localScreenStream, 'localScreenStream released on fail-closed').toBeNull();
  });

  // Rapid gate edges (or a slow SDP/produce racing a demand edge / Support-Simulcast
  // toggle) must NOT overlap two fastReproduceScreen calls — two concurrent reproduces
  // both close the single 'screen' producer and produce() twice (duplicate producer /
  // 'track ended'). The screen re-produce serializer (mirroring camera) coalesces the
  // second edge into a single queued drain that runs AFTER the first completes.
  it('serializes rapid screen-layering-gate edges into one queued drain (no overlap)', async () => {
    const svc = voiceService as any;
    resetService(svc);
    // The service is a singleton; pin the starting gate state (resetService does not).
    svc.screenLayeringEnabled = false;

    const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

    let concurrent = 0;
    let maxConcurrent = 0;
    const resolvers: Array<() => void> = [];
    const reproSpy = vi.spyOn(svc, 'fastReproduceScreen').mockImplementation(() => {
      concurrent++;
      maxConcurrent = Math.max(maxConcurrent, concurrent);
      return new Promise<void>((resolve) => {
        resolvers.push(() => {
          concurrent--;
          resolve();
        });
      });
    });

    const handler = getSocketHandler(svc, 'screen-layering-gate');

    // Edge 1 (false→true) starts a reproduce that is now in-flight.
    handler({ enabled: true });
    expect(reproSpy).toHaveBeenCalledTimes(1);

    // Edge 2 (true→false) lands while the first reproduce is still in-flight: it must
    // QUEUE behind it, NOT start a second concurrent fastReproduceScreen.
    handler({ enabled: false });
    expect(reproSpy, 'second edge queues behind the in-flight reproduce').toHaveBeenCalledTimes(1);

    // Complete the first reproduce → the queued edge drains as a SECOND, serial call.
    resolvers[0]();
    await flush();
    expect(reproSpy, 'queued edge drains after the first completes').toHaveBeenCalledTimes(2);

    // Complete the second; the queue empties with no overlap ever observed.
    resolvers[1]();
    await flush();
    expect(maxConcurrent, 'reproduces never overlapped').toBe(1);

    reproSpy.mockRestore();
  });
});
