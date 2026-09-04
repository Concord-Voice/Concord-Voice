/**
 * Server-minted ICE-server threading into the in-window mediasoup transports (#3104).
 *
 * Covers: verbatim pass-through, the empty-list key-omission rule, per-entry
 * degradation, both teardown paths, and the reconnect rebuild.
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
const socketListeners: Record<string, Array<(...args: unknown[]) => void>> = {};
const socketOnceListeners: Record<string, Array<(...args: unknown[]) => void>> = {};
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
  active: true,
  emit: vi.fn(),
  on: vi.fn().mockImplementation((event: string, cb: (...args: unknown[]) => void) => {
    (socketListeners[event] ??= []).push(cb);
  }),
  off: vi
    .fn()
    .mockImplementation((event: string, cb: (...args: unknown[]) => void) =>
      removeMockListener(socketListeners, event, cb)
    ),
  once: vi.fn().mockImplementation((event: string, cb: (...args: unknown[]) => void) => {
    (socketOnceListeners[event] ??= []).push(cb);
  }),
  disconnect: vi.fn(),
  io: {
    on: vi.fn().mockImplementation((event: string, cb: (...args: unknown[]) => void) => {
      (managerListeners[event] ??= []).push(cb);
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
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

// --- e2eeService ---
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    getChannelKey: vi.fn().mockResolvedValue(null),
    getChannelKeyMaterial: vi.fn().mockResolvedValue({ channelKey: null, keyVersion: 0 }),
    invalidateChannelKey: vi.fn(),
    getChannelKeyVersion: vi.fn().mockReturnValue(0),
    getChannelKeyByVersion: vi.fn().mockResolvedValue(null),
    onKeyRotation: vi.fn().mockReturnValue(() => {}),
  },
}));

// --- mediaEncryption ---
vi.mock('@/renderer/services/e2ee/mediaEncryption', () => ({
  MEDIA_E2EE_FRAME_CRYPTO_VERSION: 5,
  MediaEncryption: class MockMediaEncryption {
    init = vi.fn().mockResolvedValue(undefined);
    initFromKey = vi.fn();
    destroy = vi.fn();
    getCurrentKeyId = vi.fn().mockReturnValue(0);
    setCurrentKeyId = vi.fn();
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
  createMediaStreamSource = vi.fn().mockReturnValue({ connect: vi.fn(), disconnect: vi.fn() });
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
Object.defineProperty(MockRTCRtpSender.prototype, 'createEncodedStreams', { value: vi.fn() });
Object.defineProperty(globalThis, 'RTCRtpSender', {
  value: MockRTCRtpSender,
  writable: true,
  configurable: true,
});

if ('RTCRtpScriptTransform' in globalThis) {
  delete (globalThis as Record<string, unknown>)['RTCRtpScriptTransform'];
}

const mockGetUserMedia = vi.fn();
Object.defineProperty(navigator, 'mediaDevices', {
  value: {
    getUserMedia: mockGetUserMedia,
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

import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { extractSelectedCandidatePairType } from '@/renderer/services/voice/iceServers';

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const STUN = { urls: 'stun:turn.example.test:3478' };
const TURN = {
  urls: 'turn:turn.example.test:3478',
  username: '1780000000:user-1',
  credential: 'Zm9vYmFyYmF6',
};

function joinResponse(iceServers: unknown) {
  return {
    allowed: true,
    media_server_url: 'http://localhost:3000',
    ice_servers: iceServers,
    channel: { id: 'channel-1', name: 'General', server_id: 'server-1', audio_quality_tier: null },
  };
}

function makeRoomJoined() {
  return {
    rtpCapabilities: mockDeviceRtpCapabilities,
    mediaFrameCryptoVersion: 5,
    existingProducers: [],
    participants: [{ userId: 'user-1', username: 'testuser', displayName: 'Test User' }],
    channelName: 'General',
  };
}

let transportSeq = 0;
function makeTransportOpts() {
  transportSeq += 1;
  return {
    id: `transport-${transportSeq}`,
    iceParameters: { usernameFragment: 'frag', password: 'pass' }, // pragma: allowlist secret
    iceCandidates: [],
    dtlsParameters: { role: 'auto', fingerprints: [] },
  };
}

function makeMicTrack() {
  return {
    id: 'mic-1',
    kind: 'audio',
    readyState: 'live',
    enabled: true,
    stop: vi.fn(),
    clone: vi.fn(),
    getSettings: vi.fn().mockReturnValue({}),
    contentHint: '',
    onended: null as (() => void) | null,
  };
}

function makeMicStream() {
  const tracks = [makeMicTrack()];
  return {
    getAudioTracks: vi.fn().mockReturnValue(tracks),
    getVideoTracks: vi.fn().mockReturnValue([]),
    getTracks: vi.fn().mockReturnValue(tracks),
    addTrack: vi.fn(),
    removeTrack: vi.fn(),
  };
}

function makeMicProducer() {
  return {
    id: 'prod-mic',
    kind: 'audio',
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
    appData: { source: 'mic' },
  };
}

function makeSendTransport() {
  return {
    id: 'send-1',
    closed: false,
    close: vi.fn(),
    produce: vi.fn().mockResolvedValue(makeMicProducer()),
    on: vi.fn(),
    getStats: vi.fn().mockResolvedValue(new Map()),
    _awaitQueue: {
      push: vi.fn().mockImplementation(async (fn: () => Promise<void>) => {
        await fn();
      }),
    },
  };
}

function makeRecvTransport() {
  return {
    id: 'recv-1',
    closed: false,
    close: vi.fn(),
    consume: vi.fn(),
    on: vi.fn(),
    getStats: vi.fn().mockResolvedValue(new Map()),
  };
}

/** Wire the socket ack responses a full join needs. */
function setupEmitResponses() {
  mockSocket.emit.mockImplementation(
    (event: string, _payload: unknown, ack?: (r: unknown) => void) => {
      if (!ack) return;
      if (event === 'join-room') return ack(makeRoomJoined());
      if (event === 'create-transport') return ack(makeTransportOpts());
      if (event === 'produce') return ack({ id: 'prod-mic' });
      return ack(undefined);
    }
  );
}

