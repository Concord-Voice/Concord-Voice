/**
 * PiP receive-side E2EE (2026-08-21 PiP E2EE gap).
 *
 * A PiP BrowserWindow consumes producers independently of the main window, so
 * it owns its own decrypt path: its own E2EE Worker, its own transform attached
 * AT RECEIVER CREATION, and its own frame keys sourced over the PiP→Main
 * BroadcastChannel RPC proxy. Every test here pins one half of that contract —
 * either the happy path, or one of the fail-closed refusals that must never
 * degrade into playing ciphertext.
 *
 * Companion suite: pipVoiceClient.test.ts covers lifecycle/teardown and models a
 * conforming engine throughout.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { MockBroadcastChannel } from '../../helpers/broadcastChannelMock';
import { resetAllStores } from '../../helpers/store-helpers';

// ── E2EE engine stubs ───────────────────────────────────────────────────

class StubRTCRtpScriptTransform {
  constructor(
    public worker: unknown,
    public options: unknown
  ) {}
}

/** Set when a test wants the transform constructor itself to fail. */
let transformConstructorThrows = false;

vi.stubGlobal('RTCRtpScriptTransform', function (this: unknown, worker: unknown, options: unknown) {
  if (transformConstructorThrows) throw new Error('InvalidStateError');
  return new StubRTCRtpScriptTransform(worker, options);
});

const workerInstances: StubWorker[] = [];
class StubWorker {
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: ((e: unknown) => void) | null = null;
  readonly posted: any[] = [];
  terminate = vi.fn();
  postMessage = (msg: unknown) => {
    this.posted.push(msg);
  };
  constructor() {
    workerInstances.push(this);
  }
}
vi.stubGlobal('Worker', StubWorker as unknown as typeof Worker);

// An engine that also exposes the legacy createEncodedStreams pipeline. The
// shared resolver only honors the force-legacy override when this exists, so
// the override test would be vacuous without it.
function StubRTCRtpSender(): void {}
Object.defineProperty(StubRTCRtpSender.prototype, 'createEncodedStreams', { value: vi.fn() });
vi.stubGlobal('RTCRtpSender', StubRTCRtpSender as unknown as typeof RTCRtpSender);

// ── mediasoup-client mock ───────────────────────────────────────────────

/** Controls whether the modelled engine invokes onRtpReceiver during consume. */
let engineInvokesOnRtpReceiver = true;
/** Packets the modelled decoder reports to getStats(). */
let statsPacketsReceived = 0;

const mockConsumerClose = vi.fn();
const lastReceiver: { current: { transform: unknown } | null } = { current: null };

const mockTransportConsume = vi.fn(
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
    const rtpReceiver = {
      transform: null as unknown,
      getStats: async () =>
        new Map([['r0', { type: 'inbound-rtp', packetsReceived: statsPacketsReceived }]]),
    };
    lastReceiver.current = rtpReceiver;
    if (engineInvokesOnRtpReceiver) onRtpReceiver?.(rtpReceiver);
    return {
      id,
      producerId,
      kind,
      track: { id: 'track-' + id, kind },
      rtpReceiver,
      rtpParameters: { codecs: [{ mimeType: 'audio/opus' }] },
      closed: false,
      close: mockConsumerClose,
      on: vi.fn(),
    };
  }
);

const mockRtpCaps = {
  codecs: [{ mimeType: 'audio/opus', kind: 'audio', clockRate: 48000, channels: 2 }],
};

vi.mock('mediasoup-client', () => ({
  Device: vi.fn().mockImplementation(function (this: any) {
    this.load = vi.fn().mockResolvedValue(undefined);
    this.rtpCapabilities = mockRtpCaps;
    this.createRecvTransport = vi.fn().mockReturnValue({
      id: 'transport-1',
      consume: mockTransportConsume,
      on: vi.fn(),
      close: vi.fn(),
    });
    this.loaded = true;
  }),
  types: {},
}));

import { PipVoiceClient } from '@/renderer/services/voice/pipVoiceClient';

// ── Harness ─────────────────────────────────────────────────────────────

const FRAME_KEY = { type: 'secret', algorithm: { name: 'AES-GCM' } };

