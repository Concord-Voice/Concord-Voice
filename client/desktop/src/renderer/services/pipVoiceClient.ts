/**
 * PiP Voice Client — lightweight recv-only mediasoup client for PiP windows.
 *
 * PiP BrowserWindows can't share MediaStream objects with the main window
 * (separate V8 contexts). Instead, each PiP creates its own mediasoup Device
 * and recv transport, consuming producers independently.
 *
 * All signaling is proxied through the main window's socket via
 * BroadcastChannel RPC (see pipSignalingTypes.ts / pipSignalingProxy.ts).
 *
 * Lifecycle:
 *   1. PiP mounts → requests voice state from main window
 *   2. Loads Device with router RTP capabilities
 *   3. Creates a recv transport via signaling proxy
 *   4. Consumes producers sequentially (no parallel SDP negotiations)
 *   5. Signals pip-ready → main window pauses its consumers (ownership transfer)
 *   6. On dispose → pip-closing resumes main consumers, then closes owned server resources
 *   7. Always finalizes local consumers, map, transport, pending RPCs, and BroadcastChannel
 *
 * E2EE (2026-08-21 PiP E2EE gap): because the PiP owns its own consumers, it owns its own receive
 * decryption too — a separate renderer context gets no share of the main
 * window's transforms. It therefore runs its own E2EE Worker, attaches a
 * decrypt transform AT RECEIVER CREATION, and sources frame keys over the same
 * BroadcastChannel RPC proxy. It is fail-closed throughout: a stream whose
 * transform cannot be attached, or whose transform is proven to be bypassed, is
 * never played.
 */

import { Device, types as mediasoupTypes } from 'mediasoup-client';
import {
  generateRequestId,
  type PipChannelMessage,
  type AnyPipBroadcast,
  type VoiceStateResult,
  type CreateRecvTransportResult,
  type ConsumeResult,
  type GetFrameKeyResult,
} from './pipSignalingTypes';
import { errorMessage } from '../utils/redactError';
import {
  codecFamilyFromRtpParameters,
  type E2EEMainMessage,
  type E2EEWorkerMessage,
} from '../workers/e2eeProtocol';
import {
  BYPASS_PROBE_DELAY_MS,
  BYPASS_PROBE_MAX_ATTEMPTS,
  BYPASS_PROBE_SLOW_DELAY_MS,
  buildDecryptCreationAttach,
  decideBypassProbeAction,
  type BypassProbePhase,
} from './voiceTransformBypass';
import { FORCE_LEGACY_E2EE_KEY, resolveEncodedTransformSupport } from './encodedTransformSupport';

const RPC_TIMEOUT = 10_000;
/** Shorter timeout for the initial request-state RPC (retried on failure) */
const INIT_RPC_TIMEOUT = 3_000;
/**
 * Frame-key requests block the serial consume queue, and the main window
 * resolves them locally in the common case. Missing the key is recoverable —
 * the Worker drops frames and re-requests — so a slow answer must not cost the
 * full RPC_TIMEOUT before the next producer can be consumed.
 */
const KEY_RPC_TIMEOUT = 3_000;
const INIT_MAX_RETRIES = 3;
const INIT_RETRY_DELAY = 1_000;

/**
 * ponytail: the PiP supports the modern RTCRtpScriptTransform path ONLY.
 * Porting the legacy createEncodedStreams pipeline would duplicate
 * applyLegacyDecryptPipeline into a second renderer for engines
 * (Chromium 86-130) that predate every Electron version we ship. Known
 * ceiling: on an engine where the main window falls back to legacy, the PiP
 * fails closed and plays nothing rather than playing ciphertext. Add the
 * legacy path here only if that fallback becomes reachable in a shipped build.
 */
