/**
 * PiP Signaling Proxy — runs in the main renderer window.
 *
 * Listens on BroadcastChannel 'concord-pip' for RPC requests from PiP
 * BrowserWindows. Forwards signaling to the media plane via voiceService
 * and returns responses through the same channel.
 *
 * Also broadcasts participant state changes and producer events so PiP
 * windows stay in sync.
 *
 * Lifecycle:
 *   - Created when user joins a voice channel
 *   - Disposed when user leaves the voice channel
 */

import type {
  PipChannelMessage,
  PipRpcRequest,
  AnyPipRpcRequest,
  PipRpcResponse,
  StateUpdateBroadcast,
  ProducerAddedBroadcast,
  ProducerClosedBroadcast,
  OwnershipTransferBroadcast,
  VoiceEndedBroadcast,
  VoiceStateResult,
  CreateRecvTransportResult,
  ConsumeResult,
} from './pipSignalingTypes';
import { useVoiceStore } from '../stores/voiceStore';
import { useUserStore } from '../stores/userStore';
import { errorMessage } from '../utils/redactError';
import type { RtpCapabilities } from 'mediasoup-client/types';

// Type for voiceService — imported lazily to avoid circular deps
type VoiceService = {
  forwardToServer<T>(event: string, data?: unknown): Promise<T>;
  getRouterRtpCapabilities(): RtpCapabilities | null;
  getConsumerIdsBySource(source?: string): string[];
  getConsumerMeta(): Map<string, { source: string; producerUserId: string; producerId: string }>;
  pauseConsumer(consumerId: string): void;
  resumeConsumer(consumerId: string): void;
  toggleMute(): Promise<void>;
  toggleDeafen(): void;
  toggleVideo(): Promise<void>;
  toggleScreenShare(sourceId?: string): Promise<void>;
  leaveChannel(): Promise<void>;
};

export class PipSignalingProxy {
  private readonly bc: BroadcastChannel;
  private readonly voiceService: VoiceService;
  private disposed = false;

  /**
   * Consumer IDs in the main window that were paused for a given PiP window.
   * Key: pipId, Value: array of paused consumer IDs.
   * Used to resume them when the PiP closes.
   */
  private readonly pausedForPip: Map<string, string[]> = new Map();

  /** Zustand unsubscribe function for state change broadcasting */
  private stateUnsub: (() => void) | null = null;

  constructor(voiceService: VoiceService) {
    this.voiceService = voiceService;
    this.bc = new BroadcastChannel('concord-pip');
    this.bc.onmessage = this.handleMessage;

    // Subscribe to voice store changes to broadcast participant updates
    this.stateUnsub = useVoiceStore.subscribe((state, prev) => {
      if (this.disposed) return;
      if (
        state.participants !== prev.participants ||
        state.tunedInScreenShares !== prev.tunedInScreenShares
      ) {
        this.broadcastStateUpdate(state.participants, state.tunedInScreenShares);
      }
    });
  }

  dispose(): void {
    if (this.disposed) return;

    this.stateUnsub?.();
    this.stateUnsub = null;

    // Resume any consumers that were paused for PiP windows
    for (const [, consumerIds] of this.pausedForPip) {
      for (const id of consumerIds) {
        try {
          this.voiceService.resumeConsumer(id);
        } catch {
          /* consumer may already be closed */
        }
      }
    }
    this.pausedForPip.clear();

    // Notify PiP windows that voice has ended (before setting disposed flag)
    this.broadcast({ kind: 'broadcast', type: 'voice-ended' } satisfies VoiceEndedBroadcast);

    this.disposed = true;
    this.bc.close();
  }

  // ── Incoming message handler ────────────────────────────────────

  private readonly handleMessage = (event: MessageEvent<PipChannelMessage>) => {
    const msg = event.data;
    if (this.disposed) return;

    // Only handle RPC requests from PiP windows
    if (msg.kind !== 'rpc-request') return;

    this.handleRpcRequest(msg).catch((err) => {
      console.error('[PipSignalingProxy] Unhandled RPC error:', errorMessage(err));
    });
  };

