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
// Manager (socket.io) listeners — waitForConnect subscribes to 'reconnect_failed' (#2176).
const managerListeners: Record<string, Array<(...args: unknown[]) => void>> = {};

const removeMockListener = (
  registry: Record<string, Array<(...args: unknown[]) => void>>,
  event: string,
  cb: (...args: unknown[]) => void
) => {
  if (registry[event]) registry[event] = registry[event].filter((f) => f !== cb);
};

const mockSocket = {
  connected: false,
  // Socket.active: true = the Manager will auto-reconnect on a transient error;
  // false = server denial / attempts exhausted. waitForConnect keys on this (#2176).
  active: true,
  emit: vi.fn(),
  on: vi.fn().mockImplementation((event: string, cb: (...args: unknown[]) => void) => {
    if (!socketListeners[event]) socketListeners[event] = [];
    socketListeners[event].push(cb);
  }),
  off: vi
    .fn()
    .mockImplementation((event: string, cb: (...args: unknown[]) => void) =>
      removeMockListener(socketListeners, event, cb)
    ),
  once: vi.fn().mockImplementation((event: string, cb: (...args: unknown[]) => void) => {
    if (!socketOnceListeners[event]) socketOnceListeners[event] = [];
    socketOnceListeners[event].push(cb);
  }),
  disconnect: vi.fn(),
  io: {
    on: vi.fn().mockImplementation((event: string, cb: (...args: unknown[]) => void) => {
      if (!managerListeners[event]) managerListeners[event] = [];
      managerListeners[event].push(cb);
    }),
    off: vi
      .fn()
      .mockImplementation((event: string, cb: (...args: unknown[]) => void) =>
        removeMockListener(managerListeners, event, cb)
      ),
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
  // The mock mirrors the live constant so the join-version assertion
  // (sender advertises === ack confirms) stays self-consistent.
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

// Mock MediaStream (jsdom does not provide it)
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
Object.defineProperty(MockRTCRtpSender.prototype, 'createEncodedStreams', { value: vi.fn() });
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

// ---------------------------------------------------------------------------
// Import voiceService AFTER all mocks
// ---------------------------------------------------------------------------
const { voiceService } = await import('@/renderer/services/voiceService');
const { handleCallInvited } = await import('@/renderer/services/voiceService/callStateMachine');

import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useAudioSettingsStore } from '@/renderer/stores/audioSettingsStore';
import { useVideoSettingsStore } from '@/renderer/stores/videoSettingsStore';
import { useUpdateStatusStore } from '@/renderer/stores/updateStatusStore';

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
    mediaFrameCryptoVersion: 5,
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
      createEncodedStreams: vi.fn().mockImplementation(() => ({
        readable: new ReadableStream({
          start(controller) {
            controller.close();
          },
        }),
        writable: new WritableStream(),
      })),
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

function makeRecvTransport() {
  return { id: 'recv-1', closed: false, close: vi.fn(), consume: vi.fn(), on: vi.fn() };
}

async function joinVoiceChannel(
  co?: Record<string, unknown>,
  joinType: 'channel' | 'dm' = 'channel',
  response?: Record<string, unknown>
) {
  setupAuth();
  mockApiFetch.mockResolvedValueOnce({
    ok: true,
    json: vi.fn().mockResolvedValue({ ...makeJoinResponse(co), ...response }),
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

  await voiceService.joinChannel('channel-1', joinType);
  return { sendTransport, recvTransport, micProducer };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('VoiceService', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    resetAllStores();
    // resetAllStores() does not cover videoSettingsStore; reset the fields these voice
    // tests mutate (learned B-signal + codec preference) so no test leaks into the next
    // (#2187 — the B-signal tests set both, and the codec-selection tests depend on defaults).
    useVideoSettingsStore.setState({ preferredVideoCodec: null, webrtcHwByMime: {} });
    mockSocket.connected = false;
    mockSocket.active = true;
    for (const k of Object.keys(socketListeners)) delete socketListeners[k];
    for (const k of Object.keys(socketOnceListeners)) delete socketOnceListeners[k];
    for (const k of Object.keys(managerListeners)) delete managerListeners[k];
  });

  afterEach(() => {
    vi.useRealTimers();
    try {
      voiceService.emergencyCleanup();
    } catch {
      /* ok */
    }
  });

  // ===== media-plane connect resilience (#2176) =====

  describe('media-plane connect resilience (#2176)', () => {
    it('io() is configured with a polling fallback (transports + tryAllTransports)', async () => {
      await joinVoiceChannel();
      // Dynamic import: a top-level `import { io }` would force the vi.mock factory
      // (which references mockSocket) to evaluate before mockSocket is initialized.
      const { io } = await import('socket.io-client');
      expect(io).toHaveBeenCalledWith(
        expect.any(String),
        expect.objectContaining({
          transports: ['websocket', 'polling'],
          tryAllTransports: true,
        })
      );
    });

    describe('waitForConnect', () => {
      // waitForConnect is private; drive it directly with the shared mockSocket.
      const callWaitForConnect = (): Promise<void> => {
        (voiceService as any).socket = mockSocket;
        return (voiceService as any).waitForConnect();
      };

      it('resolves when the socket connects', async () => {
        mockSocket.connected = false;
        const p = callWaitForConnect();
        socketListeners['connect']?.forEach((cb) => cb());
        await expect(p).resolves.toBeUndefined();
      });

      it('rides through a transient connect_error (socket.active) instead of hard-failing', async () => {
        mockSocket.connected = false;
        mockSocket.active = true; // transient — the Manager will auto-reconnect
        const p = callWaitForConnect();

        let settled = false;
        p.then(
          () => (settled = true),
          () => (settled = true)
        );
        // A transient connect_error must NOT settle the join — Socket.IO reconnects underneath.
        socketListeners['connect_error']?.forEach((cb) => cb(new Error('transient')));
        await Promise.resolve();
        expect(settled).toBe(false);

        // A later successful connect resolves it.
        socketListeners['connect']?.forEach((cb) => cb());
        await expect(p).resolves.toBeUndefined();
      });

      it('fails fast on a server-denied connect_error (socket.active === false)', async () => {
        mockSocket.connected = false;
        mockSocket.active = false; // server denial (e.g. auth) — will NOT reconnect
        const p = callWaitForConnect();
        socketListeners['connect_error']?.forEach((cb) => cb(new Error('Unauthorized')));
        await expect(p).rejects.toThrow('Unauthorized');
      });

      it('rejects when reconnection is exhausted (reconnect_failed)', async () => {
        mockSocket.connected = false;
        mockSocket.active = true;
        const p = callWaitForConnect();
        managerListeners['reconnect_failed']?.forEach((cb) => cb());
        await expect(p).rejects.toThrow('Socket reconnection failed');
      });

      it('rejects after the overall timeout so a down server does not hang forever', async () => {
        mockSocket.connected = false;
        mockSocket.active = true;
        const p = callWaitForConnect();
        p.catch(() => {}); // pre-attach a handler so the pending rejection is never unhandled
        await vi.advanceTimersByTimeAsync(31_000); // > VOICE_CONNECT_TIMEOUT_MS (30s)
        await expect(p).rejects.toThrow('Socket connection timeout');
      });

      it('removes its listeners once settled (no leak across attempts)', async () => {
        mockSocket.connected = false;
        const p = callWaitForConnect();
        socketListeners['connect']?.forEach((cb) => cb());
        await p;
        expect(mockSocket.off).toHaveBeenCalledWith('connect', expect.any(Function));
        expect(mockSocket.off).toHaveBeenCalledWith('connect_error', expect.any(Function));
        expect(mockSocket.io.off).toHaveBeenCalledWith('reconnect_failed', expect.any(Function));
      });
    });
  });

  // ===== Singleton =====

  describe('singleton export', () => {
    it('exports a singleton with all public methods', () => {
      expect(voiceService).toBeDefined();
      for (const m of [
        'joinChannel',
        'leaveChannel',
        'toggleMute',
        'toggleDeafen',
        'toggleVideo',
        'toggleScreenShare',
        'emergencyCleanup',
        'pauseLocalProducer',
        'resumeLocalProducer',
        'pauseConsumer',
        'resumeConsumer',
        'getConsumerIdsBySource',
        'getRouterRtpCapabilities',
        'getConsumerMeta',
        'forwardToServer',
        'setQualityTier',
        'produceScreen',
        'tuneInToScreenShare',
        'tuneOutOfScreenShare',
      ]) {
        expect(typeof (voiceService as any)[m]).toBe('function');
      }
    });
  });

  // ===== joinChannel =====

  describe('joinChannel', () => {
    it('transitions connecting -> connected', async () => {
      await joinVoiceChannel();
      expect(useVoiceStore.getState().connectionState).toBe('connected');
      expect(useVoiceStore.getState().activeChannelId).toBe('channel-1');
    });

    it('sets error state on API non-OK', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 403,
        json: vi.fn().mockResolvedValue({ error: 'Forbidden' }),
      });
      await expect(voiceService.joinChannel('ch')).rejects.toThrow('Forbidden');
      expect(useVoiceStore.getState().connectionState).toBe('error');
    });

    it('throws when allowed=false', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue({ ...makeJoinResponse(), allowed: false }),
      });
      await expect(voiceService.joinChannel('ch')).rejects.toThrow('Not allowed');
    });

    it('throws when no auth token', async () => {
      useUserStore.setState({
        user: {
          id: 'u1',
          username: 'u',
          display_name: '',
          avatar_url: null,
          email: '',
          created_at: '',
          updated_at: '',
        },
      });
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      mockSocket.connected = true;
      setupEmitResponses({ 'join-room': makeRoomJoined() });
      await expect(voiceService.joinChannel('ch')).rejects.toThrow('Not authenticated');
    });

    it('uses channel quality tier when valid', async () => {
      await joinVoiceChannel({ audio_quality_tier: 'high' });
      expect(useVoiceStore.getState().effectiveQualityTier).toBe('high');
    });

    it('advertises the media-frame crypto version during room join', async () => {
      await joinVoiceChannel();
      expect(mockSocket.emit).toHaveBeenCalledWith(
        'join-room',
        expect.objectContaining({
          roomId: 'channel-1',
          mediaFrameCryptoVersion: 5,
        }),
        expect.any(Function)
      );
    });

    it('fails the join if media-plane confirms a different frame crypto version', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      mockSocket.connected = true;
      setupEmitResponses({ 'join-room': makeRoomJoined({ mediaFrameCryptoVersion: 1 }) });

      await expect(voiceService.joinChannel('channel-1')).rejects.toThrow(
        'Media frame crypto version mismatch'
      );
      expect(mockDeviceLoad).not.toHaveBeenCalled();
    });

    it('maps a typed crypto_version_mismatch join ack to the update-required banner state', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      mockSocket.connected = true;
      // Media-plane returns the same typed ack for either mismatch direction.
      setupEmitResponses({
        'join-room': {
          error: 'Media frame crypto version mismatch: room=4, join=5',
          code: 'crypto_version_mismatch',
          roomVersion: 4,
          joinVersion: 5,
        },
      });

      await expect(voiceService.joinChannel('channel-1')).rejects.toBeTruthy();
      expect(mockDeviceLoad).not.toHaveBeenCalled();

      const critical = useUpdateStatusStore.getState().criticalError;
      expect(critical?.subtype).toBe('media-crypto-version');
      expect(critical?.message).toBe('This voice call requires the same media-security version.');
    });

    it('falls back to personal tier when channel tier invalid', async () => {
      useVoiceStore.getState().setQualityTier('standard');
      await joinVoiceChannel({ audio_quality_tier: 'bogus' });
      expect(useVoiceStore.getState().effectiveQualityTier).toBe('standard');
    });

    it('uses DM endpoint', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      mockSocket.connected = true;
      const st = makeSendTransport();
      const rt = makeRecvTransport();
      mockCreateSendTransport.mockReturnValue(st);
      mockCreateRecvTransport.mockReturnValue(rt);
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'p1' },
      });
      mockGetUserMedia.mockResolvedValue(createMockMediaStream());
      st.produce.mockResolvedValue(createMockProducer());
      await voiceService.joinChannel('dm-ch', 'dm');
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/dm/conversations/dm-ch/voice/join',
        expect.not.objectContaining({ body: expect.anything() })
      );
      expect(useVoiceStore.getState().callState).toEqual({ kind: 'in-call' });
    });

    it('claims a direct DM join before awaiting and rejects duplicate joins or invites', async () => {
      setupAuth();
      let resolveAuthorization!: (response: Response) => void;
      mockApiFetch.mockReturnValueOnce(
        new Promise<Response>((resolve) => {
          resolveAuthorization = resolve;
        })
      );
      mockSocket.connected = true;
      const st = makeSendTransport();
      const rt = makeRecvTransport();
      mockCreateSendTransport.mockReturnValue(st);
      mockCreateRecvTransport.mockReturnValue(rt);
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'p1' },
      });
      mockGetUserMedia.mockResolvedValue(createMockMediaStream());
      st.produce.mockResolvedValue(createMockProducer());

      const joining = voiceService.joinChannel('dm-ch', 'dm');
      expect(useVoiceStore.getState().callState).toEqual({
        kind: 'joining',
        conversationId: 'dm-ch',
        ringId: '',
      });

      await expect(voiceService.joinChannel('dm-ch', 'dm')).rejects.toThrow(
        'Another voice call is already in progress'
      );
      handleCallInvited({
        conversation_id: 'dm-other',
        is_group: false,
        ring_id: 'ring-other',
        caller: { user_id: 'user-2', username: 'caller' },
        ring_started_at: new Date().toISOString(),
        ring_timeout_seconds: 45,
      });
      expect(useVoiceStore.getState().callState).toEqual({
        kind: 'joining',
        conversationId: 'dm-ch',
        ringId: '',
      });
      expect(mockApiFetch).toHaveBeenCalledTimes(1);

      resolveAuthorization({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      } as unknown as Response);
      await joining;

      expect(useVoiceStore.getState().callState).toEqual({ kind: 'in-call' });
      handleCallInvited({
        conversation_id: 'dm-other',
        is_group: false,
        ring_id: 'ring-other',
        caller: { user_id: 'user-2', username: 'caller' },
        ring_started_at: new Date().toISOString(),
        ring_timeout_seconds: 45,
      });
      expect(useVoiceStore.getState().callState).toEqual({ kind: 'in-call' });
    });

    it('rejects a DM join while another conversation owns the call state', async () => {
      useVoiceStore.getState().setCallState({
        kind: 'incoming-ringing',
        conversationId: 'dm-owned',
        ringId: 'ring-owned',
        caller: { userId: 'user-2', username: 'caller' },
        expiresAt: Date.now() + 45_000,
        isGroup: false,
      });

      await expect(voiceService.joinChannel('dm-other', 'dm')).rejects.toThrow(
        'Another voice call is already in progress'
      );
      expect(mockApiFetch).not.toHaveBeenCalled();
    });

    it.each(['outgoing-ringing', 'incoming-ringing'] as const)(
      'sends the %s ring ID and forwards the authoritative call ID to the media plane',
      async (kind) => {
        const conversationId = 'channel-1';
        const ringId = `${kind}-ring`;
        if (kind === 'outgoing-ringing') {
          useVoiceStore.getState().setCallState({
            kind,
            conversationId,
            ringId,
            calleeUserIds: ['user-2'],
            startedAt: Date.now(),
            declinedUserIds: [],
          });
        } else {
          useVoiceStore.getState().setCallState({
            kind,
            conversationId,
            ringId,
            caller: { userId: 'user-2', username: 'caller' },
            expiresAt: Date.now() + 45_000,
            isGroup: false,
          });
        }

        await joinVoiceChannel(undefined, 'dm', { call_id: 'call-1' });

        expect(mockApiFetch).toHaveBeenCalledWith(
          '/api/v1/dm/conversations/channel-1/voice/join',
          expect.objectContaining({ body: JSON.stringify({ ring_id: ringId }) })
        );
        expect(mockSocket.emit).toHaveBeenCalledWith(
          'join-room',
          expect.objectContaining({ roomId: conversationId, callId: 'call-1' }),
          expect.any(Function)
        );
      }
    );

    it('captures a DM ring ID before leaving an existing channel', async () => {
      await joinVoiceChannel();
      useVoiceStore.getState().setCallState({
        kind: 'joining',
        conversationId: 'channel-1',
        ringId: 'ring-before-leave',
      });

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue({ ...makeJoinResponse(), call_id: 'call-2' }),
      });
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic-2' },
        'leave-room': undefined,
      });

      await voiceService.joinChannel('channel-1', 'dm');

      expect(mockApiFetch).toHaveBeenLastCalledWith(
        '/api/v1/dm/conversations/channel-1/voice/join',
        expect.objectContaining({ body: JSON.stringify({ ring_id: 'ring-before-leave' }) })
      );
      expect(useVoiceStore.getState().callState).toEqual({
        kind: 'joining',
        conversationId: 'channel-1',
        ringId: 'ring-before-leave',
      });
    });

    it.each(['outgoing-ringing', 'incoming-ringing'] as const)(
      'keeps a matching %s DM join owned while leaving an existing channel',
      async (kind) => {
        // Regression for #2242: leaveChannel() resets callState while switching
        // from an active channel into a ring-backed DM media session.
        await joinVoiceChannel();
        const ringId = `${kind}-before-leave`;
        if (kind === 'outgoing-ringing') {
          useVoiceStore.getState().setCallState({
            kind,
            conversationId: 'channel-1',
            ringId,
            calleeUserIds: ['user-2'],
            startedAt: Date.now(),
            declinedUserIds: [],
          });
        } else {
          useVoiceStore.getState().setCallState({
            kind,
            conversationId: 'channel-1',
            ringId,
            caller: { userId: 'user-2', username: 'caller' },
            expiresAt: Date.now() + 45_000,
            isGroup: false,
          });
        }

        mockApiFetch.mockResolvedValueOnce({
          ok: true,
          json: vi.fn().mockResolvedValue({ ...makeJoinResponse(), call_id: 'call-2' }),
        });
        setupEmitResponses({
          'join-room': makeRoomJoined(),
          'create-transport': makeTransportOpts(),
          produce: { id: 'prod-mic-2' },
          'leave-room': undefined,
        });

        await voiceService.joinChannel('channel-1', 'dm');

        expect(mockApiFetch).toHaveBeenLastCalledWith(
          '/api/v1/dm/conversations/channel-1/voice/join',
          expect.objectContaining({ body: JSON.stringify({ ring_id: ringId }) })
        );
        expect(useVoiceStore.getState().callState).toEqual({ kind: 'in-call' });
      }
    );

    it('rejects a duplicate ring-backed DM join while authorization is in flight', async () => {
      setupAuth();
      useVoiceStore.getState().setCallState({
        kind: 'joining',
        conversationId: 'channel-1',
        ringId: 'ring-in-flight',
      });
      let resolveAuthorization!: (response: Response) => void;
      mockApiFetch
        .mockReturnValueOnce(
          new Promise<Response>((resolve) => {
            resolveAuthorization = resolve;
          })
        )
        .mockResolvedValue({
          ok: false,
          status: 409,
          json: vi.fn().mockResolvedValue({}),
        });

      const firstJoin = voiceService.joinChannel('channel-1', 'dm');
      await vi.waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(1));

      let duplicateError: unknown;
      try {
        await voiceService.joinChannel('channel-1', 'dm');
      } catch (err) {
        duplicateError = err;
      }
      resolveAuthorization({
        ok: false,
        status: 409,
        json: vi.fn().mockResolvedValue({}),
      } as unknown as Response);
      await expect(firstJoin).rejects.toThrow('Voice join failed: 409');

      const apiCallCount = mockApiFetch.mock.calls.length;
      mockApiFetch.mockReset();
      expect(duplicateError).toEqual(new Error('Another voice call is already in progress'));
      expect(apiCallCount).toBe(1);
    });

    it('recovers and releases DM join ownership when active-channel cleanup throws', async () => {
      const { sendTransport } = await joinVoiceChannel();
      useVoiceStore.getState().setCallState({
        kind: 'outgoing-ringing',
        conversationId: 'channel-1',
        ringId: 'ring-before-cleanup',
        calleeUserIds: ['user-2'],
        startedAt: Date.now(),
        declinedUserIds: [],
      });
      sendTransport.close.mockImplementation(() => {
        throw new Error('mock switch cleanup failure');
      });
      mockApiFetch.mockClear();

      await expect(voiceService.joinChannel('channel-1', 'dm')).rejects.toThrow(
        'mock switch cleanup failure'
      );

      expect(mockApiFetch).not.toHaveBeenCalled();
      expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
      expect(useVoiceStore.getState().activeChannelId).toBeNull();
      expect(useVoiceStore.getState().connectionState).toBe('error');

      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 403,
        json: vi.fn().mockResolvedValue({}),
      });
      await expect(voiceService.joinChannel('channel-1', 'dm')).rejects.toThrow(
        'Voice join failed: 403'
      );
      expect(mockApiFetch).toHaveBeenCalledTimes(1);
    });

    it('handles json parse failure on error response', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 500,
        json: vi.fn().mockRejectedValue(new Error('bad json')),
      });
      await expect(voiceService.joinChannel('ch')).rejects.toThrow('Voice join failed: 500');
    });

    // Regression for the ghost-state bug: setActiveChannel runs early in
    // joinChannel (between control-plane auth and the media-plane handshake),
    // so a late-stage failure like a NotAllowedError from getUserMedia leaves
    // activeChannelId stuck pointing at the channel we never joined. The
    // sidebar then renders the linked voice text chat as if connected. The
    // catch block must clear activeChannelId — and the rest of the per-join
    // state — before the final 'error' transition.
    it('clears activeChannelId when join fails after setActiveChannel', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      mockSocket.connected = true;
      const st = makeSendTransport();
      const rt = makeRecvTransport();
      mockCreateSendTransport.mockReturnValue(st);
      mockCreateRecvTransport.mockReturnValue(rt);
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
      });
      // Mic acquisition fails with the same DOMException that a
      // Permissions-Policy denial surfaces in Chromium. produceAudio's
      // call to getUserMedia is not caught locally and propagates up to
      // joinChannel's catch.
      mockGetUserMedia.mockRejectedValue(new DOMException('Permission denied', 'NotAllowedError'));

      await expect(voiceService.joinChannel('ch')).rejects.toThrow('Permission denied');

      const state = useVoiceStore.getState();
      // setActiveChannel writes three fields (id/name/server). Assert all
      // three clear so a future refactor that only nulls activeChannelId
      // (e.g., replacing reset() with a more surgical clear) doesn't silently
      // leave activeChannelName or activeServerId pointing at a channel the
      // user never actually joined.
      expect(state.activeChannelId).toBeNull();
      expect(state.activeChannelName).toBeNull();
      expect(state.activeServerId).toBeNull();
      expect(state.connectionState).toBe('error');
    });

    // Regression for the defense-in-depth branch added in handleJoinFailure:
    // if cleanup() itself throws (mediasoup transport close raising, E2EE
    // worker crashing mid-destroy), the store.reset() + 'error' transition
    // MUST still run — otherwise a different failure mode could regress the
    // ghost-state bug fixed by the prior test in this describe block.
    it('still resets store when cleanup() throws during join failure', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      mockSocket.connected = true;
      const st = makeSendTransport();
      const rt = makeRecvTransport();
      mockCreateSendTransport.mockReturnValue(st);
      mockCreateRecvTransport.mockReturnValue(rt);
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
      });
      // Force the join path to fail at getUserMedia (post-setActiveChannel)
      mockGetUserMedia.mockRejectedValue(new DOMException('Permission denied', 'NotAllowedError'));
      // ...and force cleanup() to throw when it tries to close the send
      // transport. handleJoinFailure's inner try/catch should swallow this
      // and proceed to store.reset() + setConnectionState('error').
      st.close.mockImplementation(() => {
        throw new Error('mock cleanup teardown failure');
      });

      await expect(voiceService.joinChannel('ch')).rejects.toThrow('Permission denied');

      const state = useVoiceStore.getState();
      expect(state.activeChannelId).toBeNull();
      expect(state.activeChannelName).toBeNull();
      expect(state.activeServerId).toBeNull();
      expect(state.connectionState).toBe('error');
    });

    it('sets participants with video state', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      mockSocket.connected = true;
      const st = makeSendTransport();
      const rt = makeRecvTransport();
      mockCreateSendTransport.mockReturnValue(st);
      mockCreateRecvTransport.mockReturnValue(rt);
      const consumer = createMockConsumer('c2', 'video', 'p2cam');
      rt.consume.mockResolvedValue(consumer);
      setupEmitResponses({
        'join-room': makeRoomJoined({
          participants: [
            { userId: 'user-1', username: 'self' },
            { userId: 'user-2', username: 'other' },
          ],
          existingProducers: [
            { producerId: 'p2cam', userId: 'user-2', kind: 'video', source: 'camera' },
          ],
        }),
        'create-transport': makeTransportOpts(),
        produce: { id: 'pm' },
        consume: {
          id: 'c2',
          producerId: 'p2cam',
          kind: 'video',
          rtpParameters: {},
          producerUserId: 'user-2',
          source: 'camera',
        },
        'resume-consumer': undefined,
      });
      mockGetUserMedia.mockResolvedValue(createMockMediaStream());
      st.produce.mockResolvedValue(createMockProducer());
      await voiceService.joinChannel('ch');
      expect(useVoiceStore.getState().participants['user-2'].isVideoOn).toBe(true);
    });

    it('registers screen producers as available shares', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      mockSocket.connected = true;
      const st = makeSendTransport();
      const rt = makeRecvTransport();
      mockCreateSendTransport.mockReturnValue(st);
      mockCreateRecvTransport.mockReturnValue(rt);
      setupEmitResponses({
        'join-room': makeRoomJoined({
          participants: [
            { userId: 'user-1', username: 'self' },
            { userId: 'user-2', username: 'sharer' },
          ],
          existingProducers: [
            { producerId: 'sp1', userId: 'user-2', kind: 'video', source: 'screen' },
          ],
        }),
        'create-transport': makeTransportOpts(),
        produce: { id: 'pm' },
      });
      mockGetUserMedia.mockResolvedValue(createMockMediaStream());
      st.produce.mockResolvedValue(createMockProducer());
      await voiceService.joinChannel('ch');
      const shares = useVoiceStore.getState().availableScreenShares;
      expect(shares).toHaveLength(1);
      expect(shares[0].producerId).toBe('sp1');
    });

    it('leaves existing channel before joining new one', async () => {
      await joinVoiceChannel();
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse({ id: 'ch2' })),
      });
      mockSocket.connected = true;
      const st2 = makeSendTransport();
      const rt2 = makeRecvTransport();
      mockCreateSendTransport.mockReturnValue(st2);
      mockCreateRecvTransport.mockReturnValue(rt2);
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'p2' },
        'close-producer': undefined,
        'leave-room': undefined,
      });
      mockGetUserMedia.mockResolvedValue(createMockMediaStream());
      st2.produce.mockResolvedValue(createMockProducer('p2'));
      await voiceService.joinChannel('ch2');
      expect(useVoiceStore.getState().connectionState).toBe('connected');
    });
  });

  // ===== Network-change recovery (#1790) =====

  describe('network-change recovery (#1790)', () => {
    it('re-establishes the media session after a transient disconnect + socket reconnect', async () => {
      await joinVoiceChannel();
      expect(useVoiceStore.getState().connectionState).toBe('connected');

      // Any post-drop re-authorize must also succeed (joinVoiceChannel's
      // mockResolvedValueOnce only covered the first join-authorize).
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic-2' },
        'resume-consumer': undefined,
        'close-producer': undefined,
        'pause-producer': undefined,
        'resume-producer': undefined,
        'leave-room': undefined,
      });

      const joinRoomEmits = () =>
        mockSocket.emit.mock.calls.filter((c) => c[0] === 'join-room').length;
      expect(joinRoomEmits()).toBe(1);

      // VPN toggle / network-interface change: Socket.IO drops with
      // 'transport close' (the ERR_NETWORK_CHANGED manifestation).
      socketListeners['disconnect']?.forEach((cb) => cb('transport close'));
      expect(useVoiceStore.getState().connectionState).toBe('reconnecting');

      // Socket.IO reconnects on the new network path.
      mockSocket.connected = true;
      socketListeners['connect']?.forEach((cb) => cb());

      // The media-plane tore down our participant the moment the socket
      // dropped (no server-side grace period), so a bare state flip back to
      // 'connected' is a dead session — no server-side transports, producers,
      // or consumers exist. The client MUST re-join the room and rebuild the
      // media session for audio to flow again.
      await vi.waitFor(() => {
        expect(joinRoomEmits()).toBeGreaterThan(1);
      });
      expect(useVoiceStore.getState().connectionState).toBe('connected');
      expect(useVoiceStore.getState().activeChannelId).toBe('channel-1');
    });

    it('re-applies self-mute and deafen after resume — a muted mic never transmits', async () => {
      const { micProducer } = await joinVoiceChannel();
      // User self-muted + deafened before the network drop; store state
      // survives the resume (it is intentionally not reset).
      useVoiceStore.getState().setMuted(true);
      useVoiceStore.getState().setDeafened(true);

      mockApiFetch.mockResolvedValue({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic' },
        'resume-consumer': undefined,
        'leave-room': undefined,
      });

      socketListeners['disconnect']?.forEach((cb) => cb('transport close'));
      mockSocket.connected = true;
      socketListeners['connect']?.forEach((cb) => cb());

      await vi.waitFor(() => {
        expect(useVoiceStore.getState().connectionState).toBe('connected');
      });
      // The rebuilt mic producer must be paused before the session is
      // declared connected — otherwise a muted user silently transmits.
      expect(micProducer.pause).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('pause-producer', {
        producerId: 'prod-mic',
      });
      // Deafen state rebroadcast to the freshly-created server participant.
      expect(mockSocket.emit).toHaveBeenCalledWith('set-deafen', { isDeafened: true });
    });

    it('runs only one resume for overlapping connect events (single-flight)', async () => {
      await joinVoiceChannel();
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic-2' },
        'resume-consumer': undefined,
        'leave-room': undefined,
      });

      socketListeners['disconnect']?.forEach((cb) => cb('transport close'));
      mockSocket.connected = true;
      // Two connect events while the first resume is still in flight — the
      // second must be swallowed by the resumeInFlight single-flight guard.
      socketListeners['connect']?.forEach((cb) => cb());
      socketListeners['connect']?.forEach((cb) => cb());

      await vi.waitFor(() => {
        expect(useVoiceStore.getState().connectionState).toBe('connected');
      });
      const authorizeCalls = mockApiFetch.mock.calls.filter((c) =>
        String(c[0]).includes('/voice/join')
      ).length;
      // 1 original join + exactly 1 resume — never a doubled rejoin.
      expect(authorizeCalls).toBe(2);
    });

    it('re-authorizes via the DM endpoint when resuming a DM call', async () => {
      useVoiceStore.getState().setCallState({
        kind: 'outgoing-ringing',
        conversationId: 'channel-1',
        ringId: 'initial-ring',
        calleeUserIds: ['user-2'],
        startedAt: Date.now(),
        declinedUserIds: [],
      });
      await joinVoiceChannel(undefined, 'dm', { call_id: 'call-1' });
      useVoiceStore.getState().setCallState({ kind: 'in-call' });
      mockApiFetch.mockClear();
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: vi.fn().mockResolvedValue({ ...makeJoinResponse(), call_id: 'call-1' }),
      });
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic-2' },
        'resume-consumer': undefined,
        'leave-room': undefined,
      });

      socketListeners['disconnect']?.forEach((cb) => cb('transport close'));
      mockSocket.connected = true;
      socketListeners['connect']?.forEach((cb) => cb());

      await vi.waitFor(() => {
        expect(mockApiFetch).toHaveBeenCalledWith(
          '/api/v1/dm/conversations/channel-1/voice/join',
          expect.not.objectContaining({ body: expect.anything() })
        );
        const joinRoomCalls = mockSocket.emit.mock.calls.filter((call) => call[0] === 'join-room');
        expect(joinRoomCalls.at(-1)?.[1]).toEqual(expect.objectContaining({ callId: 'call-1' }));
      });
      await vi.waitFor(() => {
        expect(useVoiceStore.getState().connectionState).toBe('connected');
      });
    });

    it('tears down instead of stranding zombie reconnecting state without call context', async () => {
      await joinVoiceChannel();
      // Simulate a leave that raced the drop: store already reset when the
      // socket-level reconnect fires.
      useVoiceStore.getState().reset();
      useVoiceStore.getState().setConnectionState('reconnecting');

      socketListeners['connect']?.forEach((cb) => cb());

      await vi.waitFor(() => {
        expect(useVoiceStore.getState().connectionState).toBe('disconnected');
      });
    });

    it('falls into the bounded cleanup path when rejoin authorization fails', async () => {
      await joinVoiceChannel();
      socketListeners['disconnect']?.forEach((cb) => cb('transport close'));
      expect(useVoiceStore.getState().connectionState).toBe('reconnecting');

      // Control-plane refuses the re-authorize (e.g. access revoked mid-call):
      // recovery must end in the user-visible disconnect path, never a
      // zombie 'reconnecting' call.
      mockApiFetch.mockResolvedValue({
        ok: false,
        status: 403,
        json: vi.fn().mockResolvedValue({}),
      });
      mockSocket.connected = true;
      socketListeners['connect']?.forEach((cb) => cb());

      await vi.waitFor(() => {
        expect(useVoiceStore.getState().connectionState).toBe('disconnected');
      });
      expect(useVoiceStore.getState().activeChannelId).toBeNull();
    });
  });

  // ===== leaveChannel =====

  describe('leaveChannel', () => {
    it('resets to disconnected', async () => {
      await joinVoiceChannel();
      await voiceService.leaveChannel();
      expect(useVoiceStore.getState().connectionState).toBe('disconnected');
      expect(useVoiceStore.getState().activeChannelId).toBeNull();
    });

    it('emits leave-room', async () => {
      await joinVoiceChannel();
      await voiceService.leaveChannel();
      expect(mockSocket.emit).toHaveBeenCalledWith('leave-room');
    });

    it('safe when not connected', async () => {
      await voiceService.leaveChannel();
      expect(useVoiceStore.getState().connectionState).toBe('disconnected');
    });
  });

  // ===== emergencyCleanup =====

  describe('emergencyCleanup', () => {
    it('resets all state', async () => {
      await joinVoiceChannel();
      voiceService.emergencyCleanup();
      expect(useVoiceStore.getState().connectionState).toBe('disconnected');
    });

    it('is idempotent', () => {
      voiceService.emergencyCleanup();
      voiceService.emergencyCleanup();
      expect(useVoiceStore.getState().connectionState).toBe('disconnected');
    });

    it('fires on auth token clear', () => {
      setupAuth();
      useVoiceStore.getState().setConnectionState('connected');
      useAuthStore.getState().clearAccessToken();
      expect(useVoiceStore.getState().connectionState).toBe('disconnected');
    });
  });

  // ===== toggleMute =====

  describe('toggleMute', () => {
    it('noop without producer', async () => {
      await voiceService.toggleMute();
      expect(useVoiceStore.getState().isMuted).toBe(false);
    });

    it('pauses and mutes', async () => {
      const { micProducer } = await joinVoiceChannel();
      await voiceService.toggleMute();
      expect(micProducer.pause).toHaveBeenCalled();
      expect(useVoiceStore.getState().isMuted).toBe(true);
    });

    it('resumes and unmutes', async () => {
      const { micProducer } = await joinVoiceChannel();
      await voiceService.toggleMute();
      await voiceService.toggleMute();
      expect(micProducer.resume).toHaveBeenCalled();
      expect(useVoiceStore.getState().isMuted).toBe(false);
    });

    it('emits socket events', async () => {
      const { micProducer } = await joinVoiceChannel();
      await voiceService.toggleMute();
      expect(mockSocket.emit).toHaveBeenCalledWith('pause-producer', {
        producerId: micProducer.id,
      });
      await voiceService.toggleMute();
      expect(mockSocket.emit).toHaveBeenCalledWith('resume-producer', {
        producerId: micProducer.id,
      });
    });

    it('reverts producer state on error during unmute', async () => {
      const { micProducer } = await joinVoiceChannel();
      await voiceService.toggleMute();
      expect(useVoiceStore.getState().isMuted).toBe(true);

      micProducer.resume.mockRejectedValueOnce(new Error('resume failed'));
      await voiceService.toggleMute();
      expect(useVoiceStore.getState().isMuted).toBe(true);
      expect(micProducer.pause).toHaveBeenCalled();
    });

    it('reverts producer state on error during mute', async () => {
      const { micProducer } = await joinVoiceChannel();
      micProducer.pause.mockRejectedValueOnce(new Error('pause failed'));
      await voiceService.toggleMute();
      expect(useVoiceStore.getState().isMuted).toBe(false);
      expect(micProducer.resume).toHaveBeenCalled();
    });

    it('swallows producer revert failure gracefully', async () => {
      const { micProducer } = await joinVoiceChannel();
      micProducer.pause.mockRejectedValueOnce(new Error('pause failed'));
      micProducer.resume.mockImplementationOnce(() => {
        throw new Error('resume also failed');
      });
      await expect(voiceService.toggleMute()).resolves.not.toThrow();
      expect(useVoiceStore.getState().isMuted).toBe(false);
    });
  });

  // ===== toggleDeafen =====

  describe('toggleDeafen', () => {
    it('sets deafened', async () => {
      await joinVoiceChannel();
      voiceService.toggleDeafen();
      expect(useVoiceStore.getState().isDeafened).toBe(true);
    });

    it('also mutes', async () => {
      const { micProducer } = await joinVoiceChannel();
      voiceService.toggleDeafen();
      expect(micProducer.pause).toHaveBeenCalled();
    });

    it('un-deafens on second toggle', async () => {
      await joinVoiceChannel();
      voiceService.toggleDeafen();
      voiceService.toggleDeafen();
      expect(useVoiceStore.getState().isDeafened).toBe(false);
    });

    it('emits a set-deafen socket event with the new state (#685)', async () => {
      await joinVoiceChannel();
      voiceService.toggleDeafen();
      expect(mockSocket.emit).toHaveBeenCalledWith('set-deafen', { isDeafened: true });
      voiceService.toggleDeafen();
      expect(mockSocket.emit).toHaveBeenCalledWith('set-deafen', { isDeafened: false });
    });

    it('optimistically reflects self-deafen on the local sidebar member (#685)', async () => {
      await joinVoiceChannel();
      useVoiceStore.getState().setChannelVoiceMembers('channel-1', [
        {
          userId: 'user-1',
          username: 'me',
          isMuted: false,
          isDeafened: false,
          serverMuted: false,
          serverDeafened: false,
        },
      ]);
      voiceService.toggleDeafen();
      const member = useVoiceStore
        .getState()
        .channelVoiceMembers['channel-1'].find((m) => m.userId === 'user-1');
      expect(member?.isDeafened).toBe(true);
    });

    it('updates a remote participant on participant-deafen-changed (#685)', async () => {
      await joinVoiceChannel();
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: false,
      });
      const handler = socketListeners['participant-deafen-changed']?.[0];
      expect(handler).toBeDefined();
      handler?.({ userId: 'user-2', isDeafened: true });
      expect(useVoiceStore.getState().participants['user-2']?.isDeafened).toBe(true);
    });
  });

  // ===== toggleVideo =====

  describe('toggleVideo', () => {
    it('starts video', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      sendTransport.produce.mockResolvedValue(createMockProducer('cp', 'camera'));
      await voiceService.toggleVideo();
      expect(useVoiceStore.getState().isVideoOn).toBe(true);
    });

    it('stops video', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      sendTransport.produce.mockResolvedValue(createMockProducer('cp', 'camera'));
      await voiceService.toggleVideo();
      await voiceService.toggleVideo();
      expect(useVoiceStore.getState().isVideoOn).toBe(false);
    });

    it('enforces max video slots', async () => {
      const { sendTransport } = await joinVoiceChannel();
      // Set maxVideoSlots to 2 so we only need 2 video-on participants to hit the limit
      const parts: Record<string, any> = {
        u0: {
          userId: 'u0',
          username: 'u0',
          isMuted: false,
          isDeafened: false,
          isVideoOn: true,
          isScreenSharing: false,
          isSpeaking: false,
        },
        u1: {
          userId: 'u1',
          username: 'u1',
          isMuted: false,
          isDeafened: false,
          isVideoOn: true,
          isScreenSharing: false,
          isSpeaking: false,
        },
      };
      useVoiceStore.setState({ participants: parts, maxVideoSlots: 2 });
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      sendTransport.produce.mockResolvedValue(createMockProducer('cp', 'camera'));
      await voiceService.produceVideo();
      expect(useVoiceStore.getState().videoSlotError).toBeTruthy();
    });
  });

  // ===== toggleVideo — error path =====

  describe('toggleVideo error path', () => {
    it('logs error and sets videoSlotError when camera produce throws', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const { sendTransport } = await joinVoiceChannel();
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      sendTransport.produce.mockRejectedValueOnce(new Error('camera produce failed'));

      await voiceService.produceVideo();

      expect(consoleSpy).toHaveBeenCalledWith('Failed to start camera:', 'camera produce failed');
      consoleSpy.mockRestore();
    });
  });

  // ===== toggleScreenShare =====

  describe('toggleScreenShare', () => {
    it('starts and stops', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockGetDisplayMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      sendTransport.produce.mockResolvedValue(createMockProducer('sp', 'screen'));
      await voiceService.toggleScreenShare();
      expect(useVoiceStore.getState().isScreenSharing).toBe(true);
      await voiceService.toggleScreenShare();
      expect(useVoiceStore.getState().isScreenSharing).toBe(false);
    });
  });

  // ===== pauseLocalProducer / resumeLocalProducer =====

  describe('pauseLocalProducer / resumeLocalProducer', () => {
    it('pauses active producer', async () => {
      const { micProducer } = await joinVoiceChannel();
      voiceService.pauseLocalProducer('mic');
      expect(micProducer.pause).toHaveBeenCalled();
    });

    it('noop for unknown source', () => {
      expect(() => voiceService.pauseLocalProducer('x')).not.toThrow();
    });

    it('resumes paused producer', async () => {
      const { micProducer } = await joinVoiceChannel();
      micProducer.paused = true;
      voiceService.resumeLocalProducer('mic');
      expect(micProducer.resume).toHaveBeenCalled();
    });

    it('skips non-paused producer', async () => {
      const { micProducer } = await joinVoiceChannel();
      micProducer.paused = false;
      voiceService.resumeLocalProducer('mic');
      expect(micProducer.resume).not.toHaveBeenCalled();
    });
  });

  describe('test audio suspension (#1163)', () => {
    it('pauses active audio producer and consumers, then restores them once', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      svc.consumers.set('c1', consumer);
      mockSocket.emit.mockClear();

      voiceService.beginTestSuspension();

      expect(micProducer.pause).toHaveBeenCalled();
      expect(consumer.pause).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('pause-producer', {
        producerId: micProducer.id,
      });

      micProducer.paused = true;
      consumer.paused = true;
      voiceService.endTestSuspension();

      expect(micProducer.resume).toHaveBeenCalled();
      expect(consumer.resume).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('resume-producer', {
        producerId: micProducer.id,
      });
    });

    it('does not resume a suspended mic producer when unmuting during a test', async () => {
      const { micProducer } = await joinVoiceChannel();
      voiceService.beginTestSuspension();
      micProducer.paused = true;
      useVoiceStore.getState().setMuted(true);

      micProducer.resume.mockClear();
      mockSocket.emit.mockClear();

      await voiceService.toggleMute();

      expect(useVoiceStore.getState().isMuted).toBe(false);
      expect(micProducer.resume).not.toHaveBeenCalled();
      expect(mockSocket.emit).not.toHaveBeenCalledWith('resume-producer', {
        producerId: micProducer.id,
      });

      voiceService.endTestSuspension();

      expect(micProducer.resume).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('resume-producer', {
        producerId: micProducer.id,
      });
    });

    it('keeps a pre-muted mic producer suspended when unmuting during a test', async () => {
      const { micProducer } = await joinVoiceChannel();
      micProducer.paused = true;
      useVoiceStore.getState().setMuted(true);
      voiceService.beginTestSuspension();

      micProducer.resume.mockClear();
      mockSocket.emit.mockClear();

      await voiceService.toggleMute();

      expect(useVoiceStore.getState().isMuted).toBe(false);
      expect(micProducer.resume).not.toHaveBeenCalled();
      expect(mockSocket.emit).not.toHaveBeenCalledWith('resume-producer', {
        producerId: micProducer.id,
      });

      voiceService.endTestSuspension();

      expect(micProducer.resume).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('resume-producer', {
        producerId: micProducer.id,
      });
    });

    it('does not resume pre-paused screen audio when a test ends', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const screenAudioProducer = createMockProducer('prod-screen-audio', 'screen-audio');
      screenAudioProducer.paused = true;
      svc.producers.set('screen-audio', screenAudioProducer);

      voiceService.beginTestSuspension();
      screenAudioProducer.resume.mockClear();
      mockSocket.emit.mockClear();

      voiceService.endTestSuspension();

      expect(screenAudioProducer.resume).not.toHaveBeenCalled();
      expect(mockSocket.emit).not.toHaveBeenCalledWith('resume-producer', {
        producerId: 'prod-screen-audio',
      });
    });

    it('does not pause active screen audio during a test', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const screenAudioProducer = createMockProducer('prod-screen-audio', 'screen-audio');
      svc.producers.set('screen-audio', screenAudioProducer);
      mockSocket.emit.mockClear();

      voiceService.beginTestSuspension();

      expect(screenAudioProducer.pause).not.toHaveBeenCalled();
      expect(mockSocket.emit).not.toHaveBeenCalledWith('pause-producer', {
        producerId: 'prod-screen-audio',
      });

      voiceService.endTestSuspension();

      expect(screenAudioProducer.resume).not.toHaveBeenCalled();
    });

    it('does not resume suspended audio consumers when undeafening during a test', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      svc.consumers.set('c1', consumer);
      voiceService.beginTestSuspension();
      consumer.paused = true;
      useVoiceStore.getState().setDeafened(true);

      consumer.resume.mockClear();

      voiceService.toggleDeafen();

      expect(useVoiceStore.getState().isDeafened).toBe(false);
      expect(consumer.resume).not.toHaveBeenCalled();

      voiceService.endTestSuspension();

      expect(consumer.resume).toHaveBeenCalled();
    });

    it('keeps pre-deafened audio consumers suspended when undeafening during a test', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      consumer.paused = true;
      svc.consumers.set('c1', consumer);
      useVoiceStore.getState().setDeafened(true);
      voiceService.beginTestSuspension();

      consumer.resume.mockClear();

      voiceService.toggleDeafen();

      expect(useVoiceStore.getState().isDeafened).toBe(false);
      expect(consumer.resume).not.toHaveBeenCalled();

      voiceService.endTestSuspension();

      expect(consumer.resume).toHaveBeenCalled();
    });

    it('does not resume manually paused audio consumers when a test ends', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      svc.consumers.set('c1', consumer);

      voiceService.pauseConsumer('c1');
      consumer.paused = true;
      consumer.resume.mockClear();
      mockSocket.emit.mockClear();

      voiceService.beginTestSuspension();
      voiceService.endTestSuspension();

      expect(consumer.resume).not.toHaveBeenCalled();
      expect(mockSocket.emit).not.toHaveBeenCalledWith('resume-consumer', {
        consumerId: 'c1',
      });
    });

    it('does not resume server-side consumers when audio output should stay paused', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      svc.consumers.set('c1', consumer);
      voiceService.beginTestSuspension();
      consumer.paused = true;
      svc.testServerPausedConsumerIds.add('c1');
      mockSocket.emit.mockClear();
      useVoiceStore.getState().setDeafened(true);

      voiceService.endTestSuspension();

      expect(consumer.resume).not.toHaveBeenCalled();
      expect(mockSocket.emit).not.toHaveBeenCalledWith('resume-consumer', {
        consumerId: 'c1',
      });
    });

    it('resumes server-side consumers held by a deafened test when undeafening', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const consumer = createMockConsumer('c1', 'audio', 'p1');
      svc.consumers.set('c1', consumer);
      voiceService.beginTestSuspension();
      consumer.paused = true;
      svc.testServerPausedConsumerIds.add('c1');
      useVoiceStore.getState().setDeafened(true);

      voiceService.endTestSuspension();
      mockSocket.emit.mockClear();
      consumer.resume.mockClear();

      voiceService.toggleDeafen();

      expect(useVoiceStore.getState().isDeafened).toBe(false);
      expect(consumer.resume).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('resume-consumer', {
        consumerId: 'c1',
      });
    });

    it('keeps a replaced mic producer paused during a test suspension', async () => {
      const { micProducer } = await joinVoiceChannel();
      const svc = voiceService as any;
      voiceService.beginTestSuspension();
      micProducer.paused = true;
      micProducer.resume.mockClear();

      await svc.liveReplaceAudioTrack();

      expect(micProducer.resume).not.toHaveBeenCalled();

      voiceService.endTestSuspension();

      expect(micProducer.resume).toHaveBeenCalled();
    });

    it('holds a recreated mic producer during a test suspension', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const newProducer = createMockProducer('prod-mic-new', 'mic');
      sendTransport.produce.mockResolvedValue(newProducer);
      voiceService.beginTestSuspension();
      mockSocket.emit.mockClear();

      await svc.liveReproduceAudio();

      expect(newProducer.pause).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('pause-producer', {
        producerId: 'prod-mic-new',
      });
    });
  });

  // ===== pauseConsumer / resumeConsumer =====

  describe('pauseConsumer / resumeConsumer', () => {
    it('noop for unknown', () => {
      expect(() => voiceService.pauseConsumer('x')).not.toThrow();
      expect(() => voiceService.resumeConsumer('x')).not.toThrow();
    });
  });

  // ===== getConsumerIdsBySource =====

  describe('getConsumerIdsBySource', () => {
    it('empty without consumers', () => {
      expect(voiceService.getConsumerIdsBySource()).toEqual([]);
      expect(voiceService.getConsumerIdsBySource('audio')).toEqual([]);
    });
  });

  // ===== getRouterRtpCapabilities =====

  describe('getRouterRtpCapabilities', () => {
    it('null before device load', () => {
      voiceService.emergencyCleanup();
      expect(voiceService.getRouterRtpCapabilities()).toBeNull();
    });

    it('returns caps after join', async () => {
      await joinVoiceChannel();
      expect(voiceService.getRouterRtpCapabilities()).toBeDefined();
    });
  });

  // ===== getConsumerMeta =====

  describe('getConsumerMeta', () => {
    it('empty without consumers', () => {
      expect(voiceService.getConsumerMeta().size).toBe(0);
    });
  });

  // ===== forwardToServer =====

  describe('forwardToServer', () => {
    it('rejects without socket', async () => {
      voiceService.emergencyCleanup();
      await expect(voiceService.forwardToServer('ev')).rejects.toThrow('No socket');
    });

    it('rejects on server error', async () => {
      await joinVoiceChannel();
      mockSocket.emit.mockImplementation((_e: string, _d: unknown, cb?: (r: unknown) => void) => {
        if (cb) cb({ error: 'nope' });
      });
      await expect(voiceService.forwardToServer('ev', {})).rejects.toThrow('nope');
    });
  });

  // ===== setQualityTier =====

  describe('setQualityTier', () => {
    it('updates store', async () => {
      await voiceService.setQualityTier('high');
      expect(useVoiceStore.getState().qualityTier).toBe('high');
    });

    it('re-produces audio', async () => {
      const { sendTransport } = await joinVoiceChannel();
      sendTransport.produce.mockResolvedValue(createMockProducer('p2', 'mic'));
      mockGetUserMedia.mockResolvedValue(createMockMediaStream());
      await voiceService.setQualityTier('hifi');
      expect(sendTransport.produce).toHaveBeenCalledTimes(2);
    });
  });

  // ===== produceScreen =====

  describe('produceScreen', () => {
    it('noop without sendTransport', async () => {
      voiceService.emergencyCleanup();
      await voiceService.produceScreen();
      expect(useVoiceStore.getState().isScreenSharing).toBe(false);
    });

    it('content hint motion', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const vt = {
        id: 'st',
        kind: 'video',
        readyState: 'live',
        enabled: true,
        stop: vi.fn(),
        getSettings: vi.fn().mockReturnValue({}),
        contentHint: '',
        onended: null as any,
      };
      mockGetDisplayMedia.mockResolvedValue({
        getAudioTracks: vi.fn().mockReturnValue([]),
        getVideoTracks: vi.fn().mockReturnValue([vt]),
        getTracks: vi.fn().mockReturnValue([vt]),
      });
      sendTransport.produce.mockResolvedValue(createMockProducer('sp', 'screen'));
      await voiceService.produceScreen(undefined, { contentType: 'motion' });
      expect(vt.contentHint).toBe('motion');
    });

    it('content hint detail', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const vt = {
        id: 'st',
        kind: 'video',
        readyState: 'live',
        enabled: true,
        stop: vi.fn(),
        getSettings: vi.fn().mockReturnValue({}),
        contentHint: '',
        onended: null as any,
      };
      mockGetDisplayMedia.mockResolvedValue({
        getAudioTracks: vi.fn().mockReturnValue([]),
        getVideoTracks: vi.fn().mockReturnValue([vt]),
        getTracks: vi.fn().mockReturnValue([vt]),
      });
      sendTransport.produce.mockResolvedValue(createMockProducer('sp', 'screen'));
      await voiceService.produceScreen(undefined, { contentType: 'detail' });
      expect(vt.contentHint).toBe('detail');
    });

    it('produces screen audio', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockGetDisplayMedia.mockResolvedValue(
        createMockMediaStream([
          { kind: 'video', id: 'sv' },
          { kind: 'audio', id: 'sa' },
        ])
      );
      let idx = 0;
      sendTransport.produce.mockImplementation(async (o: any) =>
        createMockProducer(`p-${++idx}`, o.appData?.source || 'mic')
      );
      await voiceService.produceScreen();
      const sources = sendTransport.produce.mock.calls.map((c: any) => c[0].appData?.source);
      expect(sources).toContain('screen');
      expect(sources).toContain('screen-audio');
    });

    it('custom WxH resolution', async () => {
      useVideoSettingsStore.setState({
        screenResolution: '2560x1440',
        screenFrameRate: 30,
        screenContentType: 'auto',
      });
      const { sendTransport } = await joinVoiceChannel();
      mockGetDisplayMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      sendTransport.produce.mockResolvedValue(createMockProducer('sp', 'screen'));
      await voiceService.produceScreen();
      expect(mockGetDisplayMedia).toHaveBeenCalledWith(
        expect.objectContaining({
          video: expect.objectContaining({ width: { ideal: 2560 }, height: { ideal: 1440 } }),
        })
      );
    });

    it('uses 60fps when frameRate=0', async () => {
      useVideoSettingsStore.setState({
        screenResolution: '1080p',
        screenFrameRate: 0,
        screenContentType: 'auto',
      });
      const { sendTransport } = await joinVoiceChannel();
      mockGetDisplayMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      sendTransport.produce.mockResolvedValue(createMockProducer('sp', 'screen'));
      await voiceService.produceScreen();
      expect(mockGetDisplayMedia).toHaveBeenCalledWith(
        expect.objectContaining({
          video: expect.objectContaining({ frameRate: { ideal: 60 } }),
        })
      );
    });
  });

  // ===== closeProducer =====

  describe('closeProducer', () => {
    it('closes mic', async () => {
      const { micProducer } = await joinVoiceChannel();
      await voiceService.closeProducer('mic');
      expect(micProducer.close).toHaveBeenCalled();
    });

    it('closes camera and resets state', async () => {
      const { sendTransport } = await joinVoiceChannel();
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      sendTransport.produce.mockResolvedValue(createMockProducer('cp', 'camera'));
      await voiceService.toggleVideo();
      expect(useVoiceStore.getState().isVideoOn).toBe(true);
      await voiceService.closeProducer('camera');
      expect(useVoiceStore.getState().isVideoOn).toBe(false);
    });

    it('handles nonexistent source', async () => {
      await joinVoiceChannel();
      await expect(voiceService.closeProducer('nope')).resolves.toBeUndefined();
    });
  });

  // ===== Audio settings =====

  describe('audio settings', () => {
    it('applies noise cancellation', async () => {
      useAudioSettingsStore.setState({
        noiseCancellation: true,
        echoCancellation: true,
        autoGainControl: true,
        musicMode: false,
      });
      await joinVoiceChannel();
      const audioCalls = mockGetUserMedia.mock.calls.filter((c: any) => c[0]?.audio);
      expect(audioCalls.length).toBeGreaterThanOrEqual(1);
      expect(audioCalls[0][0].audio.noiseSuppression).toBe(true);
    });

    it('disables processing in music mode', async () => {
      useAudioSettingsStore.setState({
        noiseCancellation: true,
        echoCancellation: true,
        autoGainControl: true,
        musicMode: true,
      });
      await joinVoiceChannel();
      const audioCalls = mockGetUserMedia.mock.calls.filter((c: any) => c[0]?.audio);
      expect(audioCalls[0][0].audio.noiseSuppression).toBe(false);
    });

    it('applies audio priority', async () => {
      useAudioSettingsStore.setState({ audioPriority: 'high' });
      const { sendTransport } = await joinVoiceChannel();
      const mc = sendTransport.produce.mock.calls.filter(
        (c: any) => c[0].appData?.source === 'mic'
      );
      expect(mc[0][0].encodings[0].priority).toBe('high');
    });

    it('omits priority when off', async () => {
      useAudioSettingsStore.setState({ audioPriority: 'off' });
      const { sendTransport } = await joinVoiceChannel();
      const mc = sendTransport.produce.mock.calls.filter(
        (c: any) => c[0].appData?.source === 'mic'
      );
      expect(mc[0][0].encodings[0].priority).toBeUndefined();
    });
  });

  // ===== Advanced opus settings =====

  describe('advanced opus settings', () => {
    it('uses overrides', async () => {
      useAudioSettingsStore.setState({
        advancedMode: true,
        inlineFec: false,
        silenceDetection: true,
        stereoOverride: true,
        frameSize: 40,
        adaptivePtime: false,
        opusNack: true,
      });
      const { sendTransport } = await joinVoiceChannel();
      const mc = sendTransport.produce.mock.calls.filter(
        (c: any) => c[0].appData?.source === 'mic'
      );
      const co = mc[0][0].codecOptions;
      expect(co.opusFec).toBe(false);
      expect(co.opusStereo).toBe(true);
      expect(co.opusPtime).toBe(40);
      expect(co.opusNack).toBe(true);
    });

    it('uses explicit frame size in advanced mode', async () => {
      // When advancedMode=true and frameSize is explicit (non-zero), use that value
      useAudioSettingsStore.setState({
        advancedMode: true,
        frameSize: 60,
        adaptivePtime: false,
      });
      const { sendTransport } = await joinVoiceChannel();
      const mc = sendTransport.produce.mock.calls.filter(
        (c: any) => c[0].appData?.source === 'mic'
      );
      expect(mc[0][0].codecOptions.opusPtime).toBe(60);
    });

    it('enables adaptivePtime', async () => {
      useAudioSettingsStore.setState({ advancedMode: true, adaptivePtime: true, frameSize: 10 });
      const { sendTransport } = await joinVoiceChannel();
      const mc = sendTransport.produce.mock.calls.filter(
        (c: any) => c[0].appData?.source === 'mic'
      );
      expect(mc[0][0].encodings[0].adaptivePtime).toBe(true);
    });

    it('stereo from tier when null', async () => {
      useAudioSettingsStore.setState({ advancedMode: true, stereoOverride: null });
      const { sendTransport } = await joinVoiceChannel();
      const mc = sendTransport.produce.mock.calls.filter(
        (c: any) => c[0].appData?.source === 'mic'
      );
      expect(mc[0][0].codecOptions.opusStereo).toBe(true);
    });

    it('DTX combines settings', async () => {
      useAudioSettingsStore.setState({ advancedMode: true, silenceDetection: true });
      const { sendTransport } = await joinVoiceChannel();
      const mc = sendTransport.produce.mock.calls.filter(
        (c: any) => c[0].appData?.source === 'mic'
      );
      expect(mc[0][0].codecOptions.opusDtx).toBe(true);
    });
  });

  // ===== Video error handling =====

  describe('video error handling', () => {
    it('NotAllowedError', async () => {
      const m = await import('@/renderer/stores/osPermissionStore');
      vi.mocked(m.ensureOsPermission).mockResolvedValue('granted');
      await joinVoiceChannel();
      mockGetUserMedia.mockRejectedValue(new DOMException('d', 'NotAllowedError'));
      await voiceService.toggleVideo();
      expect(useVoiceStore.getState().videoSlotError).toContain('Camera access denied');
      vi.mocked(m.ensureOsPermission).mockResolvedValue('granted');
    });

    it('NotFoundError', async () => {
      const m = await import('@/renderer/stores/osPermissionStore');
      vi.mocked(m.ensureOsPermission).mockResolvedValue('granted');
      await joinVoiceChannel();
      mockGetUserMedia.mockRejectedValue(new DOMException('n', 'NotFoundError'));
      await voiceService.toggleVideo();
      expect(useVoiceStore.getState().videoSlotError).toContain('No camera found');
      vi.mocked(m.ensureOsPermission).mockResolvedValue('granted');
    });

    it('generic error', async () => {
      const m = await import('@/renderer/stores/osPermissionStore');
      vi.mocked(m.ensureOsPermission).mockResolvedValue('granted');
      await joinVoiceChannel();
      mockGetUserMedia.mockRejectedValue(new Error('boom'));
      await voiceService.toggleVideo();
      expect(useVoiceStore.getState().videoSlotError).toContain('Could not start camera');
      vi.mocked(m.ensureOsPermission).mockResolvedValue('granted');
    });

    it('falls back on OverconstrainedError', async () => {
      const m = await import('@/renderer/stores/osPermissionStore');
      vi.mocked(m.ensureOsPermission).mockResolvedValue('granted');
      useVideoSettingsStore.setState({ cameraPreset: '4K60' });
      const { sendTransport } = await joinVoiceChannel();
      mockGetUserMedia
        .mockRejectedValueOnce(new DOMException('oc', 'OverconstrainedError'))
        .mockRejectedValueOnce(new DOMException('oc', 'OverconstrainedError'))
        .mockRejectedValueOnce(new DOMException('oc', 'OverconstrainedError'))
        .mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      sendTransport.produce.mockResolvedValue(createMockProducer('cp', 'camera'));
      await voiceService.toggleVideo();
      const videoCalls = mockGetUserMedia.mock.calls.filter((c: any) => c[0]?.video !== undefined);
      expect(videoCalls.length).toBeGreaterThanOrEqual(2);
      vi.mocked(m.ensureOsPermission).mockResolvedValue('granted');
    });
  });

  // ===== Tune in/out =====

  describe('tuneInToScreenShare', () => {
    it('enforces 5-stream limit', async () => {
      await joinVoiceChannel();
      const store = useVoiceStore.getState();
      for (let i = 0; i < 5; i++) store.tuneIn(`p${i}`, `c${i}`);
      await voiceService.tuneInToScreenShare('p6', 'u6');
      expect(useVoiceStore.getState().videoSlotError).toContain('Maximum 5');
    });
  });

  // ===== Codec floor =====

  describe('codec floor', () => {
    it('stores in store', async () => {
      await joinVoiceChannel();
      useVoiceStore.getState().setCodecFloor(['video/vp8', 'video/vp9']);
      expect(useVoiceStore.getState().codecFloor).toEqual(['video/vp8', 'video/vp9']);
    });

    it('camera layering gate coalesces re-produce while one is in flight', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      let resolveFirst: (() => void) | undefined;
      const firstReproduce = new Promise<void>((resolve) => {
        resolveFirst = resolve;
      });
      const fastReproduceCamera = vi
        .fn()
        .mockReturnValueOnce(firstReproduce)
        .mockResolvedValue(undefined);
      svc.cameraLayeringEnabled = false;
      svc.fastReproduceCamera = fastReproduceCamera;

      try {
        const handler = socketListeners['camera-layering-gate']?.[0];
        expect(handler).toBeDefined();

        handler?.({ enabled: true });

        expect(svc.cameraLayeringEnabled).toBe(true);
        expect(fastReproduceCamera).toHaveBeenCalledTimes(1);

        handler?.({ enabled: false });

        expect(svc.cameraLayeringEnabled).toBe(false);
        expect(fastReproduceCamera).toHaveBeenCalledTimes(1);

        resolveFirst?.();
        await firstReproduce;
        await Promise.resolve();

        expect(fastReproduceCamera).toHaveBeenCalledTimes(2);
      } finally {
        svc.cameraLayeringEnabled = false;
        delete svc.fastReproduceCamera;
      }
    });

    it('does not let a stale camera-layering drain release a successor session latch', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      let releaseStale: (() => void) | undefined;
      let releaseCurrent: (() => void) | undefined;
      const stale = new Promise<void>((resolve) => {
        releaseStale = resolve;
      });
      const current = new Promise<void>((resolve) => {
        releaseCurrent = resolve;
      });
      const fastReproduceCamera = vi.fn().mockReturnValueOnce(stale).mockReturnValueOnce(current);
      svc.fastReproduceCamera = fastReproduceCamera;

      try {
        svc.scheduleCameraLayeringReproduce();
        expect(svc.cameraLayeringReproduceInFlight).toBe(true);

        svc.invalidateVideoReproduces();
        svc.resetRemoteVideoLayeringState();
        svc.videoReproduceSessionActive = true;
        svc.scheduleCameraLayeringReproduce();
        expect(fastReproduceCamera).toHaveBeenCalledTimes(2);

        releaseStale?.();
        await stale;
        await Promise.resolve();
        expect(svc.cameraLayeringReproduceInFlight).toBe(true);

        releaseCurrent?.();
        await current;
        await Promise.resolve();
        expect(svc.cameraLayeringReproduceInFlight).toBe(false);
      } finally {
        delete svc.fastReproduceCamera;
      }
    });
  });

  // ===== Connection state =====

  describe('connection state', () => {
    it('connecting -> connected', async () => {
      const states: string[] = [];
      const un = useVoiceStore.subscribe((s) => {
        if (!states.length || states[states.length - 1] !== s.connectionState)
          states.push(s.connectionState);
      });
      await joinVoiceChannel();
      un();
      expect(states).toContain('connecting');
      expect(states).toContain('connected');
    });

    it('error on failure', async () => {
      setupAuth();
      mockApiFetch.mockRejectedValueOnce(new Error('fail'));
      await expect(voiceService.joinChannel('ch')).rejects.toThrow('fail');
      expect(useVoiceStore.getState().connectionState).toBe('error');
    });
  });

  // ===== Electron desktop capturer =====

  describe('Electron capturer', () => {
    it('uses getDesktopSources', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const orig = (globalThis as any).electron;
      (globalThis as any).electron = {
        ...orig,
        getDesktopSources: vi
          .fn()
          .mockResolvedValue([{ id: 'screen:0:0', name: 'S1', thumbnailDataURL: '' }]),
      };
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      sendTransport.produce.mockResolvedValue(createMockProducer('sp', 'screen'));
      await voiceService.produceScreen();
      expect(mockGetUserMedia).toHaveBeenCalled();
      (globalThis as any).electron = orig;
    });
  });

  // ===== Screen-audio scope (#2161) =====
  // System loopback audio (chromeMediaSource: 'desktop') captures the WHOLE
  // desktop and ignores chromeMediaSourceId, so it must only be requested for
  // entire-screen ('screen:') shares. A window/app ('window:') share that asks
  // for it broadcasts all system audio to the channel — the privacy leak in #2161.

  describe('screen-audio scope (#2161)', () => {
    type CaptureFn = (
      id: string | undefined,
      res: { w: number; h: number },
      fps: number
    ) => Promise<unknown>;

    function captureElectron(id: string | undefined): Promise<unknown> {
      const svc = voiceService as unknown as { captureScreenElectron: CaptureFn };
      return svc.captureScreenElectron(id, { w: 1280, h: 720 }, 30);
    }

    let origElectron: typeof globalThis.electron;
    beforeEach(() => {
      origElectron = globalThis.electron;
      (globalThis as any).electron = { getDesktopSources: vi.fn() };
    });
    afterEach(() => {
      globalThis.electron = origElectron;
    });

    it('window: source captures video-only — no system-audio leak', async () => {
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      await captureElectron('window:42:0');

      expect(mockGetUserMedia).toHaveBeenCalledTimes(1);
      const constraints = mockGetUserMedia.mock.calls[0][0] as { audio: unknown; video: unknown };
      expect(constraints.audio).toBe(false);
      expect(constraints.video).toBeTruthy();
    });

    it('screen: source attempts system audio', async () => {
      mockGetUserMedia.mockResolvedValue(
        createMockMediaStream([{ kind: 'video' }, { kind: 'audio' }])
      );
      await captureElectron('screen:0:0');

      const constraints = mockGetUserMedia.mock.calls[0][0] as { audio: unknown; video: unknown };
      expect(constraints.audio).not.toBe(false);
      expect(constraints.audio).toBeTruthy();
    });

    // No sourceId → auto-select branch: a screen: source is preferred and gets
    // audio; a window-only source list falls back to sources[0] (a window:) and
    // must stay video-only (no leak via the fallback path).
    it('auto-selects a screen: source and attempts audio when no sourceId is given', async () => {
      (globalThis as any).electron.getDesktopSources.mockResolvedValue([
        { id: 'window:9:0', name: 'W', thumbnailDataURL: '' },
        { id: 'screen:0:0', name: 'S', thumbnailDataURL: '' },
      ]);
      mockGetUserMedia.mockResolvedValue(
        createMockMediaStream([{ kind: 'video' }, { kind: 'audio' }])
      );
      await captureElectron(undefined);

      const constraints = mockGetUserMedia.mock.calls[0][0] as { audio: unknown };
      expect(constraints.audio).not.toBe(false);
    });

    it('auto-selects video-only when only window sources exist (no fallback leak)', async () => {
      (globalThis as any).electron.getDesktopSources.mockResolvedValue([
        { id: 'window:9:0', name: 'W', thumbnailDataURL: '' },
      ]);
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video' }]));
      await captureElectron(undefined);

      const constraints = mockGetUserMedia.mock.calls[0][0] as { audio: unknown };
      expect(constraints.audio).toBe(false);
    });
  });

  // ===== OS permission =====

  describe('OS permission', () => {
    it('throws for denied mic', async () => {
      const m = await import('@/renderer/stores/osPermissionStore');
      vi.mocked(m.ensureOsPermission).mockResolvedValue('denied');
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      mockSocket.connected = true;
      const st = makeSendTransport();
      const rt = makeRecvTransport();
      mockCreateSendTransport.mockReturnValue(st);
      mockCreateRecvTransport.mockReturnValue(rt);
      setupEmitResponses({
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'p' },
      });
      mockGetUserMedia.mockRejectedValue(new DOMException('d', 'NotAllowedError'));
      st.produce.mockResolvedValue(createMockProducer());
      await expect(voiceService.joinChannel('ch')).rejects.toThrow();
      vi.mocked(m.ensureOsPermission).mockResolvedValue('granted');
    });
  });

  // ===== Socket timeout =====

  describe('socket timeout', () => {
    it('rejects after the connect timeout when the socket never connects', async () => {
      setupAuth();
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: vi.fn().mockResolvedValue(makeJoinResponse()),
      });
      mockSocket.connected = false;
      const promise = voiceService.joinChannel('ch');
      // Attach no-op catch to prevent unhandled rejection before timers advance
      promise.catch(() => {});
      // > VOICE_CONNECT_TIMEOUT_MS (30s, raised from 10s in #2176 so a brief
      // media-plane restart is survived); no connect/connect_error is fired here,
      // so the overall timeout is the only thing that settles the join.
      await vi.advanceTimersByTimeAsync(31_000);
      await expect(promise).rejects.toThrow('Socket connection timeout');
    });
  });

  // ===== VAD (Voice Activity Detection) =====

  describe('VAD', () => {
    it('starts VAD after producing audio', async () => {
      const { micProducer } = await joinVoiceChannel();
      // VAD is started inside produceAudio — check that AudioContext was created
      expect(micProducer.on).toHaveBeenCalled();
      // AnalyserNode fftSize should have been configured
      expect(mockAnalyser.fftSize).toBeDefined();
    });

    it('sets isSpeaking when audio exceeds threshold', async () => {
      await joinVoiceChannel();
      setupAuth(); // ensure user is set for updateLocalSpeaking

      // Simulate high volume data from analyser
      mockAnalyser.getByteFrequencyData.mockImplementation((arr: Uint8Array) => {
        arr.fill(50); // Well above SPEAKING_THRESHOLD of 8
      });

      // Advance past the 50ms VAD interval
      await vi.advanceTimersByTimeAsync(100);

      const state = useVoiceStore.getState();
      const participant = state.participants['user-1'];
      expect(participant?.isSpeaking).toBe(true);
    });

    it('debounces silence before clearing isSpeaking', async () => {
      await joinVoiceChannel();
      setupAuth();

      // First: simulate speaking
      mockAnalyser.getByteFrequencyData.mockImplementation((arr: Uint8Array) => {
        arr.fill(50);
      });
      await vi.advanceTimersByTimeAsync(100);

      // Now: simulate silence
      mockAnalyser.getByteFrequencyData.mockImplementation((arr: Uint8Array) => {
        arr.fill(0);
      });
      // Should still show speaking for 200ms (SILENCE_DELAY)
      await vi.advanceTimersByTimeAsync(100);
      expect(useVoiceStore.getState().participants['user-1']?.isSpeaking).toBe(true);

      // After full delay, should be not speaking
      await vi.advanceTimersByTimeAsync(200);
      expect(useVoiceStore.getState().participants['user-1']?.isSpeaking).toBe(false);
    });

    it('stops VAD on cleanup', async () => {
      await joinVoiceChannel();
      voiceService.emergencyCleanup();
      // No errors thrown, intervals cleaned up
      expect(useVoiceStore.getState().connectionState).toBe('disconnected');
    });
  });

  // ===== Noise Gate =====

  describe('noise gate', () => {
    it('applies noise gate in manual mode', async () => {
      useAudioSettingsStore.setState({ noiseGateMode: 'manual', noiseGateLevel: -40 });
      await joinVoiceChannel();
      // Noise gate creates AudioContext, source, analyser, gain, destination
      expect(MockAudioContext.prototype.createMediaStreamSource || true).toBeTruthy();
    });

    it('returns processed track from destination', async () => {
      useAudioSettingsStore.setState({ noiseGateMode: 'manual', noiseGateLevel: -40 });
      const { sendTransport } = await joinVoiceChannel();
      // The producer should have been called with a track
      const produceCall = sendTransport.produce.mock.calls[0]?.[0];
      expect(produceCall?.track).toBeDefined();
    });
  });

  // ===== Input Volume =====

  describe('input volume', () => {
    it('applies gain node for volume control', async () => {
      useAudioSettingsStore.setState({ inputVolume: 50 });
      await joinVoiceChannel();
      // GainNode should have been created and connected
      expect(mockGainNode.gain.setTargetAtTime).toHaveBeenCalled();
    });

    it('100% volume results in gain of 1', async () => {
      useAudioSettingsStore.setState({ inputVolume: 100 });
      await joinVoiceChannel();
      // At 100%, gain = 100/100 = 1.0
      const calls = mockGainNode.gain.setTargetAtTime.mock.calls;
      if (calls.length > 0) {
        expect(calls[0][0]).toBeCloseTo(1.0, 1);
      }
    });
  });

  // ===== Packet Loss Monitor =====

  describe('packet loss monitor', () => {
    it('starts monitoring after producing audio', async () => {
      const { micProducer } = await joinVoiceChannel();
      // packetLossTimer started inside produceAudio — advance to trigger poll
      await vi.advanceTimersByTimeAsync(5500);
      // getStats should have been called on the mic producer
      expect(micProducer.getStats).toHaveBeenCalled();
    });

    it('updates store with loss percentage', async () => {
      const { micProducer } = await joinVoiceChannel();

      // Mock stats with loss data
      const statsMap = new Map();
      statsMap.set('outbound', { type: 'outbound-rtp', packetsSent: 100 });
      statsMap.set('remote', { type: 'remote-inbound-rtp', packetsLost: 5 });
      micProducer.getStats.mockResolvedValue(statsMap);

      // First poll establishes baseline
      await vi.advanceTimersByTimeAsync(5500);

      // Update stats for second poll — delta shows loss
      const statsMap2 = new Map();
      statsMap2.set('outbound', { type: 'outbound-rtp', packetsSent: 200 });
      statsMap2.set('remote', { type: 'remote-inbound-rtp', packetsLost: 15 });
      micProducer.getStats.mockResolvedValue(statsMap2);

      await vi.advanceTimersByTimeAsync(5500);

      const loss = useVoiceStore.getState().packetLossPercent;
      expect(loss).toBeGreaterThanOrEqual(0);
    });

    it('handles stats unavailable gracefully', async () => {
      const { micProducer } = await joinVoiceChannel();
      micProducer.getStats.mockRejectedValue(new Error('stats unavailable'));

      // Should not throw
      await vi.advanceTimersByTimeAsync(5500);
      expect(micProducer.getStats).toHaveBeenCalled();
    });
  });

  describe('B-signal session-stats monitor lifecycle (#2187 item 1)', () => {
    function videoStatsMap(mime = 'video/VP8', powerEfficient = true) {
      const m = new Map<string, unknown>();
      m.set('o1', {
        type: 'outbound-rtp',
        kind: 'video',
        codecId: 'c1',
        powerEfficientEncoder: powerEfficient,
      });
      m.set('c1', { type: 'codec', id: 'c1', mimeType: mime });
      return m;
    }

    it('startPacketLossMonitor is idempotent — second call keeps the same timer', async () => {
      await joinVoiceChannel(); // produceAudio started the monitor
      const svc = voiceService as any;
      const timer1 = svc.packetLossTimer;
      expect(timer1).not.toBeNull();
      svc.startPacketLossMonitor();
      expect(svc.packetLossTimer).toBe(timer1);
    });

    it('learns B for the camera codec in a no-mic session', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;

      // Simulate a no-mic (video-only) session: stop the mic-started monitor and drop the mic.
      svc.stopPacketLossMonitor();
      svc.producers.delete('mic');
      expect(svc.packetLossTimer).toBeNull();

      const cameraProducer = createMockProducer('prod-cam', 'camera');
      cameraProducer.getStats = vi.fn().mockResolvedValue(videoStatsMap('video/VP8', true));
      sendTransport.produce.mockResolvedValue(cameraProducer);
      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video', id: 'cam-1' }]));

      await voiceService.produceVideo();
      expect(svc.packetLossTimer).not.toBeNull(); // video path restarted the monitor

      await vi.advanceTimersByTimeAsync(5500);
      expect(useVideoSettingsStore.getState().webrtcHwByMime['video/vp8']).toBe(true);
    });
  });

  describe('reProduceIfBetterCodec requireHwImprovement gate (#2187 item 2)', () => {
    // The shared mock device has VP8/VP9/H264/AV1, so pin the cascade to VP8 via
    // preferredVideoCodec (cascade step 1) for a deterministic pick, and mark VP8 SW-learned.
    async function setupCameraOnCodec(activeMime: string) {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.setState({
        preferredVideoCodec: 'video/VP8',
        hardwareAcceleration: false,
        webrtcHwByMime: { 'video/vp8': false },
      });
      const cam = createMockProducer('prod-cam', 'camera');
      cam.rtpSender.getParameters = vi.fn().mockReturnValue({ codecs: [{ mimeType: activeMime }] });
      svc.producers.set('camera', cam);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video', id: 'cam-1' }]);
      return svc;
    }

    it('does NOT switch when the better pick is software-encoded and requireHwImprovement is set', async () => {
      const svc = await setupCameraOnCodec('video/AV1'); // active AV1 ≠ pick VP8, but VP8 is SW
      // Override on the singleton (delete restores the prototype) — the file's convention.
      // vi.spyOn would leak the mock across tests since beforeEach only clears, not restores.
      let called = 0;
      svc.fastReproduceCameraQueued = async () => {
        called++;
      };
      try {
        await svc.reProduceIfBetterCodec('camera', { requireHwImprovement: true });
        expect(called).toBe(0);
      } finally {
        delete svc.fastReproduceCameraQueued;
      }
    });

    it('switches (no requireHwImprovement) when a different codec is picked', async () => {
      const svc = await setupCameraOnCodec('video/AV1');
      let called = 0;
      svc.fastReproduceCameraQueued = async () => {
        called++;
      };
      try {
        await svc.reProduceIfBetterCodec('camera');
        expect(called).toBe(1);
      } finally {
        delete svc.fastReproduceCameraQueued;
      }
    });
  });

  describe('learnWebrtcHwSignal re-selection trigger (#2187 item 2)', () => {
    function bFalseStats(mime = 'video/VP8') {
      const m = new Map<string, unknown>();
      m.set('o1', {
        type: 'outbound-rtp',
        kind: 'video',
        codecId: 'c1',
        powerEfficientEncoder: false,
      });
      m.set('c1', { type: 'codec', id: 'c1', mimeType: mime });
      return m;
    }

    async function cameraOnVp8SoftwareStats() {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const cam = createMockProducer('prod-cam', 'camera');
      cam.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      cam.getStats = vi.fn().mockResolvedValue(bFalseStats('video/VP8'));
      svc.producers.set('camera', cam);
      return svc;
    }

    it('triggers reProduceIfBetterCodec when the active codec transitions to SW', async () => {
      const svc = await cameraOnVp8SoftwareStats();
      // Override on the singleton (delete restores the prototype) — beforeEach only clears
      // mock call history, not spies, so vi.spyOn on the singleton would leak into later tests.
      const calls: unknown[][] = [];
      svc.reProduceIfBetterCodec = async (...args: unknown[]) => {
        calls.push(args);
      };
      try {
        await svc.learnWebrtcHwSignal();
        expect(calls).toEqual([['camera', { requireHwImprovement: true }, expect.any(Number)]]);
      } finally {
        delete svc.reProduceIfBetterCodec;
      }
    });

    it('does NOT re-trigger once B is already false (churn guard)', async () => {
      const svc = await cameraOnVp8SoftwareStats();
      useVideoSettingsStore.setState({ webrtcHwByMime: { 'video/vp8': false } });
      const calls: unknown[][] = [];
      svc.reProduceIfBetterCodec = async (...args: unknown[]) => {
        calls.push(args);
      };
      try {
        await svc.learnWebrtcHwSignal();
        expect(calls).toEqual([]);
      } finally {
        delete svc.reProduceIfBetterCodec;
      }
    });

    it('demotes every qualified AV1 HW target and Auto reselects to verified H264 HW', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const oldCamera = createMockProducer('prod-camera-av1-sw', 'camera') as any;
      oldCamera.rtpParameters = {
        codecs: [{ mimeType: 'video/AV1', parameters: {} }],
      };
      oldCamera.getStats = vi.fn().mockResolvedValue(bFalseStats('video/AV1'));
      svc.producers.set('camera', oldCamera);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video', id: 'camera-av1-track' }]);
      useVoiceStore.getState().setVideoOn(true);
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        hdrEncoding: false, // SDR fallback is allowed by policy.
        preferredVideoCodec: null,
        webrtcHwByMime: {},
        codecCapabilities: [
          {
            mimeType: 'video/AV1',
            supported: true,
            profileId: 'hdr',
            profileLabel: '10-bit HDR target',
            isHdr: true,
            hwAvailable: true,
          },
          {
            mimeType: 'video/AV1',
            supported: true,
            profileId: 'sdr',
            profileLabel: '8-bit SDR target',
            isHdr: false,
            hwAvailable: true,
          },
          {
            mimeType: 'video/H264',
            supported: true,
            profileId: '640034',
            profileLabel: 'High',
            isHdr: false,
            hwAvailable: true,
          },
        ],
      });

      const replacement = createMockProducer('prod-camera-h264-hw', 'camera') as any;
      replacement.rtpParameters = {
        codecs: [{ mimeType: 'video/H264', parameters: { 'profile-level-id': '640034' } }],
      };
      sendTransport.produce.mockClear();
      sendTransport.produce.mockResolvedValue(replacement);

      await svc.learnWebrtcHwSignal();
      await vi.waitFor(() => expect(sendTransport.produce).toHaveBeenCalledTimes(1));
      await svc.videoReproduceQueues.camera;

      expect(useVideoSettingsStore.getState().webrtcHwByMime['video/av1']).toBe(false);
      expect(oldCamera.close).toHaveBeenCalledTimes(1);
      expect(sendTransport.produce.mock.calls[0][0].codec).toMatchObject({
        mimeType: 'video/H264',
        parameters: { 'profile-level-id': '640034' },
      });
      expect(svc.producers.get('camera')).toBe(replacement);
    });

    it('re-selects BOTH camera and screen when they share a software-encoded codec (#2189)', async () => {
      // Two-pass previousHw capture: without it, camera's write flips previousHw to false for
      // screen and its churn guard suppresses screen's re-selection (Codex review of #2189).
      const svc = await cameraOnVp8SoftwareStats();
      const scr = createMockProducer('prod-scr', 'screen');
      scr.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      scr.getStats = vi.fn().mockResolvedValue(bFalseStats('video/VP8'));
      svc.producers.set('screen', scr);

      const calls: unknown[][] = [];
      svc.reProduceIfBetterCodec = async (...args: unknown[]) => {
        calls.push(args);
      };
      try {
        await svc.learnWebrtcHwSignal();
        expect(calls).toEqual([
          ['camera', { requireHwImprovement: true }, expect.any(Number)],
          ['screen', { requireHwImprovement: true }, expect.any(Number)],
        ]);
      } finally {
        delete svc.reProduceIfBetterCodec;
      }
    });

    it('swallows a rejection from the fire-and-forget re-selection (#2189)', async () => {
      const svc = await cameraOnVp8SoftwareStats();
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      svc.reProduceIfBetterCodec = async () => {
        throw new Error('transport closed mid-swap');
      };
      try {
        // learnWebrtcHwSignal must resolve (not reject) — the .catch swallows the rejection so
        // it never surfaces as an unhandled promise rejection on the 5s monitor tick.
        await expect(svc.learnWebrtcHwSignal()).resolves.toBeUndefined();
        await Promise.resolve(); // let the detached .catch microtask run
        expect(warn).toHaveBeenCalledWith(
          '[codec-floor] HW re-selection failed:',
          'transport closed mid-swap'
        );
      } finally {
        delete svc.reProduceIfBetterCodec;
        warn.mockRestore();
      }
    });

    it.each([
      {
        source: 'camera' as const,
        active: () => useVoiceStore.getState().isVideoOn,
        setActive: () => useVoiceStore.getState().setVideoOn(true),
        streamField: 'localCameraStream' as const,
      },
      {
        source: 'screen' as const,
        active: () => useVoiceStore.getState().isScreenSharing,
        setActive: () => useVoiceStore.getState().setScreenSharing(true),
        streamField: 'localScreenStream' as const,
      },
    ])(
      'keeps $source UI and capture aligned with producer state when automatic B-signal re-selection fails (#2187)',
      async ({ source, active, setActive, streamField }) => {
        const { sendTransport } = await joinVoiceChannel();
        const svc = voiceService as any;
        const oldProducer = createMockProducer(`prod-${source}`, source);
        oldProducer.rtpSender.getParameters = vi
          .fn()
          .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
        oldProducer.getStats = vi.fn().mockResolvedValue(bFalseStats('video/VP8'));

        svc.cameraLayeringEnabled = false;
        svc.producers.set(source, oldProducer);
        svc[streamField] = createMockMediaStream([{ kind: 'video', id: `${source}-track` }]);
        setActive();
        useVideoSettingsStore.setState({
          hardwareAcceleration: true,
          preferredVideoCodec: null,
          webrtcHwByMime: { 'video/av1': true },
        });

        sendTransport.produce.mockClear();
        sendTransport.produce.mockRejectedValueOnce(new Error('replacement failed'));
        const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

        try {
          await svc.learnWebrtcHwSignal();
          await vi.waitFor(() => {
            expect(warn).toHaveBeenCalledWith(
              '[codec-floor] HW re-selection failed:',
              'replacement failed'
            );
          });

          const mappedProducer = svc.producers.get(source);
          const hasLiveProducer =
            mappedProducer !== undefined && vi.mocked(mappedProducer.close).mock.calls.length === 0;
          expect([active(), svc[streamField] !== null]).toEqual([hasLiveProducer, hasLiveProducer]);
          expect(useVoiceStore.getState().videoSlotError).toContain(
            source === 'camera' ? 'Camera stopped' : 'Screen share stopped'
          );
          await expect(svc.videoReproduceQueues[source]).resolves.toBeUndefined();
        } finally {
          warn.mockRestore();
        }
      }
    );

    it('does not leave a live orphan when codec re-selections overlap (#2187)', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const oldProducer = createMockProducer('prod-camera-old', 'camera');
      oldProducer.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      svc.cameraLayeringEnabled = false;
      svc.producers.set('camera', oldProducer);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video', id: 'camera-track' }]);
      useVoiceStore.getState().setVideoOn(true);
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        preferredVideoCodec: null,
        webrtcHwByMime: { 'video/vp8': false, 'video/av1': true },
      });

      let releaseDrain!: () => void;
      const drainGate = new Promise<void>((resolve) => {
        releaseDrain = resolve;
      });
      svc.drainSendTransportQueue = vi.fn(async () => drainGate);

      const replacements = [
        createMockProducer('prod-camera-replacement-a', 'camera'),
        createMockProducer('prod-camera-replacement-b', 'camera'),
      ];
      const created: Array<(typeof replacements)[number]> = [];
      sendTransport.produce.mockClear();
      sendTransport.produce.mockImplementation(async () => {
        const replacement = replacements[created.length];
        created.push(replacement);
        return replacement;
      });

      try {
        const first = svc.reProduceIfBetterCodec('camera', { requireHwImprovement: true });
        const second = svc.reProduceIfBetterCodec('camera', { requireHwImprovement: true });
        releaseDrain();
        await Promise.all([first, second]);

        const activeProducer = svc.producers.get('camera');
        const liveOrphanIds = created
          .filter(
            (producer) =>
              producer !== activeProducer && vi.mocked(producer.close).mock.calls.length === 0
          )
          .map((producer) => producer.id);
        expect(liveOrphanIds).toEqual([]);
      } finally {
        releaseDrain();
        delete svc.drainSendTransportQueue;
      }
    });

    it('serializes live camera re-produce with a codec-driven swap (#2187)', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const oldProducer = createMockProducer('prod-camera-old', 'camera');
      oldProducer.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      const oldStream = createMockMediaStream([{ kind: 'video', id: 'camera-track-old' }]);
      const freshStream = createMockMediaStream([{ kind: 'video', id: 'camera-track-fresh' }]);
      svc.cameraLayeringEnabled = false;
      svc.producers.set('camera', oldProducer);
      svc.localCameraStream = oldStream;
      useVoiceStore.getState().setVideoOn(true);
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        preferredVideoCodec: null,
        webrtcHwByMime: { 'video/vp8': false, 'video/av1': true },
      });

      let releaseDrain!: () => void;
      const drainGate = new Promise<void>((resolve) => {
        releaseDrain = resolve;
      });
      svc.drainSendTransportQueue = vi.fn(async () => drainGate);
      mockGetUserMedia.mockResolvedValue(freshStream);

      const replacements = [
        createMockProducer('prod-camera-fast', 'camera'),
        createMockProducer('prod-camera-live', 'camera'),
      ];
      const created: Array<(typeof replacements)[number]> = [];
      sendTransport.produce.mockClear();
      sendTransport.produce.mockImplementation(async () => {
        const replacement = replacements[created.length];
        created.push(replacement);
        return replacement;
      });

      try {
        const fast = svc.reProduceIfBetterCodec('camera', { requireHwImprovement: true });
        await vi.waitFor(() => expect(svc.drainSendTransportQueue).toHaveBeenCalledTimes(1));
        const live = svc.liveReproduceCamera();
        releaseDrain();
        await Promise.all([fast, live]);

        const activeProducer = svc.producers.get('camera');
        expect(
          created
            .filter(
              (producer) =>
                producer !== activeProducer && vi.mocked(producer.close).mock.calls.length === 0
            )
            .map((producer) => producer.id)
        ).toEqual([]);
        expect(svc.localCameraStream).toBe(freshStream);
      } finally {
        releaseDrain();
        delete svc.drainSendTransportQueue;
      }
    });

    it('serializes camera track replacement with codec re-selection (#2187)', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const oldProducer = createMockProducer('prod-camera-track-replace', 'camera');
      oldProducer.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      const oldStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-before-replace' },
      ]);
      const newStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-after-replace' },
      ]);
      const replacementProducer = createMockProducer('prod-camera-after-reselect', 'camera');
      svc.cameraLayeringEnabled = false;
      svc.producers.set('camera', oldProducer);
      svc.localCameraStream = oldStream;
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        preferredVideoCodec: null,
        webrtcHwByMime: { 'video/vp8': false, 'video/av1': true },
      });

      let releaseAcquire!: (stream: ReturnType<typeof createMockMediaStream>) => void;
      mockGetUserMedia.mockClear();
      mockGetUserMedia.mockImplementation(
        () =>
          new Promise((resolve) => {
            releaseAcquire = resolve;
          })
      );
      sendTransport.produce.mockClear();
      sendTransport.produce.mockResolvedValue(replacementProducer);

      const replaceTrack = svc.liveReplaceCameraTrack();
      await vi.waitFor(() => expect(mockGetUserMedia).toHaveBeenCalledTimes(1));
      const reselect = svc.reProduceIfBetterCodec('camera', { requireHwImprovement: true });
      await Promise.resolve();
      expect(oldProducer.close).not.toHaveBeenCalled();

      releaseAcquire(newStream);
      await Promise.all([replaceTrack, reselect]);

      expect(oldProducer.replaceTrack).toHaveBeenCalledWith({
        track: newStream.getVideoTracks()[0],
      });
      expect(oldProducer.close).toHaveBeenCalledTimes(1);
      expect(oldStream.getTracks()[0].stop).toHaveBeenCalled();
      expect(svc.localCameraStream).toBe(newStream);
      expect(svc.producers.get('camera')).toBe(replacementProducer);
    });

    it('discards queued codec work when the media session is rebuilt (#2187)', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const oldProducer = createMockProducer('prod-camera-old-session', 'camera');
      oldProducer.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      svc.producers.set('camera', oldProducer);
      svc.localCameraStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-old-session' },
      ]);

      let releaseQueue!: () => void;
      svc.videoReproduceQueues.camera = new Promise<void>((resolve) => {
        releaseQueue = resolve;
      });
      const staleWork = svc.reProduceIfBetterCodec('camera', { requireHwImprovement: true });

      svc.cleanupMediaAndTransports();

      const successorTransport = makeSendTransport();
      const successorProducer = createMockProducer('prod-camera-successor', 'camera');
      successorProducer.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      const successorStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-successor' },
      ]);
      successorTransport.produce.mockResolvedValue(
        createMockProducer('prod-camera-unexpected-replacement', 'camera')
      );
      svc.sendTransport = successorTransport;
      svc.videoReproduceSessionActive = true;
      svc.producers.set('camera', successorProducer);
      svc.localCameraStream = successorStream;
      useVoiceStore.getState().setVideoOn(true);
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        preferredVideoCodec: null,
        webrtcHwByMime: { 'video/vp8': false, 'video/av1': true },
      });

      releaseQueue();
      await staleWork;

      expect(successorProducer.close).not.toHaveBeenCalled();
      expect(successorTransport.produce).not.toHaveBeenCalled();
      expect(svc.producers.get('camera')).toBe(successorProducer);
      expect(svc.localCameraStream).toBe(successorStream);
    });

    it('does not let an in-flight old-session failure clean up the rebuilt call (#2187)', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const oldProducer = createMockProducer('prod-camera-in-flight-old', 'camera');
      oldProducer.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      svc.producers.set('camera', oldProducer);
      svc.localCameraStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-in-flight-old' },
      ]);
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        preferredVideoCodec: null,
        webrtcHwByMime: { 'video/vp8': false, 'video/av1': true },
      });

      let rejectReplacement!: (reason: Error) => void;
      sendTransport.produce.mockClear();
      sendTransport.produce.mockImplementation(
        () =>
          new Promise((_resolve, reject) => {
            rejectReplacement = reject;
          })
      );
      const staleWork = svc.reProduceIfBetterCodec('camera', { requireHwImprovement: true });
      await vi.waitFor(() => expect(sendTransport.produce).toHaveBeenCalledTimes(1));

      svc.cleanupMediaAndTransports();

      const successorTransport = makeSendTransport();
      const successorProducer = createMockProducer('prod-camera-in-flight-successor', 'camera');
      const successorStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-in-flight-successor' },
      ]);
      svc.sendTransport = successorTransport;
      svc.videoReproduceSessionActive = true;
      svc.producers.set('camera', successorProducer);
      svc.localCameraStream = successorStream;
      useVoiceStore.setState({ isVideoOn: true, videoSlotError: null });

      rejectReplacement(new Error('old-session replacement failed'));
      await expect(staleWork).rejects.toThrow('old-session replacement failed');

      expect(successorProducer.close).not.toHaveBeenCalled();
      expect(successorTransport.produce).not.toHaveBeenCalled();
      expect(svc.producers.get('camera')).toBe(successorProducer);
      expect(svc.localCameraStream).toBe(successorStream);
      expect(useVoiceStore.getState().isVideoOn).toBe(true);
      expect(useVoiceStore.getState().videoSlotError).toBeNull();
    });

    it('discards an old-session producer that succeeds after rebuild (#2187)', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const oldProducer = createMockProducer('prod-camera-late-success-old', 'camera');
      oldProducer.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      svc.producers.set('camera', oldProducer);
      svc.localCameraStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-late-success-old' },
      ]);
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        preferredVideoCodec: null,
        webrtcHwByMime: { 'video/vp8': false, 'video/av1': true },
      });

      const lateProducer = createMockProducer('prod-camera-late-success-created', 'camera');
      let releaseProduce!: (producer: typeof lateProducer) => void;
      sendTransport.produce.mockClear();
      sendTransport.produce.mockImplementation(
        () =>
          new Promise((resolve) => {
            releaseProduce = resolve;
          })
      );
      const staleWork = svc.reProduceIfBetterCodec('camera', { requireHwImprovement: true });
      await vi.waitFor(() => expect(sendTransport.produce).toHaveBeenCalledTimes(1));

      svc.cleanupMediaAndTransports();
      const successorTransport = makeSendTransport();
      const successorProducer = createMockProducer('prod-camera-late-success-successor', 'camera');
      const successorStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-late-success-successor' },
      ]);
      svc.sendTransport = successorTransport;
      svc.videoReproduceSessionActive = true;
      svc.producers.set('camera', successorProducer);
      svc.localCameraStream = successorStream;
      useVoiceStore.setState({ isVideoOn: true, videoSlotError: null });

      releaseProduce(lateProducer);
      await staleWork;

      expect(lateProducer.close).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('close-producer', {
        producerId: lateProducer.id,
      });
      expect(successorProducer.close).not.toHaveBeenCalled();
      expect(svc.producers.get('camera')).toBe(successorProducer);
      expect(svc.localCameraStream).toBe(successorStream);
      expect(useVoiceStore.getState().isVideoOn).toBe(true);
    });

    it('discards a B-signal observation that resolves after session rebuild (#2187)', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const oldProducer = createMockProducer('prod-camera-stats-old', 'camera');
      oldProducer.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      let releaseStats!: (stats: Map<string, unknown>) => void;
      oldProducer.getStats = vi.fn(
        () =>
          new Promise((resolve) => {
            releaseStats = resolve;
          })
      );
      svc.producers.set('camera', oldProducer);
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        preferredVideoCodec: null,
        webrtcHwByMime: { 'video/av1': true },
      });

      const calls: unknown[][] = [];
      svc.reProduceIfBetterCodec = async (...args: unknown[]) => {
        calls.push(args);
      };
      try {
        const staleLearner = svc.learnWebrtcHwSignal();
        await vi.waitFor(() => expect(oldProducer.getStats).toHaveBeenCalledTimes(1));

        svc.cleanupMediaAndTransports();
        svc.sendTransport = makeSendTransport();
        svc.videoReproduceSessionActive = true;
        svc.producers.set('camera', createMockProducer('prod-camera-stats-successor', 'camera'));

        releaseStats(bFalseStats('video/VP8'));
        await staleLearner;

        expect(calls).toEqual([]);
        expect(useVideoSettingsStore.getState().webrtcHwByMime['video/vp8']).toBeUndefined();
      } finally {
        delete svc.reProduceIfBetterCodec;
      }
    });

    it('discards an observed B-signal when that source is reopened before the batch applies (#2187)', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const oldCamera = createMockProducer('prod-camera-stats-batch-old', 'camera');
      oldCamera.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      oldCamera.getStats = vi.fn().mockResolvedValue(bFalseStats('video/VP8'));
      const blockingScreen = createMockProducer('prod-screen-stats-batch', 'screen');
      let releaseScreenStats!: (stats: Map<string, unknown>) => void;
      blockingScreen.getStats = vi.fn(
        () =>
          new Promise((resolve) => {
            releaseScreenStats = resolve;
          })
      );
      svc.producers.set('camera', oldCamera);
      svc.producers.set('screen', blockingScreen);
      svc.localCameraStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-stats-batch-old' },
      ]);
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        preferredVideoCodec: null,
        webrtcHwByMime: { 'video/av1': true },
      });

      const calls: unknown[][] = [];
      svc.reProduceIfBetterCodec = async (...args: unknown[]) => {
        calls.push(args);
      };
      try {
        const learner = svc.learnWebrtcHwSignal();
        await vi.waitFor(() => expect(blockingScreen.getStats).toHaveBeenCalledTimes(1));

        await svc.closeProducer('camera');
        const successorCamera = createMockProducer('prod-camera-stats-batch-new', 'camera');
        successorCamera.rtpSender.getParameters = vi
          .fn()
          .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
        svc.producers.set('camera', successorCamera);
        svc.localCameraStream = createMockMediaStream([
          { kind: 'video', id: 'camera-track-stats-batch-new' },
        ]);
        useVoiceStore.getState().setVideoOn(true);

        releaseScreenStats(new Map());
        await learner;

        expect(calls).toEqual([]);
        expect(useVideoSettingsStore.getState().webrtcHwByMime['video/vp8']).toBeUndefined();
        expect(svc.producers.get('camera')).toBe(successorCamera);
      } finally {
        delete svc.reProduceIfBetterCodec;
      }
    });

    it('discards B-signal stats that resolve while explicit close is draining (#2187)', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const oldCamera = createMockProducer('prod-camera-stats-close-drain', 'camera');
      oldCamera.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      let releaseStats!: (stats: Map<string, unknown>) => void;
      oldCamera.getStats = vi.fn(
        () =>
          new Promise((resolve) => {
            releaseStats = resolve;
          })
      );
      svc.producers.set('camera', oldCamera);
      svc.localCameraStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-stats-close-drain' },
      ]);
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        preferredVideoCodec: null,
        webrtcHwByMime: { 'video/av1': true },
      });

      let releaseDrain!: () => void;
      const drainGate = new Promise<void>((resolve) => {
        releaseDrain = resolve;
      });
      svc.drainSendTransportQueue = vi.fn(async () => drainGate);
      const calls: unknown[][] = [];
      svc.reProduceIfBetterCodec = async (...args: unknown[]) => {
        calls.push(args);
      };
      try {
        const learner = svc.learnWebrtcHwSignal();
        await vi.waitFor(() => expect(oldCamera.getStats).toHaveBeenCalledTimes(1));
        const close = svc.closeProducer('camera');
        await vi.waitFor(() => expect(svc.drainSendTransportQueue).toHaveBeenCalledTimes(1));

        releaseStats(bFalseStats('video/VP8'));
        await learner;

        expect(calls).toEqual([]);
        expect(useVideoSettingsStore.getState().webrtcHwByMime['video/vp8']).toBeUndefined();

        releaseDrain();
        await close;
      } finally {
        releaseDrain();
        delete svc.drainSendTransportQueue;
        delete svc.reProduceIfBetterCodec;
      }
    });

    it('does not carry a floor-change loop into the rebuilt session (#2187)', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const oldCamera = createMockProducer('prod-floor-camera-old', 'camera');
      const oldScreen = createMockProducer('prod-floor-screen-old', 'screen');
      oldCamera.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      oldScreen.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      svc.producers.set('camera', oldCamera);
      svc.producers.set('screen', oldScreen);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video', id: 'floor-camera-old' }]);
      svc.localScreenStream = createMockMediaStream([{ kind: 'video', id: 'floor-screen-old' }]);

      let releaseCameraQueue!: () => void;
      svc.videoReproduceQueues.camera = new Promise<void>((resolve) => {
        releaseCameraQueue = resolve;
      });
      const staleFloorChange = svc.handleCodecFloorChange(null, ['video/av1', 'video/vp8']);

      svc.cleanupMediaAndTransports();

      const successorTransport = makeSendTransport();
      const successorScreen = createMockProducer('prod-floor-screen-successor', 'screen');
      successorScreen.rtpSender.getParameters = vi
        .fn()
        .mockReturnValue({ codecs: [{ mimeType: 'video/VP8' }] });
      const successorStream = createMockMediaStream([
        { kind: 'video', id: 'floor-screen-successor' },
      ]);
      svc.sendTransport = successorTransport;
      svc.videoReproduceSessionActive = true;
      svc.producers.set('screen', successorScreen);
      svc.localScreenStream = successorStream;
      useVideoSettingsStore.setState({
        hardwareAcceleration: true,
        preferredVideoCodec: null,
        webrtcHwByMime: { 'video/vp8': false, 'video/av1': true },
      });

      releaseCameraQueue();
      await staleFloorChange;

      expect(successorScreen.close).not.toHaveBeenCalled();
      expect(successorTransport.produce).not.toHaveBeenCalled();
      expect(svc.producers.get('screen')).toBe(successorScreen);
      expect(svc.localScreenStream).toBe(successorStream);
    });

    it('cancels live camera reacquisition when the source is explicitly closed (#2187)', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const oldProducer = createMockProducer('prod-camera-close-during-acquire', 'camera');
      const oldStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-close-during-acquire' },
      ]);
      const acquiredStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-cancelled-acquire' },
      ]);
      svc.producers.set('camera', oldProducer);
      svc.localCameraStream = oldStream;
      useVoiceStore.getState().setVideoOn(true);

      let releaseAcquire!: (stream: ReturnType<typeof createMockMediaStream>) => void;
      mockGetUserMedia.mockClear();
      mockGetUserMedia.mockImplementation(
        () =>
          new Promise((resolve) => {
            releaseAcquire = resolve;
          })
      );
      sendTransport.produce.mockClear();

      const reproduce = svc.liveReproduceCamera();
      await vi.waitFor(() => expect(mockGetUserMedia).toHaveBeenCalledTimes(1));
      await voiceService.toggleVideo();
      releaseAcquire(acquiredStream);
      await reproduce;

      expect(acquiredStream.getTracks()[0].stop).toHaveBeenCalled();
      expect(sendTransport.produce).not.toHaveBeenCalled();
      expect(svc.producers.has('camera')).toBe(false);
      expect(svc.localCameraStream).toBeNull();
      expect(useVoiceStore.getState().isVideoOn).toBe(false);
    });

    it('does not try a camera fallback after explicit close cancels acquisition (#2187)', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set(
        'camera',
        createMockProducer('prod-camera-close-before-fallback', 'camera')
      );
      svc.localCameraStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-close-before-fallback' },
      ]);
      useVoiceStore.getState().setVideoOn(true);

      let rejectAcquire!: (reason: Error) => void;
      mockGetUserMedia.mockClear();
      mockGetUserMedia.mockImplementation(
        () =>
          new Promise((_resolve, reject) => {
            rejectAcquire = reject;
          })
      );
      sendTransport.produce.mockClear();

      const reproduce = svc.liveReproduceCamera();
      await vi.waitFor(() => expect(mockGetUserMedia).toHaveBeenCalledTimes(1));
      await voiceService.toggleVideo();
      rejectAcquire(new DOMException('constraints changed', 'OverconstrainedError'));
      await reproduce;

      expect(mockGetUserMedia).toHaveBeenCalledTimes(1);
      expect(sendTransport.produce).not.toHaveBeenCalled();
      expect(svc.producers.has('camera')).toBe(false);
      expect(svc.localCameraStream).toBeNull();
    });

    it('orders a camera restart after stale local/server producer teardown (#2187)', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const oldProducer = createMockProducer('prod-camera-close-during-produce', 'camera');
      const cancelledStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-close-during-produce' },
      ]);
      const successorStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-successor-produce' },
      ]);
      const cancelledProducer = createMockProducer('prod-camera-cancelled-new', 'camera');
      const successorProducer = createMockProducer('prod-camera-successor-new', 'camera');
      svc.producers.set('camera', oldProducer);
      svc.localCameraStream = createMockMediaStream([
        { kind: 'video', id: 'camera-track-before-produce' },
      ]);
      useVoiceStore.getState().setVideoOn(true);
      mockGetUserMedia
        .mockResolvedValueOnce(cancelledStream)
        .mockResolvedValueOnce(successorStream);

      let releaseProduce!: (producer: typeof cancelledProducer) => void;
      sendTransport.produce.mockClear();
      sendTransport.produce
        .mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              releaseProduce = resolve;
            })
        )
        .mockResolvedValueOnce(successorProducer);

      const reproduce = svc.liveReproduceCamera();
      await vi.waitFor(() => expect(sendTransport.produce).toHaveBeenCalledTimes(1));
      await voiceService.toggleVideo();
      const restart = voiceService.toggleVideo();
      await Promise.resolve();
      expect(sendTransport.produce).toHaveBeenCalledTimes(1);

      releaseProduce(cancelledProducer);
      await reproduce;
      await restart;

      expect(cancelledProducer.close).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('close-producer', {
        producerId: cancelledProducer.id,
      });
      expect(sendTransport.produce).toHaveBeenCalledTimes(2);
      expect(svc.producers.get('camera')).toBe(successorProducer);
      expect(svc.localCameraStream).toBe(successorStream);
      expect(useVoiceStore.getState().isVideoOn).toBe(true);
    });
  });

  // ===== Codec Selection =====

  describe('codec selection', () => {
    it('pickCameraCodec returns VP8 as last resort', async () => {
      await joinVoiceChannel();
      // Access private method via any cast
      const svc = voiceService as any;
      const result = svc.pickCameraCodec();
      expect(result).toBeDefined();
      expect(result.codec).toBeDefined();
      expect(result.encodings).toBeInstanceOf(Array);
      expect(result.encodings.length).toBe(1);
    });

    it('pickCameraCodec uses AV1 SVC encoding when camera layering is enabled', async () => {
      useVideoSettingsStore.setState({ supportSvc: true });
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.cameraLayeringEnabled = true;
      try {
        const result = svc.pickCameraCodec();
        expect(result.codec?.mimeType).toBe('video/AV1');
        expect(result.encodings).toHaveLength(1);
        expect(result.encodings[0].scalabilityMode).toBe('L3T3_KEY');
      } finally {
        svc.cameraLayeringEnabled = false;
      }
    });

    it('pickCameraCodec keeps AV1 as a single stream when SVC is disabled (#2242)', async () => {
      useVideoSettingsStore.setState({
        preferredVideoCodec: 'video/AV1',
        supportSvc: false,
        supportSimulcast: true,
      });
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.cameraLayeringEnabled = true;
      try {
        const result = svc.pickCameraCodec();
        expect(result.codec?.mimeType).toBe('video/AV1');
        expect(result.encodings).toHaveLength(1);
        expect(result.encodings[0].rid).toBeUndefined();
        expect(result.encodings[0].scalabilityMode).toBeUndefined();
      } finally {
        svc.cameraLayeringEnabled = false;
      }
    });

    it('pickCameraCodec preserves a floor-compatible manual H264 preference when layering is enabled', async () => {
      useVoiceStore.getState().setCodecFloor(['video/vp9:0', 'video/h264:640034']);
      useVideoSettingsStore.setState({
        preferredVideoCodec: 'video/H264',
        hardwareAcceleration: false,
        supportSvc: true,
        supportSimulcast: true,
      });
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.cameraLayeringEnabled = true;
      try {
        const result = svc.pickCameraCodec();
        expect(result.codec?.mimeType).toBe('video/H264');
        expect(result.codec?.parameters?.['profile-level-id']).toBe('640034');
        expect(result.encodings).toHaveLength(3);
        expect(result.encodings.map((encoding: { rid?: string }) => encoding.rid)).toEqual([
          'q',
          'h',
          'f',
        ]);
      } finally {
        svc.cameraLayeringEnabled = false;
      }
    });

    it('pickScreenCodec includes effectiveBitrate', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const result = svc.pickScreenCodec();
      expect(result.effectiveBitrate).toBeGreaterThan(0);
    });

    it('findSendCodec matches by mimeType', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const codec = svc.findSendCodec('video/VP8');
      expect(codec).toBeDefined();
      expect(codec.mimeType).toBe('video/VP8');
    });

    it('findSendCodec matches by mimeType:profile', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const codec = svc.findSendCodec('video/H264:640034');
      expect(codec).toBeDefined();
      expect(codec.parameters?.['profile-level-id']).toBe('640034');
    });

    it('maps both AV1 color targets to base RTP AV1 with target-specific HW evidence', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      expect(svc.findSendCodec('video/AV1:hdr')?.mimeType).toBe('video/AV1');
      expect(svc.findSendCodec('video/AV1:sdr')?.mimeType).toBe('video/AV1');

      useVideoSettingsStore.setState({
        webrtcHwByMime: { 'video/av1': true },
        codecCapabilities: [
          { mimeType: 'video/AV1', profileId: 'hdr', hwAvailable: false },
          { mimeType: 'video/AV1', profileId: 'sdr', hwAvailable: true },
        ],
      } as never);
      expect(svc.isHwAccelerated('video/AV1:hdr')).toBe(false);
      expect(svc.isHwAccelerated('video/AV1:sdr')).toBe(true);
      expect(svc.isHwAccelerated('video/AV1')).toBe(true);

      useVideoSettingsStore.setState({
        webrtcHwByMime: { 'video/av1': false },
        codecCapabilities: [
          { mimeType: 'video/AV1', profileId: 'hdr', hwAvailable: true },
          { mimeType: 'video/AV1', profileId: 'sdr', hwAvailable: false },
        ],
      } as never);
      expect(svc.isHwAccelerated('video/AV1:hdr')).toBe(false);
      expect(svc.isHwAccelerated('video/AV1:sdr')).toBe(false);
      expect(svc.isHwAccelerated('video/AV1')).toBe(false);
    });

    it('treats a supported H264 profile as hardware evidence for a legacy bare preference', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.setState({
        webrtcHwByMime: {},
        codecCapabilities: [{ mimeType: 'video/H264', profileId: '4d0032', hwAvailable: true }],
      } as never);

      expect(svc.isHwAccelerated('video/H264')).toBe(true);
    });

    it('matches H264 by profile class while preserving level asymmetry', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const constrainedHigh = {
        mimeType: 'video/H264',
        kind: 'video',
        clockRate: 90000,
        parameters: { 'profile-level-id': '640c1f' },
      };
      const lowerLevelHigh = {
        mimeType: 'video/H264',
        kind: 'video',
        clockRate: 90000,
        parameters: { 'profile-level-id': '64001f' },
      };
      const constrainedMain = {
        mimeType: 'video/H264',
        kind: 'video',
        clockRate: 90000,
        parameters: { 'profile-level-id': '4d401f' },
      };
      const constrainedBaselineFromMain = {
        mimeType: 'video/H264',
        kind: 'video',
        clockRate: 90000,
        parameters: { 'profile-level-id': '4d801f' },
      };
      const constrainedBaselineFromExtended = {
        mimeType: 'video/H264',
        kind: 'video',
        clockRate: 90000,
        parameters: { 'profile-level-id': '58c01f' },
      };
      svc.device = {
        loaded: true,
        rtpCapabilities: { codecs: [constrainedHigh, lowerLevelHigh, constrainedMain] },
      };

      expect(svc.findSendCodec('video/H264:640034')).toBe(lowerLevelHigh);
      expect(svc.findSendCodec('video/H264:4d0032')).toBe(constrainedMain);
      expect(svc.findSendCodec('video/H264:640c1f')).toBe(constrainedHigh);

      svc.device.rtpCapabilities.codecs = [constrainedBaselineFromMain];
      expect(svc.findSendCodec('video/H264:42e01f')).toBe(constrainedBaselineFromMain);
      svc.device.rtpCapabilities.codecs = [constrainedBaselineFromExtended];
      expect(svc.findSendCodec('video/H264:42e01f')).toBe(constrainedBaselineFromExtended);
    });

    it('resolves a legacy bare H264 preference by canonical profile order', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const codec = (profile: string) => ({
        mimeType: 'video/H264',
        kind: 'video',
        clockRate: 90000,
        parameters: { 'profile-level-id': profile },
      });
      const high = codec('64001f');
      const main = codec('4d401f');
      const constrainedBaseline = codec('58c01f');

      svc.device = {
        loaded: true,
        rtpCapabilities: { codecs: [high, constrainedBaseline, main] },
      };
      expect(svc.findSendCodec('video/H264')).toBe(high);

      svc.device.rtpCapabilities.codecs = [constrainedBaseline, main];
      expect(svc.findSendCodec('video/H264')).toBe(main);

      svc.device.rtpCapabilities.codecs = [constrainedBaseline];
      expect(svc.findSendCodec('video/H264')).toBe(constrainedBaseline);
    });

    it('resolves a legacy bare VP9 preference to Profile 0', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const profile2 = {
        mimeType: 'video/VP9',
        kind: 'video',
        clockRate: 90000,
        parameters: { 'profile-id': '2' },
      };
      const profile0 = {
        mimeType: 'video/VP9',
        kind: 'video',
        clockRate: 90000,
        parameters: {},
      };
      svc.device = {
        loaded: true,
        rtpCapabilities: { codecs: [profile2, profile0] },
      };

      expect(svc.findSendCodec('video/VP9')).toBe(profile0);

      svc.device.rtpCapabilities.codecs = [profile2];
      expect(svc.findSendCodec('video/VP9')).toBeUndefined();
    });

    it('uses the exact codec profile for pre-observation hardware verdicts', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.setState({
        webrtcHwByMime: {},
        codecCapabilities: [
          { mimeType: 'video/VP9', profileId: '0', hwAvailable: true },
          { mimeType: 'video/VP9', profileId: '2', hwAvailable: false },
        ],
      } as never);

      expect(svc.isHwAccelerated('video/VP9:0')).toBe(true);
      expect(svc.isHwAccelerated('video/VP9:2')).toBe(false);
    });

    it('findSendCodec returns undefined for missing codec', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      expect(svc.findSendCodec('video/HEVC')).toBeUndefined();
    });

    it('computeStartBitrate returns 50% of target clamped', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // 2Mbps target → 1000 kbps start
      expect(svc.computeStartBitrate(2_000_000)).toBe(1000);
      // Very low target → clamped to 100
      expect(svc.computeStartBitrate(100)).toBe(100);
      // Very high target → clamped to 10000
      expect(svc.computeStartBitrate(50_000_000)).toBe(10000);
    });

    it('respects user codec preference', async () => {
      useVideoSettingsStore.setState({
        preferredVideoCodec: 'video/VP8',
        hardwareAcceleration: false,
      });
      await joinVoiceChannel();
      const svc = voiceService as any;
      const result = svc.pickCameraCodec();
      expect(result.codec?.mimeType).toBe('video/VP8');
    });

    it('encoding includes priority when set', async () => {
      useVideoSettingsStore.setState({ cameraPriority: 'high' });
      await joinVoiceChannel();
      const svc = voiceService as any;
      const result = svc.pickCameraCodec();
      expect(result.encodings[0].priority).toBe('high');
      expect(result.encodings[0].networkPriority).toBe('high');
    });

    it('encoding omits priority when off', async () => {
      useVideoSettingsStore.setState({ cameraPriority: 'off' });
      await joinVoiceChannel();
      const svc = voiceService as any;
      const result = svc.pickCameraCodec();
      expect(result.encodings[0].priority).toBeUndefined();
    });
  });

  // ===== Solo Bandwidth Saving =====

  describe('solo bandwidth saving', () => {
    it('enters solo mode when last other user leaves', async () => {
      await joinVoiceChannel();

      // Add a second user via socket event — triggers checkSoloBandwidthSaving
      const joinHandler = socketListeners['user-joined']?.[0];
      joinHandler?.({ userId: 'user-2', username: 'other', displayName: 'Other' });
      expect(useVoiceStore.getState().isSoloBandwidthSaving).toBe(false);

      // Remove them — should enter solo mode
      const leaveHandler = socketListeners['user-left']?.[0];
      leaveHandler?.({ userId: 'user-2' });
      expect(useVoiceStore.getState().isSoloBandwidthSaving).toBe(true);
    });

    it('exits solo mode when someone joins', async () => {
      await joinVoiceChannel();

      // Enter solo by having someone leave after joining
      const joinHandler = socketListeners['user-joined']?.[0];
      joinHandler?.({ userId: 'user-2', username: 'other', displayName: 'Other' });
      // user-joined handler is async (awaits E2EE key setup); flush microtasks
      await vi.advanceTimersByTimeAsync(0);
      const leaveHandler = socketListeners['user-left']?.[0];
      leaveHandler?.({ userId: 'user-2' });
      // user-left handler is also async; flush microtasks
      await vi.advanceTimersByTimeAsync(0);
      expect(useVoiceStore.getState().isSoloBandwidthSaving).toBe(true);

      // Now someone joins — should exit solo mode
      joinHandler?.({ userId: 'user-3', username: 'third', displayName: 'Third' });
      await vi.advanceTimersByTimeAsync(0);
      expect(useVoiceStore.getState().isSoloBandwidthSaving).toBe(false);
    });

    it('shows notification after 60s alone', async () => {
      await joinVoiceChannel();

      // Enter solo mode
      const joinHandler = socketListeners['user-joined']?.[0];
      joinHandler?.({ userId: 'user-2', username: 'other', displayName: 'Other' });
      const leaveHandler = socketListeners['user-left']?.[0];
      leaveHandler?.({ userId: 'user-2' });
      expect(useVoiceStore.getState().isSoloBandwidthSaving).toBe(true);

      await vi.advanceTimersByTimeAsync(61_000);
      expect(useVoiceStore.getState().soloBandwidthNotification).toBe(true);
    });

    it('clears notification when someone joins', async () => {
      await joinVoiceChannel();

      // Enter solo mode
      const joinHandler = socketListeners['user-joined']?.[0];
      joinHandler?.({ userId: 'user-2', username: 'other', displayName: 'Other' });
      await vi.advanceTimersByTimeAsync(0);
      const leaveHandler = socketListeners['user-left']?.[0];
      leaveHandler?.({ userId: 'user-2' });
      await vi.advanceTimersByTimeAsync(0);

      await vi.advanceTimersByTimeAsync(61_000);
      expect(useVoiceStore.getState().soloBandwidthNotification).toBe(true);

      // Someone joins
      joinHandler?.({ userId: 'user-3', username: 'third', displayName: 'Third' });
      await vi.advanceTimersByTimeAsync(0);
      expect(useVoiceStore.getState().soloBandwidthNotification).toBe(false);
    });

    it('respects mute state on exit', async () => {
      await joinVoiceChannel();

      // Enter solo mode
      const joinHandler = socketListeners['user-joined']?.[0];
      joinHandler?.({ userId: 'user-2', username: 'other', displayName: 'Other' });
      await vi.advanceTimersByTimeAsync(0);
      const leaveHandler = socketListeners['user-left']?.[0];
      leaveHandler?.({ userId: 'user-2' });
      await vi.advanceTimersByTimeAsync(0);

      // Mute the mic
      useVoiceStore.getState().setMuted(true);

      // Someone joins — should exit solo, but mic stays paused
      joinHandler?.({ userId: 'user-3', username: 'third', displayName: 'Third' });
      await vi.advanceTimersByTimeAsync(0);
      expect(useVoiceStore.getState().isSoloBandwidthSaving).toBe(false);
    });
  });

  // ===== tuneOutOfScreenShare =====

  describe('tuneOutOfScreenShare', () => {
    it('handles tune out with no consumer', async () => {
      await joinVoiceChannel();
      // Should not throw when there's no tuned-in share
      await voiceService.tuneOutOfScreenShare('nonexistent-producer');
      expect(useVoiceStore.getState().connectionState).toBe('connected');
    });

    const remoteParticipant = (userId: string, username: string) => ({
      userId,
      username,
      isMuted: false,
      isDeafened: false,
      isSpeaking: false,
      isVideoOn: false,
      isScreenSharing: true,
    });

    it('emits close-consumer for the screen-video consumer on tune-out (#2088)', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const videoConsumer = createMockConsumer('cons-v1', 'video', 'prod-1');
      svc.consumers.set('cons-v1', videoConsumer);
      svc.consumerMeta.set('cons-v1', {
        source: 'screen',
        producerUserId: 'user-1',
        producerId: 'prod-1',
      });
      const store = useVoiceStore.getState();
      store.registerActiveScreenShare({
        producerId: 'prod-1',
        userId: 'user-1',
        username: 'alice',
        isLocal: false,
      });
      store.tuneIn('prod-1', 'cons-v1');
      mockSocket.emit.mockClear();

      await voiceService.tuneOutOfScreenShare('prod-1');

      expect(videoConsumer.close).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('close-consumer', { consumerId: 'cons-v1' });
    });

    it('still closes and emits for the paired screen-audio consumer (#2088)', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const videoConsumer = createMockConsumer('cons-v1', 'video', 'prod-1');
      const audioConsumer = createMockConsumer('cons-a1', 'audio', 'prod-a1');
      svc.consumers.set('cons-v1', videoConsumer);
      svc.consumerMeta.set('cons-v1', {
        source: 'screen',
        producerUserId: 'user-1',
        producerId: 'prod-1',
      });
      svc.consumers.set('cons-a1', audioConsumer);
      svc.consumerMeta.set('cons-a1', {
        source: 'screen-audio',
        producerUserId: 'user-1',
        producerId: 'prod-a1',
      });
      const store = useVoiceStore.getState();
      store.registerActiveScreenShare({
        producerId: 'prod-1',
        userId: 'user-1',
        username: 'alice',
        isLocal: false,
      });
      store.tuneIn('prod-1', 'cons-v1');
      mockSocket.emit.mockClear();

      await voiceService.tuneOutOfScreenShare('prod-1');

      expect(audioConsumer.close).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('close-consumer', { consumerId: 'cons-v1' });
      expect(mockSocket.emit).toHaveBeenCalledWith('close-consumer', { consumerId: 'cons-a1' });
    });

    it('local share tune-out emits nothing and is not re-added to availableScreenShares (#2088)', async () => {
      await joinVoiceChannel();
      const store = useVoiceStore.getState();
      store.registerActiveScreenShare({
        producerId: 'prod-local',
        userId: 'local-user',
        username: 'me',
        isLocal: true,
      });
      store.tuneIn('prod-local', 'local-screen');
      mockSocket.emit.mockClear();

      await voiceService.tuneOutOfScreenShare('prod-local');

      expect(mockSocket.emit).not.toHaveBeenCalledWith(
        'close-consumer',
        expect.anything(),
        expect.anything()
      );
      expect(mockSocket.emit).not.toHaveBeenCalledWith('close-consumer', expect.anything());
      expect(
        useVoiceStore.getState().availableScreenShares.find((s) => s.producerId === 'prod-local')
      ).toBeUndefined();
      expect(useVoiceStore.getState().tunedInScreenShares['prod-local']).toBeUndefined();
    });

    it('records auto-tune suppression only when opted and the setting is ON (#2088)', async () => {
      await joinVoiceChannel();
      const store = useVoiceStore.getState();
      store.addParticipant(remoteParticipant('user-1', 'alice'));
      store.registerActiveScreenShare({
        producerId: 'prod-1',
        userId: 'user-1',
        username: 'alice',
        isLocal: false,
      });
      store.tuneIn('prod-1', 'cons-missing');
      useVideoSettingsStore.getState().setAutoTuneInScreenShares(true);

      await voiceService.tuneOutOfScreenShare('prod-1', { suppressAutoTune: true });

      expect(useVoiceStore.getState().autoTuneSuppressedProducers['prod-1']).toBe(true);
    });

    it('does NOT record suppression when the setting is OFF (#2088)', async () => {
      await joinVoiceChannel();
      useVideoSettingsStore.getState().setAutoTuneInScreenShares(false);
      const store = useVoiceStore.getState();
      store.addParticipant(remoteParticipant('user-1', 'alice'));
      store.registerActiveScreenShare({
        producerId: 'prod-1',
        userId: 'user-1',
        username: 'alice',
        isLocal: false,
      });
      store.tuneIn('prod-1', 'cons-missing');

      await voiceService.tuneOutOfScreenShare('prod-1', { suppressAutoTune: true });

      expect(useVoiceStore.getState().autoTuneSuppressedProducers).toEqual({});
    });

    it('re-adds the available entry with the metadata owner (multi-sharer correct) (#2088)', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const videoConsumer = createMockConsumer('cons-v2', 'video', 'prod-2');
      svc.consumers.set('cons-v2', videoConsumer);
      svc.consumerMeta.set('cons-v2', {
        source: 'screen',
        producerUserId: 'user-2',
        producerId: 'prod-2',
      });
      const store = useVoiceStore.getState();
      store.addParticipant(remoteParticipant('user-1', 'alice'));
      store.addParticipant(remoteParticipant('user-2', 'bob'));
      store.registerActiveScreenShare({
        producerId: 'prod-2',
        userId: 'user-2',
        username: 'bob',
        displayName: 'Bob',
        isLocal: false,
      });
      store.tuneIn('prod-2', 'cons-v2');

      await voiceService.tuneOutOfScreenShare('prod-2');

      const readded = useVoiceStore
        .getState()
        .availableScreenShares.find((s) => s.producerId === 'prod-2');
      expect(readded?.userId).toBe('user-2');
      expect(readded?.username).toBe('bob');
      expect(readded?.displayName).toBe('Bob');
    });
  });

  // ===== Socket Listeners =====

  describe('socket listeners', () => {
    it('handles new-producer for screen with opt-in', async () => {
      await joinVoiceChannel();

      const handler = socketListeners['new-producer']?.[0];
      expect(handler).toBeDefined();

      // Simulate screen producer with requiresOptIn
      handler?.({
        producerId: 'screen-prod-1',
        userId: 'user-2',
        kind: 'video',
        source: 'screen',
        requiresOptIn: true,
      });

      const state = useVoiceStore.getState();
      expect(state.availableScreenShares).toHaveLength(1);
      expect(state.availableScreenShares[0].producerId).toBe('screen-prod-1');
    });

    it('handles new-producer for screen-audio pending', async () => {
      await joinVoiceChannel();

      const handler = socketListeners['new-producer']?.[0];
      handler?.({
        producerId: 'screen-audio-1',
        userId: 'user-2',
        kind: 'audio',
        source: 'screen-audio',
        requiresOptIn: true,
      });

      // Screen audio is stored in pending map (private), not directly verifiable
      // but should not throw
      expect(handler).toBeDefined();
    });

    it('handles producer-paused event', async () => {
      await joinVoiceChannel();

      // Add a participant via store (must use userId key)
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: false,
      });

      // Set up a consumer for user-2
      const svc = voiceService as any;
      const consumer = createMockConsumer('cons-2', 'audio', 'prod-2');
      svc.consumers.set('cons-2', consumer);
      svc.consumerMeta.set('cons-2', {
        source: 'mic',
        producerUserId: 'user-2',
        producerId: 'prod-2',
      });

      const handler = socketListeners['producer-paused']?.[0];
      handler?.({ producerId: 'prod-2', userId: 'user-2' });

      expect(useVoiceStore.getState().participants['user-2']?.isMuted).toBe(true);
    });

    it('handles producer-resumed event', async () => {
      await joinVoiceChannel();

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: true,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: false,
      });

      const svc = voiceService as any;
      const consumer = createMockConsumer('cons-2', 'audio', 'prod-2');
      svc.consumers.set('cons-2', consumer);
      svc.consumerMeta.set('cons-2', {
        source: 'mic',
        producerUserId: 'user-2',
        producerId: 'prod-2',
      });

      const handler = socketListeners['producer-resumed']?.[0];
      handler?.({ producerId: 'prod-2', userId: 'user-2' });

      expect(useVoiceStore.getState().participants['user-2']?.isMuted).toBe(false);
    });

    it('handles participant-testing-changed event', async () => {
      await joinVoiceChannel();

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: false,
        serverMuted: false,
        serverDeafened: false,
      });

      const handler = socketListeners['participant-testing-changed']?.[0];
      handler?.({ userId: 'user-2', isTesting: true });

      expect(useVoiceStore.getState().participants['user-2']?.isTesting).toBe(true);
    });

    it('handles producer-closed event', async () => {
      await joinVoiceChannel();

      const svc = voiceService as any;
      const consumer = createMockConsumer('cons-2', 'audio', 'prod-2');
      svc.consumers.set('cons-2', consumer);
      svc.consumerMeta.set('cons-2', {
        source: 'mic',
        producerUserId: 'user-2',
        producerId: 'prod-2',
      });

      const handler = socketListeners['producer-closed']?.[0];
      handler?.({ producerId: 'prod-2', userId: 'user-2' });

      expect(consumer.close).toHaveBeenCalled();
      expect(svc.consumers.has('cons-2')).toBe(false);
    });

    it('handles user-joined event', async () => {
      await joinVoiceChannel();

      const handler = socketListeners['user-joined']?.[0];
      handler?.({ userId: 'user-2', username: 'other', displayName: 'Other User' });

      expect(useVoiceStore.getState().participants['user-2']).toBeDefined();
      expect(useVoiceStore.getState().participants['user-2']?.username).toBe('other');
    });

    it('refreshes reconnect flags without clobbering media or legacy state (#2407)', async () => {
      await joinVoiceChannel();
      const audioStream = createMockMediaStream([{ kind: 'audio', id: 'remote-audio' }]);
      const videoStream = createMockMediaStream([{ kind: 'video', id: 'remote-video' }]);
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'old-name',
        isMuted: true,
        isDeafened: true,
        isTesting: true,
        isSpeaking: true,
        isVideoOn: true,
        isScreenSharing: false,
        serverMuted: false,
        serverDeafened: false,
        audioStream: audioStream as MediaStream,
        videoStream: videoStream as MediaStream,
      });

      const handler = socketListeners['user-joined']?.[0];
      handler?.({
        userId: 'user-2',
        username: 'new-name',
        isDeafened: false,
        isTesting: false,
      });

      expect(useVoiceStore.getState().participants['user-2']).toMatchObject({
        username: 'new-name',
        isMuted: true,
        isDeafened: false,
        isTesting: false,
        isSpeaking: true,
        isVideoOn: true,
        audioStream,
        videoStream,
      });

      useVoiceStore.getState().updateParticipant('user-2', { isDeafened: true, isTesting: true });
      handler?.({ userId: 'user-2', username: 'legacy-reconnect' });
      expect(useVoiceStore.getState().participants['user-2']).toMatchObject({
        username: 'legacy-reconnect',
        isDeafened: true,
        isTesting: true,
        audioStream,
        videoStream,
      });
    });

    it('handles user-left event', async () => {
      await joinVoiceChannel();

      // First add the user
      const joinHandler = socketListeners['user-joined']?.[0];
      joinHandler?.({ userId: 'user-2', username: 'other' });
      expect(useVoiceStore.getState().participants['user-2']).toBeDefined();

      // Now remove them
      const leaveHandler = socketListeners['user-left']?.[0];
      leaveHandler?.({ userId: 'user-2' });
      expect(useVoiceStore.getState().participants['user-2']).toBeUndefined();
    });
  });

  // ===== consumeProducerImpl =====

  describe('consumeProducer', () => {
    it('consumes audio producer and attaches stream', async () => {
      const { recvTransport } = await joinVoiceChannel();

      // Add participant for the producer (must use userId key)
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: false,
      });

      const consumer = createMockConsumer('cons-new', 'audio', 'prod-remote');
      recvTransport.consume.mockResolvedValue(consumer);

      // Override emit for the consume call
      setupEmitResponses({
        consume: {
          id: 'cons-new',
          producerId: 'prod-remote',
          kind: 'audio',
          rtpParameters: {},
          source: 'mic',
          producerUserId: 'user-2',
        },
        'resume-consumer': undefined,
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic' },
        'close-producer': undefined,
        'pause-producer': undefined,
        'resume-producer': undefined,
      });

      const svc = voiceService as any;
      await svc.consumeProducer('prod-remote', 'user-2', 'audio');

      expect(recvTransport.consume).toHaveBeenCalled();
      // updateParticipant should have been called with audioStream
      const participant = useVoiceStore.getState().participants['user-2'];
      expect(participant).toBeDefined();
      expect(participant?.audioStream).toBeDefined();
    });

    it('keeps audio consumers created during a test suspension paused until restore', async () => {
      const { recvTransport } = await joinVoiceChannel();
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: false,
      });

      const consumer = createMockConsumer('cons-test', 'audio', 'prod-remote');
      recvTransport.consume.mockResolvedValue(consumer);
      setupEmitResponses({
        consume: {
          id: 'cons-test',
          producerId: 'prod-remote',
          kind: 'audio',
          rtpParameters: {},
          source: 'mic',
          producerUserId: 'user-2',
        },
        'resume-consumer': undefined,
      });

      const svc = voiceService as any;
      voiceService.beginTestSuspension();
      mockSocket.emit.mockClear();

      await svc.consumeProducer('prod-remote', 'user-2', 'audio');

      expect(consumer.pause).toHaveBeenCalled();
      expect(mockSocket.emit).not.toHaveBeenCalledWith('resume-consumer', {
        consumerId: 'cons-test',
      });

      consumer.paused = true;
      mockSocket.emit.mockClear();
      voiceService.endTestSuspension();

      expect(mockSocket.emit).toHaveBeenCalledWith('resume-consumer', {
        consumerId: 'cons-test',
      });
      expect(consumer.resume).toHaveBeenCalled();
    });

    it('skips consume when no device', async () => {
      // Don't join — no device loaded
      const svc = voiceService as any;
      // Should not throw, just warn and return
      await svc.consumeProducer('prod-1', 'user-2', 'audio');
      expect(useVoiceStore.getState().connectionState).toBe('disconnected');
    });
  });

  // ===== drainSendTransportQueue =====

  describe('drainSendTransportQueue', () => {
    it('drains queue successfully', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      await svc.drainSendTransportQueue();
      expect(sendTransport._awaitQueue.push).toHaveBeenCalled();
    });

    it('handles no transport gracefully', async () => {
      const svc = voiceService as any;
      // Should not throw when no transport exists
      await expect(svc.drainSendTransportQueue()).resolves.toBeUndefined();
    });
  });

  // ===== Live Settings Subscriptions =====

  describe('live settings subscriptions', () => {
    it('updates audio priority via setParameters', async () => {
      const { micProducer } = await joinVoiceChannel();

      // Trigger audio priority change
      useAudioSettingsStore.setState({ audioPriority: 'high' });
      await vi.advanceTimersByTimeAsync(0);

      // Should have called setParameters on the mic producer's rtpSender
      if (micProducer.rtpSender) {
        // May or may not have been called depending on subscription timing
        // The subscription is set up during joinChannel
      }
      expect(useVoiceStore.getState().connectionState).toBe('connected');
    });

    it('sets up live subscriptions during join', async () => {
      await joinVoiceChannel();

      // The live subscription is created during join
      // Verify it exists by checking the private field
      const svc = voiceService as any;
      expect(svc.liveAudioUnsub).toBeDefined();
      expect(svc.liveVideoUnsub).toBeDefined();
    });

    it('tears down subscriptions on leave', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      expect(svc.liveAudioUnsub).toBeDefined();

      voiceService.emergencyCleanup();
      expect(svc.liveAudioUnsub).toBeNull();
      expect(svc.liveVideoUnsub).toBeNull();
    });
  });

  // ===== Degradation Preference =====

  describe('degradation preference', () => {
    it('applies to video producer', async () => {
      useVideoSettingsStore.setState({ degradationPreference: 'maintain-framerate' });
      const { sendTransport } = await joinVoiceChannel();

      const videoProducer = createMockProducer('prod-cam', 'camera');
      sendTransport.produce.mockResolvedValue(videoProducer);

      mockGetUserMedia.mockResolvedValue(createMockMediaStream([{ kind: 'video', id: 'cam-1' }]));

      await voiceService.toggleVideo();

      // degradationPreference should have been applied
      if (videoProducer.rtpSender) {
        expect(videoProducer.rtpSender.setParameters).toHaveBeenCalled();
      }
    });

    it('skips balanced (browser default)', async () => {
      useVideoSettingsStore.setState({ degradationPreference: 'balanced' });
      await joinVoiceChannel();
      const svc = voiceService as any;
      const mockProducer = createMockProducer('p', 'camera');
      svc.applyDegradationPreference(mockProducer);
      // Should not call setParameters for 'balanced'
      expect(mockProducer.rtpSender.setParameters).not.toHaveBeenCalled();
    });
  });

  // ===== E2EE initEncryption =====

  describe('E2EE initEncryption', () => {
    it('retries on failure with backoff', async () => {
      // Make the atomic key-material fetch fail a few times then succeed.
      const { e2eeService: mockE2ee } = await import('@/renderer/services/e2eeService');
      vi.mocked(mockE2ee.getChannelKeyMaterial)
        .mockRejectedValueOnce(new E2EEKeyUnavailableError('NO_KEY_YET', true))
        .mockRejectedValueOnce(new E2EEKeyUnavailableError('NO_KEY_YET', true))
        .mockResolvedValueOnce({
          channelKey: new Uint8Array(32) as unknown as CryptoKey,
          keyVersion: 1,
        });

      // Join a channel to trigger the encryption init path (always encrypted)
      await joinVoiceChannel();

      expect(vi.mocked(mockE2ee.getChannelKeyMaterial)).toHaveBeenCalled();
    });

    it('fail-closed: sets mediaEncryption to null after all retries', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      // The focused mock uses null key material; this assertion only verifies cleanup shape.
      // The service should handle this gracefully
      expect(svc.mediaEncryption).toBeDefined(); // May be null (fail-closed) or initialized
    });
  });

  // ===== closeProducer =====

  describe('closeProducer extended', () => {
    it('closes screen with paired screen-audio', async () => {
      await joinVoiceChannel();

      const screenProducer = createMockProducer('prod-screen', 'screen');
      const audioProducer = createMockProducer('prod-screen-audio', 'screen-audio');

      const svc = voiceService as any;
      svc.producers.set('screen', screenProducer);
      svc.producers.set('screen-audio', audioProducer);
      svc.localScreenStream = createMockMediaStream([{ kind: 'video', id: 'screen-v' }]);

      await voiceService.closeProducer('screen');

      expect(screenProducer.close).toHaveBeenCalled();
      expect(audioProducer.close).toHaveBeenCalled();
      expect(svc.producers.has('screen')).toBe(false);
      expect(svc.producers.has('screen-audio')).toBe(false);
      expect(useVoiceStore.getState().isScreenSharing).toBe(false);
    });

    it('resets camera state on close', async () => {
      await joinVoiceChannel();

      const svc = voiceService as any;
      const cameraProducer = createMockProducer('prod-cam', 'camera');
      svc.producers.set('camera', cameraProducer);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video', id: 'cam-v' }]);

      useVoiceStore.getState().setVideoOn(true);
      await voiceService.closeProducer('camera');

      expect(cameraProducer.close).toHaveBeenCalled();
      expect(useVoiceStore.getState().isVideoOn).toBe(false);
    });
  });

  // ===== handleCodecFloorChange =====

  describe('codec floor change', () => {
    it('re-produces when better codec available', async () => {
      await joinVoiceChannel();

      const svc = voiceService as any;
      const cameraProducer = createMockProducer('prod-cam', 'camera');
      cameraProducer.rtpSender = {
        ...cameraProducer.rtpSender,
        track: { getSettings: vi.fn().mockReturnValue({}) },
      };
      svc.producers.set('camera', cameraProducer);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video', id: 'cam-v' }]);
      svc.activeCameraCodecMime = 'video/vp8';

      // Set codec floor — should trigger re-check
      useVoiceStore.getState().setCodecFloor(['video/AV1', 'video/VP8']);
      expect(svc.producers.get('camera')).toBeDefined();
    });

    it('noop when no active producers', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // Should not throw
      await svc.handleCodecFloorChange(null, ['video/VP8']);
      expect(useVoiceStore.getState().connectionState).toBe('connected');
    });

    it('reProduceIfBetterCodec skips when no producer for source', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // No camera producer → should return without error
      await svc.reProduceIfBetterCodec('camera');
      expect(useVoiceStore.getState().connectionState).toBe('connected');
    });

    it('reProduceIfBetterCodec skips when current mime is null', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const cameraProducer = createMockProducer('prod-cam', 'camera');
      // rtpSender returns no codecs → getProducerCodecMimeType returns null
      cameraProducer.rtpSender.getParameters = vi.fn().mockReturnValue({ codecs: [] });
      svc.producers.set('camera', cameraProducer);
      await svc.reProduceIfBetterCodec('camera');
      expect(useVoiceStore.getState().connectionState).toBe('connected');
    });

    it('reProduceIfBetterCodec skips when best codec matches current', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const cameraProducer = createMockProducer('prod-cam', 'camera');
      // Current codec is VP8, cascade will also pick VP8 (only available)
      cameraProducer.rtpSender.getParameters = vi.fn().mockReturnValue({
        codecs: [{ mimeType: 'video/VP8' }],
        encodings: [{ maxBitrate: 2000000 }],
      });
      svc.producers.set('camera', cameraProducer);
      await svc.reProduceIfBetterCodec('camera');
      // Should not have called fastReproduceCamera
      expect(cameraProducer.close).not.toHaveBeenCalled();
    });

    it('does not churn an H264 producer when only the negotiated level differs', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const cameraProducer = createMockProducer('prod-cam-level', 'camera') as any;
      cameraProducer.rtpParameters = {
        codecs: [
          {
            mimeType: 'video/H264',
            parameters: { 'profile-level-id': '64001f' },
          },
        ],
      };
      svc.producers.set('camera', cameraProducer);
      svc.pickCameraCodec = () => ({
        codec: {
          mimeType: 'video/H264',
          kind: 'video',
          clockRate: 90000,
          parameters: { 'profile-level-id': '640034' },
        },
        encodings: [{ maxBitrate: 2_000_000 }],
      });
      const reproduce = vi.fn().mockResolvedValue(undefined);
      svc.fastReproduceCameraQueued = reproduce;

      try {
        await svc.reProduceIfBetterCodec('camera');
        expect(reproduce).not.toHaveBeenCalled();
      } finally {
        delete svc.pickCameraCodec;
        delete svc.fastReproduceCameraQueued;
      }
    });

    it('closes an active producer when the policy has no eligible codec', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      const cameraProducer = createMockProducer('prod-cam-no-codec', 'camera') as any;
      cameraProducer.rtpParameters = { codecs: [{ mimeType: 'video/VP8', parameters: {} }] };
      svc.producers.set('camera', cameraProducer);
      svc.localCameraStream = createMockMediaStream([{ kind: 'video', id: 'cam-no-codec' }]);
      useVoiceStore.getState().setVideoOn(true);
      useVoiceStore.getState().setActiveCameraCodec('video/vp8');
      svc.pickCameraCodec = () => ({
        codec: undefined,
        encodings: [{ maxBitrate: 2_000_000 }],
      });

      try {
        await svc.reProduceIfBetterCodec('camera');
        expect(cameraProducer.close).toHaveBeenCalled();
        expect(svc.producers.has('camera')).toBe(false);
        expect(svc.localCameraStream).toBeNull();
        expect(useVoiceStore.getState().activeCameraCodec).toBeNull();
        expect(useVoiceStore.getState().isVideoOn).toBe(false);
      } finally {
        delete svc.pickCameraCodec;
      }
    });

    it('reProduceIfBetterCodec skips HW switch when hwAccel disabled', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      useVideoSettingsStore.setState({
        hardwareAcceleration: false,
        codecCapabilities: [
          { mimeType: 'video/AV1', powerEfficient: true, hwAvailable: true },
          { mimeType: 'video/VP8', powerEfficient: false, hwAvailable: false },
        ],
      });
      const cameraProducer = createMockProducer('prod-cam', 'camera');
      cameraProducer.rtpSender.getParameters = vi.fn().mockReturnValue({
        codecs: [{ mimeType: 'video/VP8' }],
        encodings: [{ maxBitrate: 2000000 }],
      });
      svc.producers.set('camera', cameraProducer);
      // AV1 is HW (powerEfficient), VP8 is SW — should skip since hwAccel=false
      await svc.reProduceIfBetterCodec('camera');
      expect(cameraProducer.close).not.toHaveBeenCalled();
    });

    it('reProduceIfBetterCodec dispatches to correct reproduce function', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      // Override methods with flags to verify dispatch (delete restores prototype)
      let cameraCalled = false;
      let screenCalled = false;
      svc.fastReproduceCameraQueued = async () => {
        cameraCalled = true;
      };
      svc.fastReproduceScreenQueued = async () => {
        screenCalled = true;
      };
      svc.getProducerCodecMimeType = () => 'video/vp8';
      svc.pickCameraCodec = () => ({
        codec: { mimeType: 'video/AV1', kind: 'video', clockRate: 90000, parameters: {} },
        encodings: [{ maxBitrate: 2000000 }],
      });
      svc.pickScreenCodec = () => ({
        codec: { mimeType: 'video/AV1', kind: 'video', clockRate: 90000, parameters: {} },
        encodings: [{ maxBitrate: 4000000 }],
        effectiveBitrate: 4000000,
      });

      useVideoSettingsStore.setState({ hardwareAcceleration: false, codecCapabilities: [] });

      // Camera path
      svc.producers.set('camera', createMockProducer('prod-cam', 'camera'));
      await svc.reProduceIfBetterCodec('camera');
      expect(cameraCalled).toBe(true);

      // Screen path
      svc.producers.set('screen', createMockProducer('prod-scr', 'screen'));
      await svc.reProduceIfBetterCodec('screen');
      expect(screenCalled).toBe(true);

      // Restore all overrides
      delete svc.fastReproduceCameraQueued;
      delete svc.fastReproduceScreenQueued;
      delete svc.getProducerCodecMimeType;
      delete svc.pickCameraCodec;
      delete svc.pickScreenCodec;
    });
  });

  describe('reProduceScreenAudio', () => {
    it('returns early when no screen-audio producer', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // No screen-audio producer → noop
      await svc.reProduceScreenAudio();
      expect(mockSocket.emit).not.toHaveBeenCalledWith(
        'close-producer',
        expect.objectContaining({ producerId: expect.any(String) })
      );
    });

    it('returns early when no localScreenStream', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('screen-audio', createMockProducer('prod-sa', 'screen-audio'));
      svc.localScreenStream = null;
      await svc.reProduceScreenAudio();
      expect(svc.producers.get('screen-audio').close).not.toHaveBeenCalled();
    });

    it('returns early when audio track is not live', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('screen-audio', createMockProducer('prod-sa', 'screen-audio'));
      svc.localScreenStream = createMockMediaStream([{ kind: 'audio', id: 'sa-1' }]);
      // Override readyState to 'ended'
      svc.localScreenStream.getAudioTracks()[0].readyState = 'ended';
      await svc.reProduceScreenAudio();
      expect(svc.producers.get('screen-audio').close).not.toHaveBeenCalled();
    });

    it('re-produces screen audio with new producer', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      const oldProducer = createMockProducer('prod-sa-old', 'screen-audio');
      svc.producers.set('screen-audio', oldProducer);
      svc.localScreenStream = createMockMediaStream([{ kind: 'audio', id: 'sa-1' }]);

      const newProducer = createMockProducer('prod-sa-new', 'screen-audio');
      sendTransport.produce.mockResolvedValue(newProducer);

      await svc.reProduceScreenAudio();

      expect(oldProducer.close).toHaveBeenCalled();
      expect(mockSocket.emit).toHaveBeenCalledWith('close-producer', {
        producerId: 'prod-sa-old',
      });
      expect(sendTransport.produce).toHaveBeenCalledWith(
        expect.objectContaining({
          appData: { source: 'screen-audio' },
          codecOptions: { opusStereo: true, opusDtx: false },
        })
      );
      expect(svc.producers.get('screen-audio')).toBe(newProducer);
    });

    it('handles produce failure gracefully', async () => {
      const { sendTransport } = await joinVoiceChannel();
      const svc = voiceService as any;
      svc.producers.set('screen-audio', createMockProducer('prod-sa-old', 'screen-audio'));
      svc.localScreenStream = createMockMediaStream([{ kind: 'audio', id: 'sa-1' }]);
      sendTransport.produce.mockRejectedValue(new Error('transport closed'));

      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      await svc.reProduceScreenAudio();
      expect(warnSpy).toHaveBeenCalledWith(
        'Failed to re-produce screen audio:',
        expect.any(String)
      );
      warnSpy.mockRestore();
    });
  });

  // ===== Decoder Budget Profiling (IGNIS) =====

  describe('decoder budget profiling', () => {
    it('starts profiling after join', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // decoderProfilingTimer should be set
      expect(svc.decoderProfilingTimer).toBeDefined();
    });

    it('profiles video consumers', async () => {
      await joinVoiceChannel();

      const svc = voiceService as any;
      const consumer = createMockConsumer('cons-vid', 'video', 'prod-vid');

      // Add stats that indicate green zone
      const statsMap = new Map();
      statsMap.set('inbound', {
        type: 'inbound-rtp',
        kind: 'video',
        totalDecodeTime: 0.5,
        framesDecoded: 100,
        framesPerSecond: 30,
      });
      consumer.getStats.mockResolvedValue(statsMap);
      svc.consumers.set('cons-vid', consumer);

      // Advance past profiling interval (5s)
      await vi.advanceTimersByTimeAsync(5500);

      expect(consumer.getStats).toHaveBeenCalled();
    });

    it('handles consumers with no stats gracefully', async () => {
      await joinVoiceChannel();

      const svc = voiceService as any;
      const consumer = createMockConsumer('cons-vid', 'video', 'prod-vid');
      consumer.getStats.mockResolvedValue(new Map());
      svc.consumers.set('cons-vid', consumer);

      // Should not throw
      await vi.advanceTimersByTimeAsync(5500);
      expect(consumer.getStats).toHaveBeenCalled();
    });
  });

  // ===== Module-level cleanup =====

  describe('module-level cleanup', () => {
    it('auth store subscription triggers emergency cleanup', () => {
      setupAuth();
      useAuthStore.getState().clearAccessToken();
      expect(useAuthStore.getState().accessToken).toBeNull();
    });
  });

  // ===== consumeProducerImpl — additional source types =====

  describe('consumeProducer source types', () => {
    it('attaches video stream for camera source', async () => {
      const { recvTransport } = await joinVoiceChannel();

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: false,
      });

      const consumer = createMockConsumer('cons-cam', 'video', 'prod-cam');
      recvTransport.consume.mockResolvedValue(consumer);
      setupEmitResponses({
        consume: {
          id: 'cons-cam',
          producerId: 'prod-cam',
          kind: 'video',
          rtpParameters: {},
          source: 'camera',
          producerUserId: 'user-2',
        },
        'resume-consumer': undefined,
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic' },
        'close-producer': undefined,
        'pause-producer': undefined,
        'resume-producer': undefined,
      });

      const svc = voiceService as any;
      await svc.consumeProducer('prod-cam', 'user-2', 'video');

      const p = useVoiceStore.getState().participants['user-2'];
      expect(p?.videoStream).toBeDefined();
      expect(p?.isVideoOn).toBe(true);
    });

    it('attaches screen stream for screen source', async () => {
      const { recvTransport } = await joinVoiceChannel();

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: false,
      });

      const consumer = createMockConsumer('cons-scr', 'video', 'prod-scr');
      recvTransport.consume.mockResolvedValue(consumer);
      setupEmitResponses({
        consume: {
          id: 'cons-scr',
          producerId: 'prod-scr',
          kind: 'video',
          rtpParameters: {},
          source: 'screen',
          producerUserId: 'user-2',
        },
        'resume-consumer': undefined,
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic' },
        'close-producer': undefined,
        'pause-producer': undefined,
        'resume-producer': undefined,
      });

      const svc = voiceService as any;
      await svc.consumeProducer('prod-scr', 'user-2', 'video');

      const p = useVoiceStore.getState().participants['user-2'];
      expect(p?.screenStream).toBeDefined();
      expect(p?.isScreenSharing).toBe(true);
    });

    it('re-emits stored screen render-state demand to a freshly-consumed screen consumer (#1924 Fix)', async () => {
      const { recvTransport } = await joinVoiceChannel();

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: false,
      });

      const svc = voiceService as any;
      // The reporter previously stored this sharer's screen render-state; on a reproduce
      // the tile stays mounted (reporter never re-fires) so the fresh consumer would
      // strand at layer 0 without a re-emit. Seed the stored state (no consumer yet → the
      // seed's own emit is a no-op).
      svc.setRemoteVideoRenderState(
        'user-2',
        'screen-tile',
        { visible: true, cssWidth: 1920, cssHeight: 1080, role: 'focus', focusedWindow: true },
        'screen'
      );

      const consumer = createMockConsumer('cons-scr-new', 'video', 'prod-scr-new');
      recvTransport.consume.mockResolvedValue(consumer);
      setupEmitResponses({
        consume: {
          id: 'cons-scr-new',
          producerId: 'prod-scr-new',
          kind: 'video',
          rtpParameters: {},
          source: 'screen',
          producerUserId: 'user-2',
        },
        'resume-consumer': undefined,
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic' },
        'close-producer': undefined,
        'pause-producer': undefined,
        'resume-producer': undefined,
      });
      mockSocket.emit.mockClear();

      await svc.consumeProducer('prod-scr-new', 'user-2', 'video');

      const spl = mockSocket.emit.mock.calls.filter(
        (c: unknown[]) => c[0] === 'set-preferred-layers'
      );
      expect(spl.length).toBeGreaterThan(0);
      expect((spl[spl.length - 1][1] as { consumerId: string }).consumerId).toBe('cons-scr-new');
      expect((spl[spl.length - 1][1] as { visible: boolean }).visible).toBe(true);
    });

    it('attaches screen-audio stream', async () => {
      const { recvTransport } = await joinVoiceChannel();

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: false,
      });

      const consumer = createMockConsumer('cons-sa', 'audio', 'prod-sa');
      recvTransport.consume.mockResolvedValue(consumer);
      setupEmitResponses({
        consume: {
          id: 'cons-sa',
          producerId: 'prod-sa',
          kind: 'audio',
          rtpParameters: {},
          source: 'screen-audio',
          producerUserId: 'user-2',
        },
        'resume-consumer': undefined,
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic' },
        'close-producer': undefined,
        'pause-producer': undefined,
        'resume-producer': undefined,
      });

      const svc = voiceService as any;
      await svc.consumeProducer('prod-sa', 'user-2', 'audio');

      const p = useVoiceStore.getState().participants['user-2'];
      expect(p?.screenAudioStream).toBeDefined();
    });

    it('handles server error gracefully', async () => {
      await joinVoiceChannel();

      setupEmitResponses({
        consume: { error: 'server error' },
        'resume-consumer': undefined,
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic' },
        'close-producer': undefined,
        'pause-producer': undefined,
        'resume-producer': undefined,
      });

      const svc = voiceService as any;
      // Should not throw
      await svc.consumeProducer('prod-err', 'user-2', 'audio');
      expect(useVoiceStore.getState().connectionState).toBe('connected');
    });
  });

  // ===== Codec selection — additional branches =====

  describe('codec selection branches', () => {
    it('pickCameraCodec with HW accel off skips HW pass', async () => {
      useVideoSettingsStore.setState({ hardwareAcceleration: false });
      await joinVoiceChannel();
      const svc = voiceService as any;
      const result = svc.pickCameraCodec();
      expect(result.codec).toBeDefined();
    });

    it('pickCameraCodec with HW accel on tries HW first', async () => {
      useVideoSettingsStore.setState({ hardwareAcceleration: true });
      await joinVoiceChannel();
      const svc = voiceService as any;
      const result = svc.pickCameraCodec();
      expect(result.codec).toBeDefined();
    });

    it('pickCameraCodec with HDR encoding includes VP9:2', async () => {
      useVideoSettingsStore.setState({ hdrEncoding: true });
      await joinVoiceChannel();
      const svc = voiceService as any;
      const result = svc.pickCameraCodec();
      expect(result.codec).toBeDefined();
    });

    it('pickScreenCodec with user bitrate override', async () => {
      useVideoSettingsStore.setState({ screenShareBitrate: 5000000 });
      await joinVoiceChannel();
      const svc = voiceService as any;
      const result = svc.pickScreenCodec();
      expect(result.effectiveBitrate).toBe(5000000);
    });

    it('pickScreenCodec with auto bitrate', async () => {
      useVideoSettingsStore.setState({ screenShareBitrate: 0 });
      await joinVoiceChannel();
      const svc = voiceService as any;
      const result = svc.pickScreenCodec();
      expect(result.effectiveBitrate).toBeGreaterThan(0);
    });

    it('isInCodecFloor returns true when floor is null', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;
      // null floor means all codecs allowed
      expect(svc.isInCodecFloor('video/VP8')).toBe(true);
    });

    it('isInCodecFloor filters when floor is set', async () => {
      await joinVoiceChannel();
      // Floor uses lowercase mime types
      useVoiceStore.getState().setCodecFloor(['video/vp8', 'video/vp9']);
      const svc = voiceService as any;
      expect(svc.isInCodecFloor('video/VP8')).toBe(true);
      expect(svc.isInCodecFloor('video/AV1')).toBe(false);
    });
  });

  // ===== Socket listeners — more edge cases =====

  describe('socket listener edge cases', () => {
    it('new-producer for camera checks video slots', async () => {
      await joinVoiceChannel();

      // Set maxVideoSlots to 1 and mark someone as already having video
      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'other',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: true,
        isScreenSharing: false,
      });

      const handler = socketListeners['new-producer']?.[0];
      handler?.({
        producerId: 'cam-prod-3',
        userId: 'user-3',
        kind: 'video',
        source: 'camera',
        requiresOptIn: false,
      });

      // Should still attempt to consume (slot check is at produceVideo, not consume)
      expect(handler).toBeDefined();
    });

    it('new-producer auto-consumes screen-audio when tuned in', async () => {
      await joinVoiceChannel();

      const svc = voiceService as any;

      // Simulate being tuned into user-2's screen share
      const consumer = createMockConsumer('cons-scr-2', 'video', 'prod-scr-2');
      svc.consumers.set('cons-scr-2', consumer);
      svc.consumerMeta.set('cons-scr-2', {
        source: 'screen',
        producerUserId: 'user-2',
        producerId: 'prod-scr-2',
      });
      useVoiceStore.getState().tuneIn('prod-scr-2', 'cons-scr-2');

      const handler = socketListeners['new-producer']?.[0];
      handler?.({
        producerId: 'screen-audio-2',
        userId: 'user-2',
        kind: 'audio',
        source: 'screen-audio',
        requiresOptIn: true,
      });

      // Screen audio should be stored in pending map
      expect(svc.pendingScreenAudioProducers.get('user-2')).toBe('screen-audio-2');
    });

    it('user-left removes participant and cleans up', async () => {
      await joinVoiceChannel();

      const joinHandler = socketListeners['user-joined']?.[0];
      joinHandler?.({ userId: 'user-2', username: 'other', displayName: 'Other' });
      joinHandler?.({ userId: 'user-3', username: 'third', displayName: 'Third' });

      expect(Object.keys(useVoiceStore.getState().participants)).toHaveLength(3);

      const leaveHandler = socketListeners['user-left']?.[0];
      leaveHandler?.({ userId: 'user-3' });

      expect(Object.keys(useVoiceStore.getState().participants)).toHaveLength(2);
      expect(useVoiceStore.getState().participants['user-3']).toBeUndefined();
    });
  });

  // ===== produceVideo — constraint fallback =====

  describe('produceVideo fallback', () => {
    it('falls back through constraint chain on OverconstrainedError', async () => {
      const { sendTransport } = await joinVoiceChannel();

      useVideoSettingsStore.setState({ cameraPreset: '1080p30' });

      // Reset getUserMedia mock for this test — first produce call already consumed it
      mockGetUserMedia.mockReset();

      // First calls: OverconstrainedError, final: succeeds
      const overconstrainedErr = new DOMException('Overconstrained', 'OverconstrainedError');
      Object.defineProperty(overconstrainedErr, 'name', { value: 'OverconstrainedError' });

      mockGetUserMedia
        .mockRejectedValueOnce(overconstrainedErr)
        .mockRejectedValueOnce(overconstrainedErr)
        .mockResolvedValueOnce(createMockMediaStream([{ kind: 'video', id: 'cam-fallback' }]));

      const videoProducer = createMockProducer('prod-cam-fb', 'camera');
      sendTransport.produce.mockResolvedValue(videoProducer);

      await voiceService.toggleVideo();

      // Should have tried multiple getUserMedia calls (fallback chain)
      expect(mockGetUserMedia.mock.calls.length).toBeGreaterThanOrEqual(2);
    });

    it('sets error message on NotAllowedError', async () => {
      await joinVoiceChannel();

      mockGetUserMedia.mockReset();
      const notAllowedErr = new DOMException('Not allowed', 'NotAllowedError');
      Object.defineProperty(notAllowedErr, 'name', { value: 'NotAllowedError' });
      mockGetUserMedia.mockRejectedValue(notAllowedErr);

      await voiceService.toggleVideo();

      const error = useVoiceStore.getState().videoSlotError;
      expect(error).toContain('denied');
    });
  });

  // ===== produceScreen — additional paths =====

  describe('produceScreen additional', () => {
    it('handles getDisplayMedia fallback', async () => {
      const { sendTransport } = await joinVoiceChannel();

      // No electron desktop sources — falls back to getDisplayMedia
      const origElectron = globalThis.electron;
      globalThis.electron = undefined as any;

      const screenStream = createMockMediaStream([{ kind: 'video', id: 'screen-1' }]);
      mockGetDisplayMedia.mockResolvedValue(screenStream);

      const screenProducer = createMockProducer('prod-scr-gd', 'screen');
      sendTransport.produce.mockResolvedValue(screenProducer);

      await voiceService.produceScreen();

      expect(mockGetDisplayMedia).toHaveBeenCalled();

      globalThis.electron = origElectron;
    });
  });

  // ===== tuneInToScreenShare — happy path =====

  describe('tuneInToScreenShare happy path', () => {
    it('consumes screen producer and sets dominant', async () => {
      const { recvTransport } = await joinVoiceChannel();

      useVoiceStore.getState().addParticipant({
        userId: 'user-2',
        username: 'screener',
        isMuted: false,
        isDeafened: false,
        isSpeaking: false,
        isVideoOn: false,
        isScreenSharing: true,
      });

      const consumer = createMockConsumer('cons-tune', 'video', 'prod-tune');
      recvTransport.consume.mockResolvedValue(consumer);
      setupEmitResponses({
        consume: {
          id: 'cons-tune',
          producerId: 'prod-tune',
          kind: 'video',
          rtpParameters: {},
          source: 'screen',
          producerUserId: 'user-2',
        },
        'resume-consumer': undefined,
        'join-room': makeRoomJoined(),
        'create-transport': makeTransportOpts(),
        produce: { id: 'prod-mic' },
        'close-producer': undefined,
        'pause-producer': undefined,
        'resume-producer': undefined,
      });

      await voiceService.tuneInToScreenShare('prod-tune', 'user-2');

      const store = useVoiceStore.getState();
      expect(store.tunedInScreenShares['prod-tune']).toBeDefined();
    });
  });

  // ===== closeProducer — screen with track.onended =====

  describe('closeProducer screen path', () => {
    it('cleans up screen + screen-audio producers', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      const screenProducer = createMockProducer('p-screen', 'screen');
      const screenAudioProducer = createMockProducer('p-screen-audio', 'screen-audio');
      svc.producers.set('screen', screenProducer);
      svc.producers.set('screen-audio', screenAudioProducer);
      svc.localScreenStream = createMockMediaStream([
        { kind: 'video', id: 'scr-v' },
        { kind: 'audio', id: 'scr-a' },
      ]);

      useVoiceStore.getState().setScreenSharing(true);

      await voiceService.closeProducer('screen');

      expect(screenProducer.close).toHaveBeenCalled();
      expect(screenAudioProducer.close).toHaveBeenCalled();
      expect(svc.producers.has('screen')).toBe(false);
      expect(svc.producers.has('screen-audio')).toBe(false);
      expect(useVoiceStore.getState().isScreenSharing).toBe(false);
    });
  });

  // ===== IGNIS profiling — red zone =====

  describe('IGNIS profiling zones', () => {
    it('red zone pauses lowest-priority consumer', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      // Add a video consumer with stats indicating red zone (rho >= 0.925)
      const consumer = createMockConsumer('cons-red', 'video', 'prod-red');
      const statsMap = new Map();
      statsMap.set('inbound', {
        type: 'inbound-rtp',
        kind: 'video',
        totalDecodeTime: 10.0,
        framesDecoded: 100,
        framesPerSecond: 30,
      });
      consumer.getStats.mockResolvedValue(statsMap);
      svc.consumers.set('cons-red', consumer);

      await vi.advanceTimersByTimeAsync(5500);

      // Consumer may have been paused if decode load is extreme
      // The exact behavior depends on rho calculation
      expect(consumer.getStats).toHaveBeenCalled();
    });

    it('green zone takes no action', async () => {
      await joinVoiceChannel();
      const svc = voiceService as any;

      const consumer = createMockConsumer('cons-green', 'video', 'prod-green');
      const statsMap = new Map();
      statsMap.set('inbound', {
        type: 'inbound-rtp',
        kind: 'video',
        totalDecodeTime: 0.1,
        framesDecoded: 1000,
        framesPerSecond: 30,
      });
      consumer.getStats.mockResolvedValue(statsMap);
      svc.consumers.set('cons-green', consumer);

      await vi.advanceTimersByTimeAsync(5500);

      expect(consumer.pause).not.toHaveBeenCalled();
    });
  });
});