function scriptTransformAvailable(): boolean {
  // Resolved at CALL time, not module load: the path is the single gate on
  // whether this PiP can decrypt at all, and a load-time snapshot both freezes
  // that decision and is untestable.
  //
  // The manual override is honored because localStorage is shared per-origin
  // across BrowserWindows — with legacy forced, refusing here is honest and
  // immediate instead of attaching and waiting ~5s for the probe to close it.
  // The main window's AUTOMATIC fallback lives in sessionStorage, which is
  // per-window and therefore invisible to a PiP; the bypass probe is what
  // covers that case, reaching the same fail-closed outcome a little later.
  let forceLegacy = false;
  try {
    forceLegacy = globalThis.localStorage?.getItem(FORCE_LEGACY_E2EE_KEY) === '1';
  } catch {
    /* storage unavailable — fall through to plain capability detection */
  }
  // Report capabilities honestly and let the shared resolver decide: it only
  // honors forceLegacy when the legacy API actually exists, so an override on an
  // engine that has no legacy pipeline correctly stays on script-transform
  // instead of stranding the PiP. Anything the resolver returns other than
  // 'script-transform' is a path this window cannot take — fail closed.
  return (
    resolveEncodedTransformSupport(
      {
        scriptTransform:
          typeof RTCRtpScriptTransform === 'undefined' ? undefined : RTCRtpScriptTransform,
        createEncodedStreams:
          typeof RTCRtpSender === 'undefined'
            ? undefined
            : (RTCRtpSender.prototype as { createEncodedStreams?: unknown }).createEncodedStreams,
      },
      { forceLegacy }
    ) === 'script-transform'
  );
}

interface ConsumedTrack {
  consumer: mediasoupTypes.Consumer;
  stream: MediaStream;
  source: string;
  producerUserId: string;
}

export class PipVoiceClient {
  private readonly pipId: string;
  private readonly bc: BroadcastChannel;
  private device: Device | null = null;
  private recvTransport: mediasoupTypes.Transport | null = null;
  private recvTransportId: string | null = null;
  private disposePromise: Promise<void> | null = null;
  private recvTransportInfoPromise: Promise<CreateRecvTransportResult> | null = null;
  private readonly consumers: Map<string, ConsumedTrack> = new Map();
  private disposed = false;
  /** False once the main-window proxy announces that the voice session has ended. */
  private remoteCleanupAvailable = true;

  /**
   * This PiP's own E2EE Worker — decrypt-only. It is never sent `init`
   * (a PiP has no producers, so it never encrypts); `addDecryptKey` populates
   * the decrypt map independently of the encrypt side.
   */
  private e2eeWorker: Worker | null = null;

  /** In-flight bypass probes, keyed by consumer id (see scheduleBypassProbe). */
  private readonly bypassProbes = new Map<
    string,
    { senderUserId: string; phase: BypassProbePhase; attempt: number }
  >();

  /**
   * Pending probe timers, keyed by consumer id. The slow-poll phase re-arms for
   * the consumer's whole lifetime, so these must be cleared explicitly —
   * a disposed PiP that leaves one armed keeps a 30s chain alive after teardown.
   */
  private readonly probeTimers = new Map<string, ReturnType<typeof setTimeout>>();

  /** Pending RPC response callbacks keyed by request ID */
  private readonly pending: Map<
    string,
    {
      resolve: (result: unknown) => void;
      reject: (err: Error) => void;
      timer: ReturnType<typeof setTimeout>;
      preserveOnDispose: boolean;
    }
  > = new Map();

  /** Callback for state updates from main window */
  onStateUpdate: ((msg: AnyPipBroadcast) => void) | null = null;

  constructor(pipId: string) {
    this.pipId = pipId;
    this.bc = new BroadcastChannel('concord-pip');
    this.bc.onmessage = this.handleMessage;
  }

  // ── Public API ──────────────────────────────────────────────────

