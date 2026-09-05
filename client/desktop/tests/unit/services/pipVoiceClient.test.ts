import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { MockBroadcastChannel, createRpcResponder } from '../../helpers/broadcastChannelMock';
import type { AutoResponder } from '../../helpers/broadcastChannelMock';
import { resetAllStores } from '../../helpers/store-helpers';
import { deferred } from '../../helpers/deferred';

/**
 * #3104 D6: the client no longer opens `concord-pip`. It pulls a per-window
 * capability over `pip:session` and opens `concord-pip:<token>` inside `init()`,
 * so tests must supply the capability and look the channel up by prefix.
 */
const PIP_SESSION_TOKEN = 'test-session-token';
const PIP_CHANNEL = `concord-pip:${PIP_SESSION_TOKEN}`;

/**
 * The client's private session channel, once `init()` has opened it. Searches
 * `all` rather than `instances` so a post-teardown assertion can still read what
 * the channel posted — `close()` removes it from the live-delivery list.
 */
function pipChannel(): MockBroadcastChannel | undefined {
  return MockBroadcastChannel.all.filter((c) => c.name === PIP_CHANNEL).at(-1);
}

/** Arm a responder that survives the channel not existing yet. */
function setResponder(fn: AutoResponder): void {
  MockBroadcastChannel.defaultAutoResponder = fn;
  const ch = pipChannel();
  if (ch) ch.autoResponder = fn;
}

// ── mediasoup-client mock ───────────────────────────────────────────────

const mockConsumerClose = vi.fn();
const mockConsumerOn = vi.fn();
const mockTransportClose = vi.fn();
const mockTransportOn = vi.fn();
const mockTransportGetStats = vi.fn();
const pipTransportHandlers: Record<string, (...args: any[]) => unknown> = {};

// ── E2EE globals (2026-08-21 PiP E2EE gap) ────────────────────────────────────────────────
// The PiP attaches a decrypt transform at receiver creation and refuses to play
// a stream without one, so every consume() path needs a working engine modelled
// here. Suites that assert the fail-closed behaviour live in
// pipVoiceClient.e2ee.test.ts.

class StubRTCRtpScriptTransform {
  constructor(
    public worker: unknown,
    public options: unknown
  ) {}
}
vi.stubGlobal('RTCRtpScriptTransform', StubRTCRtpScriptTransform);

const mockWorkerPostMessage = vi.fn();
const mockWorkerTerminate = vi.fn();
class StubWorker {
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: ((e: unknown) => void) | null = null;
  postMessage = mockWorkerPostMessage;
  terminate = mockWorkerTerminate;
}
vi.stubGlobal('Worker', StubWorker as unknown as typeof Worker);

/** A receiver whose `transform` setter is a plain property, as in Chromium. */
function makeStubReceiver(): { transform: unknown; getStats: () => Promise<Map<string, unknown>> } {
  return { transform: null, getStats: async () => new Map() };
}

const mockTransportConsume = vi.fn().mockResolvedValue({
  id: 'consumer-1',
  producerId: 'producer-1',
  kind: 'audio',
  track: { id: 'track-1', kind: 'audio' },
  close: mockConsumerClose,
  on: mockConsumerOn,
});

const mockCreateRecvTransport = vi.fn().mockReturnValue({
  id: 'transport-1',
  consume: mockTransportConsume,
  on: mockTransportOn,
  getStats: mockTransportGetStats,
  close: mockTransportClose,
});

const mockDeviceLoad = vi.fn().mockResolvedValue(undefined);
const mockRtpCaps = {
  codecs: [{ mimeType: 'audio/opus', kind: 'audio', clockRate: 48000, channels: 2 }],
};

vi.mock('mediasoup-client', () => ({
  Device: vi.fn().mockImplementation(function (this: any) {
    this.load = mockDeviceLoad;
    this.rtpCapabilities = mockRtpCaps;
    this.createRecvTransport = mockCreateRecvTransport;
    this.loaded = true;
  }),
  types: {},
}));

// ── Import after mocks ──────────────────────────────────────────────────

import { PipVoiceClient } from '@/renderer/services/voice/pipVoiceClient';

// ── Helpers ─────────────────────────────────────────────────────────────

const mockVoiceState = {
  participants: {
    'user-1': {
      userId: 'user-1',
      username: 'alice',
      isMuted: false,
      isDeafened: false,
      isVideoOn: false,
      isScreenSharing: false,
      isSpeaking: false,
    },
  },
  tunedInScreenShares: {},
  routerRtpCapabilities: mockRtpCaps,
  activeProducers: [{ producerId: 'prod-1', userId: 'user-1', source: 'mic' }],
  localUserId: 'user-1',
};

const defaultRpcResponses: Record<string, unknown> = {
  'request-state': mockVoiceState,
  'create-recv-transport': {
    transportId: 'transport-1',
    iceParameters: { usernameFragment: 'frag', password: 'pass' },
    iceCandidates: [],
    dtlsParameters: { role: 'auto', fingerprints: [] },
  },
  'connect-transport': { success: true },
  consume: {
    consumerId: 'consumer-1',
    producerId: 'prod-1',
    kind: 'audio',
    rtpParameters: { codecs: [], headerExtensions: [], encodings: [] },
  },
  'resume-consumer': { success: true },
  'get-frame-key': { key: { type: 'secret' }, keyVersion: 1, keyId: 0 },
  'pause-consumer': { success: true },
  'set-preferred-layers': { success: true },
  action: { success: true },
  'pip-ready': { success: true, pausedCount: 0 },
  'pip-closing': { success: true },
  'close-consumer': { success: true },
  'close-recv-transport': { success: true },
};