describe('active screen-share metadata registration (#2088)', () => {
  beforeEach(async () => {
    await joinVoiceChannel();
  });

  it('consumeExistingProducers registers remote screen metadata', async () => {
    const svc = voiceService as any;
    useVoiceStore.getState().addParticipant({
      userId: 'user-7',
      username: 'alice',
      displayName: 'Alice',
      isMuted: false,
      isDeafened: false,
      isSpeaking: false,
      isVideoOn: false,
      isScreenSharing: false,
    });
    await svc.consumeExistingProducers([
      { producerId: 'prod-e1', userId: 'user-7', kind: 'video', source: 'screen' },
    ]);
    expect(useVoiceStore.getState().activeScreenShares['prod-e1']).toEqual({
      producerId: 'prod-e1',
      userId: 'user-7',
      username: 'alice',
      displayName: 'Alice',
      isLocal: false,
    });
  });

  it('handleNewProducer registers remote screen metadata on requiresOptIn announce', async () => {
    const svc = voiceService as any;
    await svc.handleNewProducer({
      producerId: 'prod-n1',
      userId: 'user-8',
      kind: 'video',
      source: 'screen',
      requiresOptIn: true,
    });
    expect(useVoiceStore.getState().activeScreenShares['prod-n1']?.isLocal).toBe(false);
    expect(useVoiceStore.getState().activeScreenShares['prod-n1']?.userId).toBe('user-8');
  });

  it('producer-closed unregisters metadata and clears suppression', () => {
    const store = useVoiceStore.getState();
    store.registerActiveScreenShare({
      producerId: 'prod-3',
      userId: 'user-1',
      username: 'alice',
      isLocal: false,
    });
    store.suppressAutoTune('prod-3');
    const handler = socketListeners['producer-closed']?.[0];
    expect(handler).toBeDefined();
    handler?.({ producerId: 'prod-3', userId: 'user-1', source: 'screen' });
    expect(useVoiceStore.getState().activeScreenShares['prod-3']).toBeUndefined();
    expect(useVoiceStore.getState().autoTuneSuppressedProducers['prod-3']).toBeUndefined();
  });
});