  /**
   * Initialize the client: load device, create transport, consume producers.
   * Returns the initial voice state for the PiP UI.
   */
  async init(): Promise<VoiceStateResult> {
    // 1. Request voice state from main window (with retry — the PipSignalingProxy
    // may not exist yet if the async import in MainView hasn't resolved)
    let state: VoiceStateResult | null = null;
    for (let attempt = 1; attempt <= INIT_MAX_RETRIES; attempt++) {
      try {
        state = await this.rpc<VoiceStateResult>('request-state', {}, INIT_RPC_TIMEOUT);
        break;
      } catch (err) {
        const error = err instanceof Error ? err : new Error(String(err));
        const isTimeout = error.message.includes('RPC timeout');
        // Only retry on transient timeouts (proxy not ready yet).
        // Surface other errors (e.g. explicit error responses) immediately.
        if (!isTimeout || attempt === INIT_MAX_RETRIES || this.disposed) throw error;
        console.debug(`[PipVoiceClient] request-state attempt ${attempt} timed out, retrying...`);
        await new Promise((r) => setTimeout(r, INIT_RETRY_DELAY));
      }
    }

    if (!state) throw new Error('Failed to get voice state after retries');

    if (!state.routerRtpCapabilities) {
      throw new Error('No router RTP capabilities available');
    }

    // 2. Load mediasoup Device
    this.device = new Device();
    await this.device.load({
      routerRtpCapabilities: state.routerRtpCapabilities,
    });

    // 3. Create recv transport. Preserve this one RPC during disposal so a late
    // acknowledgement still yields the server-owned ID that must be closed.
    this.recvTransportInfoPromise = this.rpc<CreateRecvTransportResult>(
      'create-recv-transport',
      {},
      RPC_TIMEOUT,
      true
    );
    const transportInfo = await this.recvTransportInfoPromise;
    this.recvTransportId = transportInfo.transportId;

    if (this.disposed) throw new Error('PipVoiceClient disposed');

    this.recvTransport = this.device.createRecvTransport({
      id: transportInfo.transportId,
      iceParameters: transportInfo.iceParameters as mediasoupTypes.IceParameters,
      iceCandidates: transportInfo.iceCandidates as mediasoupTypes.IceCandidate[],
      dtlsParameters: transportInfo.dtlsParameters as mediasoupTypes.DtlsParameters,
    });

    // Handle transport 'connect' event (DTLS handshake)
    this.recvTransport.on('connect', async ({ dtlsParameters }, callback, errback) => {
      try {
        await this.rpc('connect-transport', {
          transportId: transportInfo.transportId,
          dtlsParameters,
        });
        callback();
      } catch (err) {
        errback(err instanceof Error ? err : new Error(String(err)));
      }
    });

    return state;
  }

