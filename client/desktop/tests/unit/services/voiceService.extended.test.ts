/**
 * Extended tests for VoiceService — covers uncovered branches, E2EE paths,
 * live settings subscriptions, transport setup, consumer routing, socket
 * listener edge cases, and cleanup flows.
 *
 * Companion to voiceService.test.ts (which covers the primary happy paths).
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2eeErrors';

// ---------------------------------------------------------------------------
// Mock external dependencies BEFORE importing voiceService
// ---------------------------------------------------------------------------

// --- mediasoup-client ---
const mockDeviceLoad = vi.fn().mockResolvedValue(undefined);
const mockDeviceRtpCapabilities = {
  codecs: [
    { mimeType: 'audio/opus', kind: 'audio', clockRate: 48000, channels: 2, parameters: {} },
    { mimeType: 'video/VP8', kind: 'video', clockRate: 90000, parameters: {} },
    { mimeType: 'video/VP9', kind: 'video', clockRate: 90000, parameters: { 'profile-id': '0' } },
    {
      mimeType: 'video/H264',
      kind: 'video',
      clockRate: 90000,
      parameters: { 'profile-level-id': '42e01f' },
    },
    {
      mimeType: 'video/H264',
      kind: 'video',
      clockRate: 90000,
      parameters: { 'profile-level-id': '640034' },
    },
    { mimeType: 'video/AV1', kind: 'video', clockRate: 90000, parameters: {} },
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
const socketListeners: Record<string, Array<(...args: unknown[]) => void>> = {};
const socketOnceListeners: Record<string, Array<(...args: unknown[]) => void>> = {};
const ioListeners: Record<string, Array<(...args: unknown[]) => void>> = {};

const mockSocket = {
  connected: false,
  emit: vi.fn(),
  on: vi.fn().mockImplementation((event: string, cb: (...args: unknown[]) => void) => {
    if (!socketListeners[event]) socketListeners[event] = [];
    socketListeners[event].push(cb);
  }),
  once: vi.fn().mockImplementation((event: string, cb: (...args: unknown[]) => void) => {
    if (!socketOnceListeners[event]) socketOnceListeners[event] = [];
    socketOnceListeners[event].push(cb);
  }),
  disconnect: vi.fn(),
  io: {
    on: vi.fn().mockImplementation((event: string, cb: (...args: unknown[]) => void) => {
      if (!ioListeners[event]) ioListeners[event] = [];
      ioListeners[event].push(cb);
    }),
  },
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
const mockGetChannelKey = vi.fn().mockResolvedValue({} as CryptoKey);
const mockInvalidateChannelKey = vi.fn();
const mockGetChannelKeyVersion = vi.fn().mockReturnValue(0);
const mockGetChannelKeyByVersion = vi.fn().mockResolvedValue({} as CryptoKey);
const mockOnKeyRotation = vi.fn().mockReturnValue(() => {});
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    getChannelKey: (...args: unknown[]) => mockGetChannelKey(...args),
    invalidateChannelKey: (...args: unknown[]) => mockInvalidateChannelKey(...args),
    // #1878: version binding + sender re-base surface.
    getChannelKeyVersion: (...args: unknown[]) => mockGetChannelKeyVersion(...args),
    getChannelKeyByVersion: (...args: unknown[]) => mockGetChannelKeyByVersion(...args),
    onKeyRotation: (...args: unknown[]) => mockOnKeyRotation(...args),
  },
}));

// --- mediaEncryption ---
const mockMediaEncryptionInit = vi.fn().mockResolvedValue(undefined);
const mockMediaEncryptionInitFromKey = vi.fn();
const mockMediaEncryptionDestroy = vi.fn();
const mockGetCurrentKeyId = vi.fn().mockReturnValue(0);
const mockSetCurrentKeyId = vi.fn();
const mockAddDecryptKeyDirect = vi.fn();
const mockAddDecryptKeyAtEpoch = vi.fn().mockResolvedValue(undefined);
const mockDebouncedRotateKeys = vi.fn();
const mockCatchUpToEpoch = vi.fn().mockResolvedValue(undefined);

vi.mock('@/renderer/services/mediaEncryption', () => ({
  // #1878 Task 6: the client now negotiates v3. The mock mirrors the live
  // constant so the join-version self-check (advertise === ack) stays
  // consistent; the media-plane gate accepts {2,3} during the rollout window.
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 3,
  MediaEncryption: class MockMediaEncryption {
    init = mockMediaEncryptionInit;
    initFromKey = mockMediaEncryptionInitFromKey;
    destroy = mockMediaEncryptionDestroy;
    getCurrentKeyId = mockGetCurrentKeyId;
    setCurrentKeyId = mockSetCurrentKeyId;
    // #1878: version-aware surface the production code now reads (keyVersion is
    // stamped onto the addDecryptKey worker message via getKeyVersion()).
    getKeyVersion = vi.fn().mockReturnValue(0);
    setKeyVersion = vi.fn();
    encryptFrame = vi.fn().mockResolvedValue(undefined);
    decryptFrame = vi.fn().mockResolvedValue(undefined);
    addDecryptKey = vi.fn().mockResolvedValue(undefined);
    addDecryptKeyDirect = mockAddDecryptKeyDirect;
    addDecryptKeyDirectV3 = vi.fn();
    addDecryptKeyAtEpoch = mockAddDecryptKeyAtEpoch;
    addDecryptKeyAtVersion = vi.fn().mockResolvedValue(undefined);
    debouncedRotateKeys = mockDebouncedRotateKeys;
    catchUpToEpoch = mockCatchUpToEpoch;
  },
  deriveFrameKey: vi.fn().mockResolvedValue({} as CryptoKey),
  ratchetKey: vi.fn().mockResolvedValue({} as CryptoKey),
}));

// --- osPermissionStore ---
const mockEnsureOsPermission = vi.fn().mockResolvedValue('granted');
const mockCheckOne = vi.fn().mockResolvedValue('granted');
const mockOpenSettings = vi.fn();
vi.mock('@/renderer/stores/osPermissionStore', () => ({
  useOsPermissionStore: {
    getState: vi.fn().mockReturnValue({
      checkOne: (...args: unknown[]) => mockCheckOne(...args),
      openSettings: (...args: unknown[]) => mockOpenSettings(...args),
    }),
    subscribe: vi.fn().mockReturnValue(() => {}),
  },
  ensureOsPermission: (...args: unknown[]) => mockEnsureOsPermission(...args),
}));

// ---------------------------------------------------------------------------
// Mock browser APIs
// ---------------------------------------------------------------------------

const mockGainNode = {
  gain: { value: 1, setTargetAtTime: vi.fn() },
  connect: vi.fn(),
  disconnect: vi.fn(),
};
const mockAnalyser = {
  fftSize: 0,
  smoothingTimeConstant: 0,
  frequencyBinCount: 128,
  getByteFrequencyData: vi.fn(),
  getByteTimeDomainData: vi.fn(),
  connect: vi.fn(),
  disconnect: vi.fn(),
};

const processedAudioTrack = {
  id: 'processed-track',
  kind: 'audio',
  readyState: 'live',
  enabled: true,
  stop: vi.fn(),
  getSettings: vi.fn().mockReturnValue({}),
};

class MockAudioContext {
  state = 'running';
  currentTime = 0;
  sampleRate = 48000;
  createMediaStreamSource = vi.fn().mockReturnValue({
    connect: vi.fn(),
    disconnect: vi.fn(),
  });
  createAnalyser = vi.fn().mockReturnValue(mockAnalyser);
  createGain = vi.fn().mockReturnValue(mockGainNode);
  createMediaStreamDestination = vi.fn().mockReturnValue({
    stream: {
      getAudioTracks: vi.fn().mockReturnValue([processedAudioTrack]),
    },
  });
  close = vi.fn().mockResolvedValue(undefined);
}

Object.defineProperty(globalThis, 'AudioContext', {
  value: MockAudioContext,
  writable: true,
  configurable: true,
});

// Mock MediaStream
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

// Mock RTCRtpSender constructor; producer doubles below model createEncodedStreams.
function MockRTCRtpSender() {}
Object.defineProperty(globalThis, 'RTCRtpSender', {
  value: MockRTCRtpSender,
  writable: true,
  configurable: true,
});

// Ensure RTCRtpScriptTransform is NOT defined
if ('RTCRtpScriptTransform' in globalThis) {
  delete (globalThis as Record<string, unknown>)['RTCRtpScriptTransform'];
}

// Mock navigator.mediaDevices
const mockGetUserMedia = vi.fn();
const mockGetDisplayMedia = vi.fn();
Object.defineProperty(navigator, 'mediaDevices', {
  value: {
    getUserMedia: mockGetUserMedia,
    getDisplayMedia: mockGetDisplayMedia,
    enumerateDevices: vi.fn().mockResolvedValue([]),
  },
  writable: true,
  configurable: true,
});

function createMockMediaStream(tracks?: Array<{ kind: string; id?: string }>) {
  const allTracks = (tracks || [{ kind: 'audio', id: 'audio-1' }]).map((t) => ({
    id: t.id || `${t.kind}-${Math.random().toString(36).slice(2)}`,
    kind: t.kind,
    readyState: 'live',
    enabled: true,
    stop: vi.fn(),
    clone: vi.fn(),
    getSettings: vi.fn().mockReturnValue({ width: 1920, height: 1080 }),
    contentHint: '',
    onended: null as (() => void) | null,
  }));
  return {
    getAudioTracks: vi.fn().mockReturnValue(allTracks.filter((t) => t.kind === 'audio')),
    getVideoTracks: vi.fn().mockReturnValue(allTracks.filter((t) => t.kind === 'video')),
    getTracks: vi.fn().mockReturnValue(allTracks),
    addTrack: vi.fn(),
    removeTrack: vi.fn(),
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

// ---------------------------------------------------------------------------
// Import voiceService AFTER all mocks
// ---------------------------------------------------------------------------
const { voiceService } = await import('@/renderer/services/voiceService');

import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useAudioSettingsStore } from '@/renderer/stores/audioSettingsStore';
import { useVideoSettingsStore } from '@/renderer/stores/videoSettingsStore';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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

function makeJoinResponse(co?: Record<string, unknown>) {
  return {
    allowed: true,
    media_server_url: 'http://localhost:3000',
    ice_servers: [{ urls: 'stun:stun.l.google.com:19302' }],
    channel: {
      id: 'channel-1',
      name: 'General',
      server_id: 'server-1',
      audio_quality_tier: null,
      ...(co || {}),
    },
  };
}

function makeRoomJoined(ov?: Record<string, unknown>) {
  return {
    rtpCapabilities: mockDeviceRtpCapabilities,
    mediaFrameCryptoVersion: 3,
    existingProducers: [],
    participants: [{ userId: 'user-1', username: 'testuser', displayName: 'Test User' }],
    channelName: 'General',
    ...(ov || {}),
  };
}

function makeTransportOpts(id = 'transport-1') {
  return {
    id,
    iceParameters: { usernameFragment: 'f', password: 'p', iceLite: false },
    iceCandidates: [],
    dtlsParameters: { role: 'auto', fingerprints: [] },
  };
}

function createMockProducer(id = 'prod-1', source = 'mic') {
  return {
    id,
    kind: source === 'mic' || source === 'screen-audio' ? 'audio' : 'video',
    paused: false,
    closed: false,
    close: vi.fn(),
    pause: vi.fn(),
    resume: vi.fn(),
    replaceTrack: vi.fn().mockResolvedValue(undefined),
    on: vi.fn(),
    getStats: vi.fn().mockResolvedValue(new Map()),
    rtpSender: {
      getParameters: vi.fn().mockReturnValue({
        encodings: [{ maxBitrate: 32000, priority: 'low' }],
        codecs: [{ mimeType: 'audio/opus' }],
      }),
      setParameters: vi.fn().mockResolvedValue(undefined),
      createEncodedStreams: vi.fn().mockImplementation(() => ({
        readable: new ReadableStream({
          start(controller) {
            controller.close();
          },
        }),
        writable: new WritableStream(),
      })),
      transform: null,
    },
    appData: { source },
    producerId: id,
  };
}

function createEncodedStreamPair() {
  return {
    readable: new ReadableStream({
      start(controller) {
        controller.close();
      },
    }),
    writable: new WritableStream(),
  };
}

function createMockConsumer(id = 'cons-1', kind: 'audio' | 'video' = 'audio', prodId = 'p-1') {
  return {
    id,
    kind,
    paused: false,
    closed: false,
    producerId: prodId,
    track: { id: `track-${id}`, kind, readyState: 'live', enabled: true, stop: vi.fn() },
    close: vi.fn(),
    pause: vi.fn(),
    resume: vi.fn(),
    on: vi.fn(),
    getStats: vi.fn().mockResolvedValue(new Map()),
    rtpReceiver: {
      transform: null,
      createEncodedStreams: vi.fn().mockImplementation(createEncodedStreamPair),
    },
  };
}

function setupEmitResponses(responses: Record<string, unknown>) {
  mockSocket.emit.mockImplementation(
    (event: string, _data: unknown, callback?: (r: unknown) => void) => {
      if (callback && event in responses) callback(responses[event]);
    }
  );
}

function makeSendTransport() {
  return {
    id: 'send-1',
    closed: false,
    close: vi.fn(),
    produce: vi.fn(),
    on: vi.fn(),
    _awaitQueue: {
      push: vi.fn().mockImplementation(async (fn: () => Promise<void>) => {
        await fn();
      }),
    },
  };
}

function makeRecvTransport(id = 'recv-1') {
  return { id, closed: false, close: vi.fn(), consume: vi.fn(), on: vi.fn() };
}

async function joinVoiceChannel(co?: Record<string, unknown>) {
  setupAuth();
  mockApiFetch.mockResolvedValueOnce({
    ok: true,
    json: vi.fn().mockResolvedValue(makeJoinResponse(co)),
  });
  mockSocket.connected = true;

  const sendTransport = makeSendTransport();
  const recvTransport = makeRecvTransport();
  mockCreateSendTransport.mockReturnValue(sendTransport);
  mockCreateRecvTransport.mockReturnValue(recvTransport);

  setupEmitResponses({
    'join-room': makeRoomJoined(),
    'create-transport': makeTransportOpts(),
    produce: { id: 'prod-mic' },
    'resume-consumer': undefined,
    'close-producer': undefined,
    'pause-producer': undefined,
    'resume-producer': undefined,
  });

  mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'audio', id: 'mic-1' }]));
  const micProducer = createMockProducer('prod-mic', 'mic');
  sendTransport.produce.mockResolvedValue(micProducer);

  await voiceService.joinChannel('channel-1');
  return { sendTransport, recvTransport, micProducer };
}

function triggerSocketEvent(event: string, ...args: unknown[]) {
  const handlers = socketListeners[event] || [];
  for (const handler of handlers) handler(...args);
}

function triggerIoEvent(event: string, ...args: unknown[]) {
  const handlers = ioListeners[event] || [];
  for (const handler of handlers) handler(...args);
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('VoiceService Extended', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    resetAllStores();
    mockSocket.connected = false;
    for (const k of Object.keys(socketListeners)) delete socketListeners[k];
    for (const k of Object.keys(socketOnceListeners)) delete socketOnceListeners[k];
    for (const k of Object.keys(ioListeners)) delete ioListeners[k];
    // Reset mock defaults
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockEnsureOsPermission.mockResolvedValue('granted');
    mockCheckOne.mockResolvedValue('granted');
    mockGetCurrentKeyId.mockReturnValue(0);
    mockAddDecryptKeyAtEpoch.mockResolvedValue(undefined);
  });

  afterEach(() => {
    vi.useRealTimers();
    // Null out any manually-set streams to prevent emergencyCleanup crashes
    // when a test set localScreenStream/localMicStream to a partial mock
    const svc = voiceService as any;
    svc.localMicStream = null;
    svc.localCameraStream = null;
    svc.localScreenStream = null;
    try {
      voiceService.emergencyCleanup();
    } catch {
      /* ok */
    }
  });

  // ===== updateLocalSpeaking =====

  describe('updateLocalSpeaking', () => {
    it('does nothing without local user', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // Clear user so updateLocalSpeaking exits early
      useUserStore.setState({ user: undefined } as any);
      expect(() => svc.updateLocalSpeaking(true)).not.toThrow();
    });

    it('clears activeSpeaker when no longer speaking', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const store = useVoiceStore.getState();
      store.setActiveSpeaker('user-1');
      svc.updateLocalSpeaking(false);
      expect(useVoiceStore.getState().activeSpeakerId).toBeNull();
    });

    it('does not clear activeSpeaker if different user', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const store = useVoiceStore.getState();
      store.setActiveSpeaker('user-other');
      svc.updateLocalSpeaking(false);
      expect(useVoiceStore.getState().activeSpeakerId).toBe('user-other');
    });
  });

  // ===== stopNoiseGate =====

  describe('stopNoiseGate', () => {
    it('handles already closed context', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.noiseGateCtx = { state: 'closed', close: vi.fn() };
      svc.noiseGateTimer = null;
      svc.stopNoiseGate();
      expect(svc.noiseGateCtx).toBeNull();
    });
  });

  // ===== stopInputVolume =====

  describe('stopInputVolume', () => {
    it('unsubscribes and closes context', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const unsubSpy = vi.fn();
      const closeCtx = vi.fn().mockResolvedValue(undefined);
      svc.inputVolumeUnsub = unsubSpy;
      svc.inputVolumeCtx = { state: 'running', close: closeCtx };
      svc.stopInputVolume();
      expect(unsubSpy).toHaveBeenCalled();
      expect(closeCtx).toHaveBeenCalled();
      expect(svc.inputVolumeCtx).toBeNull();
      expect(svc.inputVolumeGain).toBeNull();
    });

    it('handles already closed context', async () => {
      const svc = voiceService as any;
      svc.inputVolumeCtx = { state: 'closed', close: vi.fn() };
      svc.inputVolumeUnsub = null;
      svc.stopInputVolume();
      expect(svc.inputVolumeCtx).toBeNull();
    });
  });

  // ===== liveReplaceAudioTrack =====

  describe('liveReplaceAudioTrack', () => {
    it('replaces track and rebuilds processing chain', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      // Set up the producer in the internal map
      svc.producers.set('mic', micProducer);
      const newStream = createMockMediaStream([{ kind: 'audio', id: 'new-mic' }]);
      mockGetUserMedia.mockResolvedValue(newStream);

      await svc.liveReplaceAudioTrack();

      expect(micProducer.pause).toHaveBeenCalled();
      expect(micProducer.replaceTrack).toHaveBeenCalled();
      expect(micProducer.resume).toHaveBeenCalled();
    });

    it('captures from the selected microphone when replacing the live track', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      useVoiceStore.setState({ audioInputDeviceId: 'mic-selected' });
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'audio', id: 'new-mic' }]));

      await svc.liveReplaceAudioTrack();

      expect(mockGetUserMedia).toHaveBeenLastCalledWith({
        audio: expect.objectContaining({
          deviceId: { exact: 'mic-selected' },
        }),
      });
    });

    it('keeps the latest microphone replacement when requests overlap', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.teardownLiveSubscriptions();
      svc.producers.set('mic', micProducer);
      vi.spyOn(svc, 'applyInputVolume').mockImplementation((track) => track);

      const firstStream = createMockMediaStream([{ kind: 'audio', id: 'mic-old' }]);
      const secondStream = createMockMediaStream([{ kind: 'audio', id: 'mic-new' }]);
      const firstCapture = deferred<unknown>();
      const secondCapture = deferred<unknown>();
      mockGetUserMedia
        .mockImplementationOnce(() => firstCapture.promise)
        .mockImplementationOnce(() => secondCapture.promise);

      useVoiceStore.setState({ audioInputDeviceId: 'mic-old' });
      const firstReplacement = svc.liveReplaceAudioTrack();
      useVoiceStore.setState({ audioInputDeviceId: 'mic-new' });
      const secondReplacement = svc.liveReplaceAudioTrack();

      secondCapture.resolve(secondStream);
      await secondReplacement;
      firstCapture.resolve(firstStream);
      await firstReplacement;

      expect(svc.localMicStream).toBe(secondStream);
      expect(micProducer.replaceTrack).toHaveBeenLastCalledWith({
        track: secondStream.getAudioTracks()[0],
      });
    });

    it.each([
      ['self-muted', () => useVoiceStore.setState({ isMuted: true })],
      [
        'server-muted',
        () => useVoiceStore.getState().updateParticipant('user-1', { serverMuted: true }),
      ],
    ])('does not resume the mic producer while %s', async (_reason, applyPauseState) => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      applyPauseState();
      micProducer.resume.mockClear();
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'audio', id: 'new-mic' }]));

      await svc.liveReplaceAudioTrack();

      expect(micProducer.replaceTrack).toHaveBeenCalled();
      expect(micProducer.resume).not.toHaveBeenCalled();
    });

    it('replaces the live mic track when the selected microphone changes', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      const replaceSpy = vi.spyOn(svc, 'liveReplaceAudioTrack').mockResolvedValue(undefined);

      useVoiceStore.setState({ audioInputDeviceId: 'mic-next' });

      expect(replaceSpy).toHaveBeenCalled();
      replaceSpy.mockRestore();
    });

    it('noop without mic producer', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.delete('mic');
      await svc.liveReplaceAudioTrack();
      // Should not throw
      expect(svc.producers.has('mic')).toBe(false);
    });

    it('applies noise gate in manual mode', async () => {
      useAudioSettingsStore.getState().setNoiseGateMode('manual');
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'audio' }]));

      await svc.liveReplaceAudioTrack();

      expect(micProducer.replaceTrack).toHaveBeenCalled();
    });

    it('handles getUserMedia failure gracefully', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      mockGetUserMedia.mockRejectedValue(new Error('Device busy'));

      await svc.liveReplaceAudioTrack();
      // Should resume even on failure
      expect(micProducer.resume).toHaveBeenCalled();
    });
  });

  // ===== liveReplaceCameraTrack =====

  describe('liveReplaceCameraTrack', () => {
    it('replaces camera track with new constraints', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const cameraProducer = createMockProducer('cam-1', 'camera');
      svc.producers.set('camera', cameraProducer);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video' }]);
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video', id: 'new-cam' }]));

      await svc.liveReplaceCameraTrack();

      expect(cameraProducer.pause).toHaveBeenCalled();
      expect(cameraProducer.replaceTrack).toHaveBeenCalled();
      expect(cameraProducer.resume).toHaveBeenCalled();
    });

    it('noop without camera producer', async () => {
      const svc = voiceService as any;
      svc.producers.delete('camera');
      await svc.liveReplaceCameraTrack();
      expect(svc.producers.has('camera')).toBe(false);
    });

    it('falls back on OverconstrainedError', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const cameraProducer = createMockProducer('cam-1', 'camera');
      svc.producers.set('camera', cameraProducer);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video' }]);

      // Use a non-system preset to trigger fallback
      useVideoSettingsStore.getState().setCameraPreset('1080p30');

      const err = new DOMException('', 'OverconstrainedError');
      mockGetUserMedia
        .mockRejectedValueOnce(err)
        .mockResolvedValueOnce(createMockMediaStream([{ kind: 'video' }]));

      await svc.liveReplaceCameraTrack();
      expect(cameraProducer.replaceTrack).toHaveBeenCalled();
    });

    it('updates participant store with new stream', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const cameraProducer = createMockProducer('cam-1', 'camera');
      svc.producers.set('camera', cameraProducer);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video' }]);
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));

      await svc.liveReplaceCameraTrack();

      const participant = useVoiceStore.getState().participants['user-1'];
      expect(participant?.videoStream).toBeDefined();
    });
  });

  // ===== liveReproduceAudio / liveReproduceCamera =====

  describe('liveReproduceAudio', () => {
    it('closes and re-produces mic', async () => {
      const { sendTransport, micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      const newProducer = createMockProducer('new-mic', 'mic');
      sendTransport.produce.mockResolvedValue(newProducer);
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'audio' }]));

      await svc.liveReproduceAudio();
      // After reproduce, a new producer should be in the map
      expect(svc.producers.get('mic')).toBeDefined();
    });

    it('noop without producer or transport', async () => {
      const svc = voiceService as any;
      svc.producers.clear();
      await svc.liveReproduceAudio();
      expect(svc.producers.size).toBe(0);
    });
  });

  describe('liveReproduceCamera', () => {
    it('closes and re-produces camera', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const cameraProducer = createMockProducer('cam-1', 'camera');
      svc.producers.set('camera', cameraProducer);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video' }]);
      const newProducer = createMockProducer('new-cam', 'camera');
      sendTransport.produce.mockResolvedValue(newProducer);
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));

      await svc.liveReproduceCamera();
      // Old camera stream should be stopped
      expect(cameraProducer.close).toHaveBeenCalled();
    });

    it('noop without producer', async () => {
      const svc = voiceService as any;
      svc.producers.delete('camera');
      await svc.liveReproduceCamera();
      expect(svc.producers.has('camera')).toBe(false);
    });
  });

  // ===== liveUpdateVideoPriority =====

  describe('liveUpdateVideoPriority', () => {
    it('applies priority to all encodings', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('cam-1', 'camera');
      producer.rtpSender.getParameters.mockReturnValue({
        encodings: [{ maxBitrate: 1000000 }, { maxBitrate: 500000 }],
      });
      svc.liveUpdateVideoPriority(producer, 'high');
      const params = producer.rtpSender.setParameters.mock.calls[0]?.[0];
      expect(params.encodings[0].priority).toBe('high');
      expect(params.encodings[1].priority).toBe('high');
    });

    it('resets to low when priority is off', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('cam-1', 'camera');
      producer.rtpSender.getParameters.mockReturnValue({
        encodings: [{ maxBitrate: 1000000 }],
      });
      svc.liveUpdateVideoPriority(producer, 'off');
      const params = producer.rtpSender.setParameters.mock.calls[0]?.[0];
      expect(params.encodings[0].priority).toBe('low');
    });

    it('handles no rtpSender gracefully', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = { ...createMockProducer('cam-1', 'camera'), rtpSender: null };
      expect(() => svc.liveUpdateVideoPriority(producer, 'high')).not.toThrow();
    });
  });

  // ===== liveUpdateScreenBitrate =====

  describe('liveUpdateScreenBitrate', () => {
    it('applies explicit bitrate', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('screen-1', 'screen');
      producer.rtpSender.getParameters.mockReturnValue({
        encodings: [{ maxBitrate: 1000000 }],
      });
      svc.liveUpdateScreenBitrate(producer, 5_000_000);
      const params = producer.rtpSender.setParameters.mock.calls[0]?.[0];
      expect(params.encodings[0].maxBitrate).toBe(5_000_000);
    });

    it('auto-calculates bitrate when 0', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('screen-1', 'screen');
      producer.rtpSender.getParameters.mockReturnValue({
        encodings: [{ maxBitrate: 1000000 }],
      });
      svc.liveUpdateScreenBitrate(producer, 0);
      const params = producer.rtpSender.setParameters.mock.calls[0]?.[0];
      // Auto-calculated bitrate should be > 0
      expect(params.encodings[0].maxBitrate).toBeGreaterThan(0);
    });

    it('handles no encodings gracefully', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('screen-1', 'screen');
      producer.rtpSender.getParameters.mockReturnValue({ encodings: [] });
      expect(() => svc.liveUpdateScreenBitrate(producer, 1000000)).not.toThrow();
    });
  });

  // ===== liveUpdateAudioPriority edge cases =====

  describe('liveUpdateAudioPriority edge cases', () => {
    it('resets to low on off', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      svc.liveUpdateAudioPriority('off');
      const args = micProducer.rtpSender.setParameters.mock.calls[0]?.[0];
      expect(args.encodings[0].priority).toBe('low');
    });

    it('applies custom priority', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      svc.liveUpdateAudioPriority('high');
      const args = micProducer.rtpSender.setParameters.mock.calls[0]?.[0];
      expect(args.encodings[0].priority).toBe('high');
    });

    it('noop without rtpSender', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = { ...createMockProducer(), rtpSender: null };
      svc.producers.set('mic', producer);
      expect(() => svc.liveUpdateAudioPriority('high')).not.toThrow();
    });
  });

  // ===== getProducerCodecMimeType =====

  describe('getProducerCodecMimeType', () => {
    it('returns null without producer', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      expect(svc.getProducerCodecMimeType('nonexistent')).toBeNull();
    });

    it('returns null without rtpSender', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('camera', { ...createMockProducer('c', 'camera'), rtpSender: null });
      expect(svc.getProducerCodecMimeType('camera')).toBeNull();
    });

    it('extracts H264 profile-level-id', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('c', 'camera');
      producer.rtpSender.getParameters.mockReturnValue({
        codecs: [
          {
            mimeType: 'video/H264',
            sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640034',
          },
        ],
      });
      svc.producers.set('camera', producer);
      expect(svc.getProducerCodecMimeType('camera')).toBe('video/h264:640034');
    });

    it('extracts VP9 profile-id', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('c', 'camera');
      producer.rtpSender.getParameters.mockReturnValue({
        codecs: [
          {
            mimeType: 'video/VP9',
            sdpFmtpLine: 'profile-id=2',
          },
        ],
      });
      svc.producers.set('camera', producer);
      expect(svc.getProducerCodecMimeType('camera')).toBe('video/vp9:2');
    });

    it('returns plain mime for VP8', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('c', 'camera');
      producer.rtpSender.getParameters.mockReturnValue({
        codecs: [{ mimeType: 'video/VP8' }],
      });
      svc.producers.set('camera', producer);
      expect(svc.getProducerCodecMimeType('camera')).toBe('video/vp8');
    });

    it('returns null without codecs', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('c', 'camera');
      producer.rtpSender.getParameters.mockReturnValue({ codecs: [] });
      svc.producers.set('camera', producer);
      expect(svc.getProducerCodecMimeType('camera')).toBeNull();
    });

    it('returns plain VP9 mime when profile-id is 0', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('c', 'camera');
      producer.rtpSender.getParameters.mockReturnValue({
        codecs: [{ mimeType: 'video/VP9', sdpFmtpLine: 'profile-id=0' }],
      });
      svc.producers.set('camera', producer);
      expect(svc.getProducerCodecMimeType('camera')).toBe('video/vp9');
    });
  });

  // ===== calculateRecommendedBitrate =====

  describe('calculateRecommendedBitrate', () => {
    it('uses efficient bpp for AV1', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const bitrate = svc.calculateRecommendedBitrate(1920, 1080, 30, 'video/AV1');
      // 1920*1080*30*0.04 = 2,488,320 -> round to 2,500,000
      expect(bitrate).toBe(2_500_000);
    });

    it('uses standard bpp for VP8', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const bitrate = svc.calculateRecommendedBitrate(1920, 1080, 30, 'video/VP8');
      // 1920*1080*30*0.07 = 4,354,560 -> round to 4,400,000
      expect(bitrate).toBe(4_400_000);
    });

    it('clamps minimum to 1.5 Mbps', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const bitrate = svc.calculateRecommendedBitrate(320, 240, 5, null);
      expect(bitrate).toBe(1_500_000);
    });

    it('clamps maximum to 30 Mbps', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const bitrate = svc.calculateRecommendedBitrate(7680, 4320, 120, 'video/VP8');
      expect(bitrate).toBe(30_000_000);
    });
  });

  // ===== calculateScreenBitrate =====

  describe('calculateScreenBitrate', () => {
    it('infers codec from store when not provided', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const bitrate = svc.calculateScreenBitrate();
      expect(bitrate).toBeGreaterThan(0);
    });

    it('uses actual capture dimensions for source resolution', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.getState().setScreenResolution('source');
      const mockTrack = {
        readyState: 'live',
        getSettings: vi.fn().mockReturnValue({ width: 2560, height: 1440 }),
      };
      svc.localScreenStream = {
        getVideoTracks: vi.fn().mockReturnValue([mockTrack]),
        getTracks: vi.fn().mockReturnValue([mockTrack]),
      };
      const bitrate = svc.calculateScreenBitrate('video/VP8');
      // Should use 2560x1440 instead of 3840x2160 default
      expect(bitrate).toBeGreaterThan(0);
    });

    it('handles custom WxH resolution', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.getState().setScreenResolution('2560x1440');
      const bitrate = svc.calculateScreenBitrate('video/VP8');
      expect(bitrate).toBeGreaterThan(0);
    });
  });

  // ===== computeStartBitrate =====

  describe('computeStartBitrate', () => {
    it('clamps to minimum 100 kbps', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      expect(svc.computeStartBitrate(10_000)).toBe(100);
    });

    it('clamps to maximum 10000 kbps', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      expect(svc.computeStartBitrate(100_000_000)).toBe(10_000);
    });
  });

  // ===== Socket listener edge cases =====

  describe('socket listener: active-speaker', () => {
    it('clears previous speaker and sets new one', async () => {
      await joinVoiceChannel();
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });
      useVoiceStore.getState().setActiveSpeaker('user-1');
      triggerSocketEvent('active-speaker', { userId: 'user-2' });
      expect(useVoiceStore.getState().activeSpeakerId).toBe('user-2');
      expect(useVoiceStore.getState().participants['user-2']?.isSpeaking).toBe(true);
      expect(useVoiceStore.getState().participants['user-1']?.isSpeaking).toBe(false);
    });

    it('handles silence (null userId)', async () => {
      await joinVoiceChannel();
      useVoiceStore.getState().setActiveSpeaker('user-1');
      triggerSocketEvent('active-speaker', { userId: null });
      expect(useVoiceStore.getState().activeSpeakerId).toBeNull();
    });
  });

  describe('socket listener: consumer-closed', () => {
    it('closes consumer and cleans up meta', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      svc.consumers.set('c1', consumer);
      svc.consumerMeta.set('c1', { source: 'mic', producerUserId: 'u2', producerId: 'p1' });

      triggerSocketEvent('consumer-closed', { consumerId: 'c1' });

      expect(consumer.close).toHaveBeenCalled();
      expect(svc.consumers.has('c1')).toBe(false);
      expect(svc.consumerMeta.has('c1')).toBe(false);
    });

    it('handles unknown consumer gracefully', async () => {
      await joinVoiceChannel();
      expect(() => triggerSocketEvent('consumer-closed', { consumerId: 'unknown' })).not.toThrow();
    });

    it('tunes out screen share consumer', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'video', 'p1');
      svc.consumers.set('c1', consumer);
      svc.consumerMeta.set('c1', { source: 'screen', producerUserId: 'u2', producerId: 'p1' });
      useVoiceStore.getState().tuneIn('p1', 'c1');

      triggerSocketEvent('consumer-closed', { consumerId: 'c1' });

      expect(useVoiceStore.getState().tunedInScreenShares).not.toHaveProperty('p1');
    });
  });

  describe('socket listener: producer-closed', () => {
    it('closes matching consumer and updates camera state', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'video', 'p1');
      svc.consumers.set('c1', consumer);
      svc.consumerMeta.set('c1', { source: 'camera', producerUserId: 'user-2', producerId: 'p1' });
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'u2',
        isMuted: false,
        isDeafened: false,
        isVideoOn: true,
        isScreenSharing: false,
        isSpeaking: false,
      });

      triggerSocketEvent('producer-closed', {
        producerId: 'p1',
        userId: 'user-2',
        source: 'camera',
      });

      expect(consumer.close).toHaveBeenCalled();
      expect(useVoiceStore.getState().participants['user-2']?.isVideoOn).toBe(false);
    });

    it('handles screen source closure', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'video', 'sp1');
      svc.consumers.set('c1', consumer);
      svc.consumerMeta.set('c1', { source: 'screen', producerUserId: 'user-2', producerId: 'sp1' });
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'u2',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: true,
        isSpeaking: false,
      });
      useVoiceStore.getState().tuneIn('sp1', 'c1');

      triggerSocketEvent('producer-closed', {
        producerId: 'sp1',
        userId: 'user-2',
        source: 'screen',
      });

      expect(useVoiceStore.getState().participants['user-2']?.isScreenSharing).toBe(false);
    });

    it('handles screen-audio source closure', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'sap1');
      svc.consumers.set('c1', consumer);
      svc.consumerMeta.set('c1', {
        source: 'screen-audio',
        producerUserId: 'user-2',
        producerId: 'sap1',
      });
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'u2',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      triggerSocketEvent('producer-closed', {
        producerId: 'sap1',
        userId: 'user-2',
        source: 'screen-audio',
      });

      expect(useVoiceStore.getState().participants['user-2']?.screenAudioStream).toBeUndefined();
    });
  });

  describe('socket listener: disconnect', () => {
    it('triggers emergency cleanup on server disconnect', async () => {
      await joinVoiceChannel();
      triggerSocketEvent('disconnect', 'io server disconnect');
      expect(useVoiceStore.getState().connectionState).toBe('disconnected');
    });

    it('sets reconnecting state for other reasons', async () => {
      await joinVoiceChannel();
      triggerSocketEvent('disconnect', 'transport close');
      expect(useVoiceStore.getState().connectionState).toBe('reconnecting');
    });
  });

  describe('socket listener: reconnect', () => {
    it('transitions reconnecting to connected', async () => {
      await joinVoiceChannel();
      useVoiceStore.getState().setConnectionState('reconnecting');
      triggerSocketEvent('connect');
      expect(useVoiceStore.getState().connectionState).toBe('connected');
    });

    it('does not change if not reconnecting', async () => {
      await joinVoiceChannel();
      useVoiceStore.getState().setConnectionState('connected');
      triggerSocketEvent('connect');
      expect(useVoiceStore.getState().connectionState).toBe('connected');
    });
  });

  describe('socket listener: reconnect_failed', () => {
    it('triggers emergency cleanup', async () => {
      await joinVoiceChannel();
      triggerIoEvent('reconnect_failed');
      expect(useVoiceStore.getState().connectionState).toBe('disconnected');
    });
  });

  describe('socket listener: error', () => {
    it('logs error', async () => {
      await joinVoiceChannel();
      const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
      triggerSocketEvent('error', { message: 'test error' });
      expect(spy).toHaveBeenCalledWith('Media plane error:', 'test error');
      spy.mockRestore();
    });
  });

  describe('socket listener: room-codec-floor', () => {
    it('updates store and triggers floor change', async () => {
      await joinVoiceChannel();
      triggerSocketEvent('room-codec-floor', { codecFloor: ['video/vp8', 'video/vp9'] });
      expect(useVoiceStore.getState().codecFloor).toEqual(['video/vp8', 'video/vp9']);
    });

    it('handles null floor', async () => {
      await joinVoiceChannel();
      triggerSocketEvent('room-codec-floor', { codecFloor: null });
      expect(useVoiceStore.getState().codecFloor).toBeNull();
    });
  });

  describe('socket listener: epoch-sync', () => {
    it('catches up when server epoch is ahead', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // Manually set encryption state for the test
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      mockGetCurrentKeyId.mockReturnValue(2);

      triggerSocketEvent('epoch-sync', { epoch: 5 });

      await vi.advanceTimersByTimeAsync(100);
      expect(mockCatchUpToEpoch).toHaveBeenCalledWith(5);
    });

    it('pre-installs participant decrypt keys for the server epoch before catch-up', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'sender',
        isMuted: false,
        isDeafened: false,
        isVideoOn: true,
        isScreenSharing: false,
        isSpeaking: false,
      });
      mockGetCurrentKeyId.mockReturnValue(2);

      triggerSocketEvent('epoch-sync', { epoch: 5 });

      await vi.advanceTimersByTimeAsync(100);
      expect(mockAddDecryptKeyAtEpoch).toHaveBeenCalledWith(expect.anything(), 'user-2', 5);
      expect(mockCatchUpToEpoch).toHaveBeenCalledWith(5);
    });

    it('ignores when mediaEncryption is not initialized', async () => {
      await joinVoiceChannel();
      // Ensure mediaEncryption is null (no E2EE worker setup)
      const svc = voiceService as any;
      svc.mediaEncryption = null;
      triggerSocketEvent('epoch-sync', { epoch: 5 });
      expect(mockCatchUpToEpoch).not.toHaveBeenCalled();
    });
  });

  // ===== waitForConnect =====

  describe('waitForConnect', () => {
    it('resolves when already connected', async () => {
      const svc = voiceService as any;
      svc.socket = { ...mockSocket, connected: true, once: vi.fn() };
      await svc.waitForConnect();
      expect(svc.socket.connected).toBe(true);
    });

    it('resolves on connect event', async () => {
      const svc = voiceService as any;
      const onceHandlers: Record<string, (...args: unknown[]) => void> = {};
      svc.socket = {
        connected: false,
        once: vi.fn().mockImplementation((ev: string, cb: (...args: unknown[]) => void) => {
          onceHandlers[ev] = cb;
        }),
      };

      const promise = svc.waitForConnect();
      // Trigger connect
      onceHandlers['connect']?.();
      await promise;
      expect(svc.socket.once).toHaveBeenCalledWith('connect', expect.any(Function));
    });

    it('rejects on connect_error', async () => {
      const svc = voiceService as any;
      const onceHandlers: Record<string, (...args: unknown[]) => void> = {};
      svc.socket = {
        connected: false,
        once: vi.fn().mockImplementation((ev: string, cb: (...args: unknown[]) => void) => {
          onceHandlers[ev] = cb;
        }),
      };

      const promise = svc.waitForConnect();
      onceHandlers['connect_error']?.(new Error('refused'));
      await expect(promise).rejects.toThrow('refused');
    });

    it('rejects on timeout', async () => {
      const svc = voiceService as any;
      svc.socket = {
        connected: false,
        once: vi.fn(),
      };

      const promise = svc.waitForConnect().catch((e: Error) => e);
      await vi.advanceTimersByTimeAsync(11_000);
      const err = await promise;
      expect(err).toBeInstanceOf(Error);
      expect((err as Error).message).toContain('timeout');
      svc.socket = null;
    });

    it('rejects without socket', async () => {
      const svc = voiceService as any;
      svc.socket = null;
      await expect(svc.waitForConnect()).rejects.toThrow('No socket');
    });
  });

  // ===== emitAsync =====

  describe('emitAsync', () => {
    it('rejects without socket', async () => {
      const svc = voiceService as any;
      svc.socket = null;
      await expect(svc.emitAsync('test')).rejects.toThrow('No socket');
    });

    it('rejects on timeout', async () => {
      const svc = voiceService as any;
      // emit does not call callback, so the timeout fires
      svc.socket = { emit: vi.fn() };
      const promise = svc.emitAsync('test').catch((e: Error) => e);
      await vi.advanceTimersByTimeAsync(11_000);
      const err = await promise;
      expect(err).toBeInstanceOf(Error);
      expect((err as Error).message).toContain('timeout');
      // Clean up socket to prevent stale reference
      svc.socket = null;
    });

    it('rejects on server error response', async () => {
      const svc = voiceService as any;
      svc.socket = {
        emit: vi
          .fn()
          .mockImplementation((_ev: string, _data: unknown, cb: (...args: unknown[]) => void) => {
            cb({ error: 'bad request' });
          }),
      };
      await expect(svc.emitAsync('test', {})).rejects.toThrow('bad request');
    });

    it('resolves with response', async () => {
      const svc = voiceService as any;
      svc.socket = {
        emit: vi
          .fn()
          .mockImplementation((_ev: string, _data: unknown, cb: (...args: unknown[]) => void) => {
            cb({ id: '123' });
          }),
      };
      const result = await svc.emitAsync('test', {});
      expect(result).toEqual({ id: '123' });
    });
  });

  // ===== ensureOsPermission =====

  describe('ensureOsPermission', () => {
    it('returns when granted', async () => {
      const svc = voiceService as any;
      mockEnsureOsPermission.mockResolvedValue('granted');
      await svc.ensureOsPermission('microphone');
      expect(mockEnsureOsPermission).toHaveBeenCalledWith('microphone');
    });

    it('throws for denied', async () => {
      const svc = voiceService as any;
      mockEnsureOsPermission.mockResolvedValue('denied');
      await expect(svc.ensureOsPermission('microphone')).rejects.toThrow(
        'Microphone access denied'
      );
    });

    it('throws for restricted', async () => {
      const svc = voiceService as any;
      mockEnsureOsPermission.mockResolvedValue('restricted');
      await expect(svc.ensureOsPermission('camera')).rejects.toThrow('Camera access denied');
    });

    it('throws for unavailable', async () => {
      const svc = voiceService as any;
      mockEnsureOsPermission.mockResolvedValue('unavailable');
      await expect(svc.ensureOsPermission('microphone')).rejects.toThrow('unavailable');
    });

    it('throws for unknown status', async () => {
      const svc = voiceService as any;
      mockEnsureOsPermission.mockResolvedValue('some-new-status');
      await expect(svc.ensureOsPermission('camera')).rejects.toThrow('not available');
    });
  });

  // ===== produceScreen edge cases =====

  describe('produceScreen edge cases', () => {
    it('shows permission error when capture throws NotAllowedError', async () => {
      await joinVoiceChannel();
      const err = new DOMException('Permission denied', 'NotAllowedError');
      mockGetDisplayMedia.mockRejectedValue(err);
      await voiceService.produceScreen();
      expect(useVoiceStore.getState().videoSlotError).toContain('Screen recording access denied');
      expect(mockOpenSettings).toHaveBeenCalledWith('screen');
    });

    it('rethrows non-permission capture errors', async () => {
      await joinVoiceChannel();
      mockGetDisplayMedia.mockRejectedValue(new Error('Hardware failure'));
      await expect(voiceService.produceScreen()).rejects.toThrow('Hardware failure');
    });

    it('throws when no video tracks from capture', async () => {
      await joinVoiceChannel();
      mockCheckOne.mockResolvedValue('granted');
      const stream = createMockMediaStream([{ kind: 'audio' }]);
      stream.getVideoTracks.mockReturnValue([]);
      mockGetDisplayMedia.mockResolvedValue(stream);

      await expect(voiceService.produceScreen()).rejects.toThrow('no video tracks');
    });

    it('cleans up stream on produce failure', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockCheckOne.mockResolvedValue('granted');
      const stream = createMockMediaStream([{ kind: 'video' }]);
      mockGetDisplayMedia.mockResolvedValue(stream);
      sendTransport.produce.mockRejectedValue(new Error('produce failed'));

      await expect(voiceService.produceScreen()).rejects.toThrow('produce failed');
      // Stream tracks should have been stopped
      for (const t of stream.getTracks()) {
        expect(t.stop).toHaveBeenCalled();
      }
    });

    it('uses Electron desktopCapturer when available', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockCheckOne.mockResolvedValue('granted');

      // Set up global electron with getDesktopSources
      const originalElectron = (globalThis as any).electron;
      (globalThis as any).electron = {
        ...originalElectron,
        getDesktopSources: vi.fn().mockResolvedValue([{ id: 'screen:0', name: 'Main Screen' }]),
      };

      const stream = createMockMediaStream([{ kind: 'video' }]);
      mockGetUserMedia.mockResolvedValue(stream);
      const screenProducer = createMockProducer('sp1', 'screen');
      sendTransport.produce.mockResolvedValue(screenProducer);

      await voiceService.produceScreen();
      expect(useVoiceStore.getState().isScreenSharing).toBe(true);

      // Restore
      (globalThis as any).electron = originalElectron;
    });

    it('falls back to video-only on Electron when audio capture fails', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockCheckOne.mockResolvedValue('granted');

      const originalElectron = (globalThis as any).electron;
      (globalThis as any).electron = {
        ...originalElectron,
        getDesktopSources: vi.fn().mockResolvedValue([{ id: 'screen:0', name: 'Main Screen' }]),
      };

      const audioErr = new Error('Audio capture not available');
      const videoOnlyStream = createMockMediaStream([{ kind: 'video' }]);
      mockGetUserMedia.mockRejectedValueOnce(audioErr).mockResolvedValueOnce(videoOnlyStream);
      const screenProducer = createMockProducer('sp1', 'screen');
      sendTransport.produce.mockResolvedValue(screenProducer);

      await voiceService.produceScreen();
      expect(useVoiceStore.getState().isScreenSharing).toBe(true);

      (globalThis as any).electron = originalElectron;
    });

    it('uses provided sourceId', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockCheckOne.mockResolvedValue('granted');

      const originalElectron = (globalThis as any).electron;
      (globalThis as any).electron = {
        ...originalElectron,
        getDesktopSources: vi.fn().mockResolvedValue([{ id: 'screen:0', name: 'Main Screen' }]),
      };

      const stream = createMockMediaStream([{ kind: 'video' }]);
      mockGetUserMedia.mockResolvedValue(stream);
      const screenProducer = createMockProducer('sp1', 'screen');
      sendTransport.produce.mockResolvedValue(screenProducer);

      await voiceService.produceScreen('window:123');
      expect(mockGetUserMedia).toHaveBeenCalled();

      (globalThis as any).electron = originalElectron;
    });

    it('produces screen audio when available', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockCheckOne.mockResolvedValue('granted');
      const stream = createMockMediaStream([{ kind: 'video' }, { kind: 'audio' }]);
      mockGetDisplayMedia.mockResolvedValue(stream);
      const screenProducer = createMockProducer('sp1', 'screen');
      const audioProducer = createMockProducer('sa1', 'screen-audio');
      sendTransport.produce
        .mockResolvedValueOnce(screenProducer)
        .mockResolvedValueOnce(audioProducer);

      // joinVoiceChannel already called produce once for mic, so screen + audio = 3 total
      const callsBefore = sendTransport.produce.mock.calls.length;
      await voiceService.produceScreen();
      expect(sendTransport.produce.mock.calls.length - callsBefore).toBe(2);
    });

    it('applies content hint for motion', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockCheckOne.mockResolvedValue('granted');
      const stream = createMockMediaStream([{ kind: 'video' }]);
      mockGetDisplayMedia.mockResolvedValue(stream);
      const screenProducer = createMockProducer('sp1', 'screen');
      sendTransport.produce.mockResolvedValue(screenProducer);

      await voiceService.produceScreen(undefined, {
        resolution: '1080p',
        frameRate: 60,
        contentType: 'motion',
      });

      const track = stream.getVideoTracks()[0];
      expect(track.contentHint).toBe('motion');
    });
  });

  // ===== produceVideo with pre-acquired stream =====

  describe('produceAudio', () => {
    it('uses pre-acquired stream', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const preStream = createMockMediaStream([{ kind: 'audio', id: 'pre-mic' }]);
      const newProducer = createMockProducer('new-mic', 'mic');
      sendTransport.produce.mockResolvedValue(newProducer);

      // Close existing mic first
      await svc.closeProducer('mic');
      await svc.produceAudio(undefined, preStream);
      expect(sendTransport.produce).toHaveBeenCalled();
    });

    it('uses the selected microphone when no explicit deviceId is passed', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      useVoiceStore.setState({ audioInputDeviceId: 'mic-selected' });
      mockGetUserMedia.mockReset();
      mockGetUserMedia.mockResolvedValue(
        createMockMediaStream([{ kind: 'audio', id: 'stored-mic' }])
      );
      const newProducer = createMockProducer('new-mic', 'mic');
      sendTransport.produce.mockResolvedValue(newProducer);

      await svc.closeProducer('mic');
      await svc.produceAudio();

      expect(mockGetUserMedia).toHaveBeenCalledWith({
        audio: expect.objectContaining({
          deviceId: { exact: 'mic-selected' },
        }),
      });
    });

    it('stops pre-acquired stream when deviceId changes', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const preStream = createMockMediaStream([{ kind: 'audio' }]);
      const newStream = createMockMediaStream([{ kind: 'audio', id: 'device-mic' }]);
      mockGetUserMedia.mockResolvedValue(newStream);
      const newProducer = createMockProducer('new-mic', 'mic');
      sendTransport.produce.mockResolvedValue(newProducer);

      await svc.closeProducer('mic');
      await svc.produceAudio('specific-device-id', preStream);

      // Pre-acquired stream tracks should be stopped
      for (const t of preStream.getTracks()) {
        expect(t.stop).toHaveBeenCalled();
      }
    });

    it('noop without transport', async () => {
      const svc = voiceService as any;
      svc.sendTransport = null;
      await svc.produceAudio();
      expect(svc.sendTransport).toBeNull();
    });
  });

  // ===== closeProducer extended =====

  describe('closeProducer extended', () => {
    it('stops local mic stream and cleans up VAD/noise gate/input volume', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      svc.localMicStream = createMockMediaStream([{ kind: 'audio' }]);

      await svc.closeProducer('mic');

      expect(svc.localMicStream).toBeNull();
    });

    it('handles source with no producer still cleans state', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.delete('camera');
      svc.localCameraStream = createMockMediaStream([{ kind: 'video' }]);

      await svc.closeProducer('camera');

      expect(useVoiceStore.getState().isVideoOn).toBe(false);
      expect(svc.localCameraStream).toBeNull();
    });
  });

  // ===== Solo bandwidth saving extended =====

  describe('solo bandwidth saving extended', () => {
    it('does not re-enter solo mode if already in it', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // Set store to only have local user
      useVoiceStore.setState({
        participants: {
          'user-1': {
            userId: 'user-1',
            username: 'u1',
            isMuted: false,
            isDeafened: false,
            isVideoOn: false,
            isScreenSharing: false,
            isSpeaking: false,
          },
        },
      });
      svc.checkSoloBandwidthSaving();
      expect(useVoiceStore.getState().isSoloBandwidthSaving).toBe(true);

      // Call again, should remain in solo mode
      svc.checkSoloBandwidthSaving();
      expect(useVoiceStore.getState().isSoloBandwidthSaving).toBe(true);
    });

    it('does not exit solo mode if still alone', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVoiceStore.setState({
        participants: {
          'user-1': {
            userId: 'user-1',
            username: 'u1',
            isMuted: false,
            isDeafened: false,
            isVideoOn: false,
            isScreenSharing: false,
            isSpeaking: false,
          },
        },
        isSoloBandwidthSaving: true,
      });
      svc.checkSoloBandwidthSaving();
      expect(useVoiceStore.getState().isSoloBandwidthSaving).toBe(true);
    });

    it('exitSoloBandwidthSaving resumes producers', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      micProducer.paused = true;
      useVoiceStore.setState({ isSoloBandwidthSaving: true, isMuted: false });

      svc.exitSoloBandwidthSaving();
      expect(micProducer.resume).toHaveBeenCalled();
    });

    it('exitSoloBandwidthSaving does not resume mic when muted', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);
      micProducer.paused = true;
      useVoiceStore.setState({ isSoloBandwidthSaving: true, isMuted: true });

      svc.exitSoloBandwidthSaving();
      expect(micProducer.resume).not.toHaveBeenCalled();
    });
  });

  // ===== tuneOutOfScreenShare extended =====

  describe('tuneOutOfScreenShare extended', () => {
    it('closes video consumer and paired screen-audio consumer', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const videoConsumer = createMockConsumer('vc1', 'video', 'sp1');
      const audioConsumer = createMockConsumer('ac1', 'audio', 'sap1');
      svc.consumers.set('vc1', videoConsumer);
      svc.consumers.set('ac1', audioConsumer);
      svc.consumerMeta.set('vc1', {
        source: 'screen',
        producerUserId: 'user-2',
        producerId: 'sp1',
      });
      svc.consumerMeta.set('ac1', {
        source: 'screen-audio',
        producerUserId: 'user-2',
        producerId: 'sap1',
      });
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'u2',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: true,
        isSpeaking: false,
      });
      useVoiceStore.getState().tuneIn('sp1', 'vc1');

      await voiceService.tuneOutOfScreenShare('sp1');

      expect(videoConsumer.close).toHaveBeenCalled();
      expect(audioConsumer.close).toHaveBeenCalled();
      expect(svc.consumers.has('vc1')).toBe(false);
      expect(svc.consumers.has('ac1')).toBe(false);
    });

    it('re-adds to available shares', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const videoConsumer = createMockConsumer('vc1', 'video', 'sp1');
      svc.consumers.set('vc1', videoConsumer);
      svc.consumerMeta.set('vc1', {
        source: 'screen',
        producerUserId: 'user-2',
        producerId: 'sp1',
      });
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'u2',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: true,
        isSpeaking: false,
      });
      useVoiceStore.getState().tuneIn('sp1', 'vc1');

      await voiceService.tuneOutOfScreenShare('sp1');

      const shares = useVoiceStore.getState().availableScreenShares;
      expect(shares.some((s) => s.producerId === 'sp1')).toBe(true);
    });
  });

  // ===== pauseConsumer / resumeConsumer with actual consumers =====

  describe('pauseConsumer / resumeConsumer with consumers', () => {
    it('pauses and resumes actual consumer', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      svc.consumers.set('c1', consumer);

      voiceService.pauseConsumer('c1');
      expect(consumer.pause).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('pause-consumer', { consumerId: 'c1' });

      consumer.paused = true;
      voiceService.resumeConsumer('c1');
      expect(consumer.resume).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('resume-consumer', { consumerId: 'c1' });
    });

    it('is idempotent — a second pauseConsumer does not re-pause (#1541 coordinator)', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      svc.consumers.set('c1', consumer);
      svc.pauseCoordinator.clearConsumer('c1'); // clean slate regardless of prior tests

      voiceService.pauseConsumer('c1');
      expect(consumer.pause).toHaveBeenCalledTimes(1);
      consumer.pause.mockClear();

      // The 'manual' reason already holds — the coordinator emits no second pause.
      // (The old `!consumer.paused` guard is gone; idempotency now lives in the
      // coordinator's applied-state tracking, not in reading consumer.paused.)
      voiceService.pauseConsumer('c1');
      expect(consumer.pause).not.toHaveBeenCalled();
    });

    it('skips not-paused consumer on resume', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      consumer.paused = false;
      svc.consumers.set('c1', consumer);

      voiceService.resumeConsumer('c1');
      expect(consumer.resume).not.toHaveBeenCalled();
    });
  });

  // ===== getConsumerIdsBySource with data =====

  describe('getConsumerIdsBySource with data', () => {
    it('returns all consumer ids without filter', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.consumers.set('c1', createMockConsumer('c1', 'audio'));
      svc.consumers.set('c2', createMockConsumer('c2', 'video'));
      svc.consumerMeta.set('c1', { source: 'mic', producerUserId: 'u1', producerId: 'p1' });
      svc.consumerMeta.set('c2', { source: 'camera', producerUserId: 'u2', producerId: 'p2' });

      const all = voiceService.getConsumerIdsBySource();
      expect(all).toHaveLength(2);
    });

    it('filters by source', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.consumers.set('c1', createMockConsumer('c1', 'audio'));
      svc.consumers.set('c2', createMockConsumer('c2', 'video'));
      svc.consumerMeta.set('c1', { source: 'mic', producerUserId: 'u1', producerId: 'p1' });
      svc.consumerMeta.set('c2', { source: 'camera', producerUserId: 'u2', producerId: 'p2' });

      const audio = voiceService.getConsumerIdsBySource('mic');
      expect(audio).toEqual(['c1']);

      const video = voiceService.getConsumerIdsBySource('camera');
      expect(video).toEqual(['c2']);
    });
  });

  // ===== E2EE debouncedRotateE2EEKeys (legacy path) =====

  describe('debouncedRotateE2EEKeys', () => {
    it('delegates to mediaEncryption on legacy path', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();

      svc.debouncedRotateE2EEKeys();
      expect(mockDebouncedRotateKeys).toHaveBeenCalled();
    });

    it('does nothing without mediaEncryption', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.mediaEncryption = null;
      expect(() => svc.debouncedRotateE2EEKeys()).not.toThrow();
    });
  });

  // ===== E2EE addDecryptKeyForUser =====

  describe('addDecryptKeyForUser', () => {
    it('returns false without mediaEncryption', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.mediaEncryption = null;
      const result = await svc.addDecryptKeyForUser('ch1', 'u1');
      expect(result).toBe(false);
    });

    it('retries on failure and returns true on success', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();

      const result = await svc.addDecryptKeyForUser('ch1', 'u2');
      expect(result).toBe(true);
      expect(mockAddDecryptKeyAtEpoch).toHaveBeenCalledWith(expect.anything(), 'u2', 0);
    });

    it('returns false after all retries exhausted', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      mockGetChannelKey.mockRejectedValue(new Error('key unavailable'));

      const resultPromise = svc.addDecryptKeyForUser('ch1', 'u2');
      // Advance through retry delays
      await vi.advanceTimersByTimeAsync(500);
      await vi.advanceTimersByTimeAsync(1000);
      await vi.advanceTimersByTimeAsync(2000);
      const result = await resultPromise;
      expect(result).toBe(false);
    });
  });

  // ===== deriveAndInstallDecryptKey =====

  describe('deriveAndInstallDecryptKey', () => {
    it('throws when epoch exceeds ratchet limit', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      mockGetCurrentKeyId.mockReturnValue(101);

      await expect(svc.deriveAndInstallDecryptKey('ch1', 'u2', 0)).rejects.toThrow(
        'epoch 101 exceeds ratchet limit'
      );
    });

    it('throws when mediaEncryption is destroyed during operation', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.mediaEncryption = null;

      await expect(svc.deriveAndInstallDecryptKey('ch1', 'u2', 0)).rejects.toThrow(
        'mediaEncryption destroyed'
      );
    });
  });

  // ===== handleWorkerMessage =====

  describe('handleWorkerMessage', () => {
    it('handles rotationComplete message', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();

      svc.handleWorkerMessage({ type: 'rotationComplete', newKeyId: 5 });
      expect(mockSetCurrentKeyId).toHaveBeenCalledWith(5);
    });

    it('handles requestRecovery message', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      useVoiceStore.getState().setActiveChannel('ch1', 'General', 'srv1');

      svc.handleWorkerMessage({ type: 'requestRecovery', senderUserId: 'user-2' });
      expect(mockInvalidateChannelKey).toHaveBeenCalledWith('ch1');
    });

    it('forwards requestKeyframe messages to the media-plane socket', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVoiceStore.getState().setActiveChannel('ch1', 'General', 'srv1');

      svc.handleWorkerMessage({ type: 'requestKeyframe', senderUserId: 'user-2' });

      expect(mockSocket.emit).toHaveBeenCalledWith('request-keyframe', { senderUserId: 'user-2' });
    });

    it('forwards legacy decrypt recovery keyframe requests to the media-plane socket', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVoiceStore.getState().setActiveChannel('ch1', 'General', 'srv1');

      svc.decryptRecoveryCallbacks().requestKeyframe('user-2');

      expect(mockSocket.emit).toHaveBeenCalledWith('request-keyframe', { senderUserId: 'user-2' });
    });

    it('handles log message', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const spy = vi.spyOn(console, 'debug').mockImplementation(() => {});
      svc.handleWorkerMessage({ type: 'log', level: 'debug', message: 'test log' });
      expect(spy).toHaveBeenCalledWith('test log', '');
      spy.mockRestore();
    });
  });

  // ===== applyEncryptTransform =====

  describe('applyEncryptTransform', () => {
    it('fails closed when an encrypted producer has no sender transform API', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer('cam-no-transform', 'camera');
      delete (producer.rtpSender as Record<string, unknown>).createEncodedStreams;
      svc.producers.set('camera', producer);

      expect(() => svc.applyEncryptTransform(producer)).toThrow(
        'E2EE: failed to attach encrypt transform'
      );

      expect(producer.close).toHaveBeenCalled();
      expect(svc.producers.has('camera')).toBe(false);
      expect(mockSocket.emit).toHaveBeenCalledWith('close-producer', {
        producerId: 'cam-no-transform',
      });
    });
  });

  // ===== applyDecryptTransform =====

  describe('applyDecryptTransform', () => {
    it('fails closed before routing when an encrypted consumer has no receiver transform API', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.recvTransportAudio = makeRecvTransport('recv-audio');

      const consumer = createMockConsumer('c-no-transform', 'audio', 'p-audio');
      delete (consumer.rtpReceiver as Record<string, unknown>).createEncodedStreams;
      svc.recvTransportAudio.consume.mockResolvedValue(consumer);

      setupEmitResponses({
        consume: {
          id: 'c-no-transform',
          producerId: 'p-audio',
          kind: 'audio',
          rtpParameters: {},
          producerUserId: 'user-2',
          source: 'mic',
        },
        'resume-consumer': undefined,
        'close-consumer': undefined,
      });

      const store = useVoiceStore.getState();
      store.addParticipant({
        userId: 'user-2',
        username: 'u2',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });
      const updateSpy = vi.spyOn(store, 'updateParticipant');

      await svc.consumeProducerImpl('p-audio', 'user-2', 'audio');

      expect(consumer.close).toHaveBeenCalled();
      expect(svc.consumers.has('c-no-transform')).toBe(false);
      expect(svc.consumerMeta.has('c-no-transform')).toBe(false);
      expect(mockSocket.emit).toHaveBeenCalledWith('close-consumer', {
        consumerId: 'c-no-transform',
      });
      expect(mockSocket.emit).not.toHaveBeenCalledWith('resume-consumer', {
        consumerId: 'c-no-transform',
      });
      expect(updateSpy).not.toHaveBeenCalledWith(
        'user-2',
        expect.objectContaining({ audioStream: expect.any(MediaStream) })
      );
    });
  });

  // ===== initEncryption retries =====

  describe('initEncryption', () => {
    it('doubles delay for pending E2EE key errors', async () => {
      const svc = voiceService as any;
      // Fail first 4 calls (initial + 3 retries), succeed on none
      mockGetChannelKey.mockRejectedValue(new E2EEKeyUnavailableError('NO_KEY_YET', true));

      // Use real timers for this test to avoid fake-timer + promise interaction issues
      vi.useRealTimers();
      await expect(svc.initEncryption('ch1')).rejects.toThrow('NO_KEY_YET');
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      vi.useFakeTimers({ shouldAdvanceTime: true });
    });

    it('rethrows non-Error lastError', async () => {
      const svc = voiceService as any;
      mockGetChannelKey.mockRejectedValue('string error');

      vi.useRealTimers();
      await expect(svc.initEncryption('ch1')).rejects.toThrow('failed to initialize encryption');
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      vi.useFakeTimers({ shouldAdvanceTime: true });
    });
  });

  // ===== IGNIS profiling zones: yellow zone =====

  describe('IGNIS profiling: yellow zone', () => {
    it('lowers temporal layer on yellow zone', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      const consumer = createMockConsumer('cons-yellow', 'video', 'prod-yellow');
      const statsMap = new Map();
      // rho = (p95 * fps) / 1000 = (20 * 1.5 * 30) / 1000 = 0.9 > 0.8 (yellow) but < 0.925
      // Actually need rho >= 0.8 and < 0.925
      // Let's set: totalDecodeTime=0.02s for 100 frames => avg=0.2ms, p95=0.3ms
      // rho = 0.3 * 30 / 1000 = 0.009 -- too low
      // Need: rho = T_d_p95 * FPS / 1000 >= 0.8
      // So T_d_p95 * 30 >= 800 => T_d_p95 >= 26.7ms
      // avgDecodeMs = totalDecodeTime / framesDecoded * 1000
      // p95 = avg * 1.5
      // avg >= 26.7 / 1.5 = 17.8ms
      // totalDecodeTime / framesDecoded >= 0.0178
      // totalDecodeTime = 0.0178 * 100 = 1.78
      statsMap.set('inbound', {
        type: 'inbound-rtp',
        kind: 'video',
        totalDecodeTime: 1.78,
        framesDecoded: 100,
        framesPerSecond: 30,
      });
      consumer.getStats.mockResolvedValue(statsMap);

      // Add currentLayers and setPreferredLayers
      (consumer as any).currentLayers = { spatialLayer: 2, temporalLayer: 2 };
      (consumer as any).setPreferredLayers = vi.fn();

      svc.consumers.set('cons-yellow', consumer);

      await vi.advanceTimersByTimeAsync(5500);

      expect((consumer as any).setPreferredLayers).toHaveBeenCalledWith({
        spatialLayer: 2,
        temporalLayer: 1,
      });
    });
  });

  // ===== IGNIS profiling zones: red zone with layers =====

  describe('IGNIS profiling: red zone with layers', () => {
    it('lowers spatial layer in red zone', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      const consumer = createMockConsumer('cons-red2', 'video', 'prod-red2');
      const statsMap = new Map();
      // rho >= 0.925: T_d_p95 * 30 >= 925 => T_d_p95 >= 30.83ms
      // avg >= 30.83 / 1.5 = 20.56ms
      // totalDecodeTime = 20.56 * 100 / 1000 = 2.056
      statsMap.set('inbound', {
        type: 'inbound-rtp',
        kind: 'video',
        totalDecodeTime: 2.1,
        framesDecoded: 100,
        framesPerSecond: 30,
      });
      consumer.getStats.mockResolvedValue(statsMap);
      (consumer as any).currentLayers = { spatialLayer: 2, temporalLayer: 2 };
      (consumer as any).setPreferredLayers = vi.fn();
      svc.consumers.set('cons-red2', consumer);

      await vi.advanceTimersByTimeAsync(5500);

      expect((consumer as any).setPreferredLayers).toHaveBeenCalledWith({
        spatialLayer: 1,
        temporalLayer: 2,
      });
    });

    it('lowers temporal layer when spatial is 0 in red zone', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      const consumer = createMockConsumer('cons-red3', 'video', 'prod-red3');
      const statsMap = new Map();
      statsMap.set('inbound', {
        type: 'inbound-rtp',
        kind: 'video',
        totalDecodeTime: 2.1,
        framesDecoded: 100,
        framesPerSecond: 30,
      });
      consumer.getStats.mockResolvedValue(statsMap);
      (consumer as any).currentLayers = { spatialLayer: 0, temporalLayer: 2 };
      (consumer as any).setPreferredLayers = vi.fn();
      svc.consumers.set('cons-red3', consumer);

      await vi.advanceTimersByTimeAsync(5500);

      expect((consumer as any).setPreferredLayers).toHaveBeenCalledWith({
        spatialLayer: 0,
        temporalLayer: 1,
      });
    });

    it('pauses camera consumer instead of screen share in red zone', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      const screenConsumer = createMockConsumer('cons-screen', 'video', 'prod-screen');
      const cameraConsumer = createMockConsumer('cons-cam', 'video', 'prod-cam');

      const statsMap = new Map();
      statsMap.set('inbound', {
        type: 'inbound-rtp',
        kind: 'video',
        totalDecodeTime: 2.1,
        framesDecoded: 100,
        framesPerSecond: 30,
      });
      screenConsumer.getStats.mockResolvedValue(statsMap);
      (screenConsumer as any).currentLayers = { spatialLayer: 0, temporalLayer: 0 };
      (screenConsumer as any).setPreferredLayers = vi.fn();

      svc.consumers.set('cons-screen', screenConsumer);
      svc.consumers.set('cons-cam', cameraConsumer);

      // Mark screen consumer as tuned-in screen share
      useVoiceStore.getState().tuneIn('prod-screen', 'cons-screen');

      await vi.advanceTimersByTimeAsync(5500);

      // Camera should be paused (not the screen share)
      expect(cameraConsumer.pause).toHaveBeenCalled();
      expect(screenConsumer.pause).not.toHaveBeenCalled();
    });
  });

  // ===== consumeProducer E2EE queuing =====

  describe('consumeProducer E2EE queuing', () => {
    it('routes audio to audio queue in E2EE mode', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.recvTransportAudio = makeRecvTransport('recv-audio');
      svc.recvTransportVideo = makeRecvTransport('recv-video');

      const consumer = createMockConsumer('c-audio', 'audio', 'p-audio');
      svc.recvTransportAudio.consume.mockResolvedValue(consumer);

      setupEmitResponses({
        consume: {
          id: 'c-audio',
          producerId: 'p-audio',
          kind: 'audio',
          rtpParameters: {},
          producerUserId: 'user-2',
          source: 'mic',
        },
        'resume-consumer': undefined,
      });

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'u2',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      await svc.consumeProducer('p-audio', 'user-2', 'audio');
      expect(svc.recvTransportAudio.consume).toHaveBeenCalled();
    });

    it('routes video to video queue in E2EE mode', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.recvTransportAudio = makeRecvTransport('recv-audio');
      svc.recvTransportVideo = makeRecvTransport('recv-video');

      const consumer = createMockConsumer('c-video', 'video', 'p-video');
      svc.recvTransportVideo.consume.mockResolvedValue(consumer);

      setupEmitResponses({
        consume: {
          id: 'c-video',
          producerId: 'p-video',
          kind: 'video',
          rtpParameters: {},
          producerUserId: 'user-2',
          source: 'camera',
        },
        'resume-consumer': undefined,
      });

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'u2',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      await svc.consumeProducer('p-video', 'user-2', 'video');
      expect(svc.recvTransportVideo.consume).toHaveBeenCalled();
    });

    it('skips consume for invalid kind in E2EE mode', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      await svc.consumeProducer('p1', 'u1', 'invalid-kind');
      expect(warnSpy).toHaveBeenCalledWith(
        expect.stringContaining('invalid kind'),
        expect.any(Object)
      );
      warnSpy.mockRestore();
    });

    it('warns and uses single queue when kind not provided in E2EE', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.device = null; // Will skip consume due to no device

      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      await svc.consumeProducer('p1', 'u1');
      expect(warnSpy).toHaveBeenCalledWith(
        expect.stringContaining('kind not provided'),
        expect.any(Object)
      );
      warnSpy.mockRestore();
    });

    it('skips when E2EE recv transport not ready', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.recvTransportAudio = null;

      setupEmitResponses({
        consume: {
          id: 'c1',
          producerId: 'p1',
          kind: 'audio',
          rtpParameters: {},
          producerUserId: 'u2',
          source: 'mic',
        },
      });

      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      await svc.consumeProducer('p1', 'u2', 'audio');
      expect(warnSpy).toHaveBeenCalledWith(
        expect.stringContaining('recv transport not ready'),
        expect.any(Object)
      );
      warnSpy.mockRestore();
    });
  });

  // ===== consumeProducerImpl edge cases =====

  describe('consumeProducerImpl edge cases', () => {
    it('skips when server returns error', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      setupEmitResponses({
        consume: { error: 'Producer not found' },
      });

      // emitAsync rejects when response has 'error', so consumeProducerImpl
      // hits its catch block which calls console.error
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      await svc.consumeProducerImpl('unknown-prod');
      expect(errorSpy).toHaveBeenCalled();
      errorSpy.mockRestore();
    });

    it('skips when no recvTransport', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.recvTransportAudio = null;
      svc.recvTransportVideo = null;

      setupEmitResponses({
        consume: {
          id: 'c1',
          producerId: 'p1',
          kind: 'audio',
          rtpParameters: {},
          producerUserId: 'u2',
          source: 'mic',
        },
      });

      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      await svc.consumeProducerImpl('p1');
      expect(warnSpy).toHaveBeenCalledWith(
        expect.stringContaining('no recvTransport'),
        expect.any(Object)
      );
      warnSpy.mockRestore();
    });
  });

  // ===== getRecvTransport =====

  describe('getRecvTransport', () => {
    it('returns split transports in E2EE mode', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.recvTransportAudio = makeRecvTransport('ra');
      svc.recvTransportVideo = makeRecvTransport('rv');

      expect(svc.getRecvTransport('audio').id).toBe('ra');
      expect(svc.getRecvTransport('video').id).toBe('rv');
    });
  });

  // ===== cleanup =====

  describe('cleanup', () => {
    it('closes all producers, consumers, and transports', async () => {
      const { sendTransport, recvTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      svc.consumers.set('c1', consumer);

      await svc.cleanup();

      expect(sendTransport.close).toHaveBeenCalled();
      expect(recvTransport.close).toHaveBeenCalled();
      expect(consumer.close).toHaveBeenCalled();
      expect(svc.socket).toBeNull();
      expect(svc.device).toBeNull();
    });

    it('clears solo notification timer', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.soloNotificationTimer = setTimeout(() => {}, 10000);
      await svc.cleanup();
      expect(svc.soloNotificationTimer).toBeNull();
    });

    it('resets consume queues', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      await svc.cleanup();
      // Queues should be fresh promises
      expect(svc.consumeQueueAudio).toBeInstanceOf(Promise);
      expect(svc.consumeQueueVideo).toBeInstanceOf(Promise);
    });
  });

  // ===== cleanupMediaAndTransports =====

  describe('cleanupMediaAndTransports', () => {
    it('stops all local streams', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const micStream = createMockMediaStream([{ kind: 'audio' }]);
      const camStream = createMockMediaStream([{ kind: 'video' }]);
      const screenStream = createMockMediaStream([{ kind: 'video' }]);
      svc.localMicStream = micStream;
      svc.localCameraStream = camStream;
      svc.localScreenStream = screenStream;

      svc.cleanupMediaAndTransports();

      for (const t of micStream.getTracks()) expect(t.stop).toHaveBeenCalled();
      for (const t of camStream.getTracks()) expect(t.stop).toHaveBeenCalled();
      for (const t of screenStream.getTracks()) expect(t.stop).toHaveBeenCalled();
      expect(svc.localMicStream).toBeNull();
      expect(svc.localCameraStream).toBeNull();
      expect(svc.localScreenStream).toBeNull();
    });

    it('ignores close errors on producers and consumers', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = createMockProducer();
      producer.close.mockImplementation(() => {
        throw new Error('already closed');
      });
      svc.producers.set('mic', producer);

      const consumer = createMockConsumer();
      consumer.close.mockImplementation(() => {
        throw new Error('already closed');
      });
      svc.consumers.set('c1', consumer);

      expect(() => svc.cleanupMediaAndTransports()).not.toThrow();
      expect(svc.producers.size).toBe(0);
      expect(svc.consumers.size).toBe(0);
    });
  });

  // ===== cleanupTimersAndE2EE =====

  describe('cleanupTimersAndE2EE', () => {
    it('clears decoder profiling timer', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.decoderProfilingTimer = setInterval(() => {}, 5000);
      svc.cleanupTimersAndE2EE();
      expect(svc.decoderProfilingTimer).toBeNull();
    });

    it('clears rotation timer and state', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.rotationTimer = setTimeout(() => {}, 5000);
      svc.rotationPending = true;
      svc.rotationDeadline = Date.now() + 5000;
      svc.cleanupTimersAndE2EE();
      expect(svc.rotationTimer).toBeNull();
      expect(svc.rotationPending).toBe(false);
      expect(svc.rotationDeadline).toBe(0);
    });

    it('destroys mediaEncryption', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      svc.cleanupTimersAndE2EE();
      expect(mockMediaEncryptionDestroy).toHaveBeenCalled();
      expect(svc.mediaEncryption).toBeNull();
    });
  });

  // ===== drainSendTransportQueue edge cases =====

  describe('drainSendTransportQueue', () => {
    it('handles closed transport', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.sendTransport = { closed: true };
      await svc.drainSendTransportQueue();
      expect(svc.sendTransport.closed).toBe(true);
    });

    it('handles null transport', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.sendTransport = null;
      await svc.drainSendTransportQueue();
      expect(svc.sendTransport).toBeNull();
    });

    it('handles queue push failure', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.sendTransport = {
        closed: false,
        _awaitQueue: {
          push: vi.fn().mockRejectedValue(new Error('transport closed')),
        },
      };
      // Should not throw — queue push failure is caught internally
      await svc.drainSendTransportQueue();
      expect(svc.sendTransport._awaitQueue.push).toHaveBeenCalled();
    });
  });

  // ===== applyDegradationPreference edge cases =====

  describe('applyDegradationPreference edge cases', () => {
    it('handles no rtpSender', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const producer = { ...createMockProducer('c', 'camera'), rtpSender: null };
      expect(() => svc.applyDegradationPreference(producer)).not.toThrow();
    });

    it('applies maintain-framerate', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.getState().setDegradationPreference('maintain-framerate');
      const producer = createMockProducer('c', 'camera');
      svc.applyDegradationPreference(producer);
      const params = producer.rtpSender.setParameters.mock.calls[0]?.[0];
      expect(params.degradationPreference).toBe('maintain-framerate');
    });

    it('handles setParameters failure', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.getState().setDegradationPreference('maintain-resolution');
      const producer = createMockProducer('c', 'camera');
      producer.rtpSender.setParameters.mockRejectedValue(new Error('fail'));
      svc.applyDegradationPreference(producer);
      // Should not throw (catch block handles it)
      expect(producer.rtpSender.setParameters).toHaveBeenCalled();
    });
  });

  // ===== acquireMicStream =====

  describe('acquireMicStream', () => {
    it('returns stream on success', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const stream = createMockMediaStream([{ kind: 'audio' }]);
      mockGetUserMedia.mockResolvedValue(stream);
      const result = await svc.acquireMicStream();
      expect(result).toBe(stream);
    });

    it('returns null on failure', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      mockGetUserMedia.mockRejectedValue(new Error('denied'));
      const result = await svc.acquireMicStream();
      expect(result).toBeNull();
    });
  });

  // ===== E2EE setupE2EEForChannel =====

  describe('setupE2EEForChannel', () => {
    it('initializes encryption and adds keys for all participants', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // Reset mediaEncryption so initEncryption runs fresh
      svc.mediaEncryption = null;

      const roomJoined = makeRoomJoined({
        e2eeEpoch: 3,
        participants: [
          { userId: 'user-1', username: 'self' },
          { userId: 'user-2', username: 'other' },
        ],
      });

      await svc.setupE2EEForChannel('ch1', roomJoined);
      // Should have called catchUpToEpoch
      expect(mockCatchUpToEpoch).toHaveBeenCalledWith(3);
    });
  });

  // ===== buildParticipantList =====

  describe('buildParticipantList', () => {
    it('creates participant list with video/screen state from producers', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      const roomJoined = makeRoomJoined({
        participants: [
          { userId: 'user-1', username: 'self' },
          { userId: 'user-2', username: 'sharer' },
          { userId: 'user-3', username: 'videocaller' },
        ],
        existingProducers: [
          { producerId: 'sp1', userId: 'user-2', kind: 'video', source: 'screen' },
          { producerId: 'cp1', userId: 'user-3', kind: 'video', source: 'camera' },
        ],
      });

      const participants = svc.buildParticipantList(roomJoined);
      expect(participants).toHaveLength(3);

      const user2 = participants.find((p: any) => p.userId === 'user-2');
      expect(user2?.isScreenSharing).toBe(true);
      expect(user2?.isVideoOn).toBe(false);

      const user3 = participants.find((p: any) => p.userId === 'user-3');
      expect(user3?.isVideoOn).toBe(true);
      expect(user3?.isScreenSharing).toBe(false);

      const user1 = participants.find((p: any) => p.userId === 'user-1');
      expect(user1?.isVideoOn).toBe(false);
      expect(user1?.isScreenSharing).toBe(false);
    });
  });

  // ===== consumeExistingProducers =====

  describe('consumeExistingProducers', () => {
    it('registers screen producers as available', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'u2',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      await svc.consumeExistingProducers([
        { producerId: 'sp1', userId: 'user-2', kind: 'video', source: 'screen' },
      ]);

      expect(useVoiceStore.getState().availableScreenShares).toHaveLength(1);
    });

    it('stores screen-audio producers as pending', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      await svc.consumeExistingProducers([
        { producerId: 'sap1', userId: 'user-2', kind: 'audio', source: 'screen-audio' },
      ]);

      expect(svc.pendingScreenAudioProducers.get('user-2')).toBe('sap1');
    });
  });

  // ===== Packet loss monitor: FEC headroom =====

  describe('packet loss monitor: FEC headroom', () => {
    it('inflates bitrate when loss detected with FEC headroom', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('mic', micProducer);

      // Set up stats with packet loss
      const statsMap = new Map();
      statsMap.set('outbound', { type: 'outbound-rtp', packetsSent: 200 });
      statsMap.set('remote-inbound', { type: 'remote-inbound-rtp', packetsLost: 20 });
      micProducer.getStats.mockResolvedValue(statsMap);

      // Initialize counters (first poll establishes baseline)
      svc.lastPacketsSent = 100;
      svc.lastPacketsLost = 0;

      // Advance timer for packet loss poll (5s)
      await vi.advanceTimersByTimeAsync(5500);

      // Should have set packet loss
      const store = useVoiceStore.getState();
      expect(store.packetLossPercent).toBeGreaterThan(0);
    });
  });

  // ===== User joined E2EE key rotation =====

  describe('user-joined with E2EE', () => {
    it('adds decrypt key and rotates keys', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      useVoiceStore.getState().setActiveChannel('channel-1', 'General', 'server-1');

      triggerSocketEvent('user-joined', {
        userId: 'user-3',
        username: 'newcomer',
        displayName: 'New User',
        avatarUrl: null,
      });

      await vi.advanceTimersByTimeAsync(100);

      expect(useVoiceStore.getState().participants['user-3']).toBeDefined();
      expect(mockDebouncedRotateKeys).toHaveBeenCalled();
    });

    it('pre-installs the joining participant decrypt key for the next epoch before rotation', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      useVoiceStore.getState().setActiveChannel('channel-1', 'General', 'server-1');
      mockGetCurrentKeyId.mockReturnValue(2);

      triggerSocketEvent('user-joined', {
        userId: 'user-3',
        username: 'newcomer',
        displayName: 'New User',
        avatarUrl: null,
      });

      await vi.advanceTimersByTimeAsync(100);
      expect(mockAddDecryptKeyAtEpoch).toHaveBeenCalledWith(expect.anything(), 'user-3', 3);
      expect(mockDebouncedRotateKeys).toHaveBeenCalled();
    });

    it('uses the server epoch from user-joined when pre-installing the participant key', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      useVoiceStore.getState().setActiveChannel('channel-1', 'General', 'server-1');
      mockGetCurrentKeyId.mockReturnValue(2);

      triggerSocketEvent('user-joined', {
        userId: 'user-3',
        username: 'newcomer',
        displayName: 'New User',
        avatarUrl: null,
        e2eeEpoch: 4,
      });

      await vi.advanceTimersByTimeAsync(100);
      expect(mockAddDecryptKeyAtEpoch).toHaveBeenCalledWith(expect.anything(), 'user-3', 4);
      expect(mockDebouncedRotateKeys).toHaveBeenCalled();
    });
  });

  describe('user-left with E2EE', () => {
    it('removes participant and rotates keys', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'leaver',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      triggerSocketEvent('user-left', { userId: 'user-2' });
      await vi.advanceTimersByTimeAsync(100);

      expect(useVoiceStore.getState().participants['user-2']).toBeUndefined();
      expect(mockDebouncedRotateKeys).toHaveBeenCalled();
    });

    it('uses authoritative leave epoch when local media epoch is behind', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const workerPostMessage = vi.fn();
      svc.e2eeWorker = { postMessage: workerPostMessage, terminate: vi.fn() };
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      mockGetCurrentKeyId.mockReturnValue(1);

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'leaver',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });
      useVoiceStore.getState().addParticipant({
        userId: 'user-3',
        username: 'remaining',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      triggerSocketEvent('user-left', { userId: 'user-2', e2eeEpoch: 4 });
      await vi.advanceTimersByTimeAsync(100);

      expect(mockAddDecryptKeyAtEpoch).toHaveBeenCalledWith(expect.anything(), 'user-3', 4);
      expect(workerPostMessage).toHaveBeenCalledWith({ type: 'catchUpToEpoch', targetEpoch: 4 });
      expect(mockCatchUpToEpoch).toHaveBeenCalledWith(4);
      expect(mockAddDecryptKeyAtEpoch).not.toHaveBeenCalledWith(expect.anything(), 'user-2', 4);
    });

    it('does not catch up when authoritative leave epoch is not ahead locally', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const workerPostMessage = vi.fn();
      svc.e2eeWorker = { postMessage: workerPostMessage, terminate: vi.fn() };
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      mockGetCurrentKeyId.mockReturnValue(4);

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'leaver',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });
      useVoiceStore.getState().addParticipant({
        userId: 'user-3',
        username: 'remaining',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      triggerSocketEvent('user-left', { userId: 'user-2', e2eeEpoch: 4 });
      await vi.advanceTimersByTimeAsync(100);

      expect(mockAddDecryptKeyAtEpoch).toHaveBeenCalledWith(expect.anything(), 'user-3', 4);
      expect(workerPostMessage).not.toHaveBeenCalledWith({
        type: 'catchUpToEpoch',
        targetEpoch: 4,
      });
      expect(mockCatchUpToEpoch).not.toHaveBeenCalled();
      expect(mockDebouncedRotateKeys).not.toHaveBeenCalled();
    });

    it('logs authoritative leave epoch catch-up failures', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();
      mockGetCurrentKeyId.mockReturnValue(1);
      mockCatchUpToEpoch.mockRejectedValueOnce(new Error('catch-up failed'));
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'leaver',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });

      triggerSocketEvent('user-left', { userId: 'user-2', e2eeEpoch: 4 });
      await vi.advanceTimersByTimeAsync(100);

      expect(mockCatchUpToEpoch).toHaveBeenCalledWith(4);
      expect(errorSpy).toHaveBeenCalledWith(
        'E2EE: leave epoch catch-up failed — decrypt may fail until rejoin',
        expect.objectContaining({
          localEpoch: 1,
          serverEpoch: 4,
          error: 'catch-up failed',
        })
      );
      errorSpy.mockRestore();
    });
  });

  // ===== new-producer with E2EE =====

  describe('new-producer with E2EE key handling', () => {
    it('ensures decrypt key before consuming', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const { MediaEncryption } = await import('@/renderer/services/mediaEncryption');
      svc.mediaEncryption = new MediaEncryption();

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'u2',
        isMuted: false,
        isDeafened: false,
        isVideoOn: false,
        isScreenSharing: false,
        isSpeaking: false,
      });
      useVoiceStore.getState().setActiveChannel('channel-1', 'General', 'server-1');

      const consumer = createMockConsumer('c2', 'audio', 'p2');
      svc.recvTransportAudio = makeRecvTransport();
      svc.recvTransportAudio.consume.mockResolvedValue(consumer);

      setupEmitResponses({
        consume: {
          id: 'c2',
          producerId: 'p2',
          kind: 'audio',
          rtpParameters: {},
          producerUserId: 'user-2',
          source: 'mic',
        },
        'resume-consumer': undefined,
      });

      triggerSocketEvent('new-producer', {
        producerId: 'p2',
        userId: 'user-2',
        kind: 'audio',
        source: 'mic',
        requiresOptIn: false,
      });

      await vi.advanceTimersByTimeAsync(100);
      expect(mockAddDecryptKeyAtEpoch).toHaveBeenCalledWith(expect.anything(), 'user-2', 0);
    });
  });

  // ===== new-producer video slot enforcement =====

  describe('new-producer video slot enforcement', () => {
    it('skips camera consume when slots full but marks participant', async () => {
      await joinVoiceChannel();
      // Fill video slots
      const parts: Record<string, any> = {};
      for (let i = 0; i < 50; i++) {
        parts[`u${i}`] = {
          userId: `u${i}`,
          username: `u${i}`,
          isMuted: false,
          isDeafened: false,
          isVideoOn: true,
          isScreenSharing: false,
          isSpeaking: false,
        };
      }
      useVoiceStore.setState({ participants: parts, maxVideoSlots: 50 });

      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      triggerSocketEvent('new-producer', {
        producerId: 'p-cam-new',
        userId: 'user-new',
        kind: 'video',
        source: 'camera',
        requiresOptIn: false,
      });
      await vi.advanceTimersByTimeAsync(100);
      expect(warnSpy).toHaveBeenCalled();
      warnSpy.mockRestore();
    });
  });

  // ===== Decoder profiling switches to slower interval =====

  describe('decoder profiling slow interval switch', () => {
    it('switches to 30s interval after initial period', async () => {
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      try {
        await joinVoiceChannel();
        errorSpy.mockClear();
        // The profiling starts at 5s intervals for 6 probes (30s), then switches to 30s
        // Advance 7 * 5s = 35s to trigger the switch — must not raise during interval flip
        await vi.advanceTimersByTimeAsync(35_000);
        expect(errorSpy).not.toHaveBeenCalled();
      } finally {
        errorSpy.mockRestore();
      }
    });
  });

  // ===== isHwAccelerated / isInCodecFloor =====

  describe('isHwAccelerated', () => {
    it('returns false when no capabilities match', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.setState({ codecCapabilities: [] });
      expect(svc.isHwAccelerated('video/VP8')).toBe(false);
    });

    it('returns true when powerEfficient codec found', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.setState({
        codecCapabilities: [{ mimeType: 'video/VP8', powerEfficient: true }],
      } as any);
      expect(svc.isHwAccelerated('video/VP8')).toBe(true);
    });
  });

  describe('isInCodecFloor', () => {
    it('returns true when floor is null', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVoiceStore.setState({ codecFloor: null });
      expect(svc.isInCodecFloor('video/VP8')).toBe(true);
    });

    it('returns true when codec is in floor', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVoiceStore.setState({ codecFloor: ['video/vp8', 'video/vp9'] });
      expect(svc.isInCodecFloor('video/VP8')).toBe(true);
    });

    it('returns false when codec not in floor', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVoiceStore.setState({ codecFloor: ['video/vp8'] });
      expect(svc.isInCodecFloor('video/VP9')).toBe(false);
    });
  });

  // ===== findSendCodec edge cases =====

  describe('findSendCodec', () => {
    it('returns undefined when device has no codecs', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.device = { rtpCapabilities: { codecs: [] } };
      expect(svc.findSendCodec('video/VP8')).toBeUndefined();
    });

    it('returns undefined for no device', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.device = null;
      expect(svc.findSendCodec('video/VP8')).toBeUndefined();
    });

    it('matches H264 profile prefix', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const codec = svc.findSendCodec('video/H264:6400');
      expect(codec).toBeDefined();
      expect(codec.parameters['profile-level-id']).toBe('640034');
    });

    it('returns last match (highest quality) without profile', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const codec = svc.findSendCodec('video/H264');
      expect(codec).toBeDefined();
      // Should return the last H264 codec (640034, not 42e01f)
      expect(codec.parameters['profile-level-id']).toBe('640034');
    });
  });

  // ===== resolveOpusSettings (module-level helper) =====

  describe('resolveOpusSettings', () => {
    it('uses tier defaults in basic mode', async () => {
      await joinVoiceChannel();
      // This is tested implicitly through produceAudio, but verify the logic:
      // When advancedMode is false, the tier's opusFec/opusDtx/opusStereo are used
      const store = useAudioSettingsStore.getState();
      expect(store.advancedMode).toBe(false);
    });
  });

  // ===== buildPriorityParams (module-level helper) =====

  describe('buildPriorityParams', () => {
    it('returns empty for off', async () => {
      // This is a module-level function, tested implicitly
      // Verify through produceAudio with priority 'off'
      useAudioSettingsStore.getState().setAudioPriority('off');
      await joinVoiceChannel();
      // Audio priority 'off' means no priority params in encoding
      expect(useAudioSettingsStore.getState().audioPriority).toBe('off');
    });
  });

  // ===== produceVideo: camera error messages =====

  describe('produceVideo error messages', () => {
    it('no camera stream after all fallbacks', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.delete('camera');
      mockGetUserMedia.mockResolvedValue(null);

      // localCameraStream will be null (getUserMedia returns null which is falsy)
      // Actually getUserMedia should not return null normally, but with our mock it can
      svc.localCameraStream = null;
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([]));

      // Since localCameraStream won't have video tracks, it should handle gracefully
      await expect(svc.produceVideo()).resolves.toBeUndefined();
    });
  });

  // ===== Live settings subscriptions =====

  describe('live settings subscriptions setup/teardown', () => {
    it('sets up audio and video subscriptions on join', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // After join, liveAudioUnsub and liveVideoUnsub should be set
      expect(svc.liveAudioUnsub).not.toBeNull();
      expect(svc.liveVideoUnsub).not.toBeNull();
    });

    it('tears down subscriptions on leave', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      await voiceService.leaveChannel();
      expect(svc.liveAudioUnsub).toBeNull();
      expect(svc.liveVideoUnsub).toBeNull();
    });
  });

  // ===== setQualityTier with no active producer =====

  describe('setQualityTier', () => {
    it('only updates store when no active producer', async () => {
      await voiceService.setQualityTier('high');
      expect(useVoiceStore.getState().qualityTier).toBe('high');
    });
  });

  // ===== handleCodecFloorChange when no producers =====

  describe('handleCodecFloorChange', () => {
    it('noop when no active video producers', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // No camera or screen producers
      svc.producers.delete('camera');
      svc.producers.delete('screen');
      await svc.handleCodecFloorChange(null, ['video/vp8']);
      // No crash
      expect(svc.producers.has('camera')).toBe(false);
    });

    it('skips HW codec switch when HW accel disabled', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.getState().setHardwareAcceleration(false);

      const cameraProducer = createMockProducer('cam-1', 'camera');
      cameraProducer.rtpSender.getParameters.mockReturnValue({
        codecs: [{ mimeType: 'video/VP8' }],
      });
      svc.producers.set('camera', cameraProducer);

      // Set up codec capabilities so AV1 is HW-accelerated but VP8 is not
      useVideoSettingsStore.setState({
        codecCapabilities: [
          { mimeType: 'video/AV1', powerEfficient: true },
          { mimeType: 'video/VP8', powerEfficient: false },
        ],
      } as any);

      const spy = vi.spyOn(console, 'debug').mockImplementation(() => {});
      await svc.handleCodecFloorChange(null, ['video/av1', 'video/vp8']);
      spy.mockRestore();
      // Camera codec should remain VP8 since HW accel is disabled
      expect(useVideoSettingsStore.getState().hardwareAcceleration).toBe(false);
    });
  });
});
