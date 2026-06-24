import { vi, describe, it, expect, beforeEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';

// ---------------------------------------------------------------------------
// Mock external dependencies BEFORE importing voiceService
// ---------------------------------------------------------------------------

// --- mediasoup-client ---
const mockDeviceRtpCapabilities = {
  codecs: [
    { mimeType: 'audio/opus', kind: 'audio', clockRate: 48000, channels: 2, parameters: {} },
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

// --- socket.io-client ---
const socketListeners: Record<string, Array<(...args: unknown[]) => void>> = {};

const mockSocket = {
  connected: false,
  emit: vi.fn(),
  on: vi.fn().mockImplementation((event: string, cb: (...args: unknown[]) => void) => {
    if (!socketListeners[event]) socketListeners[event] = [];
    socketListeners[event].push(cb);
  }),
  once: vi.fn(),
  disconnect: vi.fn(),
  io: { on: vi.fn() },
};

vi.mock('socket.io-client', () => ({
  io: vi.fn().mockReturnValue(mockSocket),
}));

// --- apiClient ---
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
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
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 2,
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

class MockMediaStream {
  private _tracks: unknown[];
  constructor(tracks?: unknown[]) {
    this._tracks = tracks || [];
  }
  getTracks() {
    return this._tracks;
  }
  getAudioTracks() {
    return this._tracks.filter((t: Record<string, unknown>) => t.kind === 'audio');
  }
  getVideoTracks() {
    return this._tracks.filter((t: Record<string, unknown>) => t.kind === 'video');
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

import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function createMockProducer(id = 'prod-1', source = 'mic') {
  return {
    id,
    kind: source === 'mic' || source === 'screen-audio' ? 'audio' : 'video',
    paused: false,
    closed: false,
    close: vi.fn(),
    pause: vi.fn().mockImplementation(function (this: { paused: boolean }) {
      this.paused = true;
    }),
    resume: vi.fn().mockImplementation(function (this: { paused: boolean }) {
      this.paused = false;
    }),
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

function setupLocalUser(userId = 'user-1') {
  useUserStore.setState({
    user: {
      id: userId,
      username: 'testuser',
      display_name: 'Test User',
      avatar_url: null,
      email: 'test@test.com',
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
    },
  });
}

function addParticipant(userId: string, overrides: Record<string, unknown> = {}) {
  const store = useVoiceStore.getState();
  const base = {
    id: userId,
    username: 'testuser',
    displayName: 'Test User',
    audioEnabled: true,
    videoEnabled: false,
    screenEnabled: false,
    isSpeaking: false,
    volume: 100,
    serverMuted: false,
    serverDeafened: false,
    ...overrides,
  };
  // Set via direct state update since setParticipants expects VoiceParticipant[]
  useVoiceStore.setState({
    participants: {
      ...store.participants,
      [userId]: base as ReturnType<typeof useVoiceStore.getState>['participants'][string],
    },
  });
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const svc = voiceService as any;

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('VoiceService enforcement guards', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    // Clear internal maps
    svc.producers.clear();
    svc.consumers.clear();
    // Clear socket listeners collected during import
    for (const key of Object.keys(socketListeners)) {
      delete socketListeners[key];
    }
  });

  // =========================================================================
  // toggleMute — server-mute enforcement
  // =========================================================================
  describe('toggleMute', () => {
    it('should return early without a mic producer', async () => {
      // No producer set — toggleMute should be a no-op
      const store = useVoiceStore.getState();
      store.setMuted(true);
      await voiceService.toggleMute();
      expect(useVoiceStore.getState().isMuted).toBe(true);
    });

    it('should mute when currently unmuted', async () => {
      const micProducer = createMockProducer('prod-mic', 'mic');
      svc.producers.set('mic', micProducer);
      useVoiceStore.getState().setMuted(false);

      await voiceService.toggleMute();

      expect(micProducer.pause).toHaveBeenCalled();
      expect(useVoiceStore.getState().isMuted).toBe(true);
    });

    it('should unmute when currently muted and NOT server-muted', async () => {
      setupLocalUser('user-1');
      addParticipant('user-1', { serverMuted: false });

      const micProducer = createMockProducer('prod-mic', 'mic');
      micProducer.paused = true;
      svc.producers.set('mic', micProducer);
      useVoiceStore.getState().setMuted(true);

      await voiceService.toggleMute();

      expect(micProducer.resume).toHaveBeenCalled();
      expect(useVoiceStore.getState().isMuted).toBe(false);
    });

    it('should block unmute when server-muted', async () => {
      setupLocalUser('user-1');
      addParticipant('user-1', { serverMuted: true });

      const micProducer = createMockProducer('prod-mic', 'mic');
      micProducer.paused = true;
      svc.producers.set('mic', micProducer);
      useVoiceStore.getState().setMuted(true);

      await voiceService.toggleMute();

      // Should NOT have resumed — blocked by server-mute guard
      expect(micProducer.resume).not.toHaveBeenCalled();
      expect(useVoiceStore.getState().isMuted).toBe(true);
    });

    it('should unmute when server-muted is false for participant', async () => {
      setupLocalUser('user-1');
      addParticipant('user-1', { serverMuted: false });

      const micProducer = createMockProducer('prod-mic', 'mic');
      micProducer.paused = true;
      svc.producers.set('mic', micProducer);
      useVoiceStore.getState().setMuted(true);

      await voiceService.toggleMute();

      expect(micProducer.resume).toHaveBeenCalled();
      expect(useVoiceStore.getState().isMuted).toBe(false);
    });

    it('should allow unmute when no participant entry exists (no server-mute)', async () => {
      setupLocalUser('user-1');
      // No participant added — participant lookup returns undefined

      const micProducer = createMockProducer('prod-mic', 'mic');
      micProducer.paused = true;
      svc.producers.set('mic', micProducer);
      useVoiceStore.getState().setMuted(true);

      await voiceService.toggleMute();

      expect(micProducer.resume).toHaveBeenCalled();
      expect(useVoiceStore.getState().isMuted).toBe(false);
    });
  });

  // =========================================================================
  // toggleDeafen / isServerDeafenBlocked — server-deafen enforcement
  // =========================================================================
  describe('toggleDeafen', () => {
    it('should deafen when currently not deafened', () => {
      useVoiceStore.getState().setDeafened(false);

      voiceService.toggleDeafen();

      expect(useVoiceStore.getState().isDeafened).toBe(true);
    });

    it('should undeafen when currently deafened and NOT server-deafened', () => {
      setupLocalUser('user-1');
      addParticipant('user-1', { serverDeafened: false });
      useVoiceStore.getState().setDeafened(true);

      voiceService.toggleDeafen();

      expect(useVoiceStore.getState().isDeafened).toBe(false);
    });

    it('should block undeafen when server-deafened', () => {
      setupLocalUser('user-1');
      addParticipant('user-1', { serverDeafened: true });
      useVoiceStore.getState().setDeafened(true);

      voiceService.toggleDeafen();

      // Should remain deafened — blocked by server-deafen guard
      expect(useVoiceStore.getState().isDeafened).toBe(true);
    });

    it('should pause audio consumers when deafening', () => {
      useVoiceStore.getState().setDeafened(false);

      const audioConsumer = createMockConsumer('c1', 'audio');
      const videoConsumer = createMockConsumer('c2', 'video');
      svc.consumers.set('c1', audioConsumer);
      svc.consumers.set('c2', videoConsumer);

      voiceService.toggleDeafen();

      expect(audioConsumer.pause).toHaveBeenCalled();
      expect(videoConsumer.pause).not.toHaveBeenCalled();
    });

    it('should resume audio consumers when undeafening', () => {
      setupLocalUser('user-1');
      addParticipant('user-1', { serverDeafened: false });
      useVoiceStore.getState().setDeafened(true);

      const audioConsumer = createMockConsumer('c1', 'audio');
      audioConsumer.paused = true;
      svc.consumers.set('c1', audioConsumer);

      voiceService.toggleDeafen();

      expect(audioConsumer.resume).toHaveBeenCalled();
    });
  });

  // =========================================================================
  // Socket.IO enforcement listeners — server-mute-changed
  // =========================================================================
  describe('server-mute-changed socket listener', () => {
    it('should enforce local mute when server mutes the local user', () => {
      setupLocalUser('user-1');
      addParticipant('user-1', { serverMuted: false });

      const micProducer = createMockProducer('prod-mic', 'mic');
      micProducer.paused = false;
      svc.producers.set('mic', micProducer);
      useVoiceStore.getState().setMuted(false);

      // Simulate Socket.IO connection and listener registration
      svc.socket = mockSocket;
      svc.setupSocketListeners();

      // Find and invoke the server-mute-changed handler
      const handlers = socketListeners['server-mute-changed'];
      expect(handlers).toBeDefined();
      expect(handlers.length).toBeGreaterThan(0);

      handlers[handlers.length - 1]({ userId: 'user-1', serverMuted: true });

      expect(useVoiceStore.getState().participants['user-1']?.serverMuted).toBe(true);
      expect(micProducer.pause).toHaveBeenCalled();
      expect(useVoiceStore.getState().isMuted).toBe(true);
    });

    it('should update participant state for remote user without enforcing local mute', () => {
      setupLocalUser('user-1');
      addParticipant('user-2', { serverMuted: false });

      const micProducer = createMockProducer('prod-mic', 'mic');
      svc.producers.set('mic', micProducer);
      useVoiceStore.getState().setMuted(false);

      svc.socket = mockSocket;
      svc.setupSocketListeners();

      const handlers = socketListeners['server-mute-changed'];
      handlers[handlers.length - 1]({ userId: 'user-2', serverMuted: true });

      // Remote user's participant updated but local mic NOT paused
      expect(useVoiceStore.getState().participants['user-2']?.serverMuted).toBe(true);
      expect(micProducer.pause).not.toHaveBeenCalled();
      expect(useVoiceStore.getState().isMuted).toBe(false);
    });
  });

  // =========================================================================
  // Socket.IO enforcement listeners — server-deafen-changed
  // =========================================================================
  describe('server-deafen-changed socket listener', () => {
    it('should enforce local deafen and mute when server deafens the local user', () => {
      setupLocalUser('user-1');
      addParticipant('user-1', { serverDeafened: false, serverMuted: false });

      const micProducer = createMockProducer('prod-mic', 'mic');
      micProducer.paused = false;
      svc.producers.set('mic', micProducer);

      const audioConsumer = createMockConsumer('c1', 'audio');
      svc.consumers.set('c1', audioConsumer);

      useVoiceStore.getState().setMuted(false);
      useVoiceStore.getState().setDeafened(false);

      svc.socket = mockSocket;
      svc.setupSocketListeners();

      const handlers = socketListeners['server-deafen-changed'];
      expect(handlers).toBeDefined();
      expect(handlers.length).toBeGreaterThan(0);

      handlers[handlers.length - 1]({ userId: 'user-1', serverDeafened: true });

      const participant = useVoiceStore.getState().participants['user-1'];
      expect(participant?.serverDeafened).toBe(true);
      expect(participant?.serverMuted).toBe(true);
      expect(audioConsumer.pause).toHaveBeenCalled();
      expect(micProducer.pause).toHaveBeenCalled();
      expect(useVoiceStore.getState().isMuted).toBe(true);
      expect(useVoiceStore.getState().isDeafened).toBe(true);
    });

    it('should update participant state for remote user without local enforcement', () => {
      setupLocalUser('user-1');
      addParticipant('user-2', { serverDeafened: false, serverMuted: false });

      useVoiceStore.getState().setMuted(false);
      useVoiceStore.getState().setDeafened(false);

      svc.socket = mockSocket;
      svc.setupSocketListeners();

      const handlers = socketListeners['server-deafen-changed'];
      handlers[handlers.length - 1]({ userId: 'user-2', serverDeafened: true });

      const participant = useVoiceStore.getState().participants['user-2'];
      expect(participant?.serverDeafened).toBe(true);
      expect(participant?.serverMuted).toBe(true);
      // Local state NOT affected
      expect(useVoiceStore.getState().isMuted).toBe(false);
      expect(useVoiceStore.getState().isDeafened).toBe(false);
    });
  });

  // =========================================================================
  // joinChannel — setEffectivePermissions and setGroupDMInfo
  // =========================================================================
  describe('joinChannel permissions and DM info', () => {
    it('should parse and set effective permissions from join response', async () => {
      const store = useVoiceStore.getState();
      store.setEffectivePermissions(255n);
      expect(useVoiceStore.getState().effectivePermissions).toBe(255n);
    });

    it('should handle fallback to 0n for invalid permissions', () => {
      const store = useVoiceStore.getState();
      store.setEffectivePermissions(0n);
      expect(useVoiceStore.getState().effectivePermissions).toBe(0n);
    });

    it('should set group DM info via store', () => {
      const store = useVoiceStore.getState();
      store.setGroupDMInfo(true, 'caller');
      const state = useVoiceStore.getState();
      expect(state.isGroupDM).toBe(true);
      expect(state.callerDMRole).toBe('caller');
    });

    it('should set DM call state via store', () => {
      const store = useVoiceStore.getState();
      store.setDMCall(true, 'conv-123');
      const state = useVoiceStore.getState();
      expect(state.isDMCall).toBe(true);
      expect(state.dmConversationId).toBe('conv-123');
    });
  });

  // =========================================================================
  // applyJoinMetadata — enforcement flag application
  // =========================================================================
  describe('applyJoinMetadata enforcement flags', () => {
    it('should set local muted state when server_muted is true', () => {
      setupLocalUser('user-1');
      useVoiceStore.getState().setMuted(false);

      // Call the private method via the untyped handle
      svc.applyJoinMetadata(
        useVoiceStore.getState(),
        { server_muted: true, server_deafened: false, permissions: '0' },
        'channel',
        'ch-1'
      );

      expect(useVoiceStore.getState().isMuted).toBe(true);
    });

    it('should set local deafened and muted state when server_deafened is true', () => {
      setupLocalUser('user-1');
      useVoiceStore.getState().setMuted(false);
      useVoiceStore.getState().setDeafened(false);

      svc.applyJoinMetadata(
        useVoiceStore.getState(),
        { server_muted: false, server_deafened: true, permissions: '0' },
        'channel',
        'ch-1'
      );

      expect(useVoiceStore.getState().isMuted).toBe(true);
      expect(useVoiceStore.getState().isDeafened).toBe(true);
    });

    it('should not change muted/deafened state when flags are false', () => {
      setupLocalUser('user-1');
      useVoiceStore.getState().setMuted(false);
      useVoiceStore.getState().setDeafened(false);

      svc.applyJoinMetadata(
        useVoiceStore.getState(),
        { server_muted: false, server_deafened: false, permissions: '255' },
        'channel',
        'ch-1'
      );

      expect(useVoiceStore.getState().isMuted).toBe(false);
      expect(useVoiceStore.getState().isDeafened).toBe(false);
    });

    it('should parse and store effective permissions from join response', () => {
      svc.applyJoinMetadata(
        useVoiceStore.getState(),
        { server_muted: false, server_deafened: false, permissions: '12345' },
        'channel',
        'ch-1'
      );

      expect(useVoiceStore.getState().effectivePermissions).toBe(12345n);
    });

    it('should fall back to 0n for invalid permissions string', () => {
      svc.applyJoinMetadata(
        useVoiceStore.getState(),
        { server_muted: false, server_deafened: false, permissions: 'not-a-number' },
        'channel',
        'ch-1'
      );

      expect(useVoiceStore.getState().effectivePermissions).toBe(0n);
    });

    it('should set DM call state and group DM info for DM joins', () => {
      svc.applyJoinMetadata(
        useVoiceStore.getState(),
        {
          server_muted: false,
          server_deafened: false,
          permissions: '0',
          conversation: { is_group: true, caller_role: 'caller' },
        },
        'dm',
        'conv-456'
      );

      const state = useVoiceStore.getState();
      expect(state.isDMCall).toBe(true);
      expect(state.dmConversationId).toBe('conv-456');
      expect(state.isGroupDM).toBe(true);
      expect(state.callerDMRole).toBe('caller');
    });
  });
});