  /**
   * Consume a producer. Must be called sequentially (no parallel SDP negotiations).
   * Returns the MediaStream for the consumed track.
   */
  async consume(
    producerId: string,
    source: string,
    producerUserId: string
  ): Promise<MediaStream | null> {
    if (!this.device || !this.recvTransport || this.disposed) return null;

    // Fail closed BEFORE any server-side consumer exists: without a Worker to
    // decrypt into, every frame this consumer receives is ciphertext bound for
    // the decoder.
    const worker = this.ensureE2EEWorker();
    if (!worker) {
      console.error(
        `[PipVoiceClient] Refusing to consume ${producerId} — E2EE decrypt transform unavailable`
      );
      return null;
    }

    let consumerId: string | null = null;
    let created: mediasoupTypes.Consumer | null = null;
    try {
      const result = await this.rpc<ConsumeResult>('consume', {
        producerId,
        transportId: this.recvTransport.id,
        rtpCapabilities: this.device.rtpCapabilities,
      });
      consumerId = result.consumerId;

      // Provision the sender's current frame key before frames can arrive. Not
      // load-bearing for safety — the Worker drops undecryptable frames and
      // asks for the exact key via requestFrameKey — it just avoids an opening
      // burst of key misses.
      await this.provisionFrameKey(producerUserId);

      // PiP E2EE: attach the decrypt transform AT RECEIVER CREATION via
      // onRtpReceiver (fires after setRemoteDescription, before createAnswer).
      // Chromium >=149 does not route frames through a transform attached after
      // the receiver is live — the pipe stays empty while ciphertext reaches the
      // decoder (2026-08-21 field capture, PR #2865). This mirrors the working
      // sender-side onRtpSender hook.
      const attach = buildDecryptCreationAttach(
        worker,
        producerUserId,
        codecFamilyFromRtpParameters(result.rtpParameters),
        result.consumerId
      );
      let attached = false;
      let attachError: string | null = null;

      const consumer = await this.recvTransport.consume({
        id: result.consumerId,
        producerId: result.producerId,
        kind: result.kind,
        rtpParameters: result.rtpParameters,
        onRtpReceiver: (receiver: RTCRtpReceiver) => {
          try {
            attach(receiver);
            attached = true;
          } catch (err) {
            // Recorded, not thrown: mediasoup owns this callback's frame and a
            // throw here would surface as an opaque transport error.
            attachError = errorMessage(err);
          }
        },
      });

      created = consumer;

      // No disposed re-check is needed here, and adding one is dead code: a
      // dispose() racing these awaits either rejects the resume-consumer rpc()
      // outright (its own disposed guard) or cancels it in flight
      // (cancelPendingRpcs), and both land in the catch below, which fails
      // closed. Verified by writing the guard and finding its test vacuous.
      if (!attached) {
        // mediasoup skipped onRtpReceiver, or the constructor threw. Either way
        // this consumer would decode ciphertext.
        console.error('[PipVoiceClient] E2EE: decrypt transform not attached — failing closed', {
          consumerId: consumer.id,
          senderUserId: producerUserId,
          reason: attachError ?? 'onRtpReceiver was never invoked',
        });
        this.failClosed(consumer.id, consumer);
        return null;
      }
      // Resume the consumer (mediasoup consumers start paused server-side)
      await this.rpc('resume-consumer', { consumerId: consumer.id });

      const stream = new MediaStream([consumer.track]);

      this.consumers.set(consumer.id, {
        consumer,
        stream,
        source,
        producerUserId,
      });

      // Clean up when consumer is closed
      consumer.on('transportclose', () => {
        this.consumers.delete(consumer.id);
        this.clearProbe(consumer.id);
      });

      // Attachment succeeding does not mean frames are flowing through it —
      // that was the whole shape of the 2026-08-21 incident.
      this.scheduleBypassProbe(consumer.id, producerUserId, 'first', 1);

      return stream;
    } catch (err) {
      console.error('[PipVoiceClient] Failed to consume producer:', producerId, errorMessage(err));
      // Both halves outlive a local failure: the server keeps sending, and a
      // consumer created before the throw (e.g. a failing resume-consumer RPC)
      // is not yet in this.consumers for failClosed to find.
      if (consumerId) this.failClosed(consumerId, created ?? undefined);
      return null;
    }
  }

  // ── E2EE ────────────────────────────────────────────────────────

  /** Lazily create this PiP's decrypt-only E2EE Worker. Null means fail closed. */
  private ensureE2EEWorker(): Worker | null {
    if (this.e2eeWorker) return this.e2eeWorker;
    if (this.disposed || !scriptTransformAvailable()) return null;

    try {
      const worker = new Worker(new URL('../workers/e2eeWorker.ts', import.meta.url), {
        type: 'module',
      });
      worker.onmessage = (event: MessageEvent<E2EEMainMessage>) => {
        if (this.e2eeWorker !== worker) return;
        this.handleWorkerMessage(event.data);
      };
      worker.onerror = (err) => {
        if (this.e2eeWorker !== worker) return;
        console.error('[PipVoiceClient] E2EE Worker error:', errorMessage(err));
      };
      this.e2eeWorker = worker;
      return worker;
    } catch (err) {
      console.error('[PipVoiceClient] E2EE Worker creation failed:', errorMessage(err));
      return null;
    }
  }