const defaultRpcResponses: Record<string, unknown> = {
  'request-state': {
    participants: {},
    tunedInScreenShares: {},
    routerRtpCapabilities: mockRtpCaps,
    activeProducers: [],
    localUserId: 'me',
  },
  'create-recv-transport': {
    transportId: 'transport-1',
    iceParameters: { usernameFragment: 'frag', password: 'pass' }, // pragma: allowlist secret
    iceCandidates: [],
    dtlsParameters: { role: 'auto', fingerprints: [] },
  },
  'connect-transport': { success: true },
  consume: {
    consumerId: 'consumer-1',
    producerId: 'prod-1',
    kind: 'audio',
    rtpParameters: { codecs: [{ mimeType: 'audio/opus' }] },
  },
  'resume-consumer': { success: true },
  'get-frame-key': { key: FRAME_KEY, keyVersion: 7, keyId: 3 },
  'close-consumer': { success: true },
  'close-recv-transport': { success: true },
  'pip-closing': { success: true },
  'pip-ready': { success: true, pausedCount: 0 },
};

function channel(): MockBroadcastChannel {
  return MockBroadcastChannel.instances.find((c) => c.name === 'concord-pip')!;
}

function setupResponder(overrides: Record<string, unknown> = {}): void {
  const responses = { ...defaultRpcResponses, ...overrides };
  channel().autoResponder = (data: unknown) => {
    const msg = data as { kind?: string; id?: string; method?: string };
    if (msg.kind !== 'rpc-request' || !msg.id || !msg.method) return undefined;
    const result = responses[msg.method];
    if (result === undefined) {
      return { kind: 'rpc-response', id: msg.id, error: 'No mock for ' + msg.method };
    }
    return { kind: 'rpc-response', id: msg.id, result };
  };
}

function requests(method: string): Array<{ method: string; params: any }> {
  return channel().posted.filter(
    (m: any) => m?.kind === 'rpc-request' && m.method === method
  ) as any;
}

function theWorker(): StubWorker {
  expect(workerInstances).toHaveLength(1);
  return workerInstances[0];
}

class MockMediaStream {
  constructor(readonly tracks: unknown[] = []) {}
  getTracks() {
    return this.tracks;
  }
}