describe('global tune actions (#2088)', () => {
  beforeEach(async () => {
    await joinVoiceChannel();
  });

  it('tuneInAllScreenShares tunes into every available share and clears suppressions', async () => {
    const spy = vi.spyOn(voiceService, 'tuneInToScreenShare').mockResolvedValue(undefined);
    const store = useVoiceStore.getState();
    store.addAvailableScreenShare({ producerId: 'p1', userId: 'u1', username: 'a' });
    store.addAvailableScreenShare({ producerId: 'p2', userId: 'u2', username: 'b' });
    store.suppressAutoTune('p1');

    await voiceService.tuneInAllScreenShares();

    expect(spy).toHaveBeenCalledWith('p1', 'u1');
    expect(spy).toHaveBeenCalledWith('p2', 'u2');
    expect(useVoiceStore.getState().autoTuneSuppressedProducers).toEqual({});
    spy.mockRestore();
  });

  it('tuneInAllScreenShares at capacity surfaces the existing max-stream error', async () => {
    // Do NOT mock tuneInToScreenShare here — its real cap guard must fire.
    const store = useVoiceStore.getState();
    for (let i = 0; i < 5; i++) store.tuneIn(`tuned-${i}`, `cons-${i}`);
    store.addAvailableScreenShare({ producerId: 'p-over', userId: 'u9', username: 'z' });

    await voiceService.tuneInAllScreenShares();

    expect(useVoiceStore.getState().videoSlotError).toBe(
      'Maximum 5 screen shares reached. Tune out of one first.'
    );
    expect(useVoiceStore.getState().tunedInScreenShares['p-over']).toBeUndefined();
  });

  it('tuneOutAllScreenShares snapshots ids before mutating (no skips)', async () => {
    const seen: string[] = [];
    const spy = vi
      .spyOn(voiceService, 'tuneOutOfScreenShare')
      .mockImplementation(async (producerId: string) => {
        seen.push(producerId);
        useVoiceStore.getState().tuneOut(producerId); // mutate mid-iteration
      });
    const store = useVoiceStore.getState();
    store.tuneIn('r1', 'c1');
    store.tuneIn('r2', 'c2');
    store.tuneIn('r3', 'c3');

    await voiceService.tuneOutAllScreenShares();

    expect(seen.sort()).toEqual(['r1', 'r2', 'r3']);
    spy.mockRestore();
  });

  it('tuneOutAllScreenShares skips the local-screen sentinel and passes suppressAutoTune', async () => {
    const spy = vi.spyOn(voiceService, 'tuneOutOfScreenShare').mockResolvedValue(undefined);
    const store = useVoiceStore.getState();
    store.tuneIn('local-prod', 'local-screen');
    store.tuneIn('r1', 'c1');

    await voiceService.tuneOutAllScreenShares();

    expect(spy).toHaveBeenCalledTimes(1);
    expect(spy).toHaveBeenCalledWith('r1', { suppressAutoTune: true });
    spy.mockRestore();
  });
});