  private async handleRpcRequest(req: AnyPipRpcRequest): Promise<void> {
    try {
      switch (req.method) {
        case 'request-state':
          await this.handleRequestState(req);
          break;
        case 'create-recv-transport':
          await this.handleCreateRecvTransport(req);
          break;
        case 'connect-transport':
          await this.handleConnectTransport(req);
          break;
        case 'consume':
          await this.handleConsume(req);
          break;
        case 'resume-consumer':
          await this.handleResumeConsumer(req);
          break;
        case 'pause-consumer':
          await this.handlePauseConsumer(req);
          break;
        case 'action':
          await this.handleAction(req);
          break;
        case 'pip-ready':
          await this.handlePipReady(req);
          break;
        case 'pip-closing':
          await this.handlePipClosing(req);
          break;
        default: {
          const unknownReq = req as PipRpcRequest;
          this.respond(unknownReq.id, undefined, `Unknown method: ${unknownReq.method}`);
        }
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      this.respond(req.id, undefined, message);
    }
  }

  // ── RPC handlers ────────────────────────────────────────────────

  private async handleRequestState(req: AnyPipRpcRequest): Promise<void> {
    const store = useVoiceStore.getState();
    const rtpCaps = this.voiceService.getRouterRtpCapabilities();
    const meta = this.voiceService.getConsumerMeta();

    // Build active producers list from consumer metadata.
    // Deduplicate by source+userId — each unique producer appears once.
    const activeProducers: VoiceStateResult['activeProducers'] = [];
    const seen = new Set<string>();
    for (const [, m] of meta) {
      const key = `${m.source}-${m.producerUserId}`;
      if (!seen.has(key)) {
        seen.add(key);
        activeProducers.push({
          producerId: m.producerId,
          userId: m.producerUserId,
          source: m.source,
        });
      }
    }

    // Strip MediaStream objects — BroadcastChannel uses structured clone
    // which cannot serialize MediaStream instances.
    const sanitizedParticipants: Record<string, unknown> = {};
    for (const [key, p] of Object.entries(store.participants)) {
      // eslint-disable-next-line @typescript-eslint/no-unused-vars -- rest-pattern destructure: the four MediaStream keys must be NAMED (not `_`-prefixed) so they are excluded from `...rest`; BroadcastChannel's structured clone cannot serialize MediaStream instances, so stripping them here prevents DataCloneError in the PiP bridge
      const { videoStream, screenStream, audioStream, screenAudioStream, ...rest } = p;
      sanitizedParticipants[key] = rest;
    }

    const localUserId = useUserStore.getState().user?.id ?? '';

    const result: VoiceStateResult = {
      participants: sanitizedParticipants as VoiceStateResult['participants'],
      tunedInScreenShares: store.tunedInScreenShares,
      routerRtpCapabilities: rtpCaps,
      activeProducers,
      localUserId,
    };
    this.respond(req.id, result);
  }

  private async handleCreateRecvTransport(req: AnyPipRpcRequest): Promise<void> {
    const params = (req as { params: { forceTcp?: boolean } }).params;
    const result = await this.voiceService.forwardToServer<CreateRecvTransportResult>(
      'create-transport',
      { direction: 'recv', forceTcp: params?.forceTcp }
    );
    this.respond(req.id, result);
  }

  private async handleConnectTransport(req: AnyPipRpcRequest): Promise<void> {
    const params = (req as { params: { transportId: string; dtlsParameters: unknown } }).params;
    await this.voiceService.forwardToServer('connect-transport', {
      transportId: params.transportId,
      dtlsParameters: params.dtlsParameters,
    });
    this.respond(req.id, { success: true });
  }

  private async handleConsume(req: AnyPipRpcRequest): Promise<void> {
    const params = (
      req as {
        params: { producerId: string; transportId: string; rtpCapabilities: RtpCapabilities };
      }
    ).params;
    const result = await this.voiceService.forwardToServer<ConsumeResult>('consume', {
      producerId: params.producerId,
      rtpCapabilities: params.rtpCapabilities,
      transportId: params.transportId,
    });
    this.respond(req.id, result);
  }

  private async handleResumeConsumer(req: AnyPipRpcRequest): Promise<void> {
    const params = (req as { params: { consumerId: string } }).params;
    await this.voiceService.forwardToServer('resume-consumer', {
      consumerId: params.consumerId,
    });
    this.respond(req.id, { success: true });
  }

  private async handlePauseConsumer(req: AnyPipRpcRequest): Promise<void> {
    const params = (req as { params: { consumerId: string } }).params;
    await this.voiceService.forwardToServer('pause-consumer', {
      consumerId: params.consumerId,
    });
    this.respond(req.id, { success: true });
  }

  private async handleAction(req: AnyPipRpcRequest): Promise<void> {
    const params = (req as { params: { action: string } }).params;
    switch (params.action) {
      case 'toggle-mute':
        await this.voiceService.toggleMute();
        break;
      case 'toggle-deafen':
        this.voiceService.toggleDeafen();
        break;
      case 'toggle-video':
        await this.voiceService.toggleVideo();
        break;
      case 'toggle-screen':
        await this.voiceService.toggleScreenShare();
        break;
      case 'leave':
        await this.voiceService.leaveChannel();
        break;
      default:
        this.respond(req.id, undefined, `Unknown action: ${params.action}`);
        return;
    }
    this.respond(req.id, { success: true });
  }

  private async handlePipReady(req: AnyPipRpcRequest): Promise<void> {
    const params = (
      req as { params: { consumerSources: Array<{ source: string; producerUserId: string }> } }
    ).params;
    const { pipId } = req;

    // Pause main window consumers that match the PiP's consumed sources
    // This is the consumer ownership transfer — main window stops decoding,
    // PiP has its own consumers active
    const meta = this.voiceService.getConsumerMeta();
    const toPause: string[] = [];

    for (const pipSource of params.consumerSources) {
      for (const [consumerId, m] of meta) {
        if (m.source === pipSource.source && m.producerUserId === pipSource.producerUserId) {
          toPause.push(consumerId);
        }
      }
    }

    for (const id of toPause) {
      this.voiceService.pauseConsumer(id);
    }

    this.pausedForPip.set(pipId, toPause);

    // Notify PiP that ownership transfer is complete
    this.broadcast({
      kind: 'broadcast',
      type: 'ownership-transferred',
      pipId,
      pausedConsumerIds: toPause,
    } satisfies OwnershipTransferBroadcast);

    this.respond(req.id, { success: true, pausedCount: toPause.length });
  }

  private async handlePipClosing(req: AnyPipRpcRequest): Promise<void> {
    const { pipId } = req;
    this.resumePausedForPip(pipId);
    this.respond(req.id, { success: true });
  }

  // ── PiP close handler (called from Electron IPC) ───────────────

  /**
   * Called when a PiP BrowserWindow is closed (including abnormal close).
   * Resumes any main window consumers that were paused for that PiP.
   */
  onPipClosed(pipId: string): void {
    this.resumePausedForPip(pipId);
  }

  private resumePausedForPip(pipId: string): void {
    const paused = this.pausedForPip.get(pipId);
    if (!paused) return;

    for (const id of paused) {
      try {
        this.voiceService.resumeConsumer(id);
      } catch {
        /* consumer may have been closed */
      }
    }
    this.pausedForPip.delete(pipId);
  }

  // ── Outgoing broadcasts ─────────────────────────────────────────

  broadcastStateUpdate(
    participants: Record<string, unknown>,
    tunedInScreenShares: Record<string, string>
  ): void {
    // Strip MediaStream objects before posting — BroadcastChannel uses
    // the structured clone algorithm which cannot serialize MediaStreams.
    const sanitized: Record<string, unknown> = {};
    for (const [key, p] of Object.entries(participants)) {
      const {
        videoStream: _videoStream,
        screenStream: _screenStream,
        audioStream: _audioStream,
        screenAudioStream: _screenAudioStream,
        ...rest
      } = p as Record<string, unknown> & {
        videoStream?: unknown;
        screenStream?: unknown;
        audioStream?: unknown;
        screenAudioStream?: unknown;
      };
      sanitized[key] = rest;
    }
    const localUserId = useUserStore.getState().user?.id ?? '';

    this.broadcast({
      kind: 'broadcast',
      type: 'state-update',
      // eslint-disable-next-line @typescript-eslint/no-explicit-any -- `sanitized` is already shape-compatible with StateUpdateBroadcast['participants'] at runtime, but the TS types (with their many optional mediasoup-client fields) aren't ergonomic to convert without another pass; widening to `any` at the broadcast boundary is intentional
      participants: sanitized as any,
      tunedInScreenShares,
      localUserId,
    } satisfies StateUpdateBroadcast);
  }

  broadcastProducerAdded(producerId: string, userId: string, source: string): void {
    this.broadcast({
      kind: 'broadcast',
      type: 'producer-added',
      producerId,
      userId,
      source,
    } satisfies ProducerAddedBroadcast);
  }

  broadcastProducerClosed(producerId: string, userId: string): void {
    this.broadcast({
      kind: 'broadcast',
      type: 'producer-closed',
      producerId,
      userId,
    } satisfies ProducerClosedBroadcast);
  }

  // ── Internal helpers ────────────────────────────────────────────

  private respond(requestId: string, result?: unknown, error?: string): void {
    const response: PipRpcResponse = {
      kind: 'rpc-response',
      id: requestId,
      result,
      error,
    };
    try {
      this.bc.postMessage(response);
    } catch (err) {
      console.error('[PipSignalingProxy] Failed to send response:', errorMessage(err));
    }
  }

  private broadcast(msg: PipChannelMessage): void {
    if (this.disposed) return;
    try {
      this.bc.postMessage(msg);
    } catch (err) {
      console.error('[PipSignalingProxy] Failed to broadcast:', errorMessage(err));
    }
  }
}