describe('PipVoiceClient — receive-side E2EE (2026-08-21 PiP E2EE gap)', () => {
  let client: PipVoiceClient;
  let savedMediaStream: unknown;

  beforeEach(() => {
    resetAllStores();
    vi.useFakeTimers();
    MockBroadcastChannel.install();
    savedMediaStream = globalThis.MediaStream;
    (globalThis as any).MediaStream = MockMediaStream;
    workerInstances.length = 0;
    lastReceiver.current = null;
    engineInvokesOnRtpReceiver = true;
    transformConstructorThrows = false;
    statsPacketsReceived = 0;
    globalThis.localStorage?.clear();
    mockConsumerClose.mockReset();
    mockTransportConsume.mockClear();
  });

  afterEach(async () => {
    const disposePromise = client?.dispose().catch(() => {});
    await vi.runAllTimersAsync();
    await disposePromise;
    vi.useRealTimers();
    MockBroadcastChannel.uninstall();
    (globalThis as any).MediaStream = savedMediaStream;
  });

  async function join(overrides: Record<string, unknown> = {}): Promise<void> {
    client = new PipVoiceClient('test-pip');
    setupResponder(overrides);
    await client.init();
  }

  // ── Creation-time attachment ──────────────────────────────────────────

  describe('creation-time attachment', () => {
    it('attaches the decrypt transform via onRtpReceiver, not after consume', async () => {
      await join();

      const stream = await client.consume('prod-1', 'mic', 'alice');

      expect(stream).not.toBeNull();
      // The attach happened inside consume(), through the callback mediasoup
      // invokes between setRemoteDescription and createAnswer.
      expect(mockTransportConsume.mock.calls[0][0].onRtpReceiver).toBeInstanceOf(Function);
      expect(lastReceiver.current!.transform).toBeInstanceOf(StubRTCRtpScriptTransform);
    });

    it('binds the transform to the sender, the sender codec, and the probe id', async () => {
      await join();

      await client.consume('prod-1', 'mic', 'alice');

      const transform = lastReceiver.current!.transform as StubRTCRtpScriptTransform;
      expect(transform.worker).toBe(theWorker());
      expect(transform.options).toEqual({
        role: 'decrypt',
        senderUserId: 'alice',
        // Resolved from the SENDER's rtpParameters, not the local device's.
        codecFamily: 'opus',
        // Keys the worker's entered-frame counter for the bypass probe.
        probeId: 'consumer-1',
      });
    });
  });

  // ── Fail-closed refusals ──────────────────────────────────────────────

  describe('fail-closed', () => {
    it('refuses to consume at all when RTCRtpScriptTransform is unavailable', async () => {
      await join();
      const saved = (globalThis as any).RTCRtpScriptTransform;
      (globalThis as any).RTCRtpScriptTransform = undefined;
      try {
        const stream = await client.consume('prod-1', 'mic', 'alice');

        expect(stream).toBeNull();
        // Refused BEFORE any server-side consumer exists — nothing to clean up.
        expect(requests('consume')).toHaveLength(0);
        expect(workerInstances).toHaveLength(0);
      } finally {
        (globalThis as any).RTCRtpScriptTransform = saved;
      }
    });

    it('refuses up front when the manual legacy override is set', async () => {
      await join();
      // localStorage is shared per-origin across BrowserWindows, so a support
      // engineer forcing legacy on the main window is visible here. The PiP has
      // no legacy pipeline, so the honest answer is an immediate refusal rather
      // than attaching and waiting ~5s for the probe to close it.
      globalThis.localStorage.setItem('concord.forceLegacyE2EE', '1');

      const stream = await client.consume('prod-1', 'mic', 'alice');

      expect(stream).toBeNull();
      expect(requests('consume')).toHaveLength(0);
      expect(workerInstances).toHaveLength(0);
    });

    it('does not play the stream when the engine never invokes onRtpReceiver', async () => {
      await join();
      engineInvokesOnRtpReceiver = false;

      const stream = await client.consume('prod-1', 'mic', 'alice');

      // This is the Chromium >=149 shape: consume succeeds, the receiver goes
      // live, and the transform is simply never wired up.
      expect(stream).toBeNull();
      expect(lastReceiver.current!.transform).toBeNull();
      expect(mockConsumerClose).toHaveBeenCalled();
      expect(requests('close-consumer').map((r) => r.params.consumerId)).toEqual(['consumer-1']);
      // Never resumed — no frames were allowed to start flowing.
      expect(requests('resume-consumer')).toHaveLength(0);
    });

    it('does not play the stream when the transform constructor throws', async () => {
      await join();
      transformConstructorThrows = true;

      const stream = await client.consume('prod-1', 'mic', 'alice');

      expect(stream).toBeNull();
      expect(mockConsumerClose).toHaveBeenCalled();
      expect(requests('close-consumer').map((r) => r.params.consumerId)).toEqual(['consumer-1']);
    });

    it('closes a consumer that was already created when a later step throws', async () => {
      await join({ 'resume-consumer': undefined });

      const stream = await client.consume('prod-1', 'mic', 'alice');

      expect(stream).toBeNull();
      // The consumer exists but is not yet in this.consumers, so the cleanup
      // path has to carry it explicitly or the local object leaks.
      expect(mockConsumerClose).toHaveBeenCalled();
      expect(requests('close-consumer').map((r) => r.params.consumerId)).toEqual(['consumer-1']);
    });

    it('closes the server-side consumer when local consumer creation throws', async () => {
      await join();
      mockTransportConsume.mockRejectedValueOnce(new Error('SDP negotiation failed'));

      const stream = await client.consume('prod-1', 'mic', 'alice');

      expect(stream).toBeNull();
      // Without this the SFU keeps sending to a receiver nothing owns.
      expect(requests('close-consumer').map((r) => r.params.consumerId)).toEqual(['consumer-1']);
    });
  });

  // ── Key provisioning ──────────────────────────────────────────────────

  describe('frame keys', () => {
    it('provisions the sender key before consuming and forwards it to the Worker', async () => {
      await join();

      await client.consume('prod-1', 'mic', 'alice');

      const posted = channel().posted as any[];
      const keyReqIndex = posted.findIndex((m) => m?.method === 'get-frame-key');
      const resumeIndex = posted.findIndex((m) => m?.method === 'resume-consumer');
      expect(keyReqIndex).toBeGreaterThanOrEqual(0);
      // Ordering matters: the key must be in the Worker before frames flow.
      expect(keyReqIndex).toBeLessThan(resumeIndex);

      // Version/epoch are left to the main window on the initial provision.
      expect(requests('get-frame-key')[0].params).toEqual({
        senderUserId: 'alice',
        keyVersion: undefined,
        keyId: undefined,
      });
      expect(theWorker().posted).toContainEqual({
        type: 'addDecryptKey',
        senderUserId: 'alice',
        keyVersion: 7,
        keyId: 3,
        key: FRAME_KEY,
      });
    });

    it('answers a Worker typed miss with that exact version and epoch', async () => {
      await join();
      await client.consume('prod-1', 'mic', 'alice');
      channel().posted.length = 0;

      theWorker().onmessage!({
        data: { type: 'requestFrameKey', senderUserId: 'bob', keyVersion: 9, keyId: 4 },
      } as MessageEvent);
      await vi.advanceTimersByTimeAsync(0);

      expect(requests('get-frame-key')[0].params).toEqual({
        senderUserId: 'bob',
        keyVersion: 9,
        keyId: 4,
      });
    });

    it('forwards Worker key requests 1:1 and never amplifies them', async () => {
      await join();
      await client.consume('prod-1', 'mic', 'alice');
      channel().posted.length = 0;

      // The bound on re-requests lives in the Worker and is regression-locked
      // there (e2eeWorker.test.ts — FRAME_KEY_BURST_CAP, burst exhaustion, idle
      // reset). The PiP deliberately adds no second bound, because a PiP-only
      // limiter would diverge it from the identical main-path forwarder. What
      // must hold HERE is the composition: the PiP emits exactly one RPC per
      // Worker request and never retries on its own, so the Worker's bound is
      // also the end-to-end bound. A retry loop added to provisionFrameKey
      // would break that and turn a normal epoch-rotation key miss into
      // amplification against the main window. (CodeRabbit, PR #2870)
      for (let i = 0; i < 5; i++) {
        theWorker().onmessage!({
          data: { type: 'requestFrameKey', senderUserId: 'bob', keyVersion: 9, keyId: 4 },
        } as MessageEvent);
        await vi.advanceTimersByTimeAsync(0);
      }

      expect(requests('get-frame-key')).toHaveLength(5);
    });

    it('does not retry a FAILED key request on its own', async () => {
      // The failure path is where an unbounded retry is most tempting to add.
      // A miss is self-healing: the Worker re-requests under its own bound, so
      // retrying here would multiply, not recover.
      await join({ 'get-frame-key': undefined });
      await client.consume('prod-1', 'mic', 'alice');
      channel().posted.length = 0;

      theWorker().onmessage!({
        data: { type: 'requestFrameKey', senderUserId: 'bob', keyVersion: 9, keyId: 4 },
      } as MessageEvent);
      await vi.advanceTimersByTimeAsync(30_000);

      expect(requests('get-frame-key')).toHaveLength(1);
    });

    it('keeps playing when a key request fails — the Worker drops, never passes through', async () => {
      await join({ 'get-frame-key': undefined });

      const stream = await client.consume('prod-1', 'mic', 'alice');

      // A missing key is not a fail-open: undecryptable frames are dropped in
      // the Worker, and the next typed miss re-requests. Refusing the stream
      // here would turn a recoverable hiccup into a dead tile.
      expect(stream).not.toBeNull();
      expect(theWorker().posted.some((m: any) => m.type === 'addDecryptKey')).toBe(false);
    });
  });

  // ── Bypass probe ──────────────────────────────────────────────────────

  describe('bypass probe', () => {
    async function runProbe(entered: number): Promise<void> {
      await vi.advanceTimersByTimeAsync(5_000);
      const query = theWorker()
        .posted.filter((m: any) => m.type === 'queryDecryptStats')
        .at(-1);
      expect(query).toBeDefined();
      theWorker().onmessage!({
        data: { type: 'decryptStats', probeId: (query as any).probeId, entered },
      } as MessageEvent);
      await vi.advanceTimersByTimeAsync(0);
    }

    it('verifies the transform when frames are entering it', async () => {
      await join();
      await client.consume('prod-1', 'mic', 'alice');
      statsPacketsReceived = 500;

      await runProbe(500);

      expect(mockConsumerClose).not.toHaveBeenCalled();
      expect(requests('close-consumer')).toHaveLength(0);
    });

    it('re-attaches once, then fails closed when frames still bypass the transform', async () => {
      await join();
      await client.consume('prod-1', 'mic', 'alice');
      // Packets arriving with zero frames entering the transform is the exact
      // signature of ciphertext reaching the decoder.
      statsPacketsReceived = 500;

      await runProbe(0);

      // First confirmation: re-attach rather than give up.
      expect(mockConsumerClose).not.toHaveBeenCalled();
      expect(lastReceiver.current!.transform).toBeInstanceOf(StubRTCRtpScriptTransform);

      await runProbe(0);

      // Second confirmation: silence beats decoded ciphertext.
      expect(mockConsumerClose).toHaveBeenCalled();
      expect(requests('close-consumer').map((r) => r.params.consumerId)).toEqual(['consumer-1']);
    });

    it('keeps polling instead of judging a producer that has sent nothing yet', async () => {
      await join();
      await client.consume('prod-1', 'mic', 'alice');
      statsPacketsReceived = 0; // joined muted / camera off

      await runProbe(0);

      // Inconclusive, not bypassed — a silent producer must never be closed.
      expect(mockConsumerClose).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(5_000);
      expect(theWorker().posted.filter((m: any) => m.type === 'queryDecryptStats')).toHaveLength(2);
    });

    it('fails closed when a confirmed bypass has no receiver to re-attach to', async () => {
      await join();
      await client.consume('prod-1', 'mic', 'alice');
      statsPacketsReceived = 500;
      // The receiver must still exist for getStats to report the packets that
      // CONFIRM the bypass, then vanish before the re-attach reads it — that is
      // the actual window. Nulling it up front instead yields 0 packets and the
      // probe returns 'retry', never reaching this branch.
      const track: any = (client as any).consumers.get('consumer-1');
      track.consumer.rtpReceiver.getStats = async () => {
        track.consumer.rtpReceiver = null;
        return new Map([['r0', { type: 'inbound-rtp', packetsReceived: 500 }]]);
      };

      await runProbe(0);

      expect(mockConsumerClose).toHaveBeenCalled();
      expect(requests('close-consumer').map((r) => r.params.consumerId)).toEqual(['consumer-1']);
    });

    it('arms no probe timer when a straggler getStats resolves after dispose completes', async () => {
      await join();
      await client.consume('prod-1', 'mic', 'alice');
      statsPacketsReceived = 0; // inconclusive -> the retry path re-arms

      await vi.advanceTimersByTimeAsync(5_000);
      const query = theWorker()
        .posted.filter((m: any) => m.type === 'queryDecryptStats')
        .at(-1);

      // Hold getStats pending across the WHOLE disposal. Resolving it merely
      // *during* dispose proves nothing: finalizeLocalResources() calls
      // cancelAllProbes() a second time and sweeps the re-arm. The window that
      // actually leaks is a straggler landing after that final sweep.
      let releaseStats: (v: Map<string, unknown>) => void = () => {};
      const track: any = (client as any).consumers.get('consumer-1');
      track.consumer.rtpReceiver.getStats = () =>
        new Promise((resolve) => {
          releaseStats = resolve;
        });

      theWorker().onmessage!({
        data: { type: 'decryptStats', probeId: (query as any).probeId, entered: 0 },
      } as MessageEvent);

      await client.dispose();
      await vi.runAllTimersAsync();

      releaseStats(new Map([['r0', { type: 'inbound-rtp', packetsReceived: 0 }]]));
      await vi.advanceTimersByTimeAsync(0);

      expect(vi.getTimerCount()).toBe(0);
    });

    it('fails closed when re-attachment itself throws', async () => {
      await join();
      await client.consume('prod-1', 'mic', 'alice');
      statsPacketsReceived = 500;
      // Chromium >=149 throws InvalidStateError when replacing a live transform.
      transformConstructorThrows = true;

      await runProbe(0);

      expect(mockConsumerClose).toHaveBeenCalled();
      expect(requests('close-consumer').map((r) => r.params.consumerId)).toEqual(['consumer-1']);
    });
  });

  // ── Teardown ──────────────────────────────────────────────────────────

  describe('teardown', () => {
    it('destroys and terminates the Worker so key material does not outlive the PiP', async () => {
      await join();
      await client.consume('prod-1', 'mic', 'alice');
      const worker = theWorker();

      await client.dispose();

      expect(worker.posted).toContainEqual({ type: 'destroy' });
      expect(worker.terminate).toHaveBeenCalled();
    });

    it('arms no probe timer that outlives disposal', async () => {
      await join();
      await client.consume('prod-1', 'mic', 'alice');

      const disposePromise = client.dispose();
      // Read synchronously: the probe chain re-arms for the consumer's lifetime,
      // so a timer surviving here would keep firing after teardown.
      expect(vi.getTimerCount()).toBe(0);
      await vi.runAllTimersAsync();
      await disposePromise;
    });
  });
});
