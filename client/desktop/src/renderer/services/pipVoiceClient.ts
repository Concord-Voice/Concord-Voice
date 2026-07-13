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
 */

import { Device, types as mediasoupTypes } from 'mediasoup-client';
import {
  generateRequestId,
  type PipChannelMessage,
  type AnyPipBroadcast,
  type VoiceStateResult,
  type CreateRecvTransportResult,
  type ConsumeResult,
} from './pipSignalingTypes';
import { errorMessage } from '../utils/redactError';

const RPC_TIMEOUT = 10_000;
/** Shorter timeout for the initial request-state RPC (retried on failure) */
const INIT_RPC_TIMEOUT = 3_000;
const INIT_MAX_RETRIES = 3;
const INIT_RETRY_DELAY = 1_000;

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

    try {
      const result = await this.rpc<ConsumeResult>('consume', {
        producerId,
        transportId: this.recvTransport.id,
        rtpCapabilities: this.device.rtpCapabilities,
      });

      // Create local consumer
      const consumer = await this.recvTransport.consume({
        id: result.consumerId,
        producerId: result.producerId,
        kind: result.kind,
        rtpParameters: result.rtpParameters,
      });

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
      });

      return stream;
    } catch (err) {
      console.error('[PipVoiceClient] Failed to consume producer:', producerId, errorMessage(err));
      return null;
    }
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