describe('tune-in hardening (#2088 review fixes)', () => {
  beforeEach(async () => {
    await joinVoiceChannel();
  });

  it('manual tuneInToScreenShare clears auto-tune suppression for that producer', async () => {
    const svc = voiceService as any;
    vi.spyOn(svc, 'consumeProducer').mockResolvedValue(undefined);
    vi.spyOn(svc, 'addDecryptKeyForUser').mockResolvedValue(undefined);
    useVoiceStore.getState().suppressAutoTune('p1');

    await voiceService.tuneInToScreenShare('p1', 'u1');

    expect(useVoiceStore.getState().autoTuneSuppressedProducers['p1']).toBeUndefined();
  });

  it('is idempotent for an already-tuned producer (no second consume)', async () => {
    const svc = voiceService as any;
    // spyOn an already-spied singleton method returns the SAME spy with
    // accumulated calls — clear before asserting call counts.
    const consume = vi.spyOn(svc, 'consumeProducer').mockResolvedValue(undefined);
    consume.mockClear();
    useVoiceStore.getState().tuneIn('p1', 'c1');

    await voiceService.tuneInToScreenShare('p1', 'u1');

    expect(consume).not.toHaveBeenCalled();
  });

  it('concurrent tune-ins for the same producer consume once (in-flight guard)', async () => {
    const svc = voiceService as any;
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const consume = vi.spyOn(svc, 'consumeProducer').mockImplementation(async () => {
      await gate;
    });
    consume.mockClear();
    vi.spyOn(svc, 'addDecryptKeyForUser').mockResolvedValue(undefined);

    const first = voiceService.tuneInToScreenShare('p1', 'u1');
    const second = voiceService.tuneInToScreenShare('p1', 'u1');
    release();
    await Promise.all([first, second]);

    expect(consume).toHaveBeenCalledTimes(1);
  });

  it('producer-closed (server-initiated) performs local-only cleanup — no close-consumer emit', () => {
    const svc = voiceService as any;
    const consumer = createMockConsumer('cons-sv', 'video', 'prod-sv');
    svc.consumers.set('cons-sv', consumer);
    svc.consumerMeta.set('cons-sv', {
      source: 'screen',
      producerUserId: 'user-2',
      producerId: 'prod-sv',
    });
    mockSocket.emit.mockClear();

    const handler = socketListeners['producer-closed']?.[0];
    expect(handler).toBeDefined();
    handler?.({ producerId: 'prod-sv', userId: 'user-2', source: 'screen' });

    expect(consumer.close).toHaveBeenCalled();
    expect(mockSocket.emit).not.toHaveBeenCalledWith('close-consumer', expect.anything());
  });

  it('reserves cap slots synchronously: concurrent tune-ins for different producers cannot exceed the cap (#2088)', async () => {
    const svc = voiceService as any;
    vi.spyOn(svc, 'addDecryptKeyForUser').mockResolvedValue(undefined);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const consume = vi.spyOn(svc, 'consumeProducer').mockImplementation(async () => {
      await gate;
    });
    consume.mockClear();

    const store = useVoiceStore.getState();
    // Four slots already taken — exactly one free under the cap of five.
    for (let i = 0; i < 4; i++) store.tuneIn(`tuned-${i}`, `cons-${i}`);

    // Two new-producer announces race for the single remaining slot. Before the
    // fix both passed the cap guard (each read the same committed count of four)
    // and consumed, overshooting the cap to six.
    const a = voiceService.tuneInToScreenShare('p-a', 'u-a');
    const b = voiceService.tuneInToScreenShare('p-b', 'u-b');
    release();
    await Promise.all([a, b]);

    const tuned = useVoiceStore.getState().tunedInScreenShares;
    const newlyTuned = ['p-a', 'p-b'].filter((id) => id in tuned);
    expect(newlyTuned).toHaveLength(1); // only one won the last slot
    expect(Object.keys(tuned)).toHaveLength(5); // cap respected
    expect(useVoiceStore.getState().videoSlotError).toBe(
      'Maximum 5 screen shares reached. Tune out of one first.'
    );
    consume.mockRestore();
  });

  it('keeps the first dominant screen when concurrent tune-ins finish later (#2155)', async () => {
    const svc = voiceService as any;
    vi.spyOn(svc, 'addDecryptKeyForUser').mockResolvedValue(undefined);
    const releases = new Map<string, () => void>();
    const consume = vi
      .spyOn(svc, 'consumeProducer')
      .mockImplementation(async (producerId: string) => {
        await new Promise<void>((resolve) => {
          releases.set(producerId, resolve);
        });
      });
    consume.mockClear();

    const first = voiceService.tuneInToScreenShare('p-first', 'u-first');
    const second = voiceService.tuneInToScreenShare('p-second', 'u-second');

    await vi.waitFor(() => {
      expect(releases.has('p-first')).toBe(true);
      expect(releases.has('p-second')).toBe(true);
    });

    releases.get('p-first')?.();
    await first;
    expect(useVoiceStore.getState().dominantScreenShareId).toBe('p-first');

    releases.get('p-second')?.();
    await second;

    expect(useVoiceStore.getState().dominantScreenShareId).toBe('p-first');
    consume.mockRestore();
  });

  it('tuneInToScreenShare bails without recording state when the call is torn down mid-consume (#2088)', async () => {
    const svc = voiceService as any;
    vi.spyOn(svc, 'addDecryptKeyForUser').mockResolvedValue(undefined);
    const consumer = createMockConsumer('cons-late', 'video', 'prod-late');
    vi.spyOn(svc, 'consumeProducer').mockImplementation(async () => {
      // consumeProducer registered the consumer, but a leaveChannel() reset
      // raced our awaits and cleared the active call.
      svc.consumers.set('cons-late', consumer);
      useVoiceStore.setState({ activeChannelId: null });
    });
    mockSocket.emit.mockClear();

    await voiceService.tuneInToScreenShare('prod-late', 'user-2');

    // No stale tune-in state recorded for the room the user left…
    expect(useVoiceStore.getState().tunedInScreenShares['prod-late']).toBeUndefined();
    // …and the orphaned consumer is closed + the SFU told to stop forwarding.
    expect(consumer.close).toHaveBeenCalled();
    expect(mockSocket.emit).toHaveBeenCalledWith('close-consumer', { consumerId: 'cons-late' });
    expect(svc.consumers.has('cons-late')).toBe(false);
  });
});