async function joinWith(iceServers: unknown) {
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
  mockApiFetch.mockResolvedValue({
    ok: true,
    json: vi.fn().mockResolvedValue(joinResponse(iceServers)),
  });
  mockSocket.connected = true;
  mockCreateSendTransport.mockReturnValue(makeSendTransport());
  mockCreateRecvTransport.mockReturnValue(makeRecvTransport());
  mockGetUserMedia.mockResolvedValue(makeMicStream());
  setupEmitResponses();
  await voiceService.joinChannel('channel-1', 'channel');
}

/** The private field is not directly reachable; the PiP accessor is its only reader. */
const heldIceServers = () =>
  (
    voiceService as unknown as { getIceServersForPip(): RTCIceServer[] | null }
  ).getIceServersForPip();

/**
 * Send-transport mock that captures its `on(...)` handlers so a test can fire
 * `connectionstatechange` itself (#3104 Task 4 — candidate-pair recording).
 */
function makeSendTransportWithEvents(stats: Map<string, unknown>) {
  const handlers: Record<string, Array<(...a: unknown[]) => void>> = {};
  return {
    id: 'send-1',
    closed: false,
    close: vi.fn(),
    produce: vi.fn().mockResolvedValue({ id: 'prod-mic', close: vi.fn(), on: vi.fn() }),
    on: vi.fn((event: string, cb: (...a: unknown[]) => void) => {
      (handlers[event] ??= []).push(cb);
    }),
    getStats: vi.fn().mockResolvedValue(stats),
    _fire: (event: string, ...args: unknown[]) => handlers[event]?.forEach((cb) => cb(...args)),
    _awaitQueue: { push: vi.fn(async (fn: () => Promise<void>) => fn()) },
  };
}

const RELAY_STATS = new Map<string, unknown>([
  [
    'cp-1',
    { type: 'candidate-pair', state: 'succeeded', nominated: true, localCandidateId: 'lc-1' },
  ],
  ['lc-1', { type: 'local-candidate', candidateType: 'relay' }],
]);