/** Set up auto-responder on the client's broadcast channel */
function setupAutoResponder(overrides: Record<string, unknown> = {}): void {
  setResponder(createRpcResponder({ ...defaultRpcResponses, ...overrides }));
}

function selectedRelayStats(): RTCStatsReport {
  return new Map<string, Record<string, unknown>>([
    ['transport', { type: 'transport', selectedCandidatePairId: 'pair-1' }],
    ['pair-1', { type: 'candidate-pair', localCandidateId: 'local-1' }],
    ['local-1', { type: 'local-candidate', candidateType: 'relay' }],
  ]) as unknown as RTCStatsReport;
}

interface CleanupResponderOptions {
  consumerIds?: string[];
  consumerErrors?: Set<string>;
  consumerTimeouts?: Set<string>;
  transportError?: string;
}

/** Arm the cleanup responder and hand back the live session channel. */
function withCleanupResponder(options: CleanupResponderOptions = {}): MockBroadcastChannel {
  setupCleanupResponder(options);
  // The channel does not exist yet — `init()` opens it. Bind on first read.
  let bound: MockBroadcastChannel | undefined;
  return new Proxy({} as MockBroadcastChannel, {
    get(_target, prop) {
      bound ??= pipChannel();
      if (!bound) throw new Error('No PiP session channel — init() has not opened one');
      return Reflect.get(bound, prop);
    },
  });
}

function setupCleanupResponder(options: CleanupResponderOptions = {}): void {
  const consumerIds = options.consumerIds ?? ['consumer-1'];
  let consumeIndex = 0;

  setResponder((data: unknown) => {
    const msg = data as {
      kind?: string;
      id?: string;
      method?: string;
      params?: { consumerId?: string };
    };
    if (msg.kind !== 'rpc-request' || !msg.id || !msg.method) return undefined;

    if (msg.method === 'consume') {
      const consumerId = consumerIds[consumeIndex++] ?? consumerIds.at(-1) ?? 'consumer-1';
      return {
        kind: 'rpc-response',
        id: msg.id,
        result: {
          consumerId,
          producerId: 'producer-' + consumeIndex,
          kind: 'audio',
          rtpParameters: { codecs: [], headerExtensions: [], encodings: [] },
        },
      };
    }

    if (msg.method === 'close-consumer') {
      const consumerId = msg.params?.consumerId ?? '';
      if (options.consumerTimeouts?.has(consumerId)) return undefined;
      if (options.consumerErrors?.has(consumerId)) {
        return { kind: 'rpc-response', id: msg.id, error: 'consumer cleanup rejected' };
      }
    }

    if (msg.method === 'close-recv-transport' && options.transportError) {
      return { kind: 'rpc-response', id: msg.id, error: options.transportError };
    }

    const result = defaultRpcResponses[msg.method];
    if (result === undefined) {
      return { kind: 'rpc-response', id: msg.id, error: 'No mock for ' + msg.method };
    }
    return { kind: 'rpc-response', id: msg.id, result };
  });
}

function cleanupRequests(ch: MockBroadcastChannel): Array<{
  method: string;
  params: Record<string, string>;
}> {
  return ch.posted.filter(
    (
      message
    ): message is { kind: 'rpc-request'; method: string; params: Record<string, string> } => {
      const method = (message as { method?: string }).method;
      return (
        (message as { kind?: string }).kind === 'rpc-request' &&
        (method === 'pip-closing' ||
          method === 'close-consumer' ||
          method === 'close-recv-transport')
      );
    }
  );
}

// Mock MediaStream
class MockMediaStream {
  readonly tracks: unknown[];
  constructor(tracks: unknown[] = []) {
    this.tracks = tracks;
  }
  getTracks() {
    return this.tracks;
  }
}

// ── Tests ───────────────────────────────────────────────────────────────

