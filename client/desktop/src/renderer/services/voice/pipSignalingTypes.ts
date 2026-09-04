/**
 * Typed protocol for BroadcastChannel communication between
 * the main renderer window and PiP BrowserWindows.
 *
 * PiP windows don't get their own Socket.IO connection to the media plane.
 * Instead, they proxy all SFU signaling through the main window via
 * BroadcastChannel RPC.
 *
 * **Channel name: `concord-pip:<session token>` — never plain `concord-pip`
 * (#3104 D6).** A `BroadcastChannel` reaches every same-origin document in the
 * partition, so a shared name gave any frame in the app both an RPC entry point
 * (including the privileged `action: leave`) and a copy of every reply, which
 * since #3104 carries live HMAC TURN credentials. The main process mints a
 * per-window token at `pip:open` and discloses it only to the main window
 * (`pip:opened`) and to that PiP's own main frame (`pip:session`); naming the
 * channel from it makes possession of the channel the proof of identity.
 *
 * **The token is therefore never placed IN a message.** The #3104 design sketch
 * put it in the RPC envelope and moved only the replies to the private channel;
 * that is unsound — the envelope would travel on the shared channel and hand the
 * eavesdropper the capability. Both directions move instead, and the envelope's
 * `pipId` is treated as untrusted decoration: the proxy uses the id bound to the
 * channel the message arrived on.
 */

import type { VoiceParticipant } from '../../stores/voice/voiceStore';
import type { RtpCapabilities, RtpParameters, DtlsParameters } from 'mediasoup-client/types';
import type { RemoteVideoRole } from './remoteVideoLayerPolicy';

// ── RPC Request/Response ────────────────────────────────────────────

/** Base envelope for all RPC requests (PiP → Main) */
export interface PipRpcRequest<M extends string = string, P = unknown> {
  kind: 'rpc-request';
  id: string; // Unique request ID for correlating responses
  pipId: string; // Which PiP window sent this
  method: M;
  params: P;
}

/** Base envelope for RPC responses (Main → PiP) */
export interface PipRpcResponse<R = unknown> {
  kind: 'rpc-response';
  id: string; // Matches the request ID
  result?: R;
  error?: string;
}

// ── RPC Methods ─────────────────────────────────────────────────────

/** Create a recv transport on the SFU for this PiP window */
export type CreateRecvTransportRequest = PipRpcRequest<
  'create-recv-transport',
  {
    forceTcp?: boolean;
  }
>;
export interface CreateRecvTransportResult {
  transportId: string;
  iceParameters: unknown;
  iceCandidates: unknown[];
  dtlsParameters: unknown;
  /**
   * Server-minted STUN/TURN for this PiP's own transport (#3104).
   *
   * Carried on THIS result rather than on VoiceStateResult for three reasons:
   * the other four transport parameters already travel here and there is exactly
   * one consumer; `request-state` is retried, so it would give the credentials a
   * longer residency; and VoiceStateResult is the payload already treated as
   * sanitization-sensitive (see `sanitizedParticipants` in pipSignalingProxy).
   *
   * Absent when the main window holds no list — the PiP then builds its
   * transport exactly as it did before #3104.
   *
   * Delivered only on the requesting session's private channel (#3104 D6), so a
   * document that holds no capability neither receives it nor can ask for it.
   */
  iceServers?: RTCIceServer[];
}

/** Connect a recv transport (DTLS handshake) */
export type ConnectTransportRequest = PipRpcRequest<
  'connect-transport',
  {
    transportId: string;
    dtlsParameters: DtlsParameters;
  }
>;

/** Consume a producer through the PiP's own recv transport */
export type ConsumeRequest = PipRpcRequest<
  'consume',
  {
    producerId: string;
    transportId: string;
    rtpCapabilities: RtpCapabilities;
  }
>;
export interface ConsumeResult {
  consumerId: string;
  producerId: string;
  kind: 'audio' | 'video';
  rtpParameters: RtpParameters;
}

/** Resume a consumer (mediasoup consumers start paused) */
export type ResumeConsumerRequest = PipRpcRequest<
  'resume-consumer',
  {
    consumerId: string;
  }
>;

/** Pause a consumer */
export type PauseConsumerRequest = PipRpcRequest<
  'pause-consumer',
  {
    consumerId: string;
  }
>;

/** Close one consumer owned by this PiP instance */
export type CloseConsumerRequest = PipRpcRequest<
  'close-consumer',
  {
    consumerId: string;
  }
>;

/** Close the receive transport owned by this PiP instance */
export type CloseRecvTransportRequest = PipRpcRequest<
  'close-recv-transport',
  {
    transportId: string;
  }
>;

/**
 * Report receiver render-state demand for one of THIS PiP's own consumers (#1924).
 * A PiP window has a socket-less voiceService, so it can't emit set-preferred-layers
 * itself; it proxies this to the main window, which addresses the PiP's OWN consumer
 * id on the SFU. Without it an H264/VP8 simulcast screen stays stuck at spatial
 * layer 0 in the PiP.
 */
export type SetPreferredLayersRequest = PipRpcRequest<
  'set-preferred-layers',
  {
    consumerId: string;
    cssWidth: number;
    cssHeight: number;
    visible: boolean;
    role: RemoteVideoRole;
    focusedWindow: boolean;
  }
>;

