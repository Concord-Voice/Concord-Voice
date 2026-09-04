/**
 * PiP Signaling Proxy — runs in the main renderer window.
 *
 * Forwards SFU signaling from PiP BrowserWindows to the media plane via
 * voiceService, and broadcasts participant state changes and producer events so
 * PiP windows stay in sync.
 *
 * **Every message travels on a per-session private BroadcastChannel (#3104 D6).**
 * The proxy opens one channel per PiP window, named from a capability the main
 * process minted for that window and disclosed to nobody else
 * (`pipSessionChannelName`). Arriving on that channel is the authentication:
 * before this, `handleMessage` checked the message KIND and `respond()` fanned
 * every reply out on a shared `concord-pip` channel, so any same-origin document
 * could both demand the reply — which since #3104 carries live HMAC TURN
 * credentials — and overhear a legitimate one, and could drive the privileged
 * `action: leave`. Nothing subscribes to or posts on the shared name any more.
 *
 * Lifecycle:
 *   - Created when user joins a voice channel
 *   - `registerSession` on each `pip:opened` push; `revokeSession` on close
 *   - Disposed when user leaves the voice channel
 */

import {
  pipSessionChannelName,
  type PipChannelMessage,
  type PipRpcRequest,
  type AnyPipRpcRequest,
  type PipRpcResponse,
  type StateUpdateBroadcast,
  type ProducerAddedBroadcast,
  type ProducerClosedBroadcast,
  type OwnershipTransferBroadcast,
  type VoiceEndedBroadcast,
  type VoiceStateResult,
  type CreateRecvTransportResult,
  type ConsumeResult,
  type GetFrameKeyResult,
} from './pipSignalingTypes';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { useUserStore } from '../../stores/auth/userStore';
import { errorMessage } from '../../utils/runtime/redactError';
import type { RtpCapabilities } from 'mediasoup-client/types';
import type { RemoteVideoRole } from './remoteVideoLayerPolicy';

/** Render-state demand for a specific consumer (#1924) — mirrors voiceService's
 *  private RemoteVideoTileRenderState so the PiP proxy can forward it structurally. */
interface PipConsumerRenderState {
  visible: boolean;
  cssWidth: number;
  cssHeight: number;
  role: RemoteVideoRole;
  focusedWindow: boolean;
}

/**
 * The slice of `VoiceService` this proxy depends on. Exported so the one
 * production call site (`MainView`) can be typechecked against it — an
 * unexported structural type forced an `as any` there, which meant a member
 * dropped from this contract would have compiled cleanly (#3104).
 */
export type PipVoiceServiceContract = {
  forwardToServer<T>(event: string, data?: unknown): Promise<T>;
  getRouterRtpCapabilities(): RtpCapabilities | null;
  getConsumerIdsBySource(source?: string): string[];
  getConsumerMeta(): Map<string, { source: string; producerUserId: string; producerId: string }>;
  pauseConsumer(consumerId: string): void;
  resumeConsumer(consumerId: string): void;
  emitPreferredLayersForConsumer(consumerId: string, renderState: PipConsumerRenderState): void;
  deriveFrameKeyForPip(
    senderUserId: string,
    keyVersion?: number,
    keyId?: number
  ): Promise<{ key: CryptoKey; keyVersion: number; keyId: number }>;
  /** The server-minted ICE list held for the current media session, or null (#3104). */
  getIceServersForPip(): RTCIceServer[] | null;
  toggleMute(): Promise<void>;
  toggleDeafen(): void;
  toggleVideo(): Promise<void>;
  toggleScreenShare(sourceId?: string): Promise<void>;
  leaveChannel(): Promise<void>;
};

/**
 * One authenticated PiP window. `pipId` is the id the MAIN PROCESS bound to the
 * capability, not the one an envelope claims — the two differ exactly when a
 * caller is lying about which window it is.
 */
interface PipSession {
  readonly pipId: string;
  readonly channel: BroadcastChannel;
}

export class PipSignalingProxy {
  private readonly voiceService: PipVoiceServiceContract;
  private disposed = false;