describe('PipVoiceClient', () => {
  let client: PipVoiceClient;

  let savedMediaStream: unknown;
  let savedElectron: unknown;

  beforeEach(() => {
    resetAllStores();
    vi.useFakeTimers();
    MockBroadcastChannel.install();
    // #3104 D6: init() refuses to open any channel without this capability.
    savedElectron = globalThis.electron;
    (globalThis as unknown as { electron: unknown }).electron = {
      ...(savedElectron as object),
      getPipSession: vi.fn().mockResolvedValue({ token: PIP_SESSION_TOKEN }),
    };
    savedMediaStream = globalThis.MediaStream;
    (globalThis as any).MediaStream = MockMediaStream;
    vi.clearAllMocks();
    for (const key of Object.keys(pipTransportHandlers)) delete pipTransportHandlers[key];
    mockTransportOn.mockImplementation((event: string, handler: (...args: any[]) => unknown) => {
      pipTransportHandlers[event] = handler;
    });
    mockTransportGetStats.mockResolvedValue(new Map());
    mockTransportConsume
      .mockReset()
      .mockImplementation(
        async ({
          id,
          producerId,
          kind,
          onRtpReceiver,
        }: {
          id: string;
          producerId: string;
          kind: string;
          onRtpReceiver?: (receiver: unknown) => void;
        }) => {
          // Model a conforming engine: mediasoup invokes onRtpReceiver during
          // consume, which is where the decrypt transform attaches.
          const rtpReceiver = makeStubReceiver();
          onRtpReceiver?.(rtpReceiver);
          return {
            id,
            producerId,
            kind,
            track: { id: 'track-' + id, kind },
            rtpReceiver,
            rtpParameters: { codecs: [{ mimeType: 'audio/opus' }] },
            closed: false,
            close: mockConsumerClose,
            on: mockConsumerOn,
          };
        }
      );
    mockConsumerClose.mockReset();
    mockWorkerPostMessage.mockReset();
    mockWorkerTerminate.mockReset();
  });

  afterEach(async () => {
    // Dispose may trigger an RPC that needs the timer to expire
    const disposePromise = client?.dispose().catch(() => {});
    vi.advanceTimersByTime(11_000); // Flush any RPC timeouts
    await disposePromise;
    vi.useRealTimers();
    MockBroadcastChannel.uninstall();
    (globalThis as any).MediaStream = savedMediaStream;
    (globalThis as unknown as { electron: unknown }).electron = savedElectron;
  });

  // ── Constructor ─────────────────────────────────────────────────────

  describe('constructor', () => {
    it('opens NO channel until init() proves the window holds a capability (#3104 D6)', async () => {
      client = new PipVoiceClient('controls-main');
      expect(MockBroadcastChannel.instances).toHaveLength(0);

      setupAutoResponder();
      await client.init();

      expect(pipChannel()).toBeDefined();
      // The legacy shared name is never opened — a listener on it is deaf.
      expect(MockBroadcastChannel.instances.filter((c) => c.name === 'concord-pip')).toHaveLength(
        0
      );
    });

    it('stores pipId', () => {
      client = new PipVoiceClient('frames-123');
      // Verify pipId is used in RPC requests
      setupAutoResponder();
      // pipId will appear in messages when init() is called
      expect(client).toBeDefined();
    });
  });

  // ── Session capability, fail-closed (#3104 D6) ──────────────────────

  describe('session capability (#3104 D6)', () => {
    function setPipSession(impl: unknown): void {
      (globalThis as unknown as { electron: Record<string, unknown> }).electron.getPipSession =
        impl as never;
    }

    /** No channel of ANY name was opened — not even the legacy shared one. */
    function expectNoChannelOpened(): void {
      expect(MockBroadcastChannel.all).toHaveLength(0);
    }

    it.each([
      ['the shell is too old to implement pip:session', undefined],
      ['main refuses the caller', vi.fn().mockResolvedValue(null)],
      ['main answers without a token', vi.fn().mockResolvedValue({})],
      ['main answers with a non-string token', vi.fn().mockResolvedValue({ token: 7 })],
      ['main answers with an empty token', vi.fn().mockResolvedValue({ token: '' })],
      ['the invoke rejects', vi.fn().mockRejectedValue(new Error('ipc down'))],
    ])('refuses to signal at all when %s', async (_label, impl) => {
      setPipSession(impl);
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      await expect(client.init()).rejects.toThrow('PiP session capability unavailable');
      expectNoChannelOpened();
    });

    it('does not fall back to an unauthenticated channel for later RPCs either', async () => {
      setPipSession(vi.fn().mockResolvedValue(null));
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      await expect(client.init()).rejects.toThrow('PiP session capability unavailable');
      await expect(client.action('leave')).rejects.toThrow('PiP session capability unavailable');
      expectNoChannelOpened();
    });

    it('asks main for THIS window id and opens the channel that token names', async () => {
      const getPipSession = vi.fn().mockResolvedValue({ token: PIP_SESSION_TOKEN });
      setPipSession(getPipSession);
      client = new PipVoiceClient('frames-42');
      setupAutoResponder();

      await client.init();

      expect(getPipSession).toHaveBeenCalledWith('frames-42');
      expect(MockBroadcastChannel.all.map((c) => c.name)).toEqual([PIP_CHANNEL]);
    });
  });

  // ── init() ──────────────────────────────────────────────────────────

  describe('init()', () => {
    it('sends request-state RPC and receives voice state', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      const state = await client.init();

      expect(state.participants['user-1'].username).toBe('alice');
      expect(state.localUserId).toBe('user-1');
      expect(state.activeProducers).toHaveLength(1);
    });

    it('creates mediasoup Device and loads with router RTP capabilities', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      await client.init();

      expect(mockDeviceLoad).toHaveBeenCalledWith({
        routerRtpCapabilities: mockRtpCaps,
      });
    });

    it('creates recv transport with server parameters', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      await client.init();

      expect(mockCreateRecvTransport).toHaveBeenCalledWith(
        expect.objectContaining({
          id: 'transport-1',
        })
      );
    });

    it('throws if no routerRtpCapabilities in state', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder({
        'request-state': { ...mockVoiceState, routerRtpCapabilities: null },
      });

      await expect(client.init()).rejects.toThrow('No router RTP capabilities available');
    });

    it('retries request-state RPC on timeout and succeeds on later attempt', async () => {
      client = new PipVoiceClient('test-pip');
      let callCount = 0;

      // First two calls timeout (no response), third succeeds
      setResponder((data: unknown) => {
        const msg = data as { kind?: string; id?: string; method?: string };
        if (msg.kind !== 'rpc-request' || !msg.id) return undefined;
        callCount++;
        if (msg.method === 'request-state' && callCount <= 2) {
          return undefined; // No response → will timeout
        }
        const responses: Record<string, unknown> = { ...defaultRpcResponses };
        const result = responses[msg.method ?? ''];
        if (result === undefined) return { kind: 'rpc-response', id: msg.id, error: 'no mock' };
        return { kind: 'rpc-response', id: msg.id, result };
      });

      // Advance timers to trigger the 3s init timeout + 1s retry delays
      const initPromise = client.init();
      // First attempt times out after 3s
      await vi.advanceTimersByTimeAsync(3_100);
      // Retry delay 1s
      await vi.advanceTimersByTimeAsync(1_100);
      // Second attempt times out after 3s
      await vi.advanceTimersByTimeAsync(3_100);
      // Retry delay 1s
      await vi.advanceTimersByTimeAsync(1_100);
      // Third attempt should succeed (auto-responder responds synchronously)

      const state = await initPromise;
      expect(state.participants['user-1'].username).toBe('alice');
      expect(callCount).toBeGreaterThanOrEqual(3);
    });

    it('throws after exhausting all retry attempts', async () => {
      client = new PipVoiceClient('test-pip');
      // No auto-responder — all attempts will timeout

      const initPromise = client.init().catch((err: Error) => err);

      // Advance through all 3 attempts (3s timeout each + 1s delay between)
      for (let i = 0; i < 3; i++) {
        await vi.advanceTimersByTimeAsync(3_100);
        if (i < 2) await vi.advanceTimersByTimeAsync(1_100);
      }

      const result = await initPromise;
      expect(result).toBeInstanceOf(Error);
      expect((result as Error).message).toContain('RPC timeout');
    });

    it('does not retry on non-timeout errors (fails immediately)', async () => {
      client = new PipVoiceClient('test-pip');

      // Auto-responder returns an explicit error (not a timeout)
      setResponder((data: unknown) => {
        const msg = data as { kind?: string; id?: string; method?: string };
        if (msg.kind !== 'rpc-request') return undefined;
        return { kind: 'rpc-response', id: msg.id, error: 'proxy error' };
      });

      // Should fail immediately without retrying
      await expect(client.init()).rejects.toThrow('proxy error');
    });
  });

  // ── PiP selected ICE candidate diagnostic ───────────────────────────

  describe('PiP selected candidate diagnostic', () => {
    it('observes the recv transport and records one closed-enum candidate after connected twice', async () => {
      mockTransportGetStats.mockResolvedValue(selectedRelayStats());
      const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      await client.init();

      expect(pipTransportHandlers.connectionstatechange).toBeTypeOf(
        'function',
        'PiP receive transport must install a connectionstatechange observer for ICE diagnostics'
      );
      pipTransportHandlers.connectionstatechange?.('connected');
      pipTransportHandlers.connectionstatechange?.('connected');

      await vi.waitFor(() => expect(mockTransportGetStats).toHaveBeenCalledTimes(1));
      const selectedPairLogs = debug.mock.calls.filter(
        ([message]) => message === '[ice] selected-pair'
      );
      expect(selectedPairLogs).toHaveLength(1);
      expect(selectedPairLogs[0]).toEqual([
        '[ice] selected-pair',
        { label: 'pip-recv', type: 'relay' },
      ]);
      debug.mockRestore();
    });

    it('does not record a stale selected pair when the transport disconnects during stats', async () => {
      const stats = deferred<RTCStatsReport>();
      mockTransportGetStats.mockReturnValue(stats.promise);
      const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      await client.init();
      expect(pipTransportHandlers.connectionstatechange).toBeTypeOf('function');
      pipTransportHandlers.connectionstatechange?.('connected');
      pipTransportHandlers.connectionstatechange?.('disconnected');
      stats.resolve(selectedRelayStats());
      await Promise.resolve();

      expect(
        debug.mock.calls.filter(
          ([message]) => typeof message === 'string' && message.startsWith('[ice] selected-pair')
        )
      ).toHaveLength(0);
      debug.mockRestore();
    });

    it('records a fixed-label diagnostic when stats rejects', async () => {
      mockTransportGetStats.mockRejectedValue(new Error('stats unavailable'));
      const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      await client.init();
      expect(pipTransportHandlers.connectionstatechange).toBeTypeOf('function');
      pipTransportHandlers.connectionstatechange?.('connected');
      await vi.waitFor(() =>
        expect(debug.mock.calls).toContainEqual([
          '[ice] selected-pair-stats-unavailable',
          { label: 'pip-recv' },
        ])
      );
      expect(debug.mock.calls.flat()).not.toContain('stats unavailable');
      debug.mockRestore();
    });

    it('records unresolved candidate stats with the fixed PiP label', async () => {
      mockTransportGetStats.mockResolvedValue(new Map());
      const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      await client.init();
      pipTransportHandlers.connectionstatechange?.('connected');
      await vi.waitFor(() =>
        expect(debug.mock.calls).toContainEqual([
          '[ice] selected-pair-unresolved',
          { label: 'pip-recv' },
        ])
      );
      debug.mockRestore();
    });

    it('queues one reconnect stats read and records the live pair', async () => {
      const first = deferred<RTCStatsReport>();
      const second = deferred<RTCStatsReport>();
      mockTransportGetStats.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
      const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      await client.init();
      pipTransportHandlers.connectionstatechange?.('connected');
      pipTransportHandlers.connectionstatechange?.('disconnected');
      pipTransportHandlers.connectionstatechange?.('connected');
      expect(mockTransportGetStats).toHaveBeenCalledTimes(1);

      first.resolve(new Map());
      await vi.waitFor(() => expect(mockTransportGetStats).toHaveBeenCalledTimes(2));
      second.resolve(selectedRelayStats());
      await vi.waitFor(() =>
        expect(debug.mock.calls).toContainEqual([
          '[ice] selected-pair',
          { label: 'pip-recv', type: 'relay' },
        ])
      );
      expect(
        debug.mock.calls.filter(
          ([message]) => typeof message === 'string' && message.startsWith('[ice] selected-pair')
        )
      ).toEqual([['[ice] selected-pair', { label: 'pip-recv', type: 'relay' }]]);
      debug.mockRestore();
    });
  });

  // ── PiP ICE servers (#3104) ─────────────────────────────────────────

  describe('PiP ICE servers (#3104)', () => {
    /** Byte-identical to the fixture the pipSignalingProxy suite attaches, so the
     *  two ends of the create-recv-transport contract are checked against ONE
     *  wire shape rather than against each other's mocks. */
    const TURN = {
      urls: 'turn:turn.example.test:3478',
      username: '1780000000:user-1',
      credential: 'Zm9vYmFyYmF6',
    };

    /** The pre-#3104 create-recv-transport result, before iceServers is layered on. */
    const baseTransportResult = {
      transportId: 'transport-1',
      iceParameters: { usernameFragment: 'frag', password: 'pass' }, // pragma: allowlist secret
      iceCandidates: [],
      dtlsParameters: { role: 'auto', fingerprints: [] },
    };

    it('passes the proxied ICE servers into the recv transport', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder({
        'create-recv-transport': { ...baseTransportResult, iceServers: [TURN] },
      });

      await client.init();

      const opts = mockCreateRecvTransport.mock.calls[0][0];
      expect(opts.iceServers).toEqual([TURN]);
      expect(opts.iceTransportPolicy).toBe('all');
    });

    it('omits the iceServers key when the proxy sends none', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();

      await client.init();

      const opts = mockCreateRecvTransport.mock.calls[0][0];
      expect(Object.hasOwn(opts, 'iceServers')).toBe(false);
      expect(Object.hasOwn(opts, 'iceTransportPolicy')).toBe(false);
    });

    it('omits the iceServers key when the proxy sends an empty list', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder({
        'create-recv-transport': { ...baseTransportResult, iceServers: [] },
      });

      await client.init();

      const opts = mockCreateRecvTransport.mock.calls[0][0];
      expect(Object.hasOwn(opts, 'iceServers')).toBe(false);
      expect(Object.hasOwn(opts, 'iceTransportPolicy')).toBe(false);
    });

    it('drops a malformed entry rather than the whole list', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder({
        'create-recv-transport': {
          ...baseTransportResult,
          iceServers: [{ urls: 'http://nope' }, TURN],
        },
      });

      await client.init();

      expect(mockCreateRecvTransport.mock.calls[0][0].iceServers).toEqual([TURN]);
    });

    it('drops a turn: entry that arrives without a credential pair', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder({
        'create-recv-transport': {
          ...baseTransportResult,
          iceServers: [{ urls: 'turn:turn.example.test:3478' }, TURN],
        },
      });

      await client.init();

      expect(mockCreateRecvTransport.mock.calls[0][0].iceServers).toEqual([TURN]);
    });

    it('survives a non-array iceServers field without failing the transport', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder({
        'create-recv-transport': { ...baseTransportResult, iceServers: 'turn:nope' },
      });

      await client.init();

      const opts = mockCreateRecvTransport.mock.calls[0][0];
      expect(opts.id).toBe('transport-1');
      expect(Object.hasOwn(opts, 'iceServers')).toBe(false);
    });

    it('never logs the transport result or any ICE credential (NC-1)', async () => {
      const spies = (['log', 'warn', 'error', 'debug', 'info'] as const).map((level) =>
        vi.spyOn(console, level).mockImplementation(() => {})
      );
      try {
        client = new PipVoiceClient('test-pip');
        setupAutoResponder({
          'create-recv-transport': { ...baseTransportResult, iceServers: [TURN] },
        });

        await client.init();
        await client.consume('prod-1', 'mic', 'user-1');

        const logged = spies.flatMap((s) => s.mock.calls.map((args) => JSON.stringify(args)));
        for (const line of logged) {
          expect(line).not.toContain(TURN.credential);
          expect(line).not.toContain(TURN.username);
          expect(line).not.toContain('turn.example.test');
        }
      } finally {
        for (const s of spies) s.mockRestore();
      }
    });

    it('emits no credential to console.* on the create-recv-transport path', async () => {
      const captured: unknown[][] = [];
      const spies = (['log', 'warn', 'error', 'debug', 'info'] as const).map((level) =>
        vi.spyOn(console, level).mockImplementation((...args: unknown[]) => {
          captured.push(args);
        })
      );
      client = new PipVoiceClient('test-pip');
      setupAutoResponder({
        'create-recv-transport': {
          transportId: 'transport-1',
          iceParameters: { usernameFragment: 'frag', password: 'pass' }, // pragma: allowlist secret
          iceCandidates: [],
          dtlsParameters: { role: 'auto', fingerprints: [] },
          iceServers: [
            {
              urls: 'turn:sentinel-turn.invalid:3478',
              username: 'SENTINEL-USER-8f3a',
              credential: 'SENTINEL-CRED-8f3a',
            },
          ],
        },
      });
      await client.init();
      const blob = JSON.stringify(captured);
      for (const s of ['SENTINEL-USER-8f3a', 'SENTINEL-CRED-8f3a', 'sentinel-turn.invalid']) {
        expect(blob).not.toContain(s);
      }
      for (const sp of spies) sp.mockRestore();
    });
  });

  // ── consume() ───────────────────────────────────────────────────────

  describe('consume()', () => {
    beforeEach(async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();
    });

    it('sends consume RPC and creates local consumer', async () => {
      const stream = await client.consume('prod-1', 'mic', 'user-1');

      expect(stream).toBeInstanceOf(MockMediaStream);
      expect(mockTransportConsume).toHaveBeenCalledWith(
        expect.objectContaining({
          id: 'consumer-1',
          producerId: 'prod-1',
          kind: 'audio',
        })
      );
    });

    it('sends resume-consumer RPC after creating consumer', async () => {
      await client.consume('prod-1', 'mic', 'user-1');

      // Check that resume-consumer was sent via BroadcastChannel
      const ch = pipChannel();
      const resumeMsg = ch?.posted.find(
        (m: any) => m.kind === 'rpc-request' && m.method === 'resume-consumer'
      ) as any;
      expect(resumeMsg).toBeDefined();
      expect(resumeMsg.params.consumerId).toBe('consumer-1');
    });

    it('returns null when disposed', async () => {
      await client.dispose();
      const result = await client.consume('prod-1', 'mic', 'user-1');
      expect(result).toBeNull();
    });

    it('returns null and logs error on consume failure', async () => {
      mockTransportConsume.mockRejectedValueOnce(new Error('consume fail'));

      const result = await client.consume('prod-1', 'mic', 'user-1');
      expect(result).toBeNull();
    });
  });

  // ── signalReady() & action() ────────────────────────────────────────

  describe('signalReady() & action()', () => {
    beforeEach(async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();
    });

    it('signalReady sends pip-ready RPC with consumed sources', async () => {
      await client.consume('prod-1', 'mic', 'user-1');
      await client.signalReady();

      const ch = pipChannel();
      const readyMsg = ch?.posted.find(
        (m: any) => m.kind === 'rpc-request' && m.method === 'pip-ready'
      ) as any;
      expect(readyMsg).toBeDefined();
      expect(readyMsg.params.consumerSources).toEqual([
        { source: 'mic', producerUserId: 'user-1' },
      ]);
    });

    it('action sends correct action RPC', async () => {
      await client.action('toggle-mute');

      const ch = pipChannel();
      const actionMsg = ch?.posted.find(
        (m: any) => m.kind === 'rpc-request' && m.method === 'action'
      ) as any;
      expect(actionMsg).toBeDefined();
      expect(actionMsg.params.action).toBe('toggle-mute');
    });

    it('action rejects when disposed', async () => {
      await client.dispose();
      await expect(client.action('toggle-mute')).rejects.toThrow('PipVoiceClient disposed');
    });
  });

  // ── getStreams & getStreamBySource ──────────────────────────────────

  describe('getStreams & getStreamBySource', () => {
    beforeEach(async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();
    });

    it('getStreams returns consumed tracks map', async () => {
      await client.consume('prod-1', 'mic', 'user-1');

      const streams = client.getStreams();
      expect(streams.size).toBe(1);
      const entry = streams.values().next().value;
      expect(entry.source).toBe('mic');
      expect(entry.producerUserId).toBe('user-1');
    });

    it('getStreamBySource returns matching stream', async () => {
      await client.consume('prod-1', 'mic', 'user-1');

      const stream = client.getStreamBySource('mic', 'user-1');
      expect(stream).toBeInstanceOf(MockMediaStream);
    });

    it('getStreamBySource returns null for no match', async () => {
      const stream = client.getStreamBySource('camera', 'user-1');
      expect(stream).toBeNull();
    });
  });

  // ── getConsumerIdBySource & reportPreferredLayers (#1924) ────────────
  describe('getConsumerIdBySource & reportPreferredLayers', () => {
    beforeEach(async () => {
      client = new PipVoiceClient('screen-prod-1');
      setupAutoResponder();
      await client.init();
    });

    it('getConsumerIdBySource returns the SFU consumer id for a matching source/user', async () => {
      await client.consume('prod-1', 'screen', 'user-1');
      expect(client.getConsumerIdBySource('screen', 'user-1')).toBe('consumer-1');
    });

    it('getConsumerIdBySource returns null for no match', async () => {
      await client.consume('prod-1', 'screen', 'user-1');
      expect(client.getConsumerIdBySource('screen', 'user-2')).toBeNull();
      expect(client.getConsumerIdBySource('camera', 'user-1')).toBeNull();
    });

    it('reportPreferredLayers proxies a set-preferred-layers RPC with the consumer id + payload', async () => {
      await client.consume('prod-1', 'screen', 'user-1');
      await client.reportPreferredLayers({
        consumerId: 'consumer-1',
        cssWidth: 1920,
        cssHeight: 1080,
        visible: true,
        role: 'focus',
        focusedWindow: true,
      });

      const ch = pipChannel();
      const msg = ch?.posted.find(
        (m: any) => m.kind === 'rpc-request' && m.method === 'set-preferred-layers'
      ) as any;
      expect(msg).toBeDefined();
      expect(msg.params).toEqual({
        consumerId: 'consumer-1',
        cssWidth: 1920,
        cssHeight: 1080,
        visible: true,
        role: 'focus',
        focusedWindow: true,
      });
    });

    it('reportPreferredLayers rejects when disposed (best-effort caller handles it)', async () => {
      await client.dispose();
      await expect(
        client.reportPreferredLayers({
          consumerId: 'consumer-1',
          cssWidth: 1,
          cssHeight: 1,
          visible: true,
          role: 'focus',
          focusedWindow: true,
        })
      ).rejects.toThrow('PipVoiceClient disposed');
    });
  });

  // ── dispose() ───────────────────────────────────────────────────────

  describe('dispose()', () => {
    it('finalizes locally without cleanup RPC timeouts after voice ends', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();
      await client.consume('prod-1', 'mic', 'user-1');

      const ch = pipChannel()!;
      ch.autoResponder = null;
      ch.simulateMessage({ kind: 'broadcast', type: 'voice-ended' });

      const disposePromise = client.dispose();
      const pendingTimers = vi.getTimerCount();
      await vi.runAllTimersAsync();
      await disposePromise;

      expect(pendingTimers).toBe(0);
      expect(cleanupRequests(ch)).toEqual([]);
      expect(mockConsumerClose).toHaveBeenCalledOnce();
      expect(mockTransportClose).toHaveBeenCalledOnce();
    });

    it('cancels an in-flight cleanup timeout when voice ends during disposal', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();
      await client.consume('prod-1', 'mic', 'user-1');

      const ch = pipChannel()!;
      ch.autoResponder = null;

      const disposePromise = client.dispose();
      expect(cleanupRequests(ch).map((request) => request.method)).toEqual(['pip-closing']);

      ch.simulateMessage({ kind: 'broadcast', type: 'voice-ended' });
      const pendingTimers = vi.getTimerCount();
      await vi.runAllTimersAsync();
      await disposePromise;

      expect(pendingTimers).toBe(0);
      expect(cleanupRequests(ch).map((request) => request.method)).toEqual(['pip-closing']);
      expect(mockConsumerClose).toHaveBeenCalledOnce();
      expect(mockTransportClose).toHaveBeenCalledOnce();
    });

    it('sends pip-closing RPC', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();

      // Capture channel reference before dispose closes it
      const ch = pipChannel()!;
      await client.dispose();

      const closingMsg = ch.posted.find(
        (m: any) => m.kind === 'rpc-request' && m.method === 'pip-closing'
      );
      expect(closingMsg).toBeDefined();
    });

    it('retains the server receive transport id and closes owned consumers before the transport', async () => {
      client = new PipVoiceClient('test-pip');
      const ch = withCleanupResponder();
      await client.init();
      await client.consume('prod-1', 'mic', 'user-1');

      await client.dispose();

      expect(cleanupRequests(ch)).toEqual([
        expect.objectContaining({ method: 'pip-closing' }),
        {
          kind: 'rpc-request',
          id: expect.any(String),
          pipId: 'test-pip',
          method: 'close-consumer',
          params: { consumerId: 'consumer-1' },
        },
        {
          kind: 'rpc-request',
          id: expect.any(String),
          pipId: 'test-pip',
          method: 'close-recv-transport',
          params: { transportId: 'transport-1' },
        },
      ]);
    });

    it('closes a receive transport whose acknowledgement arrives after disposal starts', async () => {
      client = new PipVoiceClient('test-pip');
      setResponder((data: unknown) => {
        const msg = data as { kind?: string; id?: string; method?: string };
        if (msg.kind !== 'rpc-request' || !msg.id || !msg.method) return undefined;
        if (msg.method === 'create-recv-transport') return undefined;
        return {
          kind: 'rpc-response',
          id: msg.id,
          result: defaultRpcResponses[msg.method],
        };
      });

      const initPromise = client.init().catch((err: Error) => err);
      // The session channel only exists once init() has obtained the capability.
      await vi.waitFor(() => expect(pipChannel()).toBeDefined());
      const ch = pipChannel()!;
      await vi.waitFor(() => {
        expect(
          ch.posted.some(
            (message: any) =>
              message.kind === 'rpc-request' && message.method === 'create-recv-transport'
          )
        ).toBe(true);
      });
      const createRequest = ch.posted.find(
        (message: any) =>
          message.kind === 'rpc-request' && message.method === 'create-recv-transport'
      ) as { id: string };

      const disposePromise = client.dispose();
      ch.simulateMessage({
        kind: 'rpc-response',
        id: createRequest.id,
        result: {
          ...(defaultRpcResponses['create-recv-transport'] as object),
          transportId: 'late-transport',
        },
      });

      await disposePromise;

      expect(cleanupRequests(ch)).toContainEqual(
        expect.objectContaining({
          method: 'close-recv-transport',
          params: { transportId: 'late-transport' },
        })
      );
      expect(mockCreateRecvTransport).not.toHaveBeenCalled();
      await expect(initPromise).resolves.toBeInstanceOf(Error);
    });

    it('closes the server receive transport when no consumers were created', async () => {
      client = new PipVoiceClient('test-pip');
      const ch = withCleanupResponder();
      await client.init();

      await client.dispose();

      expect(cleanupRequests(ch).map((request) => request.method)).toEqual([
        'pip-closing',
        'close-recv-transport',
      ]);
    });

    it('continues closing remaining consumers and the transport after a consumer close error', async () => {
      client = new PipVoiceClient('test-pip');
      const ch = withCleanupResponder({
        consumerIds: ['consumer-1', 'consumer-2'],
        consumerErrors: new Set(['consumer-1']),
      });
      await client.init();
      await client.consume('prod-1', 'mic', 'user-1');
      await client.consume('prod-2', 'camera', 'user-2');

      await client.dispose();

      expect(cleanupRequests(ch).map((request) => [request.method, request.params])).toEqual([
        ['pip-closing', {}],
        ['close-consumer', { consumerId: 'consumer-1' }],
        ['close-consumer', { consumerId: 'consumer-2' }],
        ['close-recv-transport', { transportId: 'transport-1' }],
      ]);
    });

    it('continues teardown after a consumer close timeout', async () => {
      client = new PipVoiceClient('test-pip');
      const ch = withCleanupResponder({
        consumerIds: ['consumer-1', 'consumer-2'],
        consumerTimeouts: new Set(['consumer-1']),
      });
      await client.init();
      await client.consume('prod-1', 'mic', 'user-1');
      await client.consume('prod-2', 'camera', 'user-2');

      const disposePromise = client.dispose();
      await vi.advanceTimersByTimeAsync(10_100);
      await disposePromise;

      expect(cleanupRequests(ch).map((request) => [request.method, request.params])).toEqual([
        ['pip-closing', {}],
        ['close-consumer', { consumerId: 'consumer-1' }],
        ['close-consumer', { consumerId: 'consumer-2' }],
        ['close-recv-transport', { transportId: 'transport-1' }],
      ]);
    });

    it('bounds consumer cleanup time by closing consumers concurrently before the transport', async () => {
      client = new PipVoiceClient('test-pip');
      const ch = withCleanupResponder({
        consumerIds: ['consumer-1', 'consumer-2'],
        consumerTimeouts: new Set(['consumer-1', 'consumer-2']),
      });
      await client.init();
      await client.consume('prod-1', 'mic', 'user-1');
      await client.consume('prod-2', 'camera', 'user-2');

      const disposePromise = client.dispose();
      await vi.advanceTimersByTimeAsync(10_100);

      expect(cleanupRequests(ch).map((request) => request.method)).toEqual([
        'pip-closing',
        'close-consumer',
        'close-consumer',
        'close-recv-transport',
      ]);
      await disposePromise;
    });
    it('completes local teardown when the server transport close fails', async () => {
      client = new PipVoiceClient('test-pip');
      const ch = withCleanupResponder({ transportError: 'transport cleanup rejected' });
      await client.init();
      await client.consume('prod-1', 'mic', 'user-1');

      await client.dispose();

      expect(cleanupRequests(ch).some((request) => request.method === 'close-recv-transport')).toBe(
        true
      );
      expect(mockConsumerClose).toHaveBeenCalledOnce();
      expect(mockTransportClose).toHaveBeenCalledOnce();
      expect(client.getStreams().size).toBe(0);
    });

    it('continues local finalization when a consumer close throws', async () => {
      client = new PipVoiceClient('test-pip');
      const ch = withCleanupResponder({ consumerIds: ['consumer-1', 'consumer-2'] });
      await client.init();
      await client.consume('prod-1', 'mic', 'user-1');
      await client.consume('prod-2', 'camera', 'user-2');
      mockConsumerClose.mockImplementationOnce(() => {
        throw new Error('local consumer close failed');
      });

      await client.dispose();

      expect(mockConsumerClose).toHaveBeenCalledTimes(2);
      expect(mockTransportClose).toHaveBeenCalledOnce();
      expect(client.getStreams().size).toBe(0);
      expect(MockBroadcastChannel.instances).not.toContain(ch);
    });

    it('emits one remote teardown sequence for concurrent and repeated dispose calls', async () => {
      client = new PipVoiceClient('test-pip');
      const ch = withCleanupResponder();
      await client.init();
      await client.consume('prod-1', 'mic', 'user-1');

      const first = client.dispose();
      const second = client.dispose();

      expect(second).toBe(first);
      await Promise.all([first, second]);
      expect(client.dispose()).toBe(first);
      expect(cleanupRequests(ch).map((request) => request.method)).toEqual([
        'pip-closing',
        'close-consumer',
        'close-recv-transport',
      ]);
    });

    it('rejects pending RPCs when disposal starts without leaving them to time out', async () => {
      client = new PipVoiceClient('test-pip');
      const initPromise = client.init().catch((err: Error) => err);
      await vi.advanceTimersByTimeAsync(100);

      const disposePromise = client.dispose();
      expect(await initPromise).toBeInstanceOf(Error);

      await vi.advanceTimersByTimeAsync(10_100);
      await disposePromise;
    });

    it('closes all consumers and transport', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();
      await client.consume('prod-1', 'mic', 'user-1');

      await client.dispose();

      expect(mockConsumerClose).toHaveBeenCalled();
      expect(mockTransportClose).toHaveBeenCalled();
    });

    it('cancels pending RPCs with rejection', async () => {
      client = new PipVoiceClient('test-pip');
      // No auto-responder — the RPC for request-state will hang until disposed

      // Start init (will send request-state RPC that never gets a response)
      const initPromise = client.init().catch((err: Error) => err);

      // Advance timers so any queued microtasks settle, but not long enough for RPC timeout
      await vi.advanceTimersByTimeAsync(100);

      // Dispose cancels all pending RPCs — pip-closing will also timeout
      const disposePromise = client.dispose().catch(() => {});
      await vi.advanceTimersByTimeAsync(11_000);
      await disposePromise;

      const result = await initPromise;
      expect(result).toBeInstanceOf(Error);
    });

    it('is idempotent — second call is a no-op', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();

      const ch = pipChannel()!;
      await client.dispose();
      const countAfterFirst = ch.posted.length;

      await client.dispose(); // Should not throw or send additional messages
      expect(ch.posted.length).toBe(countAfterFirst);
    });
  });

  // ── Message handling ────────────────────────────────────────────────

  describe('message handling', () => {
    it('routes broadcasts to onStateUpdate callback', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();

      const callback = vi.fn();
      client.onStateUpdate = callback;

      const ch = pipChannel();
      ch?.simulateMessage({
        kind: 'broadcast',
        type: 'state-update',
        participants: {},
        tunedInScreenShares: {},
        localUserId: 'user-1',
      });

      expect(callback).toHaveBeenCalledWith(expect.objectContaining({ type: 'state-update' }));
    });

    it('routes voice-ended broadcasts to onStateUpdate', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();

      const callback = vi.fn();
      client.onStateUpdate = callback;

      const ch = pipChannel();
      ch?.simulateMessage({ kind: 'broadcast', type: 'voice-ended' });

      expect(callback).toHaveBeenCalledWith(expect.objectContaining({ type: 'voice-ended' }));
    });

    it('handles RPC error responses', async () => {
      client = new PipVoiceClient('test-pip');

      // All RPCs return error — init() retries then throws after exhausting attempts
      setResponder((data: unknown) => {
        const msg = data as { kind?: string; id?: string; method?: string };
        if (msg.kind !== 'rpc-request') return undefined;
        return { kind: 'rpc-response', id: msg.id, error: 'test error' };
      });

      const initPromise = client.init().catch((err: Error) => err);
      // Advance through retry delays (errors are immediate, but retries use 1s setTimeout)
      await vi.advanceTimersByTimeAsync(1_100);
      await vi.advanceTimersByTimeAsync(1_100);

      const result = await initPromise;
      expect(result).toBeInstanceOf(Error);
      expect((result as Error).message).toBe('test error');
    });
  });
});
