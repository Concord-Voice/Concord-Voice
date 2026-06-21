import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { MockBroadcastChannel, createRpcResponder } from '../../helpers/broadcastChannelMock';

// ── mediasoup-client mock ───────────────────────────────────────────────

const mockConsumerClose = vi.fn();
const mockConsumerOn = vi.fn();
const mockTransportClose = vi.fn();
const mockTransportOn = vi.fn();

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

import { PipVoiceClient } from '@/renderer/services/pipVoiceClient';

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
  'pause-consumer': { success: true },
  action: { success: true },
  'pip-ready': { success: true, pausedCount: 0 },
  'pip-closing': { success: true },
};

/** Set up auto-responder on the client's broadcast channel */
function setupAutoResponder(overrides: Record<string, unknown> = {}): void {
  const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip');
  if (ch) {
    ch.autoResponder = createRpcResponder({ ...defaultRpcResponses, ...overrides });
  }
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

  beforeEach(() => {
    vi.useFakeTimers();
    MockBroadcastChannel.install();
    savedMediaStream = globalThis.MediaStream;
    (globalThis as any).MediaStream = MockMediaStream;
    vi.clearAllMocks();
  });

  afterEach(async () => {
    // Dispose may trigger an RPC that needs the timer to expire
    const disposePromise = client?.dispose().catch(() => {});
    vi.advanceTimersByTime(11_000); // Flush any RPC timeouts
    await disposePromise;
    vi.useRealTimers();
    MockBroadcastChannel.uninstall();
    (globalThis as any).MediaStream = savedMediaStream;
  });

  // ── Constructor ─────────────────────────────────────────────────────

  describe('constructor', () => {
    it('creates BroadcastChannel with name concord-pip', () => {
      client = new PipVoiceClient('controls-main');
      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip');
      expect(ch).toBeDefined();
    });

    it('stores pipId', () => {
      client = new PipVoiceClient('frames-123');
      // Verify pipId is used in RPC requests
      setupAutoResponder();
      // pipId will appear in messages when init() is called
      expect(client).toBeDefined();
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
      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip')!;
      ch.autoResponder = (data: unknown) => {
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
      };

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
      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip');
      if (ch) {
        ch.autoResponder = (data: unknown) => {
          const msg = data as { kind?: string; id?: string; method?: string };
          if (msg.kind !== 'rpc-request') return undefined;
          return { kind: 'rpc-response', id: msg.id, error: 'proxy error' };
        };
      }

      // Should fail immediately without retrying
      await expect(client.init()).rejects.toThrow('proxy error');
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
      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip');
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

      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip');
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

      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip');
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

  // ── dispose() ───────────────────────────────────────────────────────

  describe('dispose()', () => {
    it('sends pip-closing RPC', async () => {
      client = new PipVoiceClient('test-pip');
      setupAutoResponder();
      await client.init();

      // Capture channel reference before dispose closes it
      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip')!;
      await client.dispose();

      const closingMsg = ch.posted.find(
        (m: any) => m.kind === 'rpc-request' && m.method === 'pip-closing'
      );
      expect(closingMsg).toBeDefined();
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

      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip')!;
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

      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip');
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

      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip');
      ch?.simulateMessage({ kind: 'broadcast', type: 'voice-ended' });

      expect(callback).toHaveBeenCalledWith(expect.objectContaining({ type: 'voice-ended' }));
    });

    it('handles RPC error responses', async () => {
      client = new PipVoiceClient('test-pip');

      // All RPCs return error — init() retries then throws after exhausting attempts
      const ch = MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip');
      if (ch) {
        ch.autoResponder = (data: unknown) => {
          const msg = data as { kind?: string; id?: string; method?: string };
          if (msg.kind !== 'rpc-request') return undefined;
          return { kind: 'rpc-response', id: msg.id, error: 'test error' };
        };
      }

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