  /**
   * Ask the main window for one decrypt frame key and hand it to the Worker.
   *
   * Best-effort by design: a failure leaves the Worker without that key, which
   * makes it DROP the affected frames (never enqueue them as ciphertext), and
   * the next typed miss re-requests. Swallowing is therefore safe — swallowing
   * silently is not, so every failure is logged.
   */
  private async provisionFrameKey(
    senderUserId: string,
    keyVersion?: number,
    keyId?: number
  ): Promise<void> {
    const worker = this.e2eeWorker;
    if (!worker || this.disposed) return;
    try {
      const result = await this.rpc<GetFrameKeyResult>(
        'get-frame-key',
        { senderUserId, keyVersion, keyId },
        KEY_RPC_TIMEOUT
      );
      if (this.e2eeWorker !== worker) return;
      worker.postMessage({
        type: 'addDecryptKey',
        senderUserId,
        keyVersion: result.keyVersion,
        keyId: result.keyId,
        key: result.key,
      } satisfies E2EEWorkerMessage);
    } catch (err) {
      console.debug('[PipVoiceClient] E2EE: frame-key provision failed', {
        senderUserId,
        keyVersion,
        keyId,
        reason: errorMessage(err),
      });
    }
  }

  private handleWorkerMessage(msg: E2EEMainMessage): void {
    switch (msg.type) {
      case 'requestFrameKey':
        void this.provisionFrameKey(msg.senderUserId, msg.keyVersion, msg.keyId);
        break;
      case 'requestRecovery':
        // Re-derive at the channel's current version/epoch — the PiP's analogue
        // of the main window's invalidate-and-refetch self-heal.
        void this.provisionFrameKey(msg.senderUserId);
        break;
      case 'decryptStats':
        void this.evaluateBypassProbe(msg.probeId, msg.entered);
        break;
      case 'log':
        console.debug(`[PipVoiceClient] E2EE Worker: ${msg.message}`, msg.data ?? {});
        break;
      // ponytail: requestKeyframe needs a socket the PiP does not have, and the
      // main window's consumers are paused during PiP ownership so it cannot
      // relay one either. Dropping it costs a slower video recovery after a
      // decrypt drop-burst (recovers on the next natural keyframe), never
      // correctness. Add a keyframe RPC if that recovery latency is measured to
      // matter. rotationComplete is encrypt-side only and never reaches a PiP.
      default:
        break;
    }
  }

  // ── Receive-transform bypass probe (mirrors voiceService) ────────
  // Attachment succeeding is not evidence that frames traverse the transform.
  // Pair the decoder's getStats() packet count with the Worker's entered-frame
  // count; packets arriving with zero entries means ciphertext is reaching the
  // decoder. Decision policy is the shared decideBypassProbeAction so the PiP
  // and the main window can never drift apart on it.

  private scheduleBypassProbe(
    consumerId: string,
    senderUserId: string,
    phase: BypassProbePhase,
    attempt: number,
    delayMs: number = BYPASS_PROBE_DELAY_MS
  ): void {
    const timer = setTimeout(() => {
      this.probeTimers.delete(consumerId);
      const track = this.consumers.get(consumerId);
      const worker = this.e2eeWorker;
      if (this.disposed || !track || track.consumer.closed || !worker) return;
      this.bypassProbes.set(consumerId, { senderUserId, phase, attempt });
      worker.postMessage({
        type: 'queryDecryptStats',
        probeId: consumerId,
      } satisfies E2EEWorkerMessage);
    }, delayMs);
    // One probe in flight per consumer — a re-arm supersedes its predecessor.
    clearTimeout(this.probeTimers.get(consumerId));
    this.probeTimers.set(consumerId, timer);
  }