describe('VoiceService ICE-server threading (#3104)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    transportSeq = 0;
    mockSocket.connected = false;
    mockSocket.active = true;
    for (const k of Object.keys(socketListeners)) delete socketListeners[k];
    for (const k of Object.keys(socketOnceListeners)) delete socketOnceListeners[k];
    for (const k of Object.keys(managerListeners)) delete managerListeners[k];
  });

  afterEach(() => {
    try {
      voiceService.emergencyCleanup();
    } catch {
      /* ok */
    }
  });

  it('passes the server-supplied entries verbatim to the send transport', async () => {
    await joinWith([STUN, TURN]);
    // Harness sanity: the assertions below are only meaningful if the join
    // actually completed rather than bailing out mid-way.
    expect(useVoiceStore.getState().connectionState).toBe('connected');
    const opts = mockCreateSendTransport.mock.calls[0][0];
    expect(opts.iceServers).toEqual([STUN, TURN]);
    expect(opts.iceTransportPolicy).toBe('all');
  });

  it('passes the same entries to every recv transport', async () => {
    await joinWith([STUN, TURN]);
    expect(mockCreateRecvTransport.mock.calls.length).toBeGreaterThanOrEqual(2);
    for (const call of mockCreateRecvTransport.mock.calls) {
      expect(call[0].iceServers).toEqual([STUN, TURN]);
      expect(call[0].iceTransportPolicy).toBe('all');
    }
  });

  it('preserves the existing transport parameters alongside the ICE config', async () => {
    await joinWith([STUN]);
    const opts = mockCreateSendTransport.mock.calls[0][0];
    expect(opts.id).toEqual(expect.any(String));
    expect(opts.iceParameters).toEqual({ usernameFragment: 'frag', password: 'pass' }); // pragma: allowlist secret
    expect(opts.iceCandidates).toEqual([]);
    expect(opts.dtlsParameters).toEqual({ role: 'auto', fingerprints: [] });
  });

  it('omits the iceServers key entirely when the field is absent', async () => {
    await joinWith(undefined);
    const opts = mockCreateSendTransport.mock.calls[0][0];
    expect(Object.hasOwn(opts, 'iceServers')).toBe(false);
    expect(Object.hasOwn(opts, 'iceTransportPolicy')).toBe(false);
  });

  it('omits the iceServers key entirely when the list is empty', async () => {
    await joinWith([]);
    const opts = mockCreateSendTransport.mock.calls[0][0];
    expect(Object.hasOwn(opts, 'iceServers')).toBe(false);
  });

  it('omits the iceServers key entirely when every entry is malformed', async () => {
    await joinWith([null, { urls: 42 }, { urls: 'http://nope' }, { urls: 'turn:x:3478' }]);
    const opts = mockCreateSendTransport.mock.calls[0][0];
    expect(Object.hasOwn(opts, 'iceServers')).toBe(false);
  });

  it('keeps the valid entries when only some are malformed', async () => {
    await joinWith([STUN, null, { urls: 'http://nope' }, TURN]);
    expect(mockCreateSendTransport.mock.calls[0][0].iceServers).toEqual([STUN, TURN]);
  });

  it('still joins successfully when ice_servers is absent', async () => {
    await joinWith(undefined);
    expect(useVoiceStore.getState().connectionState).toBe('connected');
    expect(mockCreateSendTransport).toHaveBeenCalledTimes(1);
    expect(mockCreateRecvTransport.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it('clears the held list on leaveChannel so a later join cannot reuse it', async () => {
    await joinWith([STUN, TURN]);
    expect(heldIceServers()).toEqual([STUN, TURN]);
    await voiceService.leaveChannel();
    expect(heldIceServers()).toBeNull();
  });

  // leaveChannel() routes through cleanup(), emergencyCleanup() through
  // cleanupMediaAndTransports(). The two duplicate their teardown rather than
  // delegating, so each needs its own null write and its own assertion (#3104).
  it('clears the held list on emergencyCleanup', async () => {
    await joinWith([STUN, TURN]);
    expect(heldIceServers()).toEqual([STUN, TURN]);
    voiceService.emergencyCleanup();
    expect(heldIceServers()).toBeNull();
  });

  it('rebuilds reconnect transports with the freshly re-authorized list', async () => {
    await joinWith([STUN]);
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: vi.fn().mockResolvedValue(joinResponse([STUN, TURN])),
    });
    mockCreateSendTransport.mockClear();
    mockCreateRecvTransport.mockClear();

    socketListeners['disconnect']?.forEach((cb) => cb('transport close'));
    mockSocket.connected = true;
    socketListeners['connect']?.forEach((cb) => cb());

    await vi.waitFor(() => {
      expect(mockCreateSendTransport).toHaveBeenCalled();
    });
    expect(mockCreateSendTransport.mock.calls[0][0].iceServers).toEqual([STUN, TURN]);
  });

  // The join-time diagnostic is the one place this list touches a console sink.
  // NC-1 forbids anything derived from it beyond scheme and count; assert the
  // emitted shape rather than trusting the call site (#3104).
  it('logs only the count and scheme set for the ICE list', async () => {
    const debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});
    try {
      await joinWith([STUN, TURN]);
      const iceLines = debugSpy.mock.calls.filter((c) => c[0] === '[ice] servers');
      expect(iceLines).toHaveLength(1);
      expect(iceLines[0][1]).toEqual({ count: 2, schemes: ['stun', 'turn'] });
      // Nothing beyond the two permitted keys, and no credential anywhere.
      const serialized = JSON.stringify(debugSpy.mock.calls);
      expect(serialized).not.toContain('Zm9vYmFyYmF6');
      expect(serialized).not.toContain('1780000000:user-1');
      expect(serialized).not.toContain('turn.example.test');
    } finally {
      debugSpy.mockRestore();
    }
  });

  // Candidate-pair recording (#3104 Task 4). `joinWith` unconditionally calls
  // `mockCreateSendTransport.mockReturnValue(makeSendTransport())` internally, which
  // would clobber a plain `mockReturnValue(transport)` set by the caller beforehand —
  // `mockReturnValueOnce` queues ahead of that persistent default for the single
  // `createSendTransport()` call `joinChannel` makes, so the custom event-capturing
  // transport is the one actually wired into `voiceService.sendTransport`.
  it('logs the selected candidate-pair type once the transport connects', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    const transport = makeSendTransportWithEvents(RELAY_STATS);
    mockCreateSendTransport.mockReturnValueOnce(transport);
    await joinWith([STUN, TURN]);

    transport._fire('connectionstatechange', 'connected');
    await vi.waitFor(() => {
      expect(debug).toHaveBeenCalledWith('[ice] selected-pair', { label: 'send', type: 'relay' });
    });
    debug.mockRestore();
  });

  it('records the candidate pair at most once per transport', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    const transport = makeSendTransportWithEvents(RELAY_STATS);
    mockCreateSendTransport.mockReturnValueOnce(transport);
    await joinWith([STUN, TURN]);

    transport._fire('connectionstatechange', 'connected');
    transport._fire('connectionstatechange', 'connected');
    await vi.waitFor(() => expect(transport.getStats).toHaveBeenCalled());
    expect(transport.getStats).toHaveBeenCalledTimes(1);
    debug.mockRestore();
  });

  it('reports a rejected read under its own label, never as unresolved', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    const transport = makeSendTransportWithEvents(RELAY_STATS);
    transport.getStats = vi.fn().mockRejectedValue(new Error('stats unavailable'));
    mockCreateSendTransport.mockReturnValueOnce(transport);
    await joinWith([STUN, TURN]);

    transport._fire('connectionstatechange', 'connected');
    await vi.waitFor(() => {
      expect(
        debug,
        'a rejected getStats() must be observable -- absence of every line is ' +
          'ambiguous with the handler never running -- but under its OWN label'
      ).toHaveBeenCalledWith('[ice] selected-pair-stats-unavailable', { label: 'send' });
    });
    expect(debug).not.toHaveBeenCalledWith('[ice] selected-pair', expect.anything());
    // The discrimination IS the point. Reusing `-unresolved` here would make it
    // mean "the timing contract broke OR stats were unavailable", destroying its
    // value as the register entry's runtime proof.
    expect(
      debug,
      'a rejected read must not masquerade as a timing-contract break'
    ).not.toHaveBeenCalledWith('[ice] selected-pair-unresolved', expect.anything());
    // observability.md principle 3: the cause chain never reaches the sink.
    expect(JSON.stringify(debug.mock.calls)).not.toContain('stats unavailable');
    debug.mockRestore();
  });

  // Runtime proof for the platform-behavior register ([internal]rules/frontend.md
  // § Platform-behavior register). These CANNOT prove the Chromium timing
  // contract -- a synthetic Map returns whatever it was built to return, which
  // is the register's entire premise. They prove only that the PROOF MECHANISM
  // works, so a real engine break shows up in a bug report instead of being
  // swallowed the way #3117's dead diagnostic was.
  const UNRESOLVABLE_STATS = new Map<string, unknown>([
    [
      'cp-1',
      { id: 'cp-1', type: 'candidate-pair', state: 'in-progress', localCandidateId: 'lc-1' },
    ],
    ['lc-1', { id: 'lc-1', type: 'local-candidate', candidateType: 'relay' }],
  ]);

  it('emits selected-pair-unresolved when connected resolves no pair', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    const transport = makeSendTransportWithEvents(UNRESOLVABLE_STATS);
    mockCreateSendTransport.mockReturnValueOnce(transport);
    await joinWith([STUN, TURN]);

    transport._fire('connectionstatechange', 'connected');
    await vi.waitFor(() => {
      expect(debug).toHaveBeenCalledWith('[ice] selected-pair-unresolved', { label: 'send' });
    });
    expect(debug).not.toHaveBeenCalledWith('[ice] selected-pair', expect.anything());
    debug.mockRestore();
  });

  it('re-arms after an unresolved read so a genuine reconnect retries', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    const transport = makeSendTransportWithEvents(UNRESOLVABLE_STATS);
    mockCreateSendTransport.mockReturnValueOnce(transport);
    await joinWith([STUN, TURN]);

    transport._fire('connectionstatechange', 'connected');
    await vi.waitFor(() =>
      expect(debug).toHaveBeenCalledWith('[ice] selected-pair-unresolved', { label: 'send' })
    );

    // A reconnect CHANGES the state value, so mediasoup does not dedupe it
    // (Transport.js:794) -- unlike ICE `completed`, which collapses onto the
    // already-seen 'connected' and is why the first read is otherwise final.
    transport.getStats = vi.fn().mockResolvedValue(RELAY_STATS);
    transport._fire('connectionstatechange', 'disconnected');
    transport._fire('connectionstatechange', 'connected');
    await vi.waitFor(() => {
      expect(debug).toHaveBeenCalledWith('[ice] selected-pair', { label: 'send', type: 'relay' });
    });
    debug.mockRestore();
  });

  // The reconnect arrives WHILE the first read is still in flight, which is the
  // case the plain re-arm test cannot see: it waits for the first read to settle
  // before firing the transition, so `inFlight` is already clear by then.
  // mediasoup does not replay a state event we declined to act on, so an event
  // dropped here is lost for good and the documented retry never happens.
  it('retries a reconnect observed while the first read was still in flight', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    let settleFirst!: (v: unknown) => void;
    const pending = new Promise((resolve) => {
      settleFirst = resolve;
    });
    const transport = makeSendTransportWithEvents(UNRESOLVABLE_STATS);
    transport.getStats = vi.fn().mockReturnValueOnce(pending).mockResolvedValue(RELAY_STATS);
    mockCreateSendTransport.mockReturnValueOnce(transport);
    await joinWith([STUN, TURN]);

    transport._fire('connectionstatechange', 'connected');
    // Reconnect DURING the in-flight read. This is the event that must survive.
    transport._fire('connectionstatechange', 'disconnected');
    transport._fire('connectionstatechange', 'connected');
    // Only now does the first read settle, resolving nothing.
    settleFirst(UNRESOLVABLE_STATS);

    await vi.waitFor(() => {
      expect(
        debug,
        'a reconnect seen during an in-flight read must still produce a retry: ' +
          'mediasoup never replays the dropped state event, so the retry invariant ' +
          'documented in the platform-behavior register is otherwise unmet'
      ).toHaveBeenCalledWith('[ice] selected-pair', { label: 'send', type: 'relay' });
    });
    expect(transport.getStats).toHaveBeenCalledTimes(2);
    debug.mockRestore();
  });

  // Rapid flap: the QUEUED connection ends before the pending read settles.
  // Replaying it would measure a transport that is no longer connected, and the
  // runtime-proof marker would then mean "disconnected" as well as "the engine
  // timing contract broke" -- the same precision loss as folding a rejected read
  // into `-unresolved`, arriving by a different route.
  it('does not replay a queued reconnect that has itself already disconnected', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    let settleFirst!: (v: unknown) => void;
    const pending = new Promise((resolve) => {
      settleFirst = resolve;
    });
    const transport = makeSendTransportWithEvents(UNRESOLVABLE_STATS);
    transport.getStats = vi.fn().mockReturnValueOnce(pending).mockResolvedValue(RELAY_STATS);
    mockCreateSendTransport.mockReturnValueOnce(transport);
    await joinWith([STUN, TURN]);

    transport._fire('connectionstatechange', 'connected');
    transport._fire('connectionstatechange', 'disconnected');
    transport._fire('connectionstatechange', 'connected');
    transport._fire('connectionstatechange', 'disconnected');
    settleFirst(UNRESOLVABLE_STATS);
    // Drain the whole then/catch/finally chain on a MACROTASK. `vi.waitFor` is
    // useless here: getStats was already called once synchronously, so a
    // "has been called" predicate resolves immediately and the count assertion
    // would run before `.finally()` ever executes -- passing for the wrong
    // reason. Verified: without this the guard's own mutation goes undetected.
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(
      transport.getStats,
      'the queued connection ended before the read settled, so replaying it would ' +
        'measure a transport that is no longer connected'
    ).toHaveBeenCalledTimes(1);
    expect(
      debug,
      'the runtime-proof marker must not fire for a merely-disconnected transport'
    ).not.toHaveBeenCalledWith('[ice] selected-pair-unresolved', expect.anything());
    debug.mockRestore();
  });

  // The other half: a read that RESOLVES after its own connection ended must not
  // latch `recorded`, or a measurement from a dead connection permanently
  // suppresses the next real one.
  it('does not latch a result whose connection ended mid-read', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    let settleFirst!: (v: unknown) => void;
    const pending = new Promise((resolve) => {
      settleFirst = resolve;
    });
    const transport = makeSendTransportWithEvents(RELAY_STATS);
    transport.getStats = vi.fn().mockReturnValueOnce(pending).mockResolvedValue(RELAY_STATS);
    mockCreateSendTransport.mockReturnValueOnce(transport);
    await joinWith([STUN, TURN]);

    transport._fire('connectionstatechange', 'connected');
    transport._fire('connectionstatechange', 'disconnected');
    // Stats retained from the connection that just ended.
    settleFirst(RELAY_STATS);
    await vi.waitFor(() => expect(transport.getStats).toHaveBeenCalled());
    expect(
      debug,
      'a result belonging to an ended connection must not be reported'
    ).not.toHaveBeenCalledWith('[ice] selected-pair', expect.anything());

    // ...and the next genuine connection must still be measurable.
    transport._fire('connectionstatechange', 'connected');
    await vi.waitFor(() => {
      expect(debug).toHaveBeenCalledWith('[ice] selected-pair', { label: 'send', type: 'relay' });
    });
    debug.mockRestore();
  });

  it('does not re-read once a pair has been recorded', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    const transport = makeSendTransportWithEvents(RELAY_STATS);
    mockCreateSendTransport.mockReturnValueOnce(transport);
    await joinWith([STUN, TURN]);

    transport._fire('connectionstatechange', 'connected');
    await vi.waitFor(() =>
      expect(debug).toHaveBeenCalledWith('[ice] selected-pair', { label: 'send', type: 'relay' })
    );
    transport._fire('connectionstatechange', 'disconnected');
    transport._fire('connectionstatechange', 'connected');
    await vi.waitFor(() => expect(transport.getStats).toHaveBeenCalled());
    expect(transport.getStats).toHaveBeenCalledTimes(1);
    debug.mockRestore();
  });
});

