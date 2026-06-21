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
 *   6. On dispose → signals pip-closing → main window resumes its consumers
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
  private readonly consumers: Map<string, ConsumedTrack> = new Map();
  private disposed = false;

  /** Pending RPC response callbacks keyed by request ID */
  private readonly pending: Map<
    string,
    {
      resolve: (result: unknown) => void;
      reject: (err: Error) => void;
      timer: ReturnType<typeof setTimeout>;
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

    // 3. Create recv transport
    const transportInfo = await this.rpc<CreateRecvTransportResult>('create-recv-transport', {});

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
   * Clean up: close all consumers, transport, notify main window.
   */
  async dispose(): Promise<void> {
    if (this.disposed) return;

    // Signal pre-close so main window can start resuming (before setting
    // disposed flag, since rpc() rejects when disposed is true)
    try {
      await this.rpc('pip-closing', {});
    } catch {
      /* best effort */
    }

    this.disposed = true;

    // Close all consumers
    for (const [, track] of this.consumers) {
      try {
        track.consumer.close();
      } catch {
        /* ignore */
      }
    }
    this.consumers.clear();

    // Close transport
    try {
      this.recvTransport?.close();
    } catch {
      /* ignore */
    }
    this.recvTransport = null;

    // Cancel pending RPCs
    for (const [, p] of this.pending) {
      clearTimeout(p.timer);
      p.reject(new Error('PipVoiceClient disposed'));
    }
    this.pending.clear();

    this.bc.close();
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
      this.onStateUpdate?.(msg);
    }
  };

  // ── RPC helper ──────────────────────────────────────────────────

  private rpc<T>(method: string, params: unknown, timeout = RPC_TIMEOUT): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      if (this.disposed) {
        return reject(new Error('PipVoiceClient disposed'));
      }

      const id = generateRequestId(this.pipId);

      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`RPC timeout: ${method}`));
      }, timeout);

      this.pending.set(id, {
        resolve: resolve as (result: unknown) => void,
        reject,
        timer,
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
}