  private async evaluateBypassProbe(consumerId: string, entered: number): Promise<void> {
    const probe = this.bypassProbes.get(consumerId);
    if (!probe) return;
    this.bypassProbes.delete(consumerId);

    const track = this.consumers.get(consumerId);
    if (!track || track.consumer.closed) return;

    let packetsReceived = 0;
    try {
      const stats = await track.consumer.rtpReceiver?.getStats();
      stats?.forEach((report: { type?: string; packetsReceived?: number }) => {
        if (report.type === 'inbound-rtp') packetsReceived += report.packetsReceived ?? 0;
      });
    } catch {
      return; // stats unavailable (torn down mid-probe) — nothing to judge
    }

    // dispose() may have run during the getStats() await, after
    // cancelAllProbes() already swept probeTimers. Every action below re-arms a
    // timer (up to the 30s slow poll), so returning here is what makes
    // "dispose arms nothing" actually true. (Gitar, PR #2870)
    if (this.disposed) return;

    const action = decideBypassProbeAction(packetsReceived, entered, probe.phase, probe.attempt);
    switch (action) {
      case 'verified':
        console.debug('[PipVoiceClient] E2EE: receive transform verified', {
          consumerId,
          entered,
          packetsReceived,
        });
        return;
      case 'retry':
        this.scheduleBypassProbe(consumerId, probe.senderUserId, probe.phase, probe.attempt + 1);
        return;
      case 'slow-retry':
        if (probe.attempt === BYPASS_PROBE_MAX_ATTEMPTS) {
          console.debug('[PipVoiceClient] E2EE: bypass probe entering slow poll — no media yet', {
            consumerId,
          });
        }
        this.scheduleBypassProbe(
          consumerId,
          probe.senderUserId,
          probe.phase,
          Math.min(probe.attempt + 1, BYPASS_PROBE_MAX_ATTEMPTS + 1),
          BYPASS_PROBE_SLOW_DELAY_MS
        );
        return;
      case 'reattach': {
        console.error(
          '[PipVoiceClient] E2EE: receive transform BYPASSED — encrypted frames are reaching the decoder; re-attaching',
          { consumerId, senderUserId: probe.senderUserId, packetsReceived }
        );
        const receiver = track.consumer.rtpReceiver;
        const worker = this.e2eeWorker;
        if (!receiver || !worker) {
          // The bypass is already CONFIRMED at this point. Returning would leave
          // a consumer judged to be feeding ciphertext to the decoder playing on
          // with no further probe — the exact fail-open this file exists to
          // close. (Gitar, PR #2870)
          console.error(
            '[PipVoiceClient] E2EE: confirmed bypass cannot be re-attached — failing closed',
            { consumerId, hasReceiver: Boolean(receiver), hasWorker: Boolean(worker) }
          );
          this.failClosed(consumerId, track.consumer);
          return;
        }
        try {
          // Direct replacement — NEVER null-then-set: assigning null enables the
          // spec's PASSTHROUGH algorithm, i.e. a window where ciphertext flows
          // to the decoder by design (CWE-693, PR #2865). The shared builder
          // performs exactly that single assignment.
          buildDecryptCreationAttach(
            worker,
            probe.senderUserId,
            codecFamilyFromRtpParameters(track.consumer.rtpParameters),
            consumerId
          )(receiver);
          this.scheduleBypassProbe(consumerId, probe.senderUserId, 'reattached', 1);
        } catch (err) {
          console.error('[PipVoiceClient] E2EE: bypass re-attach failed — failing closed', {
            consumerId,
            reason: errorMessage(err),
          });
          this.failClosed(consumerId, track.consumer);
        }
        return;
      }
      case 'close':
        console.error(
          '[PipVoiceClient] E2EE: receive transform still bypassed after re-attach — closing consumer fail-closed',
          { consumerId, senderUserId: probe.senderUserId, packetsReceived }
        );
        this.failClosed(consumerId, track.consumer);
        return;
    }
  }

  /** Cancel every pending probe timer and forget all probe state. Idempotent. */
  private cancelAllProbes(): void {
    for (const timer of this.probeTimers.values()) clearTimeout(timer);
    this.probeTimers.clear();
    this.bypassProbes.clear();
  }