// ---------------------------------------------------------------------------
// extractSelectedCandidatePairType — Chrome-shaped `transport` reads
// (reproduction for the ICE selected-pair line never emitting)
// ---------------------------------------------------------------------------

describe('extractSelectedCandidatePairType — Chrome transport-entry defect', () => {
  /**
   * ORACLE (verbatim):
   * Given a Chrome-shaped `RTCStatsReport` captured at the moment ICE reaches
   * `connected`, `extractSelectedCandidatePairType` returns the selected local
   * candidate's `candidateType` — it must not return `null` merely because
   * nomination has not yet completed, when the report's `transport` entry
   * names the selected pair outright via `selectedCandidatePairId`.
   */

  /**
   * A faithful Chrome stats report captured the instant `iceConnectionState`
   * becomes `'connected'` (i.e. the ONE observation mediasoup-client's
   * `Transport.js:794` fires — before nomination completes). Every entry
   * carries an `id` field equal to its Map key, as a real `RTCStatsReport` does.
   */
  function chromeConnectedStats(): Map<string, unknown> {
    return new Map<string, unknown>([
      [
        'transport-1',
        {
          type: 'transport',
          id: 'transport-1',
          dtlsState: 'connected',
          selectedCandidatePairId: 'cp-succeeded',
          bytesSent: 48213,
          bytesReceived: 91022,
        },
      ],
      [
        'cp-succeeded',
        {
          type: 'candidate-pair',
          id: 'cp-succeeded',
          state: 'succeeded',
          nominated: false, // Chrome: false at the moment iceConnectionState -> 'connected'
          // No `selected` key at all — Chrome omits this legacy field entirely.
          localCandidateId: 'lc-relay',
          remoteCandidateId: 'rc-1',
        },
      ],
      [
        'cp-inprogress',
        {
          type: 'candidate-pair',
          id: 'cp-inprogress',
          state: 'in-progress',
          nominated: false,
          localCandidateId: 'lc-host',
          remoteCandidateId: 'rc-1',
        },
      ],
      [
        'lc-relay',
        {
          type: 'local-candidate',
          id: 'lc-relay',
          candidateType: 'relay',
          protocol: 'udp',
          address: '203.0.113.5',
          port: 54321,
        },
      ],
      [
        'lc-host',
        {
          type: 'local-candidate',
          id: 'lc-host',
          candidateType: 'host',
          protocol: 'udp',
          address: '192.168.1.5',
          port: 55000,
        },
      ],
      [
        'rc-1',
        {
          type: 'remote-candidate',
          id: 'rc-1',
          candidateType: 'host',
          protocol: 'udp',
          address: '198.51.100.9',
          port: 3478,
        },
      ],
      [
        'rtp-out-1',
        {
          type: 'outbound-rtp',
          id: 'rtp-out-1',
          kind: 'audio',
          packetsSent: 500,
          bytesSent: 40000,
        },
      ],
      [
        'rtp-in-1',
        {
          type: 'inbound-rtp',
          id: 'rtp-in-1',
          kind: 'audio',
          packetsReceived: 480,
          bytesReceived: 38000,
        },
      ],
      ['codec-1', { type: 'codec', id: 'codec-1', mimeType: 'audio/opus', clockRate: 48000 }],
    ]);
  }

  // regression for the [ice] selected-pair line never emitting
  it('reads the transport entry selectedCandidatePairId when nomination has not completed', () => {
    const stats = chromeConnectedStats();
    const result = extractSelectedCandidatePairType(stats as unknown as RTCStatsReport);
    expect(
      result,
      'extractSelectedCandidatePairType must resolve the LOCAL candidateType of the pair ' +
        "named by the transport entry's selectedCandidatePairId, not require nominated===true"
    ).toBe('relay');
  });

  // --- Guard rails: must pass both BEFORE and AFTER any fix ---

  it('still returns the type via the existing nominated-true path', () => {
    const stats = new Map<string, unknown>([
      [
        'cp-1',
        {
          type: 'candidate-pair',
          id: 'cp-1',
          state: 'succeeded',
          nominated: true,
          localCandidateId: 'lc-1',
        },
      ],
      ['lc-1', { type: 'local-candidate', id: 'lc-1', candidateType: 'relay' }],
    ]);
    expect(extractSelectedCandidatePairType(stats as unknown as RTCStatsReport)).toBe('relay');
  });

  it('returns null when there is no succeeded candidate pair at all', () => {
    const stats = new Map<string, unknown>([
      [
        'cp-1',
        {
          type: 'candidate-pair',
          id: 'cp-1',
          state: 'failed',
          nominated: false,
          localCandidateId: 'lc-1',
        },
      ],
      ['lc-1', { type: 'local-candidate', id: 'lc-1', candidateType: 'relay' }],
    ]);
    expect(extractSelectedCandidatePairType(stats as unknown as RTCStatsReport)).toBeNull();
  });

  it('returns null rather than throwing when selectedCandidatePairId points at a missing id', () => {
    const stats = new Map<string, unknown>([
      [
        'transport-1',
        {
          type: 'transport',
          id: 'transport-1',
          selectedCandidatePairId: 'does-not-exist',
        },
      ],
    ]);
    expect(() =>
      extractSelectedCandidatePairType(stats as unknown as RTCStatsReport)
    ).not.toThrow();
    expect(extractSelectedCandidatePairType(stats as unknown as RTCStatsReport)).toBeNull();
  });

  it('returns null when the selected local candidate has an unknown candidateType', () => {
    const stats = new Map<string, unknown>([
      [
        'cp-1',
        {
          type: 'candidate-pair',
          id: 'cp-1',
          state: 'succeeded',
          nominated: true,
          localCandidateId: 'lc-1',
        },
      ],
      ['lc-1', { type: 'local-candidate', id: 'lc-1', candidateType: 'bogus' }],
    ]);
    expect(extractSelectedCandidatePairType(stats as unknown as RTCStatsReport)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// NC-1: no value derived from ice_servers may reach console.* — the bug-report
// ring buffer (logBufferService) feeds a PUBLIC repo, and its scrub PATTERNS
// do not cover a `<expiry>:<userID>` TURN username.
// ---------------------------------------------------------------------------

const SENTINEL_USER = 'SENTINEL-USER-8f3a';
const SENTINEL_CRED = 'SENTINEL-CRED-8f3a';
const SENTINEL_HOST = 'sentinel-turn.invalid';
const SENTINELS = [SENTINEL_USER, SENTINEL_CRED, SENTINEL_HOST];

const SENTINEL_ICE = [
  { urls: `stun:${SENTINEL_HOST}:3478` },
  {
    urls: `turn:${SENTINEL_HOST}:3478`,
    username: SENTINEL_USER,
    credential: SENTINEL_CRED,
  },
  {
    urls: `turns:${SENTINEL_HOST}:5349`,
    username: SENTINEL_USER,
    credential: SENTINEL_CRED,
  },
];

/**
 * Serialize an arbitrary console argument list so a sentinel cannot hide inside
 * an Error, a Map, a Set, or a class instance whose own enumerable properties
 * JSON.stringify would skip.
 */
function serializeArgs(args: unknown[]): string {
  const seen = new WeakSet<object>();
  return JSON.stringify(args, (_key, value: unknown) => {
    if (typeof value === 'string') return value;
    if (typeof value === 'bigint' || typeof value === 'symbol') return String(value);
    if (typeof value === 'function') return `[fn ${value.name}]`;
    if (value instanceof Error) {
      return {
        name: value.name,
        message: value.message,
        stack: value.stack,
        cause: value.cause,
      };
    }
    if (value instanceof Map) return { __map: [...value.entries()] };
    if (value instanceof Set) return { __set: [...value.values()] };
    if (typeof value === 'object' && value !== null) {
      if (seen.has(value)) return '[circular]';
      seen.add(value);
      // Own + inherited enumerable properties, so a class instance is covered.
      const flat: Record<string, unknown> = {};
      for (const k in value as Record<string, unknown>)
        flat[k] = (value as Record<string, unknown>)[k];
      return { ...flat, ...(value as Record<string, unknown>) };
    }
    return value;
  });
}

describe('NC-1: ICE credentials never reach console.* (#3104)', () => {
  const captured: unknown[][] = [];
  let spies: Array<ReturnType<typeof vi.spyOn>> = [];

  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    for (const k of Object.keys(socketListeners)) delete socketListeners[k];
    captured.length = 0;
    spies = (['log', 'warn', 'error', 'debug', 'info'] as const).map((level) =>
      vi.spyOn(console, level).mockImplementation((...args: unknown[]) => {
        captured.push(args);
      })
    );
  });

  afterEach(async () => {
    await voiceService.leaveChannel().catch(() => undefined);
    for (const s of spies) s.mockRestore();
  });

  function expectNoSentinel() {
    const blob = captured.map(serializeArgs).join('\n');
    for (const s of SENTINELS) expect(blob).not.toContain(s);
  }

  it('emits no sentinel on the initial join path', async () => {
    await joinWith(SENTINEL_ICE);
    expect(captured.length).toBeGreaterThan(0); // the scheme+count line fired
    expectNoSentinel();
  });

  it('emits no sentinel on the reconnect resume path', async () => {
    await joinWith(SENTINEL_ICE);
    socketListeners['disconnect']?.forEach((cb) => cb('transport close'));
    mockSocket.connected = true;
    socketListeners['connect']?.forEach((cb) => cb());
    await vi.waitFor(() => {
      expect(mockCreateSendTransport.mock.calls.length).toBeGreaterThan(1);
    });
    expectNoSentinel();
  });

  it('emits no sentinel when transport creation fails', async () => {
    mockSocket.emit.mockImplementation((event: string, _p: unknown, ack?: (r: unknown) => void) => {
      if (!ack) return;
      if (event === 'join-room') {
        ack({
          rtpCapabilities: mockDeviceRtpCapabilities,
          mediaFrameCryptoVersion: 5,
          existingProducers: [],
          participants: [],
          channelName: 'General',
        });
        return;
      }
      if (event === 'create-transport') {
        ack({ error: 'transport creation refused' });
        return;
      }
      ack({});
    });
    await joinWith(SENTINEL_ICE).catch(() => undefined);
    expectNoSentinel();
  });

  it('emits only the scheme+count shape for the ICE list', async () => {
    await joinWith(SENTINEL_ICE);
    const line = captured.find((a) => a[0] === '[ice] servers');
    expect(line).toBeDefined();
    expect(line?.[1]).toEqual({ count: 3, schemes: ['stun', 'turn', 'turns'] });
    expect(line).toHaveLength(2);
  });
});