/** Request current voice state (participants, screen shares, etc.) */
export type RequestStateRequest = PipRpcRequest<'request-state', Record<string, never>>;
export interface VoiceStateResult {
  participants: Record<string, VoiceParticipant>;
  tunedInScreenShares: Record<string, string>;
  routerRtpCapabilities: RtpCapabilities | null;
  /** Active producer IDs that the PiP should consume */
  activeProducers: Array<{
    producerId: string;
    userId: string;
    source: string; // 'mic' | 'camera' | 'screen'
  }>;
  /** The local user's ID so PiP windows can identify which participant is "self" */
  localUserId: string;
}

/** Execute a voice action (mute, deafen, leave, etc.) */
export type ActionRequest = PipRpcRequest<
  'action',
  {
    action: 'toggle-mute' | 'toggle-deafen' | 'leave' | 'toggle-video' | 'toggle-screen';
  }
>;

/** Signal that PiP has consumed all producers and is ready */
export type PipReadyRequest = PipRpcRequest<
  'pip-ready',
  {
    /** Consumer IDs created by this PiP, so main can pause its matching ones */
    consumerSources: Array<{ source: string; producerUserId: string }>;
  }
>;

/**
 * Ask the main window to derive a decrypt frame key for one sender/epoch (2026-08-21 PiP E2EE gap).
 *
 * A PiP BrowserWindow is a separate renderer context with no e2eeService, no
 * IndexedDB private key and no CSK unwrap — it only needs the derived frame
 * key. `CryptoKey` is structured-cloneable, so the key crosses the
 * BroadcastChannel as a handle rather than raw bytes. Frame keys are EXTRACTABLE by
 * construction — `deriveFrameKey`/`ratchetKey` must export raw bytes to ratchet
 * the next epoch — so passing the `CryptoKey` handle avoids materializing key
 * bytes in JS buffers but does NOT make the transport safe against a
 * same-origin listener on this channel. That is the same trust boundary the
 * main window's `worker.postMessage` already relies on, widened from
 * point-to-point to a named channel; treat same-origin script execution as
 * out-of-model (it can read the CSK directly).
 *
 * `keyVersion`/`keyId` are omitted for the initial pre-consume provision (main
 * answers with the channel's current pair) and supplied verbatim when the PiP's
 * E2EE Worker reports a typed decrypt miss for an exact epoch.
 */
export type GetFrameKeyRequest = PipRpcRequest<
  'get-frame-key',
  {
    senderUserId: string;
    keyVersion?: number;
    keyId?: number;
  }
>;
export interface GetFrameKeyResult {
  key: CryptoKey;
  keyVersion: number;
  keyId: number;
}

/** Signal that PiP is about to close (pre-close for smooth transition) */
export type PipClosingRequest = PipRpcRequest<'pip-closing', Record<string, never>>;

// Union of all RPC request types
export type AnyPipRpcRequest =
  | CreateRecvTransportRequest
  | ConnectTransportRequest
  | ConsumeRequest
  | ResumeConsumerRequest
  | PauseConsumerRequest
  | CloseConsumerRequest
  | CloseRecvTransportRequest
  | SetPreferredLayersRequest
  | RequestStateRequest
  | ActionRequest
  | PipReadyRequest
  | PipClosingRequest
  | GetFrameKeyRequest;

// ── Broadcast Events (Main → PiP, no request ID) ───────────────────

/** Participant state update */
export interface StateUpdateBroadcast {
  kind: 'broadcast';
  type: 'state-update';
  participants: Record<string, VoiceParticipant>;
  tunedInScreenShares: Record<string, string>;
  localUserId: string;
}

/** New producer available to consume */
export interface ProducerAddedBroadcast {
  kind: 'broadcast';
  type: 'producer-added';
  producerId: string;
  userId: string;
  source: string;
}

/** Producer removed — PiP should close its consumer */
export interface ProducerClosedBroadcast {
  kind: 'broadcast';
  type: 'producer-closed';
  producerId: string;
  userId: string;
}

/** Main window has paused its consumers — ownership transferred to PiP */
export interface OwnershipTransferBroadcast {
  kind: 'broadcast';
  type: 'ownership-transferred';
  pipId: string;
  pausedConsumerIds: string[];
}

/** Voice session ended (user left or disconnected) */
export interface VoiceEndedBroadcast {
  kind: 'broadcast';
  type: 'voice-ended';
}

export type AnyPipBroadcast =
  | StateUpdateBroadcast
  | ProducerAddedBroadcast
  | ProducerClosedBroadcast
  | OwnershipTransferBroadcast
  | VoiceEndedBroadcast;

// ── Combined message type for the channel ───────────────────────────

export type PipChannelMessage = AnyPipRpcRequest | PipRpcResponse | AnyPipBroadcast;

// ── Helpers ─────────────────────────────────────────────────────────

/**
 * Legacy shared channel name, kept ONLY as the private channels' prefix.
 * Nothing subscribes to it and nothing is ever posted on it — see the module
 * header. Do not reintroduce a listener here "for compatibility": that would
 * restore the exact ingress #3104 D6 closed.
 */
export const PIP_CHANNEL_PREFIX = 'concord-pip';

/**
 * The private channel one PiP session runs on. The token is a base64url secret
 * minted by the main process (`mintPipSessionToken`), so every character is a
 * legal channel-name character and the name is unguessable. Knowing the name IS
 * the capability — never log it, and never post it.
 */
export function pipSessionChannelName(token: string): string {
  return `${PIP_CHANNEL_PREFIX}:${token}`;
}

let _reqCounter = 0;
export function generateRequestId(pipId: string): string {
  return `${pipId}-${++_reqCounter}-${Date.now().toString(36)}`;
}