  /** Forget one consumer's probe state and cancel its pending timer. */
  private clearProbe(consumerId: string): void {
    this.bypassProbes.delete(consumerId);
    clearTimeout(this.probeTimers.get(consumerId));
    this.probeTimers.delete(consumerId);
  }

  /**
   * Drop a consumer whose decryption cannot be trusted: close it locally, forget
   * it, and tell the SFU to stop sending. Silence and a black tile are strictly
   * better than decoded ciphertext.
   */
  private failClosed(consumerId: string, consumer?: mediasoupTypes.Consumer): void {
    const local = consumer ?? this.consumers.get(consumerId)?.consumer;
    try {
      if (local && !local.closed) local.close();
    } catch {
      /* ignore */
    }
    this.consumers.delete(consumerId);
    this.clearProbe(consumerId);
    this.sendRpc('close-consumer', { consumerId }).catch(() => {
      /* best effort — the transport teardown closes it server-side anyway */
    });
  }

  /**
   * Signal to main window that this PiP has consumed all producers
   * and is ready for ownership transfer.
   */
  async signalReady(): Promise<void> {
    const consumerSources = Array.from(this.consumers.values()).map((c) => ({
      source: c.source,
      producerUserId: c.producerUserId,
    }));

    await this.rpc('pip-ready', { consumerSources });
  }

  /**
   * Execute a voice action in the main window.
   */
  async action(
    action: 'toggle-mute' | 'toggle-deafen' | 'leave' | 'toggle-video' | 'toggle-screen'
  ): Promise<void> {
    await this.rpc('action', { action });
  }

  /**
   * Get all consumed streams grouped by type.
   */
  getStreams(): Map<string, ConsumedTrack> {
    return new Map(this.consumers);
  }

  /**
   * Get a specific stream by source and userId.
   */
  getStreamBySource(source: string, userId: string): MediaStream | null {
    for (const [, track] of this.consumers) {
      if (track.source === source && track.producerUserId === userId) {
        return track.stream;
      }
    }
    return null;
  }

  /**
   * Get the SFU consumer id for a specific source and userId — this PiP's OWN
   * consumer, which the main window addresses to report render-state demand (#1924).
   */
  getConsumerIdBySource(source: string, userId: string): string | null {
    for (const [consumerId, track] of this.consumers) {
      if (track.source === source && track.producerUserId === userId) {
        return consumerId;
      }
    }
    return null;
  }

  /**
   * Report receiver render-state demand for one of this PiP's OWN consumers (#1924).
   * The PiP has no socket, so it proxies set-preferred-layers to the main window,
   * which owns the socket the consumer was created on. Best-effort: a failure only
   * means the stream may stay at a lower spatial layer, never a hard error.
   */
  async reportPreferredLayers(params: {
    consumerId: string;
    cssWidth: number;
    cssHeight: number;
    visible: boolean;
    role: 'thumbnail' | 'grid' | 'focus';
    focusedWindow: boolean;
  }): Promise<void> {
    await this.rpc('set-preferred-layers', params);
  }

  /**
   * Clean up: close all consumers, transport, notify main window.
   */
  dispose(): Promise<void> {
    if (this.disposePromise) return this.disposePromise;

    const consumerIds = [...this.consumers.keys()];
    const transportId = this.recvTransportId;
    const transportInfoPromise = this.recvTransportInfoPromise;

    this.disposed = true;
    this.cancelPendingRpcs();
    // Purely local state — cancel synchronously rather than behind the async
    // server-side cleanup, so a disposed PiP arms nothing for the slow-poll
    // chain to pick up.
    this.cancelAllProbes();
    this.disposePromise = this.remoteCleanupAvailable
      ? this.performDispose(consumerIds, transportId, transportInfoPromise)
      : Promise.resolve().then(() => this.finalizeLocalResources());
    return this.disposePromise;
  }