  /** Live sessions, keyed by the main-process pipId. */
  private readonly sessions = new Map<string, PipSession>();

  /**
   * Consumer IDs in the main window that were paused for a given PiP window.
   * Key: pipId, Value: array of paused consumer IDs.
   * Used to resume them when the PiP closes.
   */
  private readonly pausedForPip: Map<string, string[]> = new Map();

  /** Zustand unsubscribe function for state change broadcasting */
  private stateUnsub: (() => void) | null = null;

  constructor(voiceService: PipVoiceServiceContract) {
    this.voiceService = voiceService;

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

  // ── Session admission (#3104 D6) ────────────────────────────────

  /**
   * Admit one PiP window and open the private channel its capability names.
   *
   * Called from the `pip:opened` push, which fires at window CREATION — before
   * the PiP's own renderer has booted — so the channel is listening by the time
   * that renderer pulls the same token over `pip:session` and issues its first
   * RPC. A PiP whose session was never registered is indistinguishable from an
   * arbitrary document and is served nothing.
   *
   * Idempotent for a repeated (pipId, token) pair. A pipId re-registered under a
   * NEW token replaces the old channel, so a stale capability cannot outlive its
   * window.
   */
  registerSession(pipId: string, token: string): void {
    if (this.disposed) return;
    if (typeof pipId !== 'string' || pipId.length === 0) return;
    if (typeof token !== 'string' || token.length === 0) return;

    const name = pipSessionChannelName(token);
    const existing = this.sessions.get(pipId);
    if (existing) {
      if (existing.channel.name === name) return;
      this.closeSession(existing);
    }

    const channel = new BroadcastChannel(name);
    const session: PipSession = { pipId, channel };
    channel.onmessage = (event: MessageEvent<PipChannelMessage>) =>
      this.handleMessage(session, event);
    this.sessions.set(pipId, session);
  }

  /**
   * Drop a capability. The channel is closed, so a document that somehow learned
   * the token is left talking to nothing — revocation is structural, not a flag
   * consulted at dispatch time.
   */
  revokeSession(pipId: string): void {
    const session = this.sessions.get(pipId);
    if (!session) return;
    this.sessions.delete(pipId);
    this.closeSession(session);
  }

  private closeSession(session: PipSession): void {
    session.channel.onmessage = null;
    try {
      session.channel.close();
    } catch {
      /* already closed */
    }
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
    for (const session of this.sessions.values()) this.closeSession(session);
    this.sessions.clear();
  }

  // ── Incoming message handler ────────────────────────────────────

  /**
   * Handle one message on ONE session's private channel.
   *
   * The `session` argument is the authentication: this callback is only ever
   * bound to a channel whose name came from a main-process capability. The kind
   * check below is routing. `session` is threaded through every handler instead
   * of being read off the envelope, so a PiP cannot act as another PiP by
   * claiming its `pipId`.
   */
  private handleMessage(session: PipSession, event: MessageEvent<PipChannelMessage>): void {
    const msg = event.data;
    if (this.disposed) return;
    if (msg?.kind !== 'rpc-request') return;

    this.handleRpcRequest(session, msg).catch((err) => {
      console.error('[PipSignalingProxy] Unhandled RPC error:', errorMessage(err));
    });
  }

  private async handleRpcRequest(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
    try {
      switch (req.method) {
        case 'request-state':
          await this.handleRequestState(session, req);
          break;
        case 'create-recv-transport':
          await this.handleCreateRecvTransport(session, req);
          break;
        case 'connect-transport':
          await this.handleConnectTransport(session, req);
          break;
        case 'consume':
          await this.handleConsume(session, req);
          break;
        case 'resume-consumer':
          await this.handleResumeConsumer(session, req);
          break;
        case 'pause-consumer':
          await this.handlePauseConsumer(session, req);
          break;
        case 'close-consumer':
          await this.handleCloseConsumer(session, req);
          break;
        case 'close-recv-transport':
          await this.handleCloseRecvTransport(session, req);
          break;
        case 'set-preferred-layers':
          this.handleSetPreferredLayers(session, req);
          break;
        case 'action':
          await this.handleAction(session, req);
          break;
        case 'pip-ready':
          await this.handlePipReady(session, req);
          break;
        case 'pip-closing':
          await this.handlePipClosing(session, req);
          break;
        case 'get-frame-key':
          await this.handleGetFrameKey(session, req);
          break;
        default: {
          const unknownReq = req as PipRpcRequest;
          this.respond(session, unknownReq.id, undefined, `Unknown method: ${unknownReq.method}`);
        }
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      this.respond(session, req.id, undefined, message);
    }
  }

  // ── RPC handlers ────────────────────────────────────────────────

  private async handleRequestState(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
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
    this.respond(session, req.id, result);
  }

  private async handleCreateRecvTransport(
    session: PipSession,
    req: AnyPipRpcRequest
  ): Promise<void> {
    const params = (req as { params: { forceTcp?: boolean } }).params;
    // `iceServers` is destructured away rather than merely Omit-ed from the reply
    // type: the media plane is not a carrier for ICE credentials, and a type-level
    // Omit erases at runtime, so an `iceServers` field arriving from the SFU would
    // otherwise survive `...result` whenever the main window holds no list of its
    // own. Naming it here drops it structurally, on every path.
    const {
      id,
      iceServers: _sfuIceServers,
      ...result
    } = await this.voiceService.forwardToServer<
      Omit<CreateRecvTransportResult, 'transportId'> & { id: string }
    >('create-transport', { direction: 'recv', forceTcp: params?.forceTcp });
    // The PiP window builds its own RTCPeerConnection and never sees joinData,
    // so the main window hands it the same server-minted ICE list (#3104).
    //
    // This reply carries live HMAC TURN credentials, and it is point-to-point:
    // `respond` posts on THIS session's private channel, which is named from a
    // capability the main process disclosed only to this PiP's own main frame
    // (#3104 D6). Before D6 it went out on the shared `concord-pip` name, where
    // an arbitrary same-origin document could both demand it and overhear a
    // legitimate one. Keep it that way — a reply routed anywhere else, or an RPC
    // ingress re-added on the shared name, reopens the disclosure. NC-1 still
    // binds inside the receiving window.
    const iceServers = this.voiceService.getIceServersForPip();
    this.respond(session, req.id, {
      ...result,
      transportId: id,
      // Empty list omits the key, so a degraded session's payload is deep-equal
      // to the pre-#3104 shape (D2).
      ...(iceServers && iceServers.length > 0 ? { iceServers } : {}),
    } satisfies CreateRecvTransportResult);
  }

  private async handleConnectTransport(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
    const params = (req as { params: { transportId: string; dtlsParameters: unknown } }).params;
    await this.voiceService.forwardToServer('connect-transport', {
      transportId: params.transportId,
      dtlsParameters: params.dtlsParameters,
    });
    this.respond(session, req.id, { success: true });
  }

  private async handleConsume(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
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
    this.respond(session, req.id, result);
  }

  private async handleResumeConsumer(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
    const params = (req as { params: { consumerId: string } }).params;
    await this.voiceService.forwardToServer('resume-consumer', {
      consumerId: params.consumerId,
    });
    this.respond(session, req.id, { success: true });
  }

  private async handlePauseConsumer(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
    const params = (req as { params: { consumerId: string } }).params;

    await this.voiceService.forwardToServer('pause-consumer', {
      consumerId: params.consumerId,
    });
    this.respond(session, req.id, { success: true });
  }

  private async handleCloseConsumer(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
    const params = (req as { params: { consumerId: string } }).params;
    await this.voiceService.forwardToServer('close-consumer', {
      consumerId: params.consumerId,
    });
    this.respond(session, req.id, { success: true });
  }

  private async handleCloseRecvTransport(
    session: PipSession,
    req: AnyPipRpcRequest
  ): Promise<void> {
    const params = (req as { params: { transportId: string } }).params;
    await this.voiceService.forwardToServer('close-recv-transport', {
      transportId: params.transportId,
    });
    this.respond(session, req.id, { success: true });
  }

  /**
   * PiP E2EE: derive one decrypt frame key for a PiP window's own E2EE Worker.
   *
   * The PiP consumes producers independently, so its receive transforms need
   * their own keys. Only the derived `CryptoKey` crosses the channel — it is
   * structured-cloneable, so no raw key bytes are serialized — though the
   * handle itself is extractable (the ratchet requires it), so this is not a
   * boundary against same-origin script. See pipSignalingTypes. A throw here becomes an RPC error, which the PiP treats as
   * fail-closed (the stream is not played).
   */
  private async handleGetFrameKey(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
    const params = (
      req as { params: { senderUserId: string; keyVersion?: number; keyId?: number } }
    ).params;
    const result = await this.voiceService.deriveFrameKeyForPip(
      params.senderUserId,
      params.keyVersion,
      params.keyId
    );
    this.respond(session, req.id, result satisfies GetFrameKeyResult);
  }

  /**
   * #1924: a PiP window reports render-state demand for its OWN consumer. Address that
   * consumer id directly via the main window's socket — do NOT resolve by user, which
   * would target the main window's now-PAUSED screen consumer (the wrong one).
   */
  private handleSetPreferredLayers(session: PipSession, req: AnyPipRpcRequest): void {
    const params = (
      req as {
        params: {
          consumerId: string;
          cssWidth: number;
          cssHeight: number;
          visible: boolean;
          role: RemoteVideoRole;
          focusedWindow: boolean;
        };
      }
    ).params;
    this.voiceService.emitPreferredLayersForConsumer(params.consumerId, {
      visible: params.visible,
      cssWidth: params.cssWidth,
      cssHeight: params.cssHeight,
      role: params.role,
      focusedWindow: params.focusedWindow,
    });
    this.respond(session, req.id, { success: true });
  }

  private async handleAction(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
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
        this.respond(session, req.id, undefined, `Unknown action: ${params.action}`);
        return;
    }
    this.respond(session, req.id, { success: true });
  }

  private async handlePipReady(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
    const params = (
      req as { params: { consumerSources: Array<{ source: string; producerUserId: string }> } }
    ).params;
    // The session's id, NOT `req.pipId`: the envelope field is decoration a
    // caller controls, and keying the ownership-transfer map by it would let one
    // PiP hand another PiP's consumers back to the main window.
    const { pipId } = session;

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

    this.respond(session, req.id, { success: true, pausedCount: toPause.length });
  }

  private async handlePipClosing(session: PipSession, req: AnyPipRpcRequest): Promise<void> {
    const { pipId } = session;
    this.resumePausedForPip(pipId);
    this.respond(session, req.id, { success: true });
  }

  // ── PiP close handler (called from Electron IPC) ───────────────

  /**
   * Called when a PiP BrowserWindow is closed (including abnormal close).
   * Revokes that window's capability and resumes any main-window consumers that
   * were paused for it. Revocation first: a closed window has no legitimate
   * traffic left, and the channel is the only thing standing between a leaked
   * token and a live RPC surface.
   */
  onPipClosed(pipId: string): void {
    this.revokeSession(pipId);
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

  /**
   * Reply to exactly one session. Point-to-point by construction: the reply goes
   * on the requester's private channel, so it is not merely addressed to the PiP
   * — it is unreachable by anything else, which is what closes the passive
   * eavesdrop half of the #3104 D6 finding.
   */
  private respond(session: PipSession, requestId: string, result?: unknown, error?: string): void {
    const response: PipRpcResponse = {
      kind: 'rpc-response',
      id: requestId,
      result,
      error,
    };
    try {
      session.channel.postMessage(response);
    } catch (err) {
      console.error('[PipSignalingProxy] Failed to send response:', errorMessage(err));
    }
  }

  /**
   * Fan a state event out to every admitted PiP — one post per private channel
   * rather than one post on a shared one. A document holding no capability is
   * not a recipient.
   */
  private broadcast(msg: PipChannelMessage): void {
    if (this.disposed) return;
    for (const session of this.sessions.values()) {
      try {
        session.channel.postMessage(msg);
      } catch (err) {
        console.error('[PipSignalingProxy] Failed to broadcast:', errorMessage(err));
      }
    }
  }
}
