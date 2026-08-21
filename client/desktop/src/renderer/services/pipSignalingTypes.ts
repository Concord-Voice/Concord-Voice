/**
 * Typed protocol for BroadcastChannel communication between
 * the main renderer window and PiP BrowserWindows.
 *
 * PiP windows don't get their own Socket.IO connection to the media plane.
 * Instead, they proxy all SFU signaling through the main window via
 * BroadcastChannel RPC.
 *
 * Channel name: 'concord-pip'
 */

import type { VoiceParticipant } from '../stores/voiceStore';
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

let _reqCounter = 0;
export function generateRequestId(pipId: string): string {
  return `${pipId}-${++_reqCounter}-${Date.now().toString(36)}`;
}