  private async performDispose(
    consumerIds: string[],
    transportId: string | null,
    transportInfoPromise: Promise<CreateRecvTransportResult> | null
  ): Promise<void> {
    try {
      try {
        await this.sendRpc('pip-closing', {});
      } catch {
        /* best effort */
      }

      await Promise.allSettled(
        consumerIds.map((consumerId) => this.sendRpc('close-consumer', { consumerId }))
      );

      let cleanupTransportId = transportId;
      if (!cleanupTransportId && transportInfoPromise) {
        try {
          cleanupTransportId = (await transportInfoPromise).transportId;
        } catch {
          /* create failed or timed out */
        }
      }

      if (cleanupTransportId) {
        try {
          await this.sendRpc('close-recv-transport', { transportId: cleanupTransportId });
        } catch {
          /* best effort */
        }
      }
    } finally {
      this.finalizeLocalResources();
    }
  }

  private finalizeLocalResources(): void {
    this.cancelAllProbes();
    const worker = this.e2eeWorker;
    this.e2eeWorker = null;
    if (worker) {
      try {
        worker.postMessage({ type: 'destroy' } satisfies E2EEWorkerMessage);
      } catch {
        /* still terminate a failed Worker — key material must not outlive the PiP */
      }
      try {
        worker.terminate();
      } catch {
        /* idempotent teardown */
      }
    }

    for (const [, track] of this.consumers) {
      try {
        track.consumer.close();
      } catch {
        /* ignore */
      }
    }
    this.consumers.clear();

    try {
      this.recvTransport?.close();
    } catch {
      /* ignore */
    }
    this.recvTransport = null;
    this.recvTransportId = null;
    this.recvTransportInfoPromise = null;

    this.cancelPendingRpcs(true);
    try {
      this.bc.close();
    } catch {
      /* ignore */
    }
  }

  // ── BroadcastChannel message handling ───────────────────────────

  private readonly handleMessage = (event: MessageEvent<PipChannelMessage>) => {
    const msg = event.data;

    // Handle RPC responses
    if (msg.kind === 'rpc-response') {
      const response = msg;
      const pending = this.pending.get(response.id);
      if (pending) {
        clearTimeout(pending.timer);
        this.pending.delete(response.id);
        if (response.error) {
          pending.reject(new Error(response.error));
        } else {
          pending.resolve(response.result);
        }
      }
      return;
    }

    // Handle broadcasts from main window
    if (msg.kind === 'broadcast') {
      if (msg.type === 'voice-ended') {
        this.remoteCleanupAvailable = false;
        this.cancelPendingRpcs(true);
      }
      this.onStateUpdate?.(msg);
    }
  };

  // ── RPC helper ──────────────────────────────────────────────────

  private rpc<T>(
    method: string,
    params: unknown,
    timeout = RPC_TIMEOUT,
    preserveOnDispose = false
  ): Promise<T> {
    if (this.disposed) {
      return Promise.reject(new Error('PipVoiceClient disposed'));
    }

    return this.sendRpc<T>(method, params, timeout, preserveOnDispose);
  }

  private sendRpc<T>(
    method: string,
    params: unknown,
    timeout = RPC_TIMEOUT,
    preserveOnDispose = false
  ): Promise<T> {
    if (!this.remoteCleanupAvailable) {
      return Promise.reject(new Error('PiP signaling proxy unavailable'));
    }

    return new Promise<T>((resolve, reject) => {
      const id = generateRequestId(this.pipId);

      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`RPC timeout: ${method}`));
      }, timeout);

      this.pending.set(id, {
        resolve: resolve as (result: unknown) => void,
        reject,
        timer,
        preserveOnDispose,
      });

      this.bc.postMessage({
        kind: 'rpc-request',
        id,
        pipId: this.pipId,
        method,
        params,
      });
    });
  }

  private cancelPendingRpcs(includePreserved = false): void {
    for (const [id, pending] of this.pending) {
      if (pending.preserveOnDispose && !includePreserved) continue;
      clearTimeout(pending.timer);
      pending.reject(new Error('PipVoiceClient disposed'));
      this.pending.delete(id);
    }
  }
}
