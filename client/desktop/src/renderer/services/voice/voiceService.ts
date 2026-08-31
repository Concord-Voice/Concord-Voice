/**
 * VoiceService — mediasoup-client wrapper for voice, video, and screen sharing.
 *
 * Manages the Socket.IO signaling connection to the media plane and the
 * mediasoup Device (send/recv transports, producers, consumers).
 *
 * Join flow:
 *   1. POST /channels/{id}/voice/join → media_server_url, ICE servers (channels are always E2EE)
 *   2. Connect Socket.IO to media plane with JWT auth
 *   3. Emit join-room → receive router RTP caps + existing producers
 *   4. device.load(routerRtpCapabilities)
 *   5. Create send + recv transports
 *   6. Produce audio (mic)
 *   7. Consume all existing producers
 *   8. Listen for new-producer events to auto-consume joiners
 */

import { Device, types as mediasoupTypes } from 'mediasoup-client';
import { io, Socket } from 'socket.io-client';
import { useAuthStore } from '../../stores/auth/authStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useUpdateStatusStore } from '../../stores/ui/updateStatusStore';
import { useSubscriptionStore } from '../../stores/auth/subscriptionStore';
import {
  useVoiceStore,
  AUDIO_QUALITY_TIERS,
  MAX_TUNED_SCREEN_SHARES,
  type AudioQualityTier,
  type VoiceParticipant,
} from '../../stores/voice/voiceStore';
import { useAudioSettingsStore, type AudioPriority } from '../../stores/audio/audioSettingsStore';
import {
  useVideoSettingsStore,
  VIDEO_QUALITY_PRESETS,
  type VideoPriority,
  type ScreenShareOptions,
} from '../../stores/voice/videoSettingsStore';
import { apiFetch } from '../system/apiClient';
import {
  BYPASS_PROBE_DELAY_MS,
  buildDecryptCreationAttach,
  BYPASS_PROBE_MAX_ATTEMPTS,
  BYPASS_PROBE_SLOW_DELAY_MS,
  decideBypassProbeAction,
  type BypassProbePhase,
} from '../e2ee/voiceTransformBypass';
import {
  MEDIA_E2EE_FRAME_CRYPTO_VERSION,
  MediaEncryption,
  deriveFrameKey,
} from '../e2ee/mediaEncryption';
import { e2eeService } from '../e2ee/e2eeService';
import { E2EEKeyUnavailableError, isPendingKeyError } from '../e2ee/e2eeErrors';
import {
  useOsPermissionStore,
  ensureOsPermission as ensureOsPermissionShared,
} from '../../stores/voice/osPermissionStore';
import {
  codecFamilyFromMimeType,
  codecFamilyFromRtpParameters,
  type CodecFamily,
  type E2EEWorkerMessage,
  type E2EEMainMessage,
  type E2EETransformOptions,
} from '../../workers/e2eeProtocol';
import { notificationSoundService } from '../system/notificationSoundService';
import {
  h264ProfileClass,
  h264ProfilesCompatible,
  isCodecKeyInFloor,
  selectCodecFromCascade,
  type CodecLookup,
} from './voiceCodecSelection';
import { extractWebrtcHwSignal, shouldReselectForHwDowngrade } from './webrtcHwSignal';
import {
  FORCE_LEGACY_E2EE_KEY,
  resolveEncodedTransformSupport,
  type EncodedTransformPath,
} from '../e2ee/encodedTransformSupport';
import {
  DecoderBudgetSampler,
  selectInboundVideoDecoderReport,
  type SelectedDecoderStatsReport,
} from './decoderBudgetSampler';
import { ConsumerPauseCoordinator } from './consumerPauseCoordinator';
import { buildCameraEncodingPlan } from './cameraLayering';
import {
  computeRemoteVideoLayerRequest,
  type RemoteVideoLayerRequest,
  type RemoteVideoRole,
} from './remoteVideoLayerPolicy';
import {
  applyLegacyDecryptPipeline,
  type DecryptRecoveryCallbacks,
  type InsertableStreamsReceiver,
} from '../e2ee/voiceE2eeTransforms';
import { errorMessage } from '../../utils/runtime/redactError';
import { hasPermission, SPEAK } from '../../utils/policy/permissions';
import { clampScreenForSubscription } from '../../utils/policy/videoLimits';
import { SCREEN_RES_DIMS, resolveScreenDims } from '../../utils/ui/screenResolution';
import type { CallState } from './voiceService/callStateMachine';

// Toggle for verbose E2EE/SDP diagnostics — set to true when debugging
// frame drops, BUNDLE collisions, or key rotation issues. When false,
// only errors, warnings, and key lifecycle events are logged.
const E2EE_VERBOSE = false;
const MAX_REMOTE_VIDEO_DEVICE_PIXEL_RATIO = 8;

// Overall budget for the media-plane Socket.IO connect during a voice join (#2176).
// waitForConnect() rides through transient connect_errors (letting Socket.IO's
// reconnection retry a briefly-unavailable single-node media-plane, e.g. during a
// deploy container recreate) up to this cap, then fails the join. Longer than the
// pre-#2176 flat 10s so a short restart is survived; bounded so a genuinely-down
// server still surfaces promptly rather than hanging "connecting" forever.
const VOICE_CONNECT_TIMEOUT_MS = 30_000;

// Resolve one E2EE transform path at module load. Modern Worker transforms do not
// use the legacy encodedInsertableStreams transport option.
interface RtpSenderWithEncodedStreams extends RTCRtpSender {
  createEncodedStreams?: () => { readable: ReadableStream; writable: WritableStream };
}

/** Decoder health zone classification for IGNIS profiling. */
type DecoderHealthZone = 'green' | 'yellow' | 'red';
type ConsumerLayerSelection = { spatialLayer: number; temporalLayer: number };

/** SFU-layer-aware consumer — mediasoup-client exposes currentLayers/setPreferredLayers
 *  on the server but not in the public client type definitions. */
interface ConsumerWithLayers {
  currentLayers?: ConsumerLayerSelection;
  setPreferredLayers(layers: ConsumerLayerSelection): void;
}

interface RemoteVideoTileRenderState {
  visible: boolean;
  cssWidth: number;
  cssHeight: number;
  role: RemoteVideoRole;
  focusedWindow: boolean;
}

interface E2EEInitContext {
  generation: number;
  channelId: string;
  userId: string;
  expectedMediaEncryption: MediaEncryption | null;
  expectedWorker: Worker | null;
  highestBufferedVersion: number;
  rotationOff: (() => void) | null;
  ownsSharedRotationSubscription: boolean;
  committed: boolean;
  liveRotations: boolean;
}

interface E2EEInitFlight {
  context: E2EEInitContext;
  promise: Promise<void>;
}

interface EncryptRebaseContext {
  channelId: string;
  keyVersion: number;
  userId: string;
  mediaEncryption: MediaEncryption;
  e2eeWorker: Worker | null;
}

type E2EEInitAttemptResult = { succeeded: true } | { succeeded: false; error: unknown };

interface DecryptKeyRequestContext {
  channelId: string;
  userId: string;
  targetEpoch: number | undefined;
  localUserId: string;
  mediaEncryption: MediaEncryption;
  e2eeWorker: Worker | null;
}

type DecryptKeyAttemptResult =
  { status: 'success' } | { status: 'stale' } | { status: 'retry' | 'failed'; error: unknown };

class E2EEInitializationSupersededError extends Error {
  constructor() {
    super('E2EE initialization superseded by a newer media session');
    this.name = 'E2EEInitializationSupersededError';
  }
}

interface RemoteVideoLayerPayload extends RemoteVideoTileRenderState, RemoteVideoLayerRequest {
  devicePixelRatio: number;
  pressureStepDown: boolean;
}

type CameraPressureLayerRequestResult = 'emitted' | 'handled' | 'fallback';

/** Which layered video surface a render-state report / demand emit targets (#1924).
 *  Camera and screen keep independent demand maps + server gates. */
type RemoteVideoSource = 'camera' | 'screen';

interface TestSuspensionRestorePolicy {
  keepAudioOutPaused: boolean;
  keepProducersPaused: boolean;
  keepMicPaused: boolean;
}
// Probed per call, not at module load: the legacy-fallback override can engage
// mid-session, and per-call probing keeps the decision testable and immune to
// late-defined globals. Called a handful of times per media session — never
// per frame — so the typeof checks are free.
function currentEncodedTransformApis() {
  return {
    scriptTransform:
      typeof RTCRtpScriptTransform === 'undefined' ? undefined : RTCRtpScriptTransform,
    createEncodedStreams:
      typeof RTCRtpSender === 'undefined'
        ? undefined
        : (RTCRtpSender.prototype as RtpSenderWithEncodedStreams).createEncodedStreams,
  };
}
// Session-sticky legacy override. Set automatically when the bypass probe
// confirms the engine ignores an attached receive transform even at receiver
// creation (Chromium 149/150 V2 regression), persisted for THIS page session
// only via sessionStorage — a fresh launch retries the modern path, since an
// engine update may have fixed it. localStorage 'concord.forceLegacyE2EE'='1'
// is the manual/support override and persists until cleared.
// In-memory fallback only when storage itself is unavailable; storage is the
// single source of truth otherwise, so tests reset by clearing storage.
let inMemoryLegacyOverride = false;

function readLegacyTransformOverride(): boolean {
  try {
    // Storage is the single source of truth when it works (tests reset by
    // clearing it); the in-memory flag matters only when storage throws.
    return (
      globalThis.localStorage?.getItem(FORCE_LEGACY_E2EE_KEY) === '1' ||
      globalThis.sessionStorage?.getItem('concord.e2eeLegacyFallback') === '1'
    );
  } catch {
    return inMemoryLegacyOverride;
  }
}

/** The active transform path. Dynamic — the legacy fallback can engage mid-session. */
function currentTransformPath(): EncodedTransformPath {
  return resolveEncodedTransformSupport(currentEncodedTransformApis(), {
    forceLegacy: readLegacyTransformOverride(),
  });
}

/** Engage the legacy fallback for the rest of this page session. Returns false if already engaged. */
function engageLegacyTransformOverride(): boolean {
  if (readLegacyTransformOverride()) return false;
  inMemoryLegacyOverride = true;
  try {
    globalThis.sessionStorage?.setItem('concord.e2eeLegacyFallback', '1');
  } catch {
    /* storage unavailable — the in-memory flag governs this session */
  }
  return true;
}

if (E2EE_VERBOSE) {
  console.debug('E2EE API detection:', {
    hasEncodedStreams: typeof currentEncodedTransformApis().createEncodedStreams === 'function',
    hasScriptTransform: typeof currentEncodedTransformApis().scriptTransform === 'function',
    selectedPath: currentTransformPath(),
  });
}

// ---------------------------------------------------------------------------
// Helpers to reduce cognitive complexity (extracted from class methods)
// ---------------------------------------------------------------------------

/** Resolve effective Opus codec settings from advanced settings + tier config. */
function resolveOpusSettings(
  adv: ReturnType<typeof useAudioSettingsStore.getState>,
  tierConfig: (typeof AUDIO_QUALITY_TIERS)[AudioQualityTier]
) {
  const effectiveFec = adv.advancedMode ? adv.inlineFec : tierConfig.opusFec;

  const effectiveDtx = adv.advancedMode
    ? adv.silenceDetection || tierConfig.opusDtx
    : tierConfig.opusDtx;

  const effectiveStereo =
    adv.advancedMode && adv.stereoOverride !== null ? adv.stereoOverride : tierConfig.opusStereo;

  let effectiveFrameSize: number;
  if (adv.adaptivePtime || !adv.advancedMode || adv.frameSize === 0) {
    effectiveFrameSize = tierConfig.preferredFrameSize;
  } else {
    effectiveFrameSize = adv.frameSize;
  }

  return { effectiveFec, effectiveDtx, effectiveStereo, effectiveFrameSize };
}

type RtpPriority = 'low' | 'medium' | 'high' | 'very-low';

/** Compute retry delay: double the base delay for pending E2EE key errors. */
function retryDelayForError(err: unknown, baseDelay: number): number {
  return isPendingKeyError(err) ? baseDelay * 2 : baseDelay;
}

/** Retry transient re-base failures; known access/payload failures are terminal. */
function isRetryableEncryptRebaseError(err: unknown): boolean {
  if (!(err instanceof E2EEKeyUnavailableError)) return true;
  return err.code === 'NO_KEY_YET' || err.code === 'INTERNAL_ERROR';
}

/** Handle screen capture NotAllowedError — show error and open OS settings. Returns true if handled. */
function handleScreenCaptureNotAllowed(captureErr: unknown): boolean {
  if (captureErr instanceof DOMException && captureErr.name === 'NotAllowedError') {
    useVoiceStore
      .getState()
      .setVideoSlotError(
        'Screen recording access denied. On macOS, enable Screen Recording in ' +
          'System Settings > Privacy & Security, then restart Concord.'
      );
    useOsPermissionStore.getState().openSettings('screen');
    return true;
  }
  return false;
}

/** Sentinel consumerId marking the LOCAL user's own screen share in
 *  tunedInScreenShares — there is no real consumer for your own stream. */
const LOCAL_SCREEN_CONSUMER_ID = 'local-screen';

/** Window (ms) during which a screensharer's producer-close immediately followed by
 *  a new screen announce is treated as a REPRODUCE (a fastReproduceScreen codec-floor
 *  or screen-layering-gate swap that closes the old producer and re-announces a new
 *  one) rather than a genuine stop. A viewer who was tuned into the closing producer
 *  auto-re-tunes-in to the new producer within this window — bypassing the
 *  autoTuneInScreenShares opt-in and preserving dominance — while a re-announce later
 *  than the window falls back to the normal opt-in path (#1924 review fix A). */
const SCREEN_REPRODUCE_RETUNE_WINDOW_MS = 3_000;

/** Update voice/user stores when screen sharing starts. */
function updateStoreForScreenShare(producerId: string, screenStream: MediaStream | null): void {
  const store = useVoiceStore.getState();
  store.setScreenSharing(true);
  const localUserId = useUserStore.getState().user?.id;
  if (localUserId && screenStream) {
    store.updateParticipant(localUserId, {
      screenStream,
      isScreenSharing: true,
    });
  }
  if (localUserId) {
    const localParticipant = store.participants[localUserId];
    store.registerActiveScreenShare({
      producerId,
      userId: localUserId,
      username: localParticipant?.username ?? 'You',
      displayName: localParticipant?.displayName,
      isLocal: true,
    });
  }
  store.tuneIn(producerId, LOCAL_SCREEN_CONSUMER_ID);
  if (!store.dominantScreenShareId) {
    store.setDominantScreenShare(producerId);
  }
}

/** Build DSCP priority params for RTP encoding (empty object when 'off'). */
function buildPriorityParams(
  priority: 'off' | RtpPriority
): Partial<{ priority: RtpPriority; networkPriority: RtpPriority }> {
  if (priority === 'off') return {};
  return { priority, networkPriority: priority };
}

// ---------------------------------------------------------------------------
// Types matching server signaling protocol
// ---------------------------------------------------------------------------

interface JoinResponse {
  allowed: boolean;
  call_id?: string;
  media_server_url: string;
  ice_servers: Array<{ urls: string; username?: string; credential?: string }>;
  // Server-channel responses include `channel`; DM voice responses omit it
  // and include `conversation` with {id, is_group, caller_role} instead.
  // The renderer synthesizes a channel-like object for DM at join time
  // (peerName lookup against dmStore) per #1209 plan task F4.
  channel?: {
    id: string;
    name: string;
    server_id: string;
    audio_quality_tier?: string | null;
  };
  permissions?: string;
  server_muted?: boolean;
  server_deafened?: boolean;
  conversation?: {
    id?: string;
    is_group?: boolean;
    caller_role?: 'admin' | 'member';
  };
}

interface RoomJoinedResponse {
  rtpCapabilities: mediasoupTypes.RtpCapabilities;
  mediaFrameCryptoVersion?: number;
  existingProducers: Array<{
    producerId: string;
    userId: string;
    kind: string;
    source: string;
  }>;
  participants: Array<{
    userId: string;
    username: string;
    displayName?: string;
    avatarUrl?: string;
    /** Self-deafen state from the SFU room snapshot (#685) — optional for
     *  resilience against an older media-plane that omits it. */
    isDeafened?: boolean;
    /** Audio-device testing state from the SFU room snapshot (#1163). */
    isTesting?: boolean;
  }>;
  channelName: string;
  e2eeEpoch?: number;
}

interface UserLeftEvent {
  userId: string;
  e2eeEpoch?: number;
}

interface TransportOptions {
  id: string;
  iceParameters: mediasoupTypes.IceParameters;
  iceCandidates: mediasoupTypes.IceCandidate[];
  dtlsParameters: mediasoupTypes.DtlsParameters;
}

interface ConsumeResponse {
  id: string;
  producerId: string;
  kind: mediasoupTypes.MediaKind;
  rtpParameters: mediasoupTypes.RtpParameters;
  producerUserId: string;
  source: string;
}

type VideoReproduceSource = 'camera' | 'screen';

interface VideoReproduceToken {
  source: VideoReproduceSource;
  sessionGeneration: number;
  sourceGeneration: number;
}

interface WebrtcHwObservation {
  source: VideoReproduceSource;
  producer: mediasoupTypes.Producer;
  token: VideoReproduceToken;
  signal: { mime: string; powerEfficient: boolean };
  previousHw: boolean | undefined;
  activeCodecMime: string | null;
}

// ---------------------------------------------------------------------------
// VoiceService singleton
// ---------------------------------------------------------------------------

class VoiceService {
  private socket: Socket | null = null;
  private device: Device | null = null;
  private sendTransport: mediasoupTypes.Transport | null = null;
  // Split recv transports for E2EE channels — avoids BUNDLE codec collision (#291).
  // Audio and video get separate recv PeerConnections so payload types can't collide.
  // Send transport stays single (server allows only 1, and send path has no demux).
  private recvTransportAudio: mediasoupTypes.Transport | null = null;
  private recvTransportVideo: mediasoupTypes.Transport | null = null;

  // Local producers: source → Producer
  private readonly producers: Map<string, mediasoupTypes.Producer> = new Map();
  // Remote consumers: consumerId → Consumer
  private readonly consumers: Map<string, mediasoupTypes.Consumer> = new Map();
  private testSuspensionDepth = 0;
  private readonly testSuspendedProducerIds = new Set<string>();
  private readonly testSuspendedConsumerIds = new Set<string>();
  private readonly testRestoreEligibleProducerIds = new Set<string>();
  private readonly testRestoreEligibleConsumerIds = new Set<string>();
  private readonly testServerPausedConsumerIds = new Set<string>();
  private readonly serverResumeOnUndeafenConsumerIds = new Set<string>();
  // Consumer metadata for ownership transfer (parallel to consumers Map)
  private readonly consumerMeta: Map<
    string,
    { source: string; producerUserId: string; producerId: string }
  > = new Map();
  // Local media streams
  private localMicStream: MediaStream | null = null;
  /** Single-flight guard for initial channel/DM joins. */
  private joinInFlight = false;
  /** Single-flight guard for #1790 network-change media-session resume. */
  private resumeInFlight = false;
  private localCameraStream: MediaStream | null = null;
  private localScreenStream: MediaStream | null = null;

  // Pending screen-audio producers from remote users (userId → producerId)
  // Consumed when the local user tunes into the corresponding screen share
  private readonly pendingScreenAudioProducers: Map<string, string> = new Map();

  /** producerIds with a tune-in currently in flight — dedupe guard (#2088). */
  private readonly tuneInsInFlight = new Set<string>();

  // Original router RTP capabilities from the SFU (stored for PiP Device.load())
  private routerRtpCapabilities: mediasoupTypes.RtpCapabilities | null = null;

  // PiP producer lifecycle callbacks — wired by MainView when proxy is active
  onProducerAdded: ((producerId: string, userId: string, source: string) => void) | null = null;
  onProducerClosed: ((producerId: string, userId: string) => void) | null = null;

  // E2EE
  private mediaEncryption: MediaEncryption | null = null;
  // RTCRtpScriptTransform Worker (Chromium 129+) — owns frame crypto in a dedicated thread
  private e2eeWorker: Worker | null = null;
  // #1878: unsubscribe handle for the e2eeService key-rotation subscription
  // (sender re-base trigger). Cleared in cleanupTimersAndE2EE.
  private keyRotationOff: (() => void) | null = null;
  // Highest CSK version requested for the current encryption instance. Async
  // fetches may resolve out of order, but only this version may install.
  private latestRequestedEncryptKeyVersion = 0;
  // Monotonic owner for asynchronous media-E2EE initialization. Cleanup and a
  // successor init advance this before any await, making every older
  // continuation incapable of publishing Worker/key/subscription state.
  private e2eeInitGeneration = 0;
  private pendingE2EEInitContext: E2EEInitContext | null = null;
  private e2eeInitInFlight: E2EEInitFlight | null = null;
  // Debounced rotation state (extracted from MediaEncryption for Worker path)
  private rotationTimer: ReturnType<typeof setTimeout> | null = null;
  private rotationPending = false;
  private rotationDeadline = 0;
  private static readonly ROTATION_DEBOUNCE_MS = 2000;
  private static readonly ROTATION_MAX_CAP_MS = 5000;

  // Consume queues — serialized per-transport to prevent concurrent SDP
  // negotiations that cause "Duplicate a=mid value" errors.
  private consumeQueueAudio: Promise<void> = Promise.resolve();
  private consumeQueueVideo: Promise<void> = Promise.resolve();

  // Serialize codec-driven producer swaps per source. Camera and screen may still
  // re-produce in parallel, but two callers for the same source must re-check the
  // live producer/codec after the earlier swap completes (#2187).
  private readonly videoReproduceQueues: Record<VideoReproduceSource, Promise<void>> = {
    camera: Promise.resolve(),
    screen: Promise.resolve(),
  };
  // Invalidates queued/in-flight swaps when their owning media session is torn
  // down. Queue reset lets a successor call start immediately; generation checks
  // keep continuations already attached to an old tail from touching it (#2187).
  private videoReproduceGeneration = 0;
  private readonly videoReproduceSourceGenerations: Record<VideoReproduceSource, number> = {
    camera: 0,
    screen: 0,
  };
  private videoReproduceSessionActive = false;

  // Client-side voice activity detection (Web Audio API)
  private vadAudioContext: AudioContext | null = null;
  private vadAnalyser: AnalyserNode | null = null;
  private vadSource: MediaStreamAudioSourceNode | null = null;
  private vadTimer: ReturnType<typeof setInterval> | null = null;
  private vadSpeaking = false;

  // Noise gate (Web Audio API)
  private noiseGateCtx: AudioContext | null = null;
  private noiseGateTimer: ReturnType<typeof setInterval> | null = null;

  // Input volume (Web Audio API GainNode)
  private inputVolumeCtx: AudioContext | null = null;
  private inputVolumeGain: GainNode | null = null;
  private inputVolumeUnsub: (() => void) | null = null;

  // Live settings subscriptions (apply changes during active calls)
  private liveAudioUnsub: (() => void) | null = null;
  private liveVideoUnsub: (() => void) | null = null;
  private liveVoiceUnsub: (() => void) | null = null;
  private liveAudioTrackReplaceSeq = 0;

  // Packet loss monitor
  private packetLossTimer: ReturnType<typeof setInterval> | null = null;
  private lastPacketsLost = 0;
  private lastPacketsSent = 0;

  // Solo bandwidth saving
  private soloNotificationTimer: ReturnType<typeof setTimeout> | null = null;

  // ─── Client-side Voice Activity Detection (VAD) ──────────────────

  /**
   * Start local VAD using Web Audio AnalyserNode on the mic stream.
   * Updates the local user's isSpeaking state in the store at ~20 Hz.
   * This provides instant visual feedback without server roundtrip latency.
   */
  private startLocalVAD(micStream: MediaStream): void {
    this.stopLocalVAD();

    try {
      this.vadAudioContext = new AudioContext();
      this.vadSource = this.vadAudioContext.createMediaStreamSource(micStream);
      this.vadAnalyser = this.vadAudioContext.createAnalyser();
      this.vadAnalyser.fftSize = 256;
      this.vadAnalyser.smoothingTimeConstant = 0.3;
      this.vadSource.connect(this.vadAnalyser);

      const bufferLength = this.vadAnalyser.frequencyBinCount;
      const dataArray = new Uint8Array(bufferLength);
      const SPEAKING_THRESHOLD = 8; // byte average (0-255 range) — lower = more sensitive
      const SILENCE_DELAY = 200; // ms to hold speaking state after drop
      let silenceStart = 0;

      this.vadTimer = setInterval(() => {
        if (!this.vadAnalyser) return;

        this.vadAnalyser.getByteFrequencyData(dataArray);
        // Average volume across frequency bins
        let sum = 0;
        for (let i = 0; i < bufferLength; i++) sum += dataArray[i];
        const average = sum / bufferLength;

        const now = Date.now();
        const isSpeakingNow = average > SPEAKING_THRESHOLD;

        if (isSpeakingNow) {
          silenceStart = 0;
          if (!this.vadSpeaking) {
            this.vadSpeaking = true;
            this.updateLocalSpeaking(true);
          }
        } else if (this.vadSpeaking) {
          // Debounce: hold speaking state briefly to avoid flickering
          if (silenceStart === 0) {
            silenceStart = now;
          } else if (now - silenceStart > SILENCE_DELAY) {
            this.vadSpeaking = false;
            this.updateLocalSpeaking(false);
          }
        }
      }, 50); // 20 Hz poll
    } catch (err) {
      console.warn('Failed to start local VAD:', errorMessage(err));
    }
  }

  /** Stop local VAD and clean up audio nodes */
  private stopLocalVAD(): void {
    if (this.vadTimer) {
      clearInterval(this.vadTimer);
      this.vadTimer = null;
    }
    if (this.vadSpeaking) {
      this.vadSpeaking = false;
      this.updateLocalSpeaking(false);
    }
    this.vadSource?.disconnect();
    this.vadSource = null;
    this.vadAnalyser = null;
    if (this.vadAudioContext?.state !== 'closed') {
      this.vadAudioContext?.close().catch(() => {});
    }
    this.vadAudioContext = null;
  }

  /** Update the local user's isSpeaking state in the store */
  private updateLocalSpeaking(speaking: boolean): void {
    const store = useVoiceStore.getState();
    const localUserId = useUserStore.getState().user?.id;
    if (!localUserId) return;

    store.updateParticipant(localUserId, { isSpeaking: speaking });
    if (speaking) {
      store.setActiveSpeaker(localUserId);
    } else if (store.activeSpeakerId === localUserId) {
      store.setActiveSpeaker(null);
    }
  }

  // ─── Noise Gate ──────────────────────────────────────────────────

  /**
   * Apply a noise gate to the mic stream using Web Audio API.
   * Returns a new MediaStreamTrack from a MediaStreamDestination node.
   * Audio below the threshold (dBFS) is silenced via a GainNode.
   */
  private applyNoiseGate(micStream: MediaStream, thresholdDbfs: number): MediaStreamTrack {
    this.stopNoiseGate();

    const ctx = new AudioContext({ sampleRate: 48000 });
    this.noiseGateCtx = ctx;

    const source = ctx.createMediaStreamSource(micStream);
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 256;
    const gain = ctx.createGain();
    const destination = ctx.createMediaStreamDestination();

    source.connect(analyser);
    analyser.connect(gain);
    gain.connect(destination);

    const dataArray = new Uint8Array(analyser.frequencyBinCount);
    // Convert dBFS threshold to a 0–255 byte level (AnalyserNode getByteTimeDomainData range)
    // dBFS -80 → ~0, -20 → ~200. Formula: 128 * 10^(dBFS/20) maps to amplitude offset from 128.
    const thresholdAmplitude = 128 * Math.pow(10, thresholdDbfs / 20);

    this.noiseGateTimer = setInterval(() => {
      analyser.getByteTimeDomainData(dataArray);
      // Peak amplitude offset from silence (128)
      let peak = 0;
      for (const sample of dataArray) {
        const offset = Math.abs(sample - 128);
        if (offset > peak) peak = offset;
      }

      const isOpen = peak >= thresholdAmplitude;
      const target = isOpen ? 1 : 0;
      gain.gain.setTargetAtTime(target, ctx.currentTime, 0.015);
    }, 20); // 50 Hz poll for responsive gating

    return destination.stream.getAudioTracks()[0];
  }

  /** Stop noise gate and clean up audio nodes */
  private stopNoiseGate(): void {
    if (this.noiseGateTimer) {
      clearInterval(this.noiseGateTimer);
      this.noiseGateTimer = null;
    }
    if (this.noiseGateCtx?.state !== 'closed') {
      this.noiseGateCtx?.close().catch(() => {});
    }
    this.noiseGateCtx = null;
  }

  // ─── Input Volume ───────────────────────────────────────────────

  /**
   * Apply input volume via a GainNode. Returns a processed MediaStreamTrack.
   * Stores references so the gain can be updated in real-time from the settings store.
   */
  private applyInputVolume(track: MediaStreamTrack, volumePercent: number): MediaStreamTrack {
    this.stopInputVolume();

    const ctx = new AudioContext({ sampleRate: 48000 });
    this.inputVolumeCtx = ctx;

    const source = ctx.createMediaStreamSource(new MediaStream([track]));
    const gain = ctx.createGain();
    const destination = ctx.createMediaStreamDestination();

    gain.gain.value = volumePercent / 100;
    source.connect(gain);
    gain.connect(destination);

    this.inputVolumeGain = gain;

    // Subscribe to real-time volume changes from the settings store
    this.inputVolumeUnsub = useAudioSettingsStore.subscribe((state, prevState) => {
      if (
        state.inputVolume !== prevState.inputVolume &&
        this.inputVolumeGain &&
        this.inputVolumeCtx &&
        this.inputVolumeCtx.state !== 'closed'
      ) {
        this.inputVolumeGain.gain.setTargetAtTime(
          state.inputVolume / 100,
          this.inputVolumeCtx.currentTime,
          0.01
        );
      }
    });

    return destination.stream.getAudioTracks()[0];
  }

  /** Stop input volume processing and clean up */
  private stopInputVolume(): void {
    if (this.inputVolumeUnsub) {
      this.inputVolumeUnsub();
      this.inputVolumeUnsub = null;
    }
    if (this.inputVolumeCtx?.state !== 'closed') {
      this.inputVolumeCtx?.close().catch(() => {});
    }
    this.inputVolumeCtx = null;
    this.inputVolumeGain = null;
  }

  // ─── Packet loss monitor ─────────────────────────────────────────

  /**
   * Poll outbound-rtp + remote-inbound-rtp stats every 5s to measure packet loss.
   * When FEC headroom is enabled, reactively inflates maxBitrate so Opus has room
   * for in-band FEC without sacrificing primary audio quality.
   * Loss > threshold → packetLossWarning in store.
   */
  /** Extract packet loss stats from a producer's stats report. */
  private static extractPacketLossStats(stats: RTCStatsReport): {
    packetsSent: number;
    packetsLost: number;
  } {
    let packetsSent = 0;
    let packetsLost = 0;
    for (const report of stats.values()) {
      if (report.type === 'outbound-rtp') {
        packetsSent = (report as { packetsSent?: number }).packetsSent ?? 0;
      }
      if (report.type === 'remote-inbound-rtp') {
        packetsLost = (report as { packetsLost?: number }).packetsLost ?? 0;
      }
    }
    return { packetsSent, packetsLost };
  }

  /**
   * Learn the runtime WebRTC hardware-encode signal (question B) from active video
   * producers and cache it per codec in the video settings store. Fail-safe: a codec
   * whose stats aren't ready yet stays unlearned, and isHwAccelerated falls back to the
   * WebCodecs silicon capability (A) until a producer of that codec reports.
   */
  private async learnWebrtcHwSignal(): Promise<void> {
    const expectedSessionGeneration = this.videoReproduceGeneration;
    if (!this.isCurrentVideoReproduceSession(expectedSessionGeneration)) return;
    // Two-pass so camera and screen sharing the same codec each evaluate the re-selection
    // trigger against the PRE-tick B value (#2187 item 2). If we wrote B mid-loop, the first
    // source's write would flip previousHw to false for the second source and its churn guard
    // would suppress re-selection — leaving the second source pinned on the CPU encoder
    // (Codex review of #2189). Pass 1 captures previousHw before any write; pass 2 writes the
    // learned B and then decides per source.
    const observed: WebrtcHwObservation[] = [];
    for (const source of ['camera', 'screen'] as const) {
      if (!this.isCurrentVideoReproduceSession(expectedSessionGeneration)) return;
      const token = this.captureVideoReproduceToken(source);
      const producer = this.producers.get(source);
      if (!producer) continue;
      try {
        const stats = await producer.getStats();
        if (!this.isCurrentVideoReproduceSession(expectedSessionGeneration)) return;
        const signal = extractWebrtcHwSignal(stats);
        if (!signal) continue;
        if (!this.isCurrentVideoProducer(token, source, producer)) {
          continue;
        }
        observed.push({
          source,
          producer,
          token,
          signal,
          previousHw: useVideoSettingsStore.getState().webrtcHwByMime[signal.mime],
          activeCodecMime: this.getProducerCodecMimeType(source)?.split(':')[0] ?? null,
        });
      } catch {
        // stats unavailable this tick — leave B unlearned for this codec
      }
    }

    this.applyWebrtcHwObservations(observed, expectedSessionGeneration);
  }

  private applyWebrtcHwObservations(
    observed: WebrtcHwObservation[],
    expectedSessionGeneration: number
  ): void {
    const currentObservations = observed.filter((observation) =>
      this.isCurrentVideoProducer(observation.token, observation.source, observation.producer)
    );
    const store = useVideoSettingsStore.getState();
    for (const o of currentObservations) {
      store.setWebrtcHwForMime(o.signal.mime, o.signal.powerEfficient);
    }

    // #2187 item 2: for each source whose ACTIVE codec just transitioned to software-encode,
    // re-select toward a HW codec (once per SW transition; reProduceIfBetterCodec no-ops if no
    // genuinely-better HW codec exists).
    for (const o of currentObservations) {
      if (
        shouldReselectForHwDowngrade({
          learnedHw: o.signal.powerEfficient,
          previousHw: o.previousHw,
          learnedMime: o.signal.mime,
          activeCodecMime: o.activeCodecMime,
        })
      ) {
        // Fire-and-forget with an explicit catch: the void detaches this promise from the
        // caller, so a mid-swap reproduce rejection (transport/track race — fastReproduce* has
        // already closed the old producer) must be swallowed here, not surface as an unhandled
        // rejection. The swap path cleans up the source on replacement failure so UI/capture
        // cannot claim a live publish. Log .message only, never the raw error
        // (observability.md § Console error logging).
        void this.reProduceIfBetterCodec(
          o.source,
          { requireHwImprovement: true },
          expectedSessionGeneration
        ).catch((err) =>
          console.warn('[codec-floor] HW re-selection failed:', (err as Error).message)
        );
      }
    }
  }

  /** Calculate FEC headroom bitrate multiplier based on loss % and tier. */
  private static calculateFecBitrate(
    lossPercent: number,
    tierMaxBitrate: number,
    effectiveHeadroom: boolean
  ): number {
    if (!effectiveHeadroom || lossPercent <= 0) return tierMaxBitrate;

    let K: number;
    if (tierMaxBitrate < 64_000) K = 4;
    else if (tierMaxBitrate < 128_000) K = 2.5;
    else K = 1.5;
    const headroomPercent = Math.min(50, lossPercent * K);
    return Math.round(tierMaxBitrate * (1 + headroomPercent / 100));
  }

  private startPacketLossMonitor(): void {
    // Idempotent: the first producer of ANY kind (mic, camera, or screen) starts the
    // session-stats loop; later produce calls are no-ops (#2187 item 1). This loop drives
    // both the per-tick B-signal learn (learnWebrtcHwSignal) and mic FEC headroom tuning,
    // so it must run in no-mic (video/screen-only) sessions too.
    if (this.packetLossTimer) return;
    this.lastPacketsLost = 0;
    this.lastPacketsSent = 0;

    this.packetLossTimer = setInterval(async () => {
      // Learn the runtime WebRTC hardware-encode signal (question B) from video
      // producers each tick. Independent of the mic packet-loss read below, and
      // non-blocking (fire-and-forget) so a slow getStats never delays loss sampling.
      void this.learnWebrtcHwSignal();

      const micProducer = this.producers.get('mic');
      if (!micProducer) return;

      try {
        const stats = await micProducer.getStats();
        const { packetsSent, packetsLost } = VoiceService.extractPacketLossStats(stats);

        const deltaSent = packetsSent - this.lastPacketsSent;
        const deltaLost = packetsLost - this.lastPacketsLost;
        this.lastPacketsSent = packetsSent;
        this.lastPacketsLost = packetsLost;

        if (deltaSent <= 0) return;

        const lossPercent = Math.max(0, (deltaLost / deltaSent) * 100);
        const adv = useAudioSettingsStore.getState();
        useVoiceStore.getState().setPacketLoss(lossPercent, adv.packetLossWarningThreshold);

        if (micProducer.rtpSender) {
          const tier = useVoiceStore.getState().effectiveQualityTier;
          const tierConfig = AUDIO_QUALITY_TIERS[tier];
          const effectiveHeadroom = adv.advancedMode
            ? adv.inlineFec && adv.fecHeadroom
            : tierConfig.opusFec;

          const params = micProducer.rtpSender.getParameters();
          if (params.encodings?.[0]) {
            params.encodings[0].maxBitrate = VoiceService.calculateFecBitrate(
              lossPercent,
              tierConfig.maxBitrate,
              effectiveHeadroom
            );
            micProducer.rtpSender.setParameters(params).catch(() => {});
          }
        }
      } catch {
        // Stats unavailable — ignore
      }
    }, 5000);
  }

  private stopPacketLossMonitor(): void {
    if (this.packetLossTimer) {
      clearInterval(this.packetLossTimer);
      this.packetLossTimer = null;
    }
    useVoiceStore.getState().setPacketLoss(0);
  }

  // ─── Codec preference helpers ──────────────────────────────────────

  /** Codec identity for re-selection: H.264 levels may differ under level asymmetry. */
  private codecKeysEquivalent(left: string, right: string): boolean {
    const [leftMime, leftProfile] = left.toLowerCase().split(':');
    const [rightMime, rightProfile] = right.toLowerCase().split(':');
    if (leftMime !== rightMime) return false;
    if (leftMime === 'video/h264') {
      return Boolean(
        leftProfile && rightProfile && h264ProfilesCompatible(leftProfile, rightProfile)
      );
    }
    if (leftMime === 'video/vp9') return (leftProfile ?? '0') === (rightProfile ?? '0');
    return leftProfile === rightProfile;
  }

  private capabilityMatchesCodecKey(
    capability: { mimeType: string; profileId?: string | null },
    key: string
  ): boolean {
    const [mime, requestedProfile] = key.toLowerCase().split(':');
    if (capability.mimeType.toLowerCase() !== mime) return false;
    const capabilityProfile = capability.profileId?.toLowerCase();
    if (mime === 'video/h264') {
      if (requestedProfile === undefined) {
        const profileClass = capabilityProfile ? h264ProfileClass(capabilityProfile) : null;
        return (
          profileClass === 'high' ||
          profileClass === 'main' ||
          profileClass === 'constrained-baseline'
        );
      }
      return Boolean(
        capabilityProfile && h264ProfilesCompatible(requestedProfile, capabilityProfile)
      );
    }
    if (mime === 'video/vp9') {
      return (requestedProfile ?? '0') === (capabilityProfile ?? '0');
    }
    if (mime === 'video/av1') {
      if (requestedProfile === 'hdr') return capabilityProfile === 'hdr';
      if (requestedProfile === 'sdr')
        return capabilityProfile === 'sdr' || capabilityProfile == null;
      return (
        capabilityProfile === undefined ||
        capabilityProfile === 'hdr' ||
        capabilityProfile === 'sdr'
      );
    }
    return requestedProfile === undefined;
  }

  /**
   * Find a video codec from the loaded device's send capabilities.
   * Accepts a codec key like "video/H264:640034" or plain mimeType "video/VP9".
   * H.264 profile matches ignore the level byte when level asymmetry is allowed.
   * A legacy mime-only VP9 preference resolves to Profile 0 so it cannot bypass
   * the HDR preference gate. Bare H.264 follows Concord's canonical profile order.
   */
  private findSendCodec(key: string): mediasoupTypes.RtpCodecCapability | undefined {
    if (!this.device?.rtpCapabilities?.codecs) return undefined;
    const [mime, profileId] = key.split(':');
    const mimeLower = mime.toLowerCase();

    const matches = this.device.rtpCapabilities.codecs.filter(
      (c) => c.mimeType.toLowerCase() === mimeLower && c.kind === 'video'
    );
    if (matches.length === 0) return undefined;

    if (profileId) {
      return matches.find((c) => {
        const params = c.parameters ?? {};
        if (mimeLower === 'video/h264') {
          return h264ProfilesCompatible(profileId, String(params['profile-level-id'] ?? ''));
        }
        if (mimeLower === 'video/vp9') {
          return String(params['profile-id'] ?? '0') === profileId;
        }
        return true;
      });
    }

    if (mimeLower === 'video/vp9') {
      return matches.find((c) => String(c.parameters?.['profile-id'] ?? '0') === '0');
    }

    if (mimeLower === 'video/h264') {
      for (const canonicalProfile of ['640034', '4d0032', '42e01f']) {
        const match = matches.find((codec) =>
          h264ProfilesCompatible(
            canonicalProfile,
            String(codec.parameters?.['profile-level-id'] ?? '')
          )
        );
        if (match) return match;
      }
      return undefined;
    }

    // Unprofiled codecs have only one routable variant.
    return matches[matches.length - 1];
  }

  private codecKeyFromParameters(codec: {
    mimeType: string;
    parameters?: Record<string, unknown>;
    sdpFmtpLine?: string;
  }): string {
    const mime = codec.mimeType.toLowerCase();
    const parameters = codec.parameters ?? {};
    if (mime === 'video/h264') {
      const parameterProfile = parameters['profile-level-id'];
      const profile =
        typeof parameterProfile === 'string'
          ? parameterProfile
          : /profile-level-id=([0-9a-f]{6})/i.exec(codec.sdpFmtpLine ?? '')?.[1];
      if (profile) return `${mime}:${profile.toLowerCase()}`;
    }
    if (mime === 'video/vp9') {
      const parameterProfile = parameters['profile-id'];
      const serializedProfile =
        typeof parameterProfile === 'string' || typeof parameterProfile === 'number'
          ? String(parameterProfile)
          : null;
      const profile =
        serializedProfile ?? /profile-id=(\d+)/.exec(codec.sdpFmtpLine ?? '')?.[1] ?? '0';
      return `${mime}:${profile}`;
    }
    return mime;
  }

  private requireSelectedVideoCodec(
    codec: mediasoupTypes.RtpCodecCapability | undefined,
    source: 'camera' | 'screen'
  ): mediasoupTypes.RtpCodecCapability {
    if (!codec) {
      throw new Error(`No eligible ${source} codec is available for the current room codec floor`);
    }
    return codec;
  }

  /** Check if an application codec target is compatible with the RTP codec floor. */
  private isInCodecFloor(key: string): boolean {
    return isCodecKeyInFloor(key, useVoiceStore.getState().codecFloor);
  }

  /**
   * Resolve hardware evidence without confusing an active MIME-wide observation with
   * target/profile capability. A live false verdict conservatively demotes every
   * qualified hardware target of that MIME for this session. A live true verdict can
   * affirm only the unqualified MIME; qualified AV1/VP9/H.264 targets still require an
   * affirmative exact WebCodecs probe.
   */
  private isHwAccelerated(key: string): boolean {
    const store = useVideoSettingsStore.getState();
    const [mime, requestedProfile] = key.toLowerCase().split(':');
    const learned = store.webrtcHwByMime[mime];
    if (learned === false) return false;
    if (requestedProfile === undefined && learned === true) return true;
    return store.codecCapabilities.some(
      (capability) =>
        capability.hwAvailable === true && this.capabilityMatchesCodecKey(capability, key)
    );
  }

  /**
   * Calculate recommended bitrate from resolution × FPS × codec-aware bits-per-pixel.
   * Efficient codecs (AV1, HEVC, VP9) use 0.04 bpp; H.264/VP8 use 0.07 bpp.
   * Clamped to [1.5 Mbps, 30 Mbps] and rounded to nearest 100 kbps.
   */
  private calculateRecommendedBitrate(
    width: number,
    height: number,
    fps: number,
    codecMime?: string | null
  ): number {
    const isEfficient = codecMime ? /AV1|H265|HEVC|VP9/i.test(codecMime) : false;
    const bpp = isEfficient ? 0.04 : 0.07;
    const bps = width * height * fps * bpp;
    return Math.max(1_500_000, Math.min(30_000_000, Math.round(bps / 100_000) * 100_000));
  }

  /**
   * Calculate recommended screen share bitrate using current screen settings.
   * Resolves 'source' resolution to actual capture dimensions when available.
   * If codecMime is not provided, infers from active screen codec or preferred codec.
   */
  private calculateScreenBitrate(codecMime?: string | null): number {
    if (!codecMime) {
      const activeCodec = useVoiceStore.getState().activeScreenCodec;
      const preferredCodec = useVideoSettingsStore.getState().preferredVideoCodec;
      codecMime = activeCodec ?? preferredCodec ?? null;
    }
    const vs = useVideoSettingsStore.getState();
    const resMap: Record<string, { w: number; h: number }> = {
      ...SCREEN_RES_DIMS,
      source: { w: 3840, h: 2160 },
    };
    const parsed = vs.screenResolution.match(/^(\d+)x(\d+)$/);
    let res = parsed
      ? { w: Number(parsed[1]), h: Number(parsed[2]) }
      : resMap[vs.screenResolution] || resMap['1080p'];

    // For 'source' resolution, use actual capture dimensions if available
    if (vs.screenResolution === 'source') {
      const screenTrack = this.localScreenStream?.getVideoTracks()[0];
      if (screenTrack?.readyState === 'live') {
        const settings = screenTrack.getSettings();
        if (settings.width && settings.height) {
          res = { w: settings.width, h: settings.height };
        }
      }
    }

    const effectiveFps = vs.screenFrameRate === 0 ? 60 : vs.screenFrameRate;
    // #2163: clamp the estimate inputs to the free stream entitlement so the
    // recommended bitrate matches the (clamped) capture. No-op for premium/native.
    const clamped = this.clampScreenToEntitlement(res.w, res.h, effectiveFps);
    return this.calculateRecommendedBitrate(clamped.width, clamped.height, clamped.fps, codecMime);
  }

  /**
   * Clamp screen-capture (w, h, fps) to the stream entitlement (#2163). Reads the
   * live subscription snapshot; premium/native and the pre-hydrate window are
   * no-ops (fail-open premium safety, mirroring useLaunchReset).
   */
  private clampScreenToEntitlement(
    w: number,
    h: number,
    fps: number
  ): { width: number; height: number; fps: number } {
    return clampScreenForSubscription(w, h, fps, useSubscriptionStore.getState());
  }

  /**
   * Compute videoGoogleStartBitrate (kbps) from target bitrate (bps).
   * Starting at ~50% of target reduces encoder ramp-up delay vs. the low defaults.
   * Clamped to [100, 10000] kbps.
   */
  private computeStartBitrate(targetBps: number): number {
    return Math.max(100, Math.min(10_000, Math.round((targetBps * 0.5) / 1000)));
  }

  private cameraStartBitrate(encodings: mediasoupTypes.RtpEncodingParameters[]): number {
    let maxBitrate = 0;
    for (const encoding of encodings) {
      const bitrate = encoding.maxBitrate;
      if (typeof bitrate === 'number' && Number.isFinite(bitrate)) {
        maxBitrate = Math.max(maxBitrate, bitrate);
      }
    }
    const target = maxBitrate || 2_500_000;
    // User manual camera-bitrate cap (#1602): 0 = auto (no override). When set, it
    // lowers the encoder start-bitrate hint only — the SFU is advisory for video
    // bitrate (media-plane.md), so this never fights simulcast/SVC layer bitrates.
    const userCap = useVideoSettingsStore.getState().cameraBitrate;
    return userCap > 0 ? Math.min(target, userCap) : target;
  }

  /**
   * Pick the best codec for camera video. Single-encoding remains the default;
   * when the room gate enables camera layering, buildCameraEncodingPlan supplies
   * SVC or simulcast encodings for compatible codecs.
   *
   * Uses the shared hardware-first codec policy. Layering gates only decide whether
   * the selected codec publishes layered or single-stream encodings.
   */
  /** Build a CodecLookup bound to this VoiceService instance. */
  private codecLookup(): CodecLookup {
    return {
      isInCodecFloor: (key: string) => this.isInCodecFloor(key),
      isHwAccelerated: (key: string) => this.isHwAccelerated(key),
      findSendCodec: (key: string) => this.findSendCodec(key),
    };
  }

  private pickCameraCodec(): {
    codec?: mediasoupTypes.RtpCodecCapability;
    encodings: mediasoupTypes.RtpEncodingParameters[];
  } {
    const vs = useVideoSettingsStore.getState();
    const preset = VIDEO_QUALITY_PRESETS[vs.cameraPreset] || VIDEO_QUALITY_PRESETS['720p30'];
    const prio = vs.cameraPriority;

    const codec = selectCodecFromCascade({
      preferred: vs.preferredVideoCodec,
      hwAccel: vs.hardwareAcceleration,
      hdrEncoding: vs.hdrEncoding,
      ...this.codecLookup(),
    });

    const base: Partial<mediasoupTypes.RtpEncodingParameters> =
      prio === 'off' ? {} : { priority: prio, networkPriority: prio };

    if (!this.cameraLayeringEnabled) {
      return { codec, encodings: [{ ...base, maxBitrate: preset.maxBitrate }] };
    }

    const layeringCodec = this.pickLayeringCodec();
    const plan = buildCameraEncodingPlan({
      codec: layeringCodec,
      maxBitrate: preset.maxBitrate,
      scalabilityMode: vs.scalabilityMode,
      priority: base,
      eligibility: { svc: vs.supportSvc, simulcast: vs.supportSimulcast },
    });
    return { codec: layeringCodec, encodings: plan.encodings };
  }

  private pickLayeringCodec(): mediasoupTypes.RtpCodecCapability | undefined {
    const vs = useVideoSettingsStore.getState();
    return selectCodecFromCascade({
      preferred: vs.preferredVideoCodec,
      hwAccel: vs.hardwareAcceleration,
      hdrEncoding: vs.hdrEncoding,
      ...this.codecLookup(),
    });
  }

  /**
   * Pick the best codec for screen sharing. Screen honors BOTH casting toggles,
   * but simulcast is server-gated (#1924): AV1/VP9 screen → SVC (1 encoding,
   * server-passive, cost-neutral, ungated); H264/VP8 screen → 3-RID simulcast ONLY
   * when `supportSimulcast` AND the media-plane's `screen-layering-gate` are both on
   * (`this.screenLayeringEnabled`), else a single encode. The gate is
   * server-authoritative — a client can never publish simulcast screen unilaterally
   * (the `risk: security` capacity guardrail). AV1+simulcast stays structurally
   * unreachable via `castingKindForCodec` (AV1 → svc), so an AV1 screen is never
   * simulcast regardless of the gate.
   *
   * Uses the shared hardware-first codec policy. Layering toggles change only
   * the encoding plan; they never replace the selected codec.
   */
  private pickScreenCodec(): {
    codec?: mediasoupTypes.RtpCodecCapability;
    encodings: mediasoupTypes.RtpEncodingParameters[];
    effectiveBitrate: number;
  } {
    const vs = useVideoSettingsStore.getState();
    const prio = vs.screenSharePriority;
    const userBitrate = vs.screenShareBitrate; // 0 = auto

    const base: Partial<mediasoupTypes.RtpEncodingParameters> =
      prio === 'off' ? {} : { priority: prio, networkPriority: prio };

    const layeringCodec = this.pickLayeringCodec();

    // Derive the auto bitrate from the codec actually published. Layering toggles
    // may collapse its encoding plan, but do not replace the codec. A non-zero user
    // override is honored verbatim and never re-derived.
    const bitrate = userBitrate || this.calculateScreenBitrate(layeringCodec?.mimeType ?? null);

    const plan = buildCameraEncodingPlan({
      codec: layeringCodec,
      maxBitrate: bitrate,
      scalabilityMode: vs.scalabilityMode,
      priority: base,
      // #1924: simulcast screen is server-gated. Both the user toggle AND the
      // media-plane's screen-layering-gate (this.screenLayeringEnabled) must be
      // on — a client can never publish simulcast screen unilaterally.
      eligibility: {
        svc: vs.supportSvc,
        simulcast: vs.supportSimulcast && this.screenLayeringEnabled,
      },
    });

    return {
      codec: layeringCodec,
      encodings: plan.encodings,
      effectiveBitrate: bitrate,
    };
  }

  /**
   * Apply degradation preference to a video producer's RTP sender.
   * Controls whether the encoder drops framerate or resolution under congestion.
   */
  private applyDegradationPreference(producer: mediasoupTypes.Producer): void {
    const pref = useVideoSettingsStore.getState().degradationPreference;
    if (pref === 'balanced') return; // Browser default — no override needed

    try {
      const sender = producer.rtpSender;
      if (!sender) return;
      const params = sender.getParameters();
      params.degradationPreference = pref;
      sender.setParameters(params).catch((err: unknown) => {
        console.warn('Failed to set degradationPreference:', errorMessage(err));
      });
    } catch {
      // Ignore — rtpSender may not be available in all environments
    }
  }

  // ─── Live Settings Application ─────────────────────────────────────
  // Subscribe to audio/video settings stores and apply changes mid-call.

  private setupLiveSubscriptions(): void {
    this.teardownLiveSubscriptions();

    // Audio settings subscription
    this.liveAudioUnsub = useAudioSettingsStore.subscribe((state, prev) => {
      if (!this.producers.get('mic') || !this.sendTransport) return;

      // --- Instant: setParameters (DSCP priority) ---
      if (state.audioPriority !== prev.audioPriority) {
        this.liveUpdateAudioPriority(state.audioPriority);
      }

      // --- replaceTrack: re-acquire mic with new constraints ---
      const constraintFields = [
        'noiseCancellation',
        'echoCancellation',
        'autoGainControl',
        'noiseGateMode',
        'noiseGateLevel',
      ] as const;
      if (constraintFields.some((f) => state[f] !== prev[f])) {
        this.liveReplaceAudioTrack();
        return;
      }

      // --- Re-produce: codec options changed ---
      const codecOptionFields = [
        'musicMode',
        'frameSize',
        'silenceDetection',
        'inlineFec',
        'fecHeadroom',
        'opusNack',
        'adaptivePtime',
      ] as const;
      if (codecOptionFields.some((f) => state[f] !== prev[f])) {
        this.liveReproduceAudio();
      }
    });

    // Video settings subscription
    this.liveVideoUnsub = useVideoSettingsStore.subscribe((state, prev) => {
      this.handleVideoSettingsChange(state, prev);
    });

    this.liveVoiceUnsub = useVoiceStore.subscribe((state, prev) => {
      if (state.audioInputDeviceId !== prev.audioInputDeviceId) {
        this.liveReplaceAudioTrack();
      }
    });
  }

  /** Handle video settings store changes — extracted to reduce cognitive complexity. */
  private handleVideoSettingsChange(
    state: ReturnType<typeof useVideoSettingsStore.getState>,
    prev: ReturnType<typeof useVideoSettingsStore.getState>
  ): void {
    this.applyCameraSettingsChange(state, prev);
    this.applyScreenShareSettingsChange(state, prev);
    // Auto-tune receive policy flipped ON mid-call (#2088) — sweep what's available.
    if (state.autoTuneInScreenShares && !prev.autoTuneInScreenShares) {
      void this.autoTuneSweep();
    }
  }

  /** Camera instant parameter updates — extracted for complexity reduction. */
  private applyCameraSettingsChange(
    state: ReturnType<typeof useVideoSettingsStore.getState>,
    prev: ReturnType<typeof useVideoSettingsStore.getState>
  ): void {
    const cameraProducer = this.producers.get('camera');
    if (!cameraProducer) return;

    if (state.degradationPreference !== prev.degradationPreference) {
      this.applyDegradationPreference(cameraProducer);
    }
    if (state.cameraPriority !== prev.cameraPriority) {
      this.liveUpdateVideoPriority(cameraProducer, state.cameraPriority);
    }
    if (state.cameraPreset !== prev.cameraPreset) {
      void this.liveReplaceCameraTrack().catch((err) =>
        console.warn('[video-settings] Camera track replacement failed:', errorMessage(err))
      );
    }
    // SVC/Simulcast casting toggles (#1921): a shape-only change does not alter the
    // codec MIME, so reProduceIfBetterCodec early-returns — reproduce explicitly.
    // liveReproduceCamera rides the existing stopTracks:false (#1902) + fail-closed
    // capture-stop (CWE-212) path; do NOT call sendTransport.produce directly.
    const codecPlanChanged =
      state.preferredVideoCodec !== prev.preferredVideoCodec ||
      state.hdrEncoding !== prev.hdrEncoding;
    const layeringChanged =
      state.supportSvc !== prev.supportSvc || state.supportSimulcast !== prev.supportSimulcast;
    if (codecPlanChanged || layeringChanged) {
      void this.liveReproduceCamera().catch((err) =>
        console.warn('[video-settings] Camera re-produce failed:', errorMessage(err))
      );
    }
  }

  /** Screen share instant parameter updates — extracted for complexity reduction. */
  private applyScreenShareSettingsChange(
    state: ReturnType<typeof useVideoSettingsStore.getState>,
    prev: ReturnType<typeof useVideoSettingsStore.getState>
  ): void {
    const screenProducer = this.producers.get('screen');
    if (!screenProducer) return;

    if (state.screenSharePriority !== prev.screenSharePriority) {
      this.liveUpdateVideoPriority(screenProducer, state.screenSharePriority);
    }
    if (state.screenShareBitrate !== prev.screenShareBitrate) {
      this.liveUpdateScreenBitrate(screenProducer, state.screenShareBitrate);
    }
    // SVC/Simulcast casting toggles: a supportSvc change always affects the screen
    // plan (AV1/VP9 SVC mode). Post-#1924, a supportSimulcast change ALSO
    // affects it — but only once the server screen-layering gate is enabled
    // (this.screenLayeringEnabled), since simulcast screen is gated. When the gate
    // is off a supportSimulcast flip is a no-op for screen and must NOT trigger a
    // wasteful reproduce. Route through the screen re-produce serializer (not a bare
    // fastReproduceScreen) so a toggle flip and a concurrent gate edge cannot overlap
    // two reproduces; the serializer rides the existing stopTracks:false (#1902) +
    // fail-closed capture-stop (CWE-212) path.
    const codecPlanChanged =
      state.preferredVideoCodec !== prev.preferredVideoCodec ||
      state.hdrEncoding !== prev.hdrEncoding;
    const svcChanged = state.supportSvc !== prev.supportSvc;
    const simulcastChanged = state.supportSimulcast !== prev.supportSimulcast;
    if (codecPlanChanged) {
      void this.fastReproduceScreen().catch((err) =>
        console.warn('[video-settings] Screen re-produce failed:', errorMessage(err))
      );
    } else if (svcChanged || (simulcastChanged && this.screenLayeringEnabled)) {
      this.scheduleScreenLayeringReproduce();
    }
    // Note: screen resolution/FPS/contentType changes cannot use replaceTrack
    // because getDisplayMedia requires a user gesture. Apply on next session.
  }

  private teardownLiveSubscriptions(): void {
    this.liveAudioUnsub?.();
    this.liveAudioUnsub = null;
    this.liveVideoUnsub?.();
    this.liveVideoUnsub = null;
    this.liveVoiceUnsub?.();
    this.liveVoiceUnsub = null;
  }

  // --- Instant update helpers (setParameters, no track change) ---

  private liveUpdateAudioPriority(priority: AudioPriority): void {
    const producer = this.producers.get('mic');
    if (!producer?.rtpSender) return;
    try {
      const params = producer.rtpSender.getParameters();
      if (params.encodings?.[0]) {
        if (priority === 'off') {
          // Reset to default (low = DF / best effort)
          params.encodings[0].priority = 'low';
          (params.encodings[0] as Record<string, unknown>).networkPriority = 'low';
        } else {
          params.encodings[0].priority = priority;
          (params.encodings[0] as Record<string, unknown>).networkPriority = priority;
        }
        producer.rtpSender.setParameters(params).catch(() => {});
      }
    } catch {
      /* rtpSender may not be available */
    }
  }

  private liveUpdateVideoPriority(
    producer: mediasoupTypes.Producer,
    priority: VideoPriority
  ): void {
    if (!producer?.rtpSender) return;
    try {
      const params = producer.rtpSender.getParameters();
      const effectivePriority = priority === 'off' ? 'low' : priority;
      for (const enc of params.encodings) {
        enc.priority = effectivePriority;
        (enc as Record<string, unknown>).networkPriority = effectivePriority;
      }
      producer.rtpSender.setParameters(params).catch(() => {});
    } catch {
      /* rtpSender may not be available */
    }
  }

  private liveUpdateScreenBitrate(producer: mediasoupTypes.Producer, bitrate: number): void {
    if (!producer?.rtpSender) return;
    try {
      const params = producer.rtpSender.getParameters();
      if (!params.encodings?.[0]) return;
      // When bitrate is 0 (auto), recalculate from current screen settings
      const effectiveBitrate = bitrate > 0 ? bitrate : this.calculateScreenBitrate();
      params.encodings[0].maxBitrate = effectiveBitrate;
      producer.rtpSender.setParameters(params).catch(() => {});
    } catch {
      /* rtpSender may not be available */
    }
  }

  // --- replaceTrack: re-acquire media with new constraints, swap on existing producer ---

  private shouldResumeMicAfterTrackReplacement(producerId: string): boolean {
    if (this.shouldKeepProducerSuspendedForTest(producerId)) return false;

    const store = useVoiceStore.getState();
    if (store.isMuted || store.isDeafened || store.isSoloBandwidthSaving) return false;

    const localUserId = useUserStore.getState().user?.id;
    const localParticipant = localUserId ? store.participants[localUserId] : undefined;
    return localParticipant?.serverMuted !== true && localParticipant?.serverDeafened !== true;
  }

  private stopMediaStream(stream: MediaStream | null): void {
    if (!stream) return;
    for (const t of stream.getTracks()) t.stop();
  }

  private isStaleAudioTrackReplacement(replaceSeq: number, stream: MediaStream): boolean {
    if (replaceSeq === this.liveAudioTrackReplaceSeq) return false;
    this.stopMediaStream(stream);
    return true;
  }

  private swapLiveMicStream(
    stream: MediaStream,
    adv: ReturnType<typeof useAudioSettingsStore.getState>
  ): MediaStreamTrack {
    this.stopNoiseGate();
    this.stopInputVolume();
    this.stopMediaStream(this.localMicStream);
    this.localMicStream = stream;

    let track = stream.getAudioTracks()[0];
    if (adv.noiseGateMode === 'manual') {
      track = this.applyNoiseGate(stream, adv.noiseGateLevel);
    }
    return this.applyInputVolume(track, adv.inputVolume);
  }

  private async liveReplaceAudioTrack(): Promise<void> {
    const producer = this.producers.get('mic');
    if (!producer) return;

    const replaceSeq = ++this.liveAudioTrackReplaceSeq;
    const adv = useAudioSettingsStore.getState();
    const useProcessing = !adv.musicMode;
    const selectedDeviceId = useVoiceStore.getState().audioInputDeviceId;

    // Briefly mute to hide transition
    producer.pause();

    try {
      // Re-acquire mic with new constraints
      const stream = await navigator.mediaDevices.getUserMedia({
        audio: {
          deviceId: selectedDeviceId ? { exact: selectedDeviceId } : undefined,
          echoCancellation: useProcessing && adv.echoCancellation,
          noiseSuppression: useProcessing && adv.noiseCancellation,
          autoGainControl: useProcessing && adv.autoGainControl,
          sampleRate: 48000,
          channelCount: 2,
        },
      });

      if (this.isStaleAudioTrackReplacement(replaceSeq, stream)) return;

      // Swap track on existing producer (no SDP renegotiation)
      await producer.replaceTrack({ track: this.swapLiveMicStream(stream, adv) });
    } catch (err) {
      if (replaceSeq === this.liveAudioTrackReplaceSeq) {
        console.warn('liveReplaceAudioTrack failed:', errorMessage(err));
      }
    } finally {
      if (
        replaceSeq === this.liveAudioTrackReplaceSeq &&
        this.shouldResumeMicAfterTrackReplacement(producer.id)
      ) {
        producer.resume();
      }
    }
  }

  private async liveReplaceCameraTrack(): Promise<void> {
    return this.enqueueVideoReproduce('camera', (token) =>
      this.liveReplaceCameraTrackQueued(token)
    );
  }

  private async liveReplaceCameraTrackQueued(token: VideoReproduceToken): Promise<void> {
    if (!this.isCurrentVideoReproduceToken(token)) return;
    const producer = this.producers.get('camera');
    if (!producer) return;
    const oldStream = this.localCameraStream;

    const vs = useVideoSettingsStore.getState();
    const preset = VIDEO_QUALITY_PRESETS[vs.cameraPreset] || VIDEO_QUALITY_PRESETS['system'];
    const isSystemDefault = vs.cameraPreset === 'system' || preset.width === 0;
    const videoConstraints: MediaTrackConstraints = isSystemDefault
      ? {}
      : {
          width: { ideal: preset.width },
          height: { ideal: preset.height },
          frameRate: { ideal: preset.frameRate },
        };

    producer.pause();
    let stream: MediaStream | null = null;

    try {
      stream = await this.acquireReplacementCameraStream(
        token,
        producer,
        videoConstraints,
        !isSystemDefault
      );
      if (!stream) return;
      if (!this.isCurrentVideoProducer(token, 'camera', producer)) {
        this.stopMediaStream(stream);
        return;
      }
      const track = stream.getVideoTracks()[0];

      await producer.replaceTrack({ track });
      if (!this.isCurrentVideoProducer(token, 'camera', producer)) {
        this.stopMediaStream(stream);
        return;
      }

      this.stopMediaStream(oldStream);
      const activeStream = stream;
      this.localCameraStream = activeStream;
      stream = null;

      // Update participant store with new stream
      const localUserId = useUserStore.getState().user?.id;
      if (localUserId) {
        useVoiceStore.getState().updateParticipant(localUserId, { videoStream: activeStream });
      }
    } catch (err) {
      this.stopMediaStream(stream);
      if (this.isCurrentVideoReproduceToken(token)) {
        console.warn('liveReplaceCameraTrack failed:', errorMessage(err));
      }
    } finally {
      if (this.isCurrentVideoProducer(token, 'camera', producer)) {
        producer.resume();
      }
    }
  }

  private async acquireReplacementCameraStream(
    token: VideoReproduceToken,
    producer: mediasoupTypes.Producer,
    videoConstraints: MediaTrackConstraints,
    allowFallback: boolean
  ): Promise<MediaStream | null> {
    try {
      return await navigator.mediaDevices.getUserMedia({ video: videoConstraints });
    } catch (err) {
      const shouldFallback =
        err instanceof DOMException && err.name === 'OverconstrainedError' && allowFallback;
      if (!shouldFallback) throw err;
      if (!this.isCurrentVideoProducer(token, 'camera', producer)) return null;

      console.warn('Camera overconstrained during track replace, falling back:', errorMessage(err));
      return navigator.mediaDevices.getUserMedia({ video: true });
    }
  }

  // --- Re-produce: close + re-create producer with new codec options ---

  private async liveReproduceAudio(): Promise<void> {
    if (!this.producers.get('mic') || !this.sendTransport) return;
    await this.closeProducer('mic');
    await this.produceAudio();
  }

  private async liveReproduceCamera(): Promise<void> {
    return this.enqueueVideoReproduce('camera', (token) => this.liveReproduceCameraQueued(token));
  }

  private async liveReproduceCameraQueued(token: VideoReproduceToken): Promise<void> {
    if (!this.isCurrentVideoReproduceToken(token)) return;

    const producer = this.producers.get('camera');
    const transport = this.sendTransport;
    if (!producer || !transport) return;

    producer.close();
    await this.drainSendTransportQueue(transport);
    if (
      !this.isCurrentVideoReproduce(token, transport) ||
      this.producers.get('camera') !== producer
    ) {
      return;
    }

    this.producers.delete('camera');
    this.socket?.emit('close-producer', { producerId: producer.id });
    this.stopMediaStream(this.localCameraStream);
    this.localCameraStream = null;
    await this.produceVideoQueued(undefined, token, true, transport);
    if (this.isCurrentVideoReproduce(token, transport) && !this.producers.has('camera')) {
      this.cleanupCameraState();
    }
  }

  // --- Codec floor: fast re-produce (reuses existing track, no media re-acquisition) ---

  /**
   * Get the codec key of the codec currently used by a producer.
   * Returns profile-aware keys like "video/h264:640034" or "video/vp9:2".
   */
  private getProducerCodecMimeType(source: string): string | null {
    const producer = this.producers.get(source);
    if (!producer) return null;
    const isPrimaryCodec = (codec: { mimeType: string }) =>
      !/(\/rtx|\/red|\/ulpfec|\/flexfec|\/cn|\/telephone-event)$/i.test(codec.mimeType);

    // mediasoup's Producer RTP parameters describe the codec actually published.
    // Prefer them over RTCRtpSender.getParameters(), whose codec list is a
    // preference/capability sequence rather than an active-codec observation.
    const producerCodec = producer.rtpParameters?.codecs?.find(isPrimaryCodec);
    if (producerCodec) return this.codecKeyFromParameters(producerCodec);

    const senderCodec = producer.rtpSender?.getParameters().codecs?.find(isPrimaryCodec);
    return senderCodec ? this.codecKeyFromParameters(senderCodec) : null;
  }

  /**
   * Fast re-produce camera: close producer and re-produce with best floor-compatible
   * codec, reusing the existing video track for a subsecond switch.
   */
  private async fastReproduceCamera(): Promise<void> {
    return this.enqueueVideoReproduce('camera', (token) => this.fastReproduceCameraQueued(token));
  }

  private async fastReproduceCameraQueued(token: VideoReproduceToken): Promise<void> {
    if (!this.isCurrentVideoReproduceToken(token)) return;

    const producer = this.producers.get('camera');
    const transport = this.sendTransport;
    const stream = this.localCameraStream;
    const socket = this.socket;
    if (!producer || !transport || !stream) return;

    const track = stream.getVideoTracks()[0];
    if (track?.readyState !== 'live') {
      await this.liveReproduceCameraQueued(token);
      return;
    }

    // Close old producer. The track is reused below, so it MUST survive close():
    // producers are created with stopTracks:false (the track is owned by
    // localCameraStream), so close() does not stop it. (Without that, mediasoup
    // close() stops the track and the produce() below throws 'track ended'.)
    producer.close();
    // Drain transport queue so stopSending SDP renegotiation finishes before produce()
    await this.drainSendTransportQueue(transport);
    if (
      !this.isCurrentVideoReproduce(token, transport) ||
      this.producers.get('camera') !== producer ||
      this.localCameraStream !== stream
    ) {
      return;
    }
    this.producers.delete('camera');
    this.socket?.emit('close-producer', { producerId: producer.id });

    let codec: mediasoupTypes.RtpCodecCapability;
    let newProducer: mediasoupTypes.Producer;
    try {
      const selection = this.pickCameraCodec();
      codec = this.requireSelectedVideoCodec(selection.codec, 'camera');
      const cameraBitrate = this.cameraStartBitrate(selection.encodings);
      newProducer = await this.produceEncrypted(transport, {
        track,
        encodings: selection.encodings,
        codec,
        codecOptions: { videoGoogleStartBitrate: this.computeStartBitrate(cameraBitrate) },
        stopTracks: false,
        appData: { source: 'camera' },
      });
    } catch (err) {
      if (
        this.isCurrentVideoReproduce(token, transport) &&
        this.localCameraStream === stream &&
        !this.producers.has('camera')
      ) {
        this.cleanupCameraState();
        this.setVideoReproduceError('camera');
      }
      throw err;
    }

    if (
      !this.isCurrentVideoReproduce(token, transport) ||
      this.localCameraStream !== stream ||
      this.producers.has('camera')
    ) {
      this.discardProducedProducer(newProducer, socket);
      return;
    }

    this.applyDegradationPreference(newProducer);
    this.producers.set('camera', newProducer);

    newProducer.on('transportclose', () => {
      if (this.producers.get('camera') !== newProducer) return;
      this.producers.delete('camera');
      if (this.localCameraStream) {
        for (const t of this.localCameraStream.getTracks()) t.stop();
        this.localCameraStream = null;
      }
      const s = useVoiceStore.getState();
      s.setActiveCameraCodec(null);
      s.setVideoOn(false);
      const uid = useUserStore.getState().user?.id;
      if (uid) s.updateParticipant(uid, { videoStream: undefined, isVideoOn: false });
    });

    useVoiceStore
      .getState()
      .setActiveCameraCodec(
        this.getProducerCodecMimeType('camera') ?? this.codecKeyFromParameters(codec)
      );
    const selectedCodecKey = this.codecKeyFromParameters(codec);
    const hwTag = this.isHwAccelerated(selectedCodecKey) ? 'HW' : 'SW';
    console.debug(`[codec-floor] Fast re-produced camera with ${selectedCodecKey} (${hwTag})`);
  }

  /**
   * Fast re-produce screen: close producer and re-produce with best floor-compatible
   * codec, reusing the existing screen track.
   */
  private async fastReproduceScreen(): Promise<void> {
    return this.enqueueVideoReproduce('screen', (token) => this.fastReproduceScreenQueued(token));
  }

  private async fastReproduceScreenQueued(token: VideoReproduceToken): Promise<void> {
    if (!this.isCurrentVideoReproduceToken(token)) return;

    const oldProducer = this.producers.get('screen');
    const transport = this.sendTransport;
    const stream = this.localScreenStream;
    const socket = this.socket;
    if (!oldProducer || !transport || !stream) return;

    const track = stream.getVideoTracks()[0];
    if (track?.readyState !== 'live') {
      console.warn('[codec-floor] Screen track is dead, cannot re-acquire without user gesture');
      return;
    }

    const oldProducerId = oldProducer.id;

    // Snapshot pre-swap consumption/metadata state BEFORE any await: the
    // close-producer self-echo (producer-closed via the media-plane room
    // bridge reaches the sender too) can land mid-swap and prune these
    // entries, which would otherwise drop the local share's tuned-in state
    // and its ShareTunePill/StreamBar row on codec swaps (#2088).
    const storePre = useVoiceStore.getState();
    const wasTunedIn = Boolean(storePre.tunedInScreenShares[oldProducerId]);
    const wasDominant = storePre.dominantScreenShareId === oldProducerId;
    const oldShareMeta = storePre.activeScreenShares[oldProducerId];

    // Close old producer. The track is reused below, so it MUST survive close():
    // producers are created with stopTracks:false (the track is owned by
    // localScreenStream), so close() does not stop it. (Without that, mediasoup
    // close() stops the track and the produce() below throws 'track ended'.)
    oldProducer.close();
    // Drain transport queue so stopSending SDP renegotiation finishes before produce()
    await this.drainSendTransportQueue(transport);
    if (
      !this.isCurrentVideoReproduce(token, transport) ||
      this.producers.get('screen') !== oldProducer ||
      this.localScreenStream !== stream
    ) {
      return;
    }
    this.producers.delete('screen');
    this.socket?.emit('close-producer', { producerId: oldProducerId });

    const replacement = await this.produceScreenReplacement(
      token,
      transport,
      stream,
      track,
      socket
    );
    if (!replacement) return;
    const { producer: newProducer, codec } = replacement;

    this.applyDegradationPreference(newProducer);
    this.producers.set('screen', newProducer);

    // Re-wire track ended handler
    track.onended = () => {
      if (
        !this.isCurrentVideoReproduceToken(token) ||
        this.localScreenStream !== stream ||
        this.producers.get('screen') !== newProducer
      ) {
        return;
      }
      void this.closeProducer('screen').catch((err) =>
        console.warn('[screen-share] Track-ended cleanup failed:', errorMessage(err))
      );
    };

    // Update tuned-in mapping since producer ID changed. Keyed off the
    // pre-swap snapshot — the producer-closed self-echo may already have
    // pruned the old entries during the awaits above (#2088).
    const store = useVoiceStore.getState();
    if (wasTunedIn) {
      store.tuneOut(oldProducerId); // no-op if the echo already pruned it
      store.tuneIn(newProducer.id, LOCAL_SCREEN_CONSUMER_ID);
    }
    // Carry the owner metadata to the new producerId (#2088) — re-register
    // unconditionally from the snapshot/local identity so an echo-pruned
    // entry cannot leave the live share without its ShareTunePill/StreamBar row.
    store.unregisterActiveScreenShare(oldProducerId);
    const localUserId = useUserStore.getState().user?.id;
    const shareOwnerId = oldShareMeta?.userId ?? localUserId;
    if (shareOwnerId) {
      const localParticipant = store.participants[shareOwnerId];
      store.registerActiveScreenShare({
        producerId: newProducer.id,
        userId: shareOwnerId,
        username: oldShareMeta?.username ?? localParticipant?.username ?? 'You',
        displayName: oldShareMeta?.displayName ?? localParticipant?.displayName,
        isLocal: true,
      });
    }
    // The self-echo may also have cleared the local participant's screen
    // state mid-swap; re-assert it while the capture stream is live.
    if (localUserId && this.localScreenStream) {
      store.updateParticipant(localUserId, {
        screenStream: this.localScreenStream,
        isScreenSharing: true,
      });
    }
    if (wasDominant || (wasTunedIn && useVoiceStore.getState().dominantScreenShareId === null)) {
      store.setDominantScreenShare(newProducer.id);
    }

    newProducer.on('transportclose', () => {
      if (this.producers.get('screen') !== newProducer) return;
      this.producers.delete('screen');
      if (this.localScreenStream) {
        for (const t of this.localScreenStream.getTracks()) t.stop();
        this.localScreenStream = null;
      }
      const s = useVoiceStore.getState();
      s.setActiveScreenCodec(null);
      s.setScreenSharing(false);
      const uid = useUserStore.getState().user?.id;
      if (uid) s.updateParticipant(uid, { screenStream: undefined, isScreenSharing: false });
    });

    await this.reProduceScreenAudio(token, transport);
    if (
      !this.isCurrentVideoReproduce(token, transport) ||
      this.producers.get('screen') !== newProducer
    ) {
      return;
    }

    useVoiceStore
      .getState()
      .setActiveScreenCodec(
        this.getProducerCodecMimeType('screen') ?? this.codecKeyFromParameters(codec)
      );
    const selectedCodecKey = this.codecKeyFromParameters(codec);
    const hwTag = this.isHwAccelerated(selectedCodecKey) ? 'HW' : 'SW';
    console.debug(`[codec-floor] Fast re-produced screen with ${selectedCodecKey} (${hwTag})`);
  }

  private async produceScreenReplacement(
    token: VideoReproduceToken,
    transport: mediasoupTypes.Transport,
    stream: MediaStream,
    track: MediaStreamTrack,
    socket: Socket | null
  ): Promise<{
    producer: mediasoupTypes.Producer;
    codec: mediasoupTypes.RtpCodecCapability;
  } | null> {
    let codec: mediasoupTypes.RtpCodecCapability;
    let producer: mediasoupTypes.Producer;
    try {
      const selection = this.pickScreenCodec();
      codec = this.requireSelectedVideoCodec(selection.codec, 'screen');
      producer = await this.produceEncrypted(transport, {
        track,
        encodings: selection.encodings,
        codec,
        codecOptions: {
          videoGoogleStartBitrate: this.computeStartBitrate(selection.effectiveBitrate),
        },
        stopTracks: false,
        appData: { source: 'screen' },
      });
    } catch (err) {
      if (
        this.isCurrentVideoReproduce(token, transport) &&
        this.localScreenStream === stream &&
        !this.producers.has('screen')
      ) {
        await this.cleanupScreenState({ token, transport, stream });
        if (this.isCurrentVideoReproduce(token, transport)) {
          this.setVideoReproduceError('screen');
        }
      }
      throw err;
    }

    if (
      !this.isCurrentVideoReproduce(token, transport) ||
      this.localScreenStream !== stream ||
      this.producers.has('screen')
    ) {
      this.discardProducedProducer(producer, socket);
      return null;
    }
    return { producer, codec };
  }

  /** Re-produce screen audio if an active screen-audio producer and live audio track exist. */
  private async reProduceScreenAudio(
    token = this.captureVideoReproduceToken('screen'),
    transport: mediasoupTypes.Transport | null = this.sendTransport
  ): Promise<void> {
    const oldAudioProducer = this.producers.get('screen-audio');
    const stream = this.localScreenStream;
    const socket = this.socket;
    if (
      !oldAudioProducer ||
      !stream ||
      !transport ||
      !this.isCurrentVideoReproduce(token, transport)
    ) {
      return;
    }

    const audioTrack = stream.getAudioTracks()[0];
    if (audioTrack?.readyState !== 'live') return;

    const oldAudioId = oldAudioProducer.id;
    oldAudioProducer.close();
    await this.drainSendTransportQueue(transport);
    if (
      !this.isCurrentVideoReproduce(token, transport) ||
      this.producers.get('screen-audio') !== oldAudioProducer ||
      this.localScreenStream !== stream
    ) {
      return;
    }
    this.producers.delete('screen-audio');
    this.socket?.emit('close-producer', { producerId: oldAudioId });

    try {
      const newAudioProducer = await this.produceEncrypted(transport, {
        track: audioTrack,
        codecOptions: { opusStereo: true, opusDtx: false },
        stopTracks: false,
        appData: { source: 'screen-audio' },
      });
      if (
        !this.isCurrentVideoReproduce(token, transport) ||
        this.localScreenStream !== stream ||
        this.producers.has('screen-audio')
      ) {
        this.discardProducedProducer(newAudioProducer, socket);
        return;
      }
      this.producers.set('screen-audio', newAudioProducer);
      newAudioProducer.on('transportclose', () => {
        if (this.producers.get('screen-audio') !== newAudioProducer) return;
        this.producers.delete('screen-audio');
      });
    } catch (err) {
      if (this.isCurrentVideoReproduce(token, transport)) {
        console.warn('Failed to re-produce screen audio:', errorMessage(err));
      }
    }
  }

  /**
   * Handle a codec floor update. Check each active video producer and
   * re-produce if a better floor-compatible codec is available or the
   * current codec is no longer in the floor.
   */
  private async handleCodecFloorChange(
    _previousFloor: string[] | null,
    _newFloor: string[] | null,
    expectedSessionGeneration = this.videoReproduceGeneration
  ): Promise<void> {
    if (!this.isCurrentVideoReproduceSession(expectedSessionGeneration)) {
      return;
    }
    await Promise.all(
      (['camera', 'screen'] as const).map((source) =>
        this.reProduceIfBetterCodec(source, undefined, expectedSessionGeneration)
      )
    );
  }

  private enqueueVideoReproduce(
    source: VideoReproduceSource,
    operation: (token: VideoReproduceToken) => Promise<void>,
    expectedSessionGeneration = this.videoReproduceGeneration
  ): Promise<void> {
    if (!this.isCurrentVideoReproduceSession(expectedSessionGeneration)) {
      return Promise.resolve();
    }
    const token = this.captureVideoReproduceToken(source);
    const queued = this.videoReproduceQueues[source]
      .catch(() => undefined)
      .then(async () => {
        if (!this.isCurrentVideoReproduceToken(token)) return;
        await operation(token);
      });
    // Keep the tail fulfilled so a detached caller's rejection cannot become a
    // latent unhandled rejection. Return the original promise so awaited callers
    // still observe the operation failure.
    this.videoReproduceQueues[source] = queued.catch(() => undefined);
    return queued;
  }

  private captureVideoReproduceToken(source: VideoReproduceSource): VideoReproduceToken {
    return {
      source,
      sessionGeneration: this.videoReproduceGeneration,
      sourceGeneration: this.videoReproduceSourceGenerations[source],
    };
  }

  private isCurrentVideoReproduceSession(generation: number): boolean {
    return this.videoReproduceSessionActive && generation === this.videoReproduceGeneration;
  }

  private isCurrentVideoReproduceToken(token: VideoReproduceToken): boolean {
    return (
      this.isCurrentVideoReproduceSession(token.sessionGeneration) &&
      token.sourceGeneration === this.videoReproduceSourceGenerations[token.source]
    );
  }

  private isCurrentVideoProducer(
    token: VideoReproduceToken,
    source: VideoReproduceSource,
    producer: mediasoupTypes.Producer
  ): boolean {
    return this.isCurrentVideoReproduceToken(token) && this.producers.get(source) === producer;
  }

  private isCurrentVideoReproduce(
    token: VideoReproduceToken,
    transport: mediasoupTypes.Transport
  ): boolean {
    return (
      this.isCurrentVideoReproduceToken(token) &&
      this.sendTransport === transport &&
      !transport.closed
    );
  }

  private cancelVideoReproduce(source: VideoReproduceSource): void {
    this.videoReproduceSourceGenerations[source]++;
  }

  private invalidateVideoReproduces(): void {
    this.videoReproduceSessionActive = false;
    this.videoReproduceGeneration++;
    this.cancelVideoReproduce('camera');
    this.cancelVideoReproduce('screen');
    // A successor media session uses different transports, so it must not wait
    // for an old session's potentially blocked device/transport operation.
    this.videoReproduceQueues.camera = Promise.resolve();
    this.videoReproduceQueues.screen = Promise.resolve();
  }

  private setVideoReproduceError(source: VideoReproduceSource): void {
    useVoiceStore
      .getState()
      .setVideoSlotError(
        source === 'camera'
          ? 'Camera stopped because codec re-selection failed. Turn it back on to retry.'
          : 'Screen share stopped because codec re-selection failed. Start sharing again to retry.'
      );
  }

  private discardProducedProducer(producer: mediasoupTypes.Producer, socket: Socket | null): void {
    producer.close();
    socket?.emit('close-producer', { producerId: producer.id });
  }

  /** Re-produce a video source if the codec cascade now selects a different codec. */
  private async reProduceIfBetterCodec(
    source: VideoReproduceSource,
    opts?: { requireHwImprovement?: boolean },
    expectedSessionGeneration = this.videoReproduceGeneration
  ): Promise<void> {
    await this.enqueueVideoReproduce(
      source,
      (token) => this.reProduceIfBetterCodecQueued(source, token, opts),
      expectedSessionGeneration
    );
  }

  private async reProduceIfBetterCodecQueued(
    source: VideoReproduceSource,
    token: VideoReproduceToken,
    opts?: { requireHwImprovement?: boolean }
  ): Promise<void> {
    const producer = this.producers.get(source);
    if (!producer) return;

    const currentMime = this.getProducerCodecMimeType(source);
    if (!currentMime) return;

    const bestPick = source === 'camera' ? this.pickCameraCodec() : this.pickScreenCodec();
    if (!bestPick.codec) {
      await this.closeProducer(source);
      return;
    }
    const bestMime = this.codecKeyFromParameters(bestPick.codec);
    if (this.codecKeysEquivalent(bestMime, currentMime)) return;

    // #2187 item 2: a B=false-driven re-selection only switches toward a genuinely
    // HW-accelerated codec — prevents SW→SW churn/oscillation.
    if (opts?.requireHwImprovement && !this.isHwAccelerated(bestMime)) return;

    // Never switch to a HW codec if hardware acceleration is disabled
    const hwAccel = useVideoSettingsStore.getState().hardwareAcceleration;
    if (!hwAccel && this.isHwAccelerated(bestMime) && !this.isHwAccelerated(currentMime)) {
      console.debug(
        `[codec-floor] Skipping ${source} switch to ${bestMime} (HW) — hardware accel is off`
      );
      return;
    }

    const hwLabel = this.isHwAccelerated(bestMime) ? 'HW' : 'SW';
    console.debug(
      `[codec-floor] Re-producing ${source}: ${currentMime} → ${bestMime} (${hwLabel})`
    );

    if (source === 'camera') {
      await this.fastReproduceCameraQueued(token);
    } else {
      await this.fastReproduceScreenQueued(token);
    }
  }

  // Decoder budget profiling (IGNIS)
  private decoderProfilingTimer: ReturnType<typeof setInterval> | null = null;
  private decoderProfilingInFlight = false;
  private readonly decoderBudgetSampler = new DecoderBudgetSampler();

  /** Number of consecutive green profileDecoders() cycles required before recovering a paused consumer. */
  private static readonly IGNIS_RECOVERY_GREEN_INTERVALS = 3;

  /** Count of consecutive green cycles observed since the last non-green cycle or recovery. */
  private consecutiveGreenIntervals = 0;

  /**
   * Single owner of per-consumer pause state across visibility / IGNIS / PiP reasons.
   * Effects are bound to mediasoup-client pause/resume + the SFU pause/resume emits.
   * See [internal]rules/media-plane.md and the #1541 spec.
   */
  private readonly pauseCoordinator = new ConsumerPauseCoordinator({
    pauseLocalDecode: (id) => {
      this.consumers.get(id)?.pause();
    },
    resumeLocalDecode: (id) => {
      this.consumers.get(id)?.resume();
    },
    pauseServerForwarding: (id) => {
      this.socket?.emit('pause-consumer', { consumerId: id });
    },
    resumeServerForwarding: (id) => {
      this.socket?.emit('resume-consumer', { consumerId: id });
    },
  });

  /**
   * Per-user, per-tile visibility (`userId → tileId → isVisible`). A participant can render
   * in several tiles at once (grid + bar + PiP); a consumer is hidden only when EVERY known
   * tile is hidden. Survives consumer create/teardown.
   */
  private readonly tileVisibilityByUser = new Map<string, Map<string, boolean>>();
  private cameraLayeringEnabled = false;
  private cameraLayeringReproduceInFlight = false;
  private cameraLayeringReproducePending = false;
  /** Server-authoritative screen-layering gate (#1924). Only when the media-plane
   *  flips `screen-layering-gate` on may the local publisher emit simulcast screen. */
  private screenLayeringEnabled = false;
  private screenLayeringReproduceInFlight = false;
  private screenLayeringReproducePending = false;
  private readonly remoteVideoPressureByUser = new Map<string, boolean>();
  private readonly lastPreferredLayerKeyByConsumer = new Map<string, string>();
  private readonly remoteVideoRenderStateByUser = new Map<
    string,
    Map<string, RemoteVideoTileRenderState>
  >();
  /** Receiver-driven SCREEN render-state, keyed by producing userId → tileId (#1924).
   *  Mirrors remoteVideoRenderStateByUser (camera) but feeds screen set-preferred-layers
   *  demand. Kept separate so a user's screen demand never mixes with their camera demand. */
  private readonly remoteScreenRenderStateByUser = new Map<
    string,
    Map<string, RemoteVideoTileRenderState>
  >();
  /** Per-sharer (userId → intent) marker recording that THIS viewer was tuned into
   *  that sharer's screen when its producer just closed, so the NEXT screen announce
   *  from the same sharer auto-re-tunes-in regardless of the autoTuneInScreenShares
   *  opt-in (#1924 review fix A). Armed at producer-closed time (a reproduce closes
   *  the old producer and re-announces a new one, and producer-closed generally lands
   *  before the new-producer announce), consumed at new-producer time, and bounded by
   *  a timer so a genuine stop-then-restart later than the reproduce window falls back
   *  to the normal opt-in path. */
  private readonly screenReproducePending = new Map<
    string,
    { wasDominant: boolean; timer: ReturnType<typeof setTimeout> }
  >();
  /** Whether the whole window is currently hidden (document.hidden). */
  private documentHidden = false;
  /** Bound visibilitychange handler, retained for removeEventListener. */
  private boundDocVisibility: (() => void) | null = null;

  // ─── Join / Leave ──────────────────────────────────────────────────

  /** Set up E2EE encryption, decrypt keys, and epoch catch-up for an encrypted channel */
  private async setupE2EEForChannel(
    channelId: string,
    roomJoined: RoomJoinedResponse
  ): Promise<void> {
    await this.initEncryption(channelId);
    for (const p of roomJoined.participants) {
      await this.addDecryptKeyForUser(channelId, p.userId);
    }
    if (roomJoined.e2eeEpoch && this.mediaEncryption) {
      if (this.e2eeWorker) {
        this.e2eeWorker.postMessage({
          type: 'catchUpToEpoch',
          targetEpoch: roomJoined.e2eeEpoch,
        } satisfies E2EEWorkerMessage);
      }
      await this.mediaEncryption.catchUpToEpoch(roomJoined.e2eeEpoch);
    }
  }

  /** Build VoiceParticipant list from room-join response, including video/screen state */
  private buildParticipantList(roomJoined: RoomJoinedResponse): VoiceParticipant[] {
    const producersByUser = new Map<string, { isVideoOn: boolean; isScreenSharing: boolean }>();
    for (const ep of roomJoined.existingProducers) {
      const entry = producersByUser.get(ep.userId) || { isVideoOn: false, isScreenSharing: false };
      if (ep.source === 'camera') entry.isVideoOn = true;
      if (ep.source === 'screen') entry.isScreenSharing = true;
      producersByUser.set(ep.userId, entry);
    }
    return roomJoined.participants.map((p) => {
      const producerState = producersByUser.get(p.userId);
      return {
        userId: p.userId,
        username: p.username,
        displayName: p.displayName,
        avatarUrl: p.avatarUrl,
        isMuted: false,
        isDeafened: p.isDeafened ?? false,
        isTesting: p.isTesting ?? false,
        serverMuted: false,
        serverDeafened: false,
        isVideoOn: producerState?.isVideoOn ?? false,
        isScreenSharing: producerState?.isScreenSharing ?? false,
        isSpeaking: false,
      };
    });
  }

  /** Consume or register existing producers from the room-join response */
  private async consumeExistingProducers(
    existingProducers: RoomJoinedResponse['existingProducers']
  ): Promise<void> {
    const store = useVoiceStore.getState();
    for (const producer of existingProducers) {
      if (producer.source === 'screen') {
        const participant = store.participants[producer.userId];
        store.addAvailableScreenShare({
          producerId: producer.producerId,
          userId: producer.userId,
          username: participant?.username || 'Unknown',
          displayName: participant?.displayName,
        });
        store.registerActiveScreenShare({
          producerId: producer.producerId,
          userId: producer.userId,
          username: participant?.username || 'Unknown',
          displayName: participant?.displayName,
          isLocal: false,
        });
      } else if (producer.source === 'screen-audio') {
        this.pendingScreenAudioProducers.set(producer.userId, producer.producerId);
      } else {
        await this.consumeProducer(
          producer.producerId,
          producer.userId,
          producer.kind as mediasoupTypes.MediaKind
        );
      }
    }
    // Join trigger (#2088): the setting check lives inside the sweep.
    // Fire-and-forget — the sweep's per-share key/consume round trips must
    // not delay the join reaching 'connected' (mirrors the toggle trigger).
    void this.autoTuneSweep();
  }

  /** Apply permissions, enforcement flags, and DM state from the join response. */
  private applyJoinMetadata(
    store: ReturnType<typeof useVoiceStore.getState>,
    joinData: JoinResponse,
    joinType: 'channel' | 'dm',
    channelId: string
  ): void {
    // Store effective permissions
    if (joinData.permissions) {
      try {
        store.setEffectivePermissions(BigInt(joinData.permissions));
      } catch {
        store.setEffectivePermissions(0n);
      }
    }

    // Apply server enforcement flags (local audio state only;
    // participant record is updated after setParticipants in joinChannel)
    if (joinData.server_muted || joinData.server_deafened) {
      if (joinData.server_muted) store.setMuted(true);
      if (joinData.server_deafened) {
        store.setDeafened(true);
        store.setMuted(true);
      }
    }

    // Track DM call state
    if (joinType === 'dm') {
      store.setDMCall(true, channelId);
      store.setGroupDMInfo(
        joinData.conversation?.is_group || false,
        joinData.conversation?.caller_role || null
      );
    }
  }

  /** Resolve effective audio quality tier from channel override or personal setting. */
  private resolveQualityTier(
    store: ReturnType<typeof useVoiceStore.getState>,
    channelTier: string | undefined
  ): void {
    const validTiers = ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'];
    if (channelTier && validTiers.includes(channelTier)) {
      store.setEffectiveQualityTier(channelTier as AudioQualityTier);
    } else {
      store.setEffectiveQualityTier(store.qualityTier);
    }
  }

  /** Apply server enforcement flags to the local participant record. */
  private applyEnforcementToParticipant(
    store: ReturnType<typeof useVoiceStore.getState>,
    joinData: JoinResponse
  ): void {
    if (!joinData.server_muted && !joinData.server_deafened) return;
    const localUserId = useUserStore.getState().user?.id;
    if (!localUserId) return;
    store.updateParticipant(localUserId, {
      serverMuted: joinData.server_muted || false,
      serverDeafened: joinData.server_deafened || false,
    });
  }

  /**
   * Synthesize a channel-shaped object for DM voice joins where the server
   * response includes `conversation` instead of `channel` (#1209 plan F4
   * spec §7.9). Extracted from joinChannel to keep that function's
   * cognitive complexity within the project's S3776 bound. The synth name
   * is "group name" for group DMs, peer's displayName for 1:1 DMs (via
   * the utils/messaging/dm.peerName helper, which is store-agnostic per the
   * SonarCloud architecture rule), or a fallback string.
   */
  private async synthesizeDMChannel(channelId: string): Promise<{
    id: string;
    name: string;
    server_id: string;
    audio_quality_tier: string | null;
  }> {
    const { useDMStore: importedDMStore } = await import('../../stores/chat/dmStore');
    const { useUserStore: importedUserStore } = await import('../../stores/auth/userStore');
    const conversation = importedDMStore.getState().conversations.find((c) => c.id === channelId);
    const currentUserId = importedUserStore.getState().user?.id;
    const synthName = await this.resolveDMChannelName(conversation, currentUserId);
    return {
      id: channelId,
      name: synthName,
      server_id: '', // DM rooms aren't server-scoped (per spec §3.4)
      audio_quality_tier: null,
    };
  }

  /** Resolve the display name for a DM voice call (group name, peer name, or fallback). */
  private async resolveDMChannelName(
    conversation:
      | {
          isGroup: boolean;
          name: string | null;
          participants: Array<{ userId: string; username: string; displayName?: string }>;
        }
      | undefined,
    currentUserId: string | undefined
  ): Promise<string> {
    if (!conversation) return 'Voice call';
    if (conversation.isGroup) return conversation.name ?? 'Group voice call';
    if (!currentUserId) return 'Voice call';
    const { peerName: synthesizePeerName } = await import('../../utils/messaging/dm');
    return synthesizePeerName(conversation.participants, currentUserId);
  }

  private claimDMJoinOwnership(
    channelId: string,
    joinType: 'channel' | 'dm',
    store: ReturnType<typeof useVoiceStore.getState>
  ): Extract<CallState, { kind: 'joining' }> | undefined {
    if (joinType !== 'dm') return undefined;

    const callState = store.callState;
    const isMatchingRingJoin =
      (callState.kind === 'outgoing-ringing' ||
        callState.kind === 'incoming-ringing' ||
        callState.kind === 'joining') &&
      callState.conversationId === channelId &&
      callState.ringId.length > 0;

    if (callState.kind !== 'idle') {
      if (!isMatchingRingJoin) throw new Error('Another voice call is already in progress');
      if (callState.kind === 'joining') return undefined;
    }

    // Claim global ownership synchronously so a second click or a racing
    // invitation cannot start another session before the first await.
    const joiningState: Extract<CallState, { kind: 'joining' }> = {
      kind: 'joining',
      conversationId: channelId,
      ringId:
        callState.kind === 'outgoing-ringing' || callState.kind === 'incoming-ringing'
          ? callState.ringId
          : '',
    };
    store.setCallState(joiningState);
    return joiningState;
  }

  private dmJoiningStateToPreserve(
    channelId: string,
    joinType: 'channel' | 'dm'
  ): Extract<CallState, { kind: 'joining' }> | undefined {
    const callState = useVoiceStore.getState().callState;
    if (
      joinType === 'dm' &&
      callState.kind === 'joining' &&
      callState.conversationId === channelId
    ) {
      return callState;
    }
    return undefined;
  }

  private async leaveActiveChannelBeforeJoin(
    store: ReturnType<typeof useVoiceStore.getState>,
    callStateToPreserve: Extract<CallState, { kind: 'joining' }> | undefined
  ): Promise<void> {
    if (!store.activeChannelId) return;
    await this.leaveChannel();
    if (!callStateToPreserve || useVoiceStore.getState().callState.kind !== 'idle') return;
    useVoiceStore.getState().setCallState(callStateToPreserve);
  }

  private async resolveAuthorizedJoinChannel(
    joinData: JoinResponse,
    channelId: string,
    joinType: 'channel' | 'dm'
  ): Promise<NonNullable<JoinResponse['channel']>> {
    if (joinData.channel) return joinData.channel;
    if (joinType === 'dm') return this.synthesizeDMChannel(channelId);
    throw new Error('Voice join response missing channel info');
  }

  /** Authorize and join a voice channel */
  async joinChannel(
    channelId: string,
    joinType: 'channel' | 'dm' = 'channel',
    opts?: { internalRebuild?: boolean }
  ): Promise<void> {
    // A user joining anything supersedes a pending legacy-fallback rebuild.
    // The rebuild's own join runs after its intent check, so clearing here is
    // harmless for it and decisive against a racing user join.
    this.legacyRebuildIntent = null;
    if (this.joinInFlight) throw new Error('Another voice call is already in progress');
    this.joinInFlight = true;
    let shouldRecoverJoinFailure = false;
    try {
      const store = useVoiceStore.getState();
      const directDMJoiningState = this.claimDMJoinOwnership(channelId, joinType, store);
      shouldRecoverJoinFailure = true;
      const callStateToPreserve = this.dmJoiningStateToPreserve(channelId, joinType);
      const ringId = joinType === 'dm' ? this.currentDMRingID(channelId) : undefined;
      await this.leaveActiveChannelBeforeJoin(store, callStateToPreserve);

      store.setConnectionState('connecting');

      // Step 1: Authorize via control plane (uses apiFetch for automatic token refresh)
      const joinData = await this.authorizeVoiceJoin(channelId, joinType, ringId);

      const { media_server_url } = joinData;
      // For DM voice joins the response omits `channel` and includes
      // `conversation` instead. Synthesize a channel-shaped object from
      // the conversation + dmStore peer/group-name lookup per spec §7.9
      // (#1209 plan task F4 — VoiceView channelName synthesis). Extracted
      // into synthesizeDMChannel to keep joinChannel's cognitive complexity
      // under the 15-statement bound (was 19 before extraction).
      const channel = await this.resolveAuthorizedJoinChannel(joinData, channelId, joinType);

      store.setActiveChannel(channel.id, channel.name, channel.server_id);

      this.applyJoinMetadata(store, joinData, joinType, channelId);

      // Start outgoing ringback for DM calls
      // Silent during a legacy-fallback rebuild: the peer never left the call,
      // so ringback would be a false signal (Gitar, PR #2866).
      if (joinType === 'dm' && !opts?.internalRebuild)
        notificationSoundService.playLoop('call-outgoing');

      this.resolveQualityTier(store, channel.audio_quality_tier ?? undefined);

      // Pre-acquire mic stream in parallel with socket connection + room join.
      // getUserMedia can take 500ms+ (device enumeration, permission prompt).
      // Starting it now overlaps that latency with the network handshake below.
      // Skip it entirely for a listen-only join (Speak not granted): the
      // media-plane produce() gate would reject the mic publish anyway, and
      // starting getUserMedia would pop an unnecessary permission prompt and
      // block the join behind slow device acquisition for a user who is not
      // allowed to speak. establishMediaSession then takes its listen-only
      // branch with a null mic promise (releasePreAcquiredMic(null) no-ops).
      const micPromise = this.joinPermitsSpeak(joinData) ? this.acquireMicStream() : null;

      // Step 2: Connect Socket.IO to media plane
      // Fail fast when unauthenticated; the socket's auth callback below
      // re-reads the store token on every connection attempt.
      if (!useAuthStore.getState().accessToken) throw new Error('Not authenticated');

      this.socket = io(media_server_url, {
        // The auth CALLBACK form is evaluated on EVERY connection attempt
        // (initial handshake + each automatic reconnection), so the
        // handshake always presents the freshest store token — a static
        // auth object would snapshot the call-start JWT, which is expired
        // after the 15-min access-token TTL on long calls and would make
        // every post-drop reconnect fail auth (#1790). The proactive
        // refresh timer (#240) keeps the store token current mid-call.
        auth: (cb) => {
          const user = useUserStore.getState().user;
          cb({
            token: useAuthStore.getState().accessToken,
            username: user?.username || 'unknown',
            displayName: user?.display_name || undefined,
            avatarUrl: user?.avatar_url || undefined,
            // room_kind routing hint per #1209 spec §6.5 / plan task C1.
            // Tells the media-plane which control-plane endpoint to
            // authoritatively validate against (server-channel join vs DM
            // authorize). The user's identity for AUTHORIZATION is established
            // by the JWT (token field above) — the room_kind hint just selects
            // which validation endpoint runs. The username/displayName/
            // avatarUrl fields here are client-supplied display data, NOT
            // identity claims, and are documented as a known gap in
            // [internal]rules/media-plane.md (server uses JWT-derived userId
            // for participation enforcement).
            room_kind: joinType === 'dm' ? 'dm' : 'channel',
          });
        },
        // Websocket-first for latency, with HTTP long-polling as a fallback so a
        // transient WS-transport blip degrades instead of hard-failing the join.
        // tryAllTransports is REQUIRED: with 'websocket' listed first, engine.io does
        // NOT auto-try the next transport on a failed WS *open* unless this is set
        // (default false since socket.io-client 4.7.0). It is consulted ONLY when the
        // initial WS transport fails, so the normal (~30ms) WS path is unchanged. Safe
        // here because the media-plane is single-instance and its Socket.IO server
        // advertises polling (handshake 200 + upgrades:['websocket']) — no sticky
        // session requirement. #2176.
        transports: ['websocket', 'polling'],
        tryAllTransports: true,
        reconnection: true,
        reconnectionAttempts: 10,
        reconnectionDelay: 1000,
      });

      await this.waitForConnect();
      this.setupSocketListeners();
      this.registerDocumentVisibilityListener();

      // Steps 3–9: join room → load device → transports → E2EE →
      // participants → mic producer → existing consumers. Shared with
      // resumeAfterReconnect (#1790) via establishMediaSession.
      await this.establishMediaSession(store, channel.id, joinData, micPromise);

      store.setConnectionState('connected');
      notificationSoundService.stopAllLoops();
      if (!opts?.internalRebuild)
        notificationSoundService.play(joinType === 'dm' ? 'call-connected' : 'voice-join');
      if (directDMJoiningState && useVoiceStore.getState().callState === directDMJoiningState) {
        store.setCallState({ kind: 'in-call' });
      }

      // Step 10: Start decoder budget profiling (IGNIS insight)
      // Profiles decode performance and adjusts SVC layers to avoid queue buildup
      this.startDecoderBudgetProfiling();
    } catch (err) {
      if (shouldRecoverJoinFailure) await this.handleJoinFailure(err);
      throw err;
    } finally {
      this.joinInFlight = false;
    }
  }

  /**
   * Recover from a joinChannel failure: log, stop loops, tear down media
   * (resilient to cleanup throwing), and reset the Zustand store before
   * surfacing 'error' to the UI.
   *
   * Extracted from joinChannel so that body stays within the project's
   * cognitive-complexity bound. See voiceService.test.ts regression cases
   * for the contract this method enforces.
   */
  /**
   * Sanitize an unknown thrown value for safe logging. Strips ASCII
   * control characters (defends against CRLF log injection — Sonar S4790)
   * and caps length so a pathologically long error message can't blow up
   * log volume. Used by handleJoinFailure and its cleanup-error sibling.
   */
  private sanitizeErrForLog(err: unknown): string {
    const raw = err instanceof Error ? err.message : 'non-Error thrown';
    return raw.replace(/[\x00-\x1F\x7F]/g, '').slice(0, 200);
  }

  /**
   * True when a thrown join error carries the media-plane's typed
   * `crypto_version_mismatch` ack code (#1878). emitAsync copies the ack's
   * `code` onto the rejected Error so this branch is reachable from the
   * handleJoinFailure recovery path.
   */
  private isCryptoVersionMismatch(err: unknown): boolean {
    return (
      err instanceof Error && (err as Error & { code?: string }).code === 'crypto_version_mismatch'
    );
  }

  private async handleJoinFailure(err: unknown): Promise<void> {
    const store = useVoiceStore.getState();
    // Sanitized error for log sink — strips control chars + bounds length
    // (Sonar S4790 / [internal]rules/observability.md "Console error logging").
    console.error('Failed to join voice channel:', this.sanitizeErrForLog(err));
    // A typed `crypto_version_mismatch` ack means this client and the active room
    // use incompatible media-security formats. Either side may be newer, so keep
    // the persistent banner copy direction-neutral.
    if (this.isCryptoVersionMismatch(err)) {
      useUpdateStatusStore
        .getState()
        .setSecurityError(
          'media-crypto-version',
          'This voice call requires the same media-security version.'
        );
    }
    notificationSoundService.stopAllLoops();
    // Defense in depth: if cleanup() throws (mediasoup transport teardown,
    // E2EE worker crash mid-destroy, etc.), the store.reset() + 'error'
    // transition below MUST still run — otherwise the ghost-state bug this
    // catch block fixes regresses on a different failure mode. Log the
    // cleanup failure separately so it isn't silently lost.
    try {
      await this.cleanup();
    } catch (cleanupErr) {
      console.error(
        'Cleanup failed during join-error recovery:',
        this.sanitizeErrForLog(cleanupErr)
      );
    }
    // store.setActiveChannel runs early in joinChannel (before mic / media-
    // plane handshake), so a late-stage failure leaves activeChannelId
    // pointing at the channel we never actually joined. Reset clears that
    // ghost state (including participants, encryption flags, mute state)
    // before the final 'error' transition surfaces the failure to the UI.
    // Reset must precede the setConnectionState('error') call because
    // reset() restores the default 'disconnected' connectionState.
    store.reset();
    store.setConnectionState('error');
  }

  /** Leave the current voice channel */
  async leaveChannel(opts?: { internalRebuild?: boolean }): Promise<void> {
    // Any user-initiated leave cancels a pending legacy-fallback rebuild —
    // rejoining a call the user explicitly left is never acceptable
    // (CodeRabbit, PR #2866). The rebuild's own leave passes internalRebuild.
    if (!opts?.internalRebuild) this.legacyRebuildIntent = null;
    const store = useVoiceStore.getState();
    const channelId = store.activeChannelId;
    const isDMCall = store.isDMCall;
    const localUserId = useUserStore.getState().user?.id;

    if (this.socket?.connected) {
      this.socket.emit('leave-room');
    }
    await this.cleanup();

    notificationSoundService.stopAllLoops();
    // Silent during the legacy-fallback rebuild — the user is not leaving.
    if (!opts?.internalRebuild)
      notificationSoundService.play(isDMCall ? 'call-ended' : 'voice-leave');

    // Remove local user from channel voice members immediately so the
    // channel sidebar updates without waiting for the server roundtrip.
    if (channelId && localUserId) {
      store.removeChannelVoiceMember(channelId, localUserId);
    }

    store.reset();
  }

  /**
   * Emergency cleanup: synchronously stop all local media tracks and tear
   * down transports without awaiting any server-side acknowledgment.
   * Used for forced logout, token revocation, connection loss, and app close
   * where we can't wait for network round-trips.
   * All operations are idempotent — safe to call multiple times.
   */
  emergencyCleanup(): void {
    this.cleanupMediaAndTransports();
    this.cleanupTimersAndE2EE();

    // Disconnect socket (fire-and-forget)
    try {
      this.socket?.disconnect();
    } catch {
      /* ignore */
    }
    this.socket = null;

    // Reset device, router caps, and consume queues
    this.device = null;
    this.routerRtpCapabilities = null;
    this.consumeQueueAudio = Promise.resolve();
    this.consumeQueueVideo = Promise.resolve();

    // Reset store
    useVoiceStore.getState().clearAvailableScreenShares();
    useVoiceStore.getState().reset();
  }

  /**
   * Step 1 of the join flow: authenticated control-plane join authorization.
   * Shared by joinChannel (initial join) and resumeAfterReconnect (#1790).
   */
  private async authorizeVoiceJoin(
    channelId: string,
    joinType: 'channel' | 'dm',
    ringId?: string
  ): Promise<JoinResponse> {
    const endpoint =
      joinType === 'dm'
        ? `/api/v1/dm/conversations/${channelId}/voice/join`
        : `/api/v1/channels/${channelId}/voice/join`;
    const res = await apiFetch(endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      ...(ringId ? { body: JSON.stringify({ ring_id: ringId }) } : {}),
    });

    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      throw new Error(err.error || `Voice join failed: ${res.status}`);
    }

    const joinData: JoinResponse = await res.json();
    if (!joinData.allowed) throw new Error('Not allowed to join this channel');
    return joinData;
  }

  /** Ring correlation is valid only while joining the conversation that owns it. */
  private currentDMRingID(channelId: string): string | undefined {
    const state = useVoiceStore.getState().callState;
    if (
      state.kind !== 'outgoing-ringing' &&
      state.kind !== 'incoming-ringing' &&
      state.kind !== 'joining'
    ) {
      return undefined;
    }
    if (state.conversationId !== channelId) {
      return undefined;
    }
    return state.ringId || undefined;
  }

  /**
   * Steps 3–9 of the join flow: join the room on the connected socket and
   * build the full client-side media session — device load, transports,
   * E2EE, participants, mic producer, existing consumers. Shared by
   * joinChannel (initial join) and resumeAfterReconnect (#1790).
   *
   * micStreamPromise is awaited only immediately before produceAudio so a
   * pre-acquired getUserMedia (joinChannel's latency overlap) keeps running
   * in parallel with the room join and transport creation.
   */
  private async establishMediaSession(
    store: ReturnType<typeof useVoiceStore.getState>,
    channelId: string,
    joinData: JoinResponse,
    micStreamPromise: Promise<MediaStream | null> | null
  ): Promise<void> {
    const roomJoined = await this.emitAsync<RoomJoinedResponse>('join-room', {
      roomId: channelId,
      rtpCapabilities: undefined, // Will be set after device.load
      mediaFrameCryptoVersion: MEDIA_E2EE_FRAME_CRYPTO_VERSION,
      ...(joinData.call_id ? { callId: joinData.call_id } : {}),
    });
    if (roomJoined.mediaFrameCryptoVersion !== MEDIA_E2EE_FRAME_CRYPTO_VERSION) {
      throw new Error('Media frame crypto version mismatch');
    }

    // Load device with router capabilities
    this.device = new Device();
    this.routerRtpCapabilities = roomJoined.rtpCapabilities;
    await this.device.load({ routerRtpCapabilities: roomJoined.rtpCapabilities });

    // Send actual RTP capabilities to server (join-room sent undefined)
    if (!this.socket) throw new Error('Socket disconnected before RTP capabilities update');
    this.socket.emit('update-rtp-capabilities', {
      rtpCapabilities: this.device.rtpCapabilities,
    });

    // Create transports in parallel (no dependency between send/recv)
    await Promise.all([this.createSendTransport(), this.createRecvTransports()]);

    // Initialize E2EE (all channels are always encrypted)
    await this.setupE2EEForChannel(channelId, roomJoined);

    // Set participants before consuming producers
    store.setParticipants(this.buildParticipantList(roomJoined));

    // Apply enforcement flags after participants are populated
    this.applyEnforcementToParticipant(store, joinData);

    // Produce audio (using the pre-acquired mic stream when provided) — unless
    // the server denies Speak. A listen-only member (ViewVoiceChannels|JoinVoice
    // without Speak, e.g. a listen-only role or channel override) is admitted by
    // AuthorizeJoin, but the media-plane produce() gate would reject the mic
    // publish and handleJoinFailure would tear the whole session down. Skip
    // auto-produce and join with mic off so the stock client can listen.
    if (this.joinPermitsSpeak(joinData)) {
      const preAcquiredStream = micStreamPromise ? await micStreamPromise : null;
      await this.produceAudio(undefined, preAcquiredStream);
    } else {
      // Release any pre-acquired mic so no capture device stays open (no stray
      // mic light), and reflect the mic-off state in the UI.
      await this.releasePreAcquiredMic(micStreamPromise);
      store.setMuted(true);
    }
    this.setupLiveSubscriptions();

    // Consume existing producers
    await this.consumeExistingProducers(roomJoined.existingProducers);
  }

  /**
   * Rebuild the media session on the freshly reconnected socket after a
   * transient network change (#1790 — VPN toggle, interface switch). The
   * media-plane removes a participant the moment its socket drops (no
   * server-side grace period), so everything signaling-bound — transports,
   * producers, consumers, E2EE transforms — is dead and must be re-created.
   *
   * Re-runs the authenticated control-plane join authorization (identical
   * trust path to the original join: no client-supplied identity beyond the
   * JWT), then joinChannel's steps 3–9 on the already-connected socket.
   * Bounded: any failure falls through to emergencyCleanup(), the existing
   * user-visible disconnect path — never a silent zombie call.
   */
  private async resumeAfterReconnect(): Promise<void> {
    if (this.resumeInFlight) return;
    const store = useVoiceStore.getState();
    const channelId = store.activeChannelId;
    if (!channelId || !this.socket) {
      // No call context to resume — tear down rather than strand the UI in
      // a zombie 'reconnecting' state.
      this.emergencyCleanup();
      return;
    }
    this.resumeInFlight = true;
    const joinType: 'channel' | 'dm' = store.isDMCall ? 'dm' : 'channel';
    try {
      // Step 1 (as in joinChannel): re-authorize via control plane so
      // enforcement flags and media entitlements are fresh.
      const joinData = await this.authorizeVoiceJoin(channelId, joinType);

      // Quietly discard the dead client-side media/E2EE state. Keeps the
      // socket, the store's call identity, and the visible call UI; local
      // capture restarts below via produceAudio.
      this.cleanupMediaAndTransports();
      this.cleanupTimersAndE2EE();
      this.consumeQueueAudio = Promise.resolve();
      this.consumeQueueVideo = Promise.resolve();
      store.clearAvailableScreenShares();
      // #2088: the rebuilt session re-discovers screen shares from scratch —
      // drop stale consumption/metadata/suppression state (the consumers were
      // torn down above, and producer-closed events missed during the drop
      // could never prune these maps).
      store.resetScreenShareConsumption();
      // A consume that never settled (socket died mid-flight) would strand its
      // producerId in the in-flight guard forever; remote producer ids survive
      // OUR reconnect, so clear the guard with the rest of the tune state.
      this.tuneInsInFlight.clear();
      // Local camera/screen producers did not survive the network change;
      // reflect that honestly instead of showing a dead camera as live.
      store.setVideoOn(false);
      store.setScreenSharing(false);

      this.applyJoinMetadata(store, joinData, joinType, channelId);
      this.registerDocumentVisibilityListener();

      // Test-suspension state (testSuspensionDepth / testSuspended*Ids) is
      // intentionally NOT reset here: the depth ref-count pairs with the
      // device-test UI's begin/end calls, and the stale producer/consumer IDs
      // are harmless no-ops against the rebuilt session (new IDs are never in
      // the stale sets; endTestSuspension clears them).

      // Steps 3–9 (as in joinChannel): join-room → device → transports →
      // E2EE → participants → mic producer → existing consumers.
      await this.establishMediaSession(store, channelId, joinData, null);

      // Stale-context guard: a leaveChannel() racing the awaits above has
      // already torn the call down (socket nulled, store reset) — don't
      // resurrect 'connected' UI state for a call the user left.
      if (!this.socket || useVoiceStore.getState().activeChannelId !== channelId) {
        return;
      }

      // Re-apply the user's pre-drop self-mute/deafen: establishMediaSession
      // builds an UNMUTED mic producer and RESUMED consumers, so without this
      // a muted user would silently transmit after recovery (Gitar finding,
      // PR #2029). Server-mute/deafen is separately re-applied server-side at
      // re-join; this covers the client-side self state. Deafen also implies
      // self-mute (toggleDeafen), so the mic branch covers both.
      const postResume = useVoiceStore.getState();
      const mic = this.producers.get('mic');
      if (postResume.isMuted && mic && !mic.paused) {
        mic.pause();
        this.socket.emit('pause-producer', { producerId: mic.id });
        this.stopLocalVAD();
      }
      if (postResume.isDeafened) {
        // Rebroadcast so the freshly-created server-side participant and
        // peers' sidebars reflect the deafen state (#685 path).
        this.socket.emit('set-deafen', { isDeafened: true });
        for (const [, consumer] of this.consumers) {
          if (consumer.kind === 'audio') consumer.pause();
        }
      }

      useVoiceStore.getState().setConnectionState('connected');
      this.startDecoderBudgetProfiling();
      // PII-safe diagnostic: distinguishes an expected network-transition
      // recovery from a fatal voice disconnect (issue acceptance criterion).
      console.warn('Voice media session resumed after network change');
    } catch (err) {
      console.warn('Voice resume after network change failed:', errorMessage(err));
      // Stale-context guard (mirror of the success path): only tear down if
      // this resume still owns the call context. If a successor joinChannel
      // switched channels during our awaits, this.* holds the NEW call's
      // objects — emergencyCleanup here would destroy a live call. When the
      // user merely left (activeChannelId null), cleanup is idempotent.
      const owner = useVoiceStore.getState().activeChannelId;
      if (owner === channelId || owner === null) {
        this.emergencyCleanup();
      }
    } finally {
      this.resumeInFlight = false;
    }
  }

  /** Stop local streams, close producers/consumers/transports. */
  private cleanupMediaAndTransports(): void {
    this.invalidateVideoReproduces();
    for (const stream of [this.localMicStream, this.localCameraStream, this.localScreenStream]) {
      if (stream) for (const t of stream.getTracks()) t.stop();
    }
    this.localMicStream = null;
    this.localCameraStream = null;
    this.localScreenStream = null;

    for (const [, producer] of this.producers) {
      try {
        producer.close();
      } catch {
        /* ignore */
      }
    }
    this.producers.clear();

    for (const [, consumer] of this.consumers) {
      try {
        consumer.close();
      } catch {
        /* ignore */
      }
    }
    this.consumers.clear();
    this.decoderBudgetSampler.clear();
    this.consumerMeta.clear();
    this.pendingScreenAudioProducers.clear();
    this.resetRemoteVideoLayeringState();

    for (const t of [this.sendTransport, this.recvTransportAudio, this.recvTransportVideo]) {
      try {
        t?.close();
      } catch {
        /* ignore */
      }
    }
    this.sendTransport = null;
    this.recvTransportAudio = null;
    this.recvTransportVideo = null;
  }

  /** Tear down already-invalidated shared E2EE state synchronously. */
  private teardownSharedE2EEState(): void {
    this.keyRotationOff?.();
    this.keyRotationOff = null;
    this.terminateE2EEWorker();
    if (this.rotationTimer) {
      clearTimeout(this.rotationTimer);
      this.rotationTimer = null;
    }
    this.rotationPending = false;
    this.rotationDeadline = 0;
    this.mediaEncryption?.destroy();
    this.mediaEncryption = null;
    this.latestRequestedEncryptKeyVersion = 0;
  }

  /** Stop timers, subscriptions, and E2EE state. */
  private cleanupTimersAndE2EE(): void {
    // Must precede every other teardown operation: a key-fetch/derivation
    // continuation may otherwise publish a fresh Worker after cleanup began.
    this.invalidatePendingE2EEInit();
    // Pending bypass probes reference consumers of the session being torn
    // down; a worker terminated mid-probe can never deliver the reply that
    // would clear them (CodeRabbit, PR #2865).
    this.bypassProbes.clear();
    this.teardownSharedE2EEState();
    this.stopLocalVAD();
    this.stopPacketLossMonitor();
    this.teardownLiveSubscriptions();
    if (this.decoderProfilingTimer) {
      clearInterval(this.decoderProfilingTimer);
      this.decoderProfilingTimer = null;
    }
    this.decoderProfilingInFlight = false;
    this.unregisterDocumentVisibilityListener();
    this.pauseCoordinator.reset();
    this.consecutiveGreenIntervals = 0;
  }

  // ─── Audio Producer ────────────────────────────────────────────────

  /**
   * Whether the join response grants Speak (permission to publish a mic
   * producer). Channel rooms carry a server-authoritative decimal bitfield; a
   * listen-only member is admitted to the channel but must NOT auto-produce mic
   * (the media-plane produce() gate would reject it). DM rooms have no server
   * permission model (`permissions` undefined) so Speak is always allowed. A
   * malformed bitfield fails closed to listen-only. Administrator bypasses
   * (hasPermission short-circuits), mirroring the control-plane resolver.
   */
  private joinPermitsSpeak(joinData: JoinResponse): boolean {
    if (joinData.permissions === undefined) return true;
    let bits: bigint;
    try {
      bits = BigInt(joinData.permissions);
    } catch {
      return false;
    }
    return hasPermission(bits, SPEAK);
  }

  /**
   * Stop any pre-acquired mic stream (from acquireMicStream's latency overlap)
   * that a listen-only join will not produce, so the capture device is released
   * and no mic indicator lingers. Safe when the promise is null or resolved null.
   */
  private async releasePreAcquiredMic(
    micStreamPromise: Promise<MediaStream | null> | null
  ): Promise<void> {
    if (!micStreamPromise) return;
    const stream = await micStreamPromise;
    stream?.getTracks().forEach((track) => track.stop());
  }

  /**
   * Pre-acquire the mic stream so getUserMedia latency overlaps with
   * socket connection, room join, and transport creation.
   */
  private async acquireMicStream(): Promise<MediaStream | null> {
    try {
      // JIT permission check (#197): request mic access on macOS before getUserMedia.
      // On macOS, the plist patch (scripts/patch-electron-plist.sh) ensures the
      // helper process has NSMicrophoneUsageDescription, so getUserMedia can safely
      // trigger the native TCC prompt. We only block if explicitly denied/restricted.
      const micStatus = await ensureOsPermissionShared('microphone');
      if (micStatus === 'denied' || micStatus === 'restricted') {
        console.warn(`[VoiceService] Mic permission ${micStatus}, skipping getUserMedia`);
        return null;
      }

      const adv = useAudioSettingsStore.getState();
      const useProcessing = !adv.musicMode;
      return await navigator.mediaDevices.getUserMedia({
        audio: {
          echoCancellation: useProcessing && adv.echoCancellation,
          noiseSuppression: useProcessing && adv.noiseCancellation,
          autoGainControl: useProcessing && adv.autoGainControl,
          sampleRate: 48000,
          channelCount: 2,
        },
      });
    } catch (err) {
      console.warn(
        '[VoiceService] Pre-acquire mic failed, will retry in produceAudio:',
        errorMessage(err)
      );
      return null;
    }
  }

  /**
   * Resolve the audio stream: use pre-acquired if available and device unchanged,
   * otherwise acquire fresh via getUserMedia.
   */
  private async resolveAudioStream(
    deviceId: string | undefined,
    preAcquiredStream: MediaStream | null | undefined,
    musicMode: boolean,
    audioSettings: {
      echoCancellation: boolean;
      noiseCancellation: boolean;
      autoGainControl: boolean;
    }
  ): Promise<MediaStream> {
    if (preAcquiredStream && !deviceId) return preAcquiredStream;

    // Stop any pre-acquired stream we're not using (device changed)
    if (preAcquiredStream) {
      for (const t of preAcquiredStream.getTracks()) t.stop();
    }

    await this.ensureOsPermission('microphone');

    const useProcessing = !musicMode;
    return navigator.mediaDevices.getUserMedia({
      audio: {
        deviceId: deviceId ? { exact: deviceId } : undefined,
        echoCancellation: useProcessing && audioSettings.echoCancellation,
        noiseSuppression: useProcessing && audioSettings.noiseCancellation,
        autoGainControl: useProcessing && audioSettings.autoGainControl,
        sampleRate: 48000,
        channelCount: 2,
      },
    });
  }

  async produceAudio(deviceId?: string, preAcquiredStream?: MediaStream | null): Promise<void> {
    if (!this.sendTransport || !this.device) return;

    const adv = useAudioSettingsStore.getState();
    const selectedDeviceId = deviceId ?? useVoiceStore.getState().audioInputDeviceId ?? undefined;
    this.localMicStream = await this.resolveAudioStream(
      selectedDeviceId,
      preAcquiredStream,
      adv.musicMode,
      adv
    );
    let track = this.localMicStream.getAudioTracks()[0];

    // Apply noise gate in manual mode
    if (adv.noiseGateMode === 'manual') {
      track = this.applyNoiseGate(this.localMicStream, adv.noiseGateLevel);
    }

    // Apply input volume control (GainNode)
    track = this.applyInputVolume(track, adv.inputVolume);

    const tier = useVoiceStore.getState().effectiveQualityTier;
    const tierConfig = AUDIO_QUALITY_TIERS[tier];
    const { effectiveFec, effectiveDtx, effectiveStereo, effectiveFrameSize } = resolveOpusSettings(
      adv,
      tierConfig
    );
    const audioPrioParams = buildPriorityParams(adv.audioPriority);

    const producer = await this.produceEncrypted(this.sendTransport, {
      track,
      encodings: [
        {
          maxBitrate: tierConfig.maxBitrate,
          adaptivePtime: adv.adaptivePtime || undefined,
          ...audioPrioParams,
        },
      ],
      codecOptions: {
        opusStereo: effectiveStereo,
        opusDtx: effectiveDtx,
        opusFec: effectiveFec,
        opusNack: adv.opusNack,
        opusMaxAverageBitrate: tierConfig.maxBitrate,
        opusMaxPlaybackRate: 48000,
        opusPtime: effectiveFrameSize,
      },
      appData: { source: 'mic' },
    });

    this.producers.set('mic', producer);

    if (this.testSuspensionDepth > 0) {
      producer.pause();
      this.socket?.emit('pause-producer', { producerId: producer.id });
      this.testSuspendedProducerIds.add(producer.id);
      this.testRestoreEligibleProducerIds.add(producer.id);
    }

    // Start client-side VAD for instant speaking indicator
    if (!this.shouldKeepProducerSuspendedForTest(producer.id)) {
      this.startLocalVAD(this.localMicStream);
    }

    // Start packet loss monitor for dynamic FEC
    this.startPacketLossMonitor();

    producer.on('transportclose', () => {
      this.producers.delete('mic');
      this.stopLocalVAD();
      this.stopNoiseGate();
      this.stopInputVolume();
      if (this.localMicStream) {
        for (const t of this.localMicStream.getTracks()) t.stop();
        this.localMicStream = null;
      }
    });
  }

  /**
   * Build a progressive fallback chain of MediaStreamConstraints for camera capture.
   * Relaxes constraints step by step on OverconstrainedError.
   */
  private buildCameraFallbackChain(
    deviceId: string | undefined,
    preset: { width: number; height: number; frameRate: number },
    isSystemDefault: boolean
  ): MediaStreamConstraints[] {
    const idealConstraints = isSystemDefault
      ? {}
      : {
          width: { ideal: preset.width },
          height: { ideal: preset.height },
          frameRate: { ideal: preset.frameRate },
        };

    const chain: MediaStreamConstraints[] = [
      { video: { deviceId: deviceId ? { exact: deviceId } : undefined, ...idealConstraints } },
    ];
    if (deviceId && !isSystemDefault) {
      chain.push({ video: { deviceId: { exact: deviceId } } });
    }
    if (!isSystemDefault) {
      chain.push({ video: idealConstraints });
    }
    chain.push({ video: true });
    return chain;
  }

  /** Try each constraint set in order, relaxing on OverconstrainedError. */
  private async acquireCameraWithFallback(
    fallbackChain: MediaStreamConstraints[],
    shouldContinue: () => boolean = () => true
  ): Promise<MediaStream | null> {
    for (const constraints of fallbackChain) {
      if (!shouldContinue()) return null;
      try {
        return await navigator.mediaDevices.getUserMedia(constraints);
      } catch (err) {
        if (
          err instanceof DOMException &&
          err.name === 'OverconstrainedError' &&
          constraints !== fallbackChain.at(-1)
        ) {
          console.warn('Camera overconstrained, relaxing constraints:', errorMessage(err));
          continue;
        }
        throw err;
      }
    }
    return null;
  }

  /** Map a camera capture error to a user-friendly message. */
  private static mapCameraError(err: unknown): string {
    if (err instanceof DOMException && err.name === 'NotAllowedError') {
      return 'Camera access denied. Check your browser or system permissions.';
    }
    if (err instanceof DOMException && err.name === 'NotFoundError') {
      return 'No camera found. Check that your camera is connected.';
    }
    return 'Could not start camera. Try a different camera or video preset in Settings.';
  }

  /** Produce camera video — single-layer until the media-plane gate enables AV1/VP9 SVC or H264/VP8 simulcast. */
  async produceVideo(deviceId?: string): Promise<void> {
    return this.enqueueVideoReproduce('camera', (token) =>
      this.produceVideoQueued(deviceId, token)
    );
  }

  private async produceVideoQueued(
    deviceId: string | undefined,
    token: VideoReproduceToken,
    replacing = false,
    transport = this.sendTransport
  ): Promise<void> {
    if (!transport || !this.device || !this.isCurrentVideoReproduce(token, transport)) return;
    if (this.producers.has('camera')) return;
    const socket = this.socket;

    // Video slot enforcement
    const voiceState = useVoiceStore.getState();
    const videoOnCount = Object.values(voiceState.participants).filter((p) => p.isVideoOn).length;
    if (!replacing && videoOnCount >= voiceState.maxVideoSlots) {
      voiceState.setVideoSlotError(
        `Maximum video streams reached (${voiceState.maxVideoSlots}). ` +
          'Wait for someone to turn off video or a screen share to end.'
      );
      return;
    }

    let acquiredStream: MediaStream | null = null;
    try {
      await this.ensureOsPermission('camera');
      if (!this.isCurrentVideoReproduce(token, transport)) return;

      const videoSettings = useVideoSettingsStore.getState();
      const preset =
        VIDEO_QUALITY_PRESETS[videoSettings.cameraPreset] || VIDEO_QUALITY_PRESETS['system'];
      const isSystemDefault = videoSettings.cameraPreset === 'system' || preset.width === 0;

      const fallbackChain = this.buildCameraFallbackChain(deviceId, preset, isSystemDefault);
      acquiredStream = await this.acquireCameraWithFallback(fallbackChain, () =>
        this.isCurrentVideoReproduce(token, transport)
      );

      if (!acquiredStream) {
        if (this.isCurrentVideoReproduce(token, transport)) {
          useVoiceStore
            .getState()
            .setVideoSlotError(
              'Could not access camera. Check that your camera is connected and not in use by another application.'
            );
        }
        return;
      }
      if (!this.isCurrentVideoReproduce(token, transport) || this.producers.has('camera')) {
        this.stopMediaStream(acquiredStream);
        return;
      }
      this.localCameraStream = acquiredStream;
      const track = acquiredStream.getVideoTracks()[0];

      const selection = this.pickCameraCodec();
      const codec = this.requireSelectedVideoCodec(selection.codec, 'camera');
      const { encodings } = selection;
      const cameraBitrate = this.cameraStartBitrate(encodings);

      const producer = await this.produceEncrypted(transport, {
        track,
        encodings,
        codec,
        codecOptions: { videoGoogleStartBitrate: this.computeStartBitrate(cameraBitrate) },
        // The track is owned by localCameraStream and reused across codec/layer
        // re-produces (fastReproduceCamera). stopTracks:false keeps producer.close()
        // from stopping it; every teardown path stops localCameraStream explicitly.
        stopTracks: false,
        appData: { source: 'camera' },
      });
      if (!this.commitCameraProducer(producer, acquiredStream, token, transport, socket, codec)) {
        return;
      }
    } catch (err) {
      if (!this.isCurrentVideoReproduce(token, transport)) {
        this.stopMediaStream(acquiredStream);
        return;
      }
      console.error('Failed to start camera:', errorMessage(err));
      this.stopMediaStream(acquiredStream);
      if (this.localCameraStream === acquiredStream) {
        this.localCameraStream = null;
      }
      useVoiceStore.getState().setVideoSlotError(VoiceService.mapCameraError(err));
    }
  }

  private commitCameraProducer(
    producer: mediasoupTypes.Producer,
    stream: MediaStream,
    token: VideoReproduceToken,
    transport: mediasoupTypes.Transport,
    socket: Socket | null,
    codec?: mediasoupTypes.RtpCodecCapability
  ): boolean {
    if (
      !this.isCurrentVideoReproduce(token, transport) ||
      this.localCameraStream !== stream ||
      this.producers.has('camera')
    ) {
      this.discardProducedProducer(producer, socket);
      this.stopMediaStream(stream);
      if (this.localCameraStream === stream) this.localCameraStream = null;
      return false;
    }

    this.applyDegradationPreference(producer);
    this.producers.set('camera', producer);

    // Start the session-stats monitor from the video path too, so the WebRTC HW
    // B-signal is learned in no-mic (video/screen-only) sessions (#2187 item 1). Idempotent.
    this.startPacketLossMonitor();

    useVoiceStore
      .getState()
      .setActiveCameraCodec(
        this.getProducerCodecMimeType('camera') ??
          (codec ? this.codecKeyFromParameters(codec) : null)
      );

    const store = useVoiceStore.getState();
    store.setVideoOn(true);
    const localUserId = useUserStore.getState().user?.id;
    if (localUserId && this.localCameraStream) {
      store.updateParticipant(localUserId, {
        videoStream: this.localCameraStream,
        isVideoOn: true,
      });
    }

    producer.on('transportclose', () => this.handleCameraProducerTransportClose(producer));
    return true;
  }

  private handleCameraProducerTransportClose(producer: mediasoupTypes.Producer): void {
    if (this.producers.get('camera') !== producer) return;
    this.producers.delete('camera');
    if (this.localCameraStream) {
      for (const track of this.localCameraStream.getTracks()) track.stop();
      this.localCameraStream = null;
    }
    const store = useVoiceStore.getState();
    store.setActiveCameraCodec(null);
    store.setVideoOn(false);
    const localUserId = useUserStore.getState().user?.id;
    if (localUserId) {
      store.updateParticipant(localUserId, { videoStream: undefined, isVideoOn: false });
    }
  }

  /**
   * Resolve a screen-share resolution selection to capture dimensions (#2163).
   * 'source'/native resolves to the ACTUAL best-display size via getDisplayInfo so
   * the entitlement clamp tiers fps against the REAL resolution — a free 720p Native
   * share keeps 60fps — instead of a hard-coded 4K fallback that would over-clamp it
   * to 1080p30 (Codex review #2172). Mirrors the Settings UI, which resolves 'source'
   * via getDisplayInfo. Fixed presets and WxH parse via the shared resolveScreenDims;
   * falls back to 4K when getDisplayInfo is unavailable (dev/web) — a safe over-clamp.
   *
   * Uses the largest-area display, NOT the specific shared `sourceId` (Gitar review
   * #2172): on a multi-monitor setup, sharing a smaller secondary is conservatively
   * over-clamped to the largest display's tier. This is intentional — it stays in
   * lockstep with the Settings UI's `bestDisplay`, so the offered fps options and the
   * actual capture never disagree. Per-sourceId fidelity is a deferred follow-up.
   */
  private async resolveCaptureDims(resolution: string): Promise<{ w: number; h: number }> {
    let sourceDims = { w: 3840, h: 2160 };
    if (resolution === 'source') {
      try {
        const displays = (await globalThis.electron?.getDisplayInfo?.()) ?? [];
        if (displays.length > 0) {
          // Seed reduce with displays[0]; never reduce an array without an initial
          // value (throws on empty, and keeps the SonarQube reduce-init gate clean).
          const best = displays.reduce(
            (a, d) => (d.width * d.height > a.width * a.height ? d : a),
            displays[0]
          );
          // Guard against a 0-sized / malformed display report (Gitar review #2172):
          // a {0,0} source would feed resolveScreenDims → clampScreenToEntitlement a
          // 0-pixel target, which trivially admits any fps. Fall back to 4K instead.
          if (best.width > 0 && best.height > 0) {
            sourceDims = { w: best.width, h: best.height };
          }
        }
      } catch (err) {
        // A rejected getDisplayInfo IPC must degrade to the conservative 4K
        // fallback, NOT propagate out of produceScreen (Gitar review #2172): this
        // call runs before the capture try block, so an unguarded rejection would
        // break screen sharing entirely for ALL tiers, including premium.
        console.debug('resolveCaptureDims: getDisplayInfo unavailable, using 4K fallback', err);
      }
    }
    return resolveScreenDims(resolution, sourceDims);
  }

  /**
   * Capture screen via Electron desktopCapturer (with audio fallback) or getDisplayMedia.
   * Throws on permission denial or no sources.
   */
  private async captureScreen(
    sourceId: string | undefined,
    screenRes: { w: number; h: number },
    screenFps: number
  ): Promise<MediaStream> {
    if (typeof globalThis.electron?.getDesktopSources === 'function') {
      return this.captureScreenElectron(sourceId, screenRes, screenFps);
    }
    console.debug('produceScreen: using getDisplayMedia fallback');
    // Non-Electron path only (dev/web) — packaged builds always take the
    // getDesktopSources branch above. getDisplayMedia is the W3C OS-mediated
    // picker, so `audio: true` captures only the user-selected surface's audio
    // with explicit consent — NOT chromeMediaSource:'desktop' whole-desktop
    // loopback, so this is not the #2161 leak path.
    return navigator.mediaDevices.getDisplayMedia({
      video: {
        width: { ideal: screenRes.w },
        height: { ideal: screenRes.h },
        frameRate: { ideal: screenFps },
      },
      audio: true,
    });
  }

  /** Electron desktopCapturer path — tries video+audio, falls back to video-only. */
  private async captureScreenElectron(
    sourceId: string | undefined,
    screenRes: { w: number; h: number },
    screenFps: number
  ): Promise<MediaStream> {
    const electron = globalThis.electron;
    if (!electron) throw new Error('captureScreenElectron called without Electron bridge');

    let chosenId = sourceId;
    if (!chosenId) {
      const sources = await electron.getDesktopSources();
      if (sources.length === 0) throw new Error('No screen sources available');
      chosenId = sources.find((s) => s.id.startsWith('screen:'))?.id || sources[0].id;
    }
    console.debug('produceScreen: capturing desktop source', chosenId);

    const videoConstraints = {
      mandatory: {
        chromeMediaSource: 'desktop',
        chromeMediaSourceId: chosenId,
        maxWidth: screenRes.w,
        maxHeight: screenRes.h,
        maxFrameRate: screenFps,
      },
    } as unknown as MediaTrackConstraints;

    // System loopback audio (chromeMediaSource: 'desktop') captures the ENTIRE
    // desktop and ignores chromeMediaSourceId — Electron has no per-window
    // loopback. Request it only for entire-screen ('screen:') shares; a
    // window/app ('window:') share must be video-only or it leaks all system
    // audio to the channel (#2161).
    if (!chosenId.startsWith('screen:')) {
      return navigator.mediaDevices.getUserMedia({ audio: false, video: videoConstraints });
    }

    try {
      return await navigator.mediaDevices.getUserMedia({
        audio: {
          mandatory: { chromeMediaSource: 'desktop', chromeMediaSourceId: chosenId },
        } as unknown as MediaTrackConstraints,
        video: videoConstraints,
      });
    } catch (audioErr) {
      console.debug(
        'produceScreen: audio capture unavailable, falling back to video-only',
        audioErr
      );
      return navigator.mediaDevices.getUserMedia({ audio: false, video: videoConstraints });
    }
  }

  /** Produce screen audio from the capture stream if audio tracks are available. */
  private async produceScreenAudioFromStream(stream: MediaStream): Promise<void> {
    const audioTracks = stream.getAudioTracks();
    if (audioTracks.length === 0 || !this.sendTransport) return;

    try {
      const audioProducer = await this.produceEncrypted(this.sendTransport, {
        track: audioTracks[0],
        codecOptions: { opusStereo: true, opusDtx: false },
        // Track owned by localScreenStream and reused across re-produces
        // (reProduceScreenAudio); stopTracks:false keeps close() from stopping it.
        stopTracks: false,
        appData: { source: 'screen-audio' },
      });

      this.producers.set('screen-audio', audioProducer);

      audioProducer.on('transportclose', () => {
        this.producers.delete('screen-audio');
      });

      console.debug('produceScreen: screen audio producer created', audioProducer.id);
    } catch (err) {
      console.warn('Failed to produce screen audio:', errorMessage(err));
    }
  }

  /** Produce screen share — single-layer, codec-floor compatible publishing for opt-in viewing. */
  async produceScreen(sourceId?: string, options?: ScreenShareOptions): Promise<void> {
    if (!this.sendTransport || !this.device) {
      console.warn('produceScreen: no sendTransport or device — cannot share screen');
      return;
    }

    const screenSettings = useVideoSettingsStore.getState();
    const resolution = options?.resolution ?? screenSettings.screenResolution;
    const frameRate = options?.frameRate ?? screenSettings.screenFrameRate;
    const contentType = options?.contentType ?? screenSettings.screenContentType;
    const screenFps = frameRate === 0 ? 60 : frameRate;
    // #2163: resolve 'source'/native to the REAL display dims (not a 4K fallback),
    // then clamp res/fps to the free stream entitlement (tiered pixel-rate) so a
    // sub-1080p Native share keeps its correct fps tier. Premium/native + pre-hydrate
    // are structural no-ops.
    const rawRes = await this.resolveCaptureDims(resolution);
    const clampedCap = this.clampScreenToEntitlement(rawRes.w, rawRes.h, screenFps);
    const screenRes = { w: clampedCap.width, h: clampedCap.height };

    let stream: MediaStream;
    try {
      stream = await this.captureScreen(sourceId, screenRes, clampedCap.fps);
    } catch (captureErr) {
      if (handleScreenCaptureNotAllowed(captureErr)) return;
      throw captureErr;
    }

    const videoTracks = stream.getVideoTracks();
    if (videoTracks.length === 0) {
      for (const t of stream.getTracks()) t.stop();
      throw new Error('Screen capture returned no video tracks');
    }

    const track = videoTracks[0];
    if (contentType === 'motion') track.contentHint = 'motion';
    else if (contentType === 'detail') track.contentHint = 'detail';

    this.localScreenStream = stream;

    try {
      const selection = this.pickScreenCodec();
      const codec = this.requireSelectedVideoCodec(selection.codec, 'screen');
      const { encodings, effectiveBitrate: screenBitrate } = selection;

      const producer = await this.produceEncrypted(this.sendTransport, {
        track,
        encodings,
        codec,
        codecOptions: { videoGoogleStartBitrate: this.computeStartBitrate(screenBitrate) },
        // Track owned by localScreenStream and reused across codec re-produces
        // (fastReproduceScreen); stopTracks:false keeps close() from stopping it.
        stopTracks: false,
        appData: { source: 'screen' },
      });

      this.applyDegradationPreference(producer);
      this.producers.set('screen', producer);

      // #2187 item 1: idempotent — learns the B-signal in screen-only (no-mic) sessions.
      this.startPacketLossMonitor();

      useVoiceStore
        .getState()
        .setActiveScreenCodec(
          this.getProducerCodecMimeType('screen') ?? this.codecKeyFromParameters(codec)
        );

      updateStoreForScreenShare(producer.id, this.localScreenStream);

      await this.produceScreenAudioFromStream(stream);

      track.onended = () => {
        this.closeProducer('screen');
      };

      producer.on('transportclose', () => {
        this.producers.delete('screen');
        if (this.localScreenStream) {
          for (const t of this.localScreenStream.getTracks()) t.stop();
          this.localScreenStream = null;
        }
        const s = useVoiceStore.getState();
        s.setActiveScreenCodec(null);
        s.setScreenSharing(false);
        const uid = useUserStore.getState().user?.id;
        if (uid) s.updateParticipant(uid, { screenStream: undefined, isScreenSharing: false });
      });
    } catch (err) {
      await this.cleanupScreenState();
      throw err;
    }
  }

  // ─── Producer Controls ─────────────────────────────────────────────

  /**
   * Optimistically apply a partial state change to the local user in BOTH the
   * participant map and the active-channel sidebar member list, so the UI updates
   * immediately without waiting for the SFU echo. Single source of the update
   * plumbing shared by the mute and deafen helpers (#685).
   */
  private applyOptimisticLocalState(
    store: ReturnType<typeof useVoiceStore.getState>,
    update: { isMuted?: boolean; isDeafened?: boolean },
    label: string
  ): void {
    const localUserId = useUserStore.getState().user?.id;
    if (!localUserId) {
      console.warn(`[VoiceService] ${label}: no local user ID — sidebar state may be stale`);
      return;
    }
    store.updateParticipant(localUserId, update);
    if (store.activeChannelId) {
      store.updateChannelVoiceMember(store.activeChannelId, localUserId, update);
    }
  }

  private applyOptimisticMute(
    store: ReturnType<typeof useVoiceStore.getState>,
    muted: boolean
  ): void {
    this.applyOptimisticLocalState(store, { isMuted: muted }, 'applyOptimisticMute');
  }

  /**
   * Optimistically reflect the local user's self-deafen in both the participant
   * map and the channel-sidebar member list (#685) — symmetric with
   * applyOptimisticMute, so the sidebar updates immediately without the SFU echo.
   */
  private applyOptimisticDeafen(
    store: ReturnType<typeof useVoiceStore.getState>,
    deafened: boolean
  ): void {
    this.applyOptimisticLocalState(store, { isDeafened: deafened }, 'applyOptimisticDeafen');
  }

  private getTestSuspensionRestorePolicy(): TestSuspensionRestorePolicy {
    const store = useVoiceStore.getState();
    const localUserId = useUserStore.getState().user?.id;
    const localParticipant = localUserId ? store.participants[localUserId] : undefined;

    return {
      keepAudioOutPaused: store.isDeafened || localParticipant?.serverDeafened === true,
      keepProducersPaused: store.isSoloBandwidthSaving || localParticipant?.serverDeafened === true,
      keepMicPaused: store.isMuted || localParticipant?.serverMuted === true,
    };
  }

  private finishTestSuspension(): boolean {
    if (this.testSuspensionDepth === 0) return false;
    this.testSuspensionDepth--;
    return this.testSuspensionDepth === 0;
  }

  private clearTestSuspensionState(): void {
    this.testSuspendedProducerIds.clear();
    this.testSuspendedConsumerIds.clear();
    this.testRestoreEligibleProducerIds.clear();
    this.testRestoreEligibleConsumerIds.clear();
    this.testServerPausedConsumerIds.clear();
  }

  private shouldKeepProducerSuspendedForTest(producerId: string): boolean {
    return this.testSuspensionDepth > 0 && this.testSuspendedProducerIds.has(producerId);
  }

  private shouldKeepConsumerSuspendedForTest(consumerId: string): boolean {
    return this.testSuspensionDepth > 0 && this.testSuspendedConsumerIds.has(consumerId);
  }

  private shouldRestoreProducerPausedBeforeTest(
    source: string,
    policy: TestSuspensionRestorePolicy
  ): boolean {
    return source === 'mic' && (policy.keepProducersPaused || policy.keepMicPaused);
  }

  private hasCoordinatedConsumerPause(consumerId: string): boolean {
    return (['visibility', 'ignis', 'manual'] as const).some((reason) =>
      this.pauseCoordinator.hasReason(consumerId, reason)
    );
  }

  private shouldRestoreConsumerPausedBeforeTest(
    consumerId: string,
    policy: TestSuspensionRestorePolicy
  ): boolean {
    return policy.keepAudioOutPaused && !this.hasCoordinatedConsumerPause(consumerId);
  }

  private suspendExistingAudioProducerForTest(
    source: string,
    producer: mediasoupTypes.Producer,
    policy: TestSuspensionRestorePolicy
  ): void {
    if (source !== 'mic' || producer.kind !== 'audio') return;
    this.testSuspendedProducerIds.add(producer.id);
    if (producer.paused) {
      if (this.shouldRestoreProducerPausedBeforeTest(source, policy)) {
        this.testRestoreEligibleProducerIds.add(producer.id);
      }
      return;
    }
    producer.pause();
    this.socket?.emit('pause-producer', { producerId: producer.id });
    this.testRestoreEligibleProducerIds.add(producer.id);
  }

  private suspendExistingAudioConsumerForTest(
    consumerId: string,
    consumer: mediasoupTypes.Consumer,
    policy: TestSuspensionRestorePolicy
  ): void {
    if (consumer.kind !== 'audio') return;
    this.testSuspendedConsumerIds.add(consumerId);
    if (consumer.paused) {
      if (this.shouldRestoreConsumerPausedBeforeTest(consumerId, policy)) {
        this.testRestoreEligibleConsumerIds.add(consumerId);
      }
      return;
    }
    consumer.pause();
    this.testRestoreEligibleConsumerIds.add(consumerId);
  }

  private restoreTestSuspendedProducer(
    source: string,
    producer: mediasoupTypes.Producer,
    policy: TestSuspensionRestorePolicy
  ): void {
    if (!this.testSuspendedProducerIds.has(producer.id)) return;
    if (!this.testRestoreEligibleProducerIds.has(producer.id)) return;
    if (!producer.paused || producer.closed) return;
    if (policy.keepProducersPaused) return;
    if (source === 'mic' && policy.keepMicPaused) return;

    producer.resume();
    this.socket?.emit('resume-producer', { producerId: producer.id });
  }

  private restoreTestSuspendedConsumer(
    consumerId: string,
    consumer: mediasoupTypes.Consumer,
    policy: TestSuspensionRestorePolicy
  ): void {
    if (!this.testSuspendedConsumerIds.has(consumerId) || consumer.closed) return;
    if (!this.testRestoreEligibleConsumerIds.has(consumerId)) return;
    if (policy.keepAudioOutPaused) {
      if (this.testServerPausedConsumerIds.has(consumerId)) {
        this.serverResumeOnUndeafenConsumerIds.add(consumerId);
      }
      return;
    }

    if (this.testServerPausedConsumerIds.has(consumerId)) {
      this.socket?.emit('resume-consumer', { consumerId });
      this.serverResumeOnUndeafenConsumerIds.delete(consumerId);
    }
    if (consumer.paused) consumer.resume();
  }

  beginTestSuspension(): void {
    this.testSuspensionDepth++;
    if (this.testSuspensionDepth > 1) return;

    const policy = this.getTestSuspensionRestorePolicy();
    this.testSuspendedProducerIds.clear();
    this.testSuspendedConsumerIds.clear();
    this.testRestoreEligibleProducerIds.clear();
    this.testRestoreEligibleConsumerIds.clear();
    this.testServerPausedConsumerIds.clear();

    for (const [source, producer] of this.producers) {
      this.suspendExistingAudioProducerForTest(source, producer, policy);
    }

    for (const [consumerId, consumer] of this.consumers) {
      this.suspendExistingAudioConsumerForTest(consumerId, consumer, policy);
    }
  }

  endTestSuspension(): void {
    if (!this.finishTestSuspension()) return;

    const policy = this.getTestSuspensionRestorePolicy();
    for (const [source, producer] of this.producers) {
      this.restoreTestSuspendedProducer(source, producer, policy);
    }

    for (const [consumerId, consumer] of this.consumers) {
      this.restoreTestSuspendedConsumer(consumerId, consumer, policy);
    }

    this.clearTestSuspensionState();
  }

  setLocalTestingStatus(isTesting: boolean): void {
    const store = useVoiceStore.getState();
    store.setLocalIsTesting(isTesting);
    const localUserId = useUserStore.getState().user?.id;
    if (localUserId) store.updateParticipant(localUserId, { isTesting });
    if (store.activeChannelId) this.socket?.emit('update-test-status', { isTesting });
  }

  private canUnmuteMic(store: ReturnType<typeof useVoiceStore.getState>): boolean {
    const localUserId = useUserStore.getState().user?.id;
    return !localUserId || store.participants[localUserId]?.serverMuted !== true;
  }

  private async unmuteMicProducer(
    store: ReturnType<typeof useVoiceStore.getState>,
    producer: mediasoupTypes.Producer
  ): Promise<void> {
    if (!this.canUnmuteMic(store)) return;

    const keepSuspended = this.shouldKeepProducerSuspendedForTest(producer.id);
    if (!keepSuspended) {
      await producer.resume();
      this.socket?.emit('resume-producer', { producerId: producer.id });
    }

    store.setMuted(false);
    this.applyOptimisticMute(store, false);
    notificationSoundService.play('unmute');
    if (this.localMicStream && !keepSuspended) this.startLocalVAD(this.localMicStream);
  }

  private async muteMicProducer(
    store: ReturnType<typeof useVoiceStore.getState>,
    producer: mediasoupTypes.Producer
  ): Promise<void> {
    await producer.pause();
    this.socket?.emit('pause-producer', { producerId: producer.id });
    store.setMuted(true);
    this.applyOptimisticMute(store, true);
    notificationSoundService.play('mute');
    this.stopLocalVAD();
  }

  private async revertMicProducerState(
    producer: mediasoupTypes.Producer,
    wasMuted: boolean
  ): Promise<void> {
    try {
      if (wasMuted) {
        await producer.pause();
      } else {
        await producer.resume();
      }
    } catch {
      // Best-effort producer revert — UI state is already rolled back.
    }
  }

  /** Toggle mute (pause/resume mic producer) */
  async toggleMute(): Promise<void> {
    const store = useVoiceStore.getState();
    const producer = this.producers.get('mic');
    if (!producer) return;

    const wasMuted = store.isMuted;
    try {
      if (wasMuted) {
        await this.unmuteMicProducer(store, producer);
      } else {
        await this.muteMicProducer(store, producer);
      }
    } catch (error) {
      console.error('[VoiceService] toggleMute failed:', errorMessage(error));
      store.setMuted(wasMuted);
      this.applyOptimisticMute(store, wasMuted);
      await this.revertMicProducerState(producer, wasMuted);
    }
  }

  /** Returns true if the local user is currently server-deafened and cannot undeafen. */
  private isServerDeafenBlocked(): boolean {
    const store = useVoiceStore.getState();
    if (!store.isDeafened) return false;
    const localUserId = useUserStore.getState().user?.id;
    if (!localUserId) return false;
    return store.participants[localUserId]?.serverDeafened === true;
  }

  private resumeServerIfHeldForAudioOutput(consumerId: string): void {
    if (!this.serverResumeOnUndeafenConsumerIds.delete(consumerId)) return;
    this.socket?.emit('resume-consumer', { consumerId });
  }

  /** Toggle deafen (mute all incoming audio) */
  toggleDeafen(): void {
    const store = useVoiceStore.getState();

    // Check server-deafen enforcement — cannot undeafen while server-deafened
    if (this.isServerDeafenBlocked()) return;

    const newDeafened = !store.isDeafened;
    store.setDeafened(newDeafened);
    this.applyOptimisticDeafen(store, newDeafened);
    // Broadcast self-deafen to the room so other participants' sidebars update in
    // real time (#685) — mirrors self-mute's pause-producer → producer-paused path.
    this.socket?.emit('set-deafen', { isDeafened: newDeafened });
    notificationSoundService.play(newDeafened ? 'deafen' : 'undeafen');

    // Mute/unmute all audio consumers
    for (const [consumerId, consumer] of this.consumers) {
      if (consumer.kind === 'audio') {
        if (newDeafened) {
          consumer.pause();
        } else if (!this.shouldKeepConsumerSuspendedForTest(consumerId)) {
          // #2162: don't locally resume a screenshare consumer the viewer has muted;
          // the coordinator owns its paused state (server forwarding is already stopped).
          if (this.pauseCoordinator.hasReason(consumerId, 'stream-mute')) continue;
          this.resumeServerIfHeldForAudioOutput(consumerId);
          consumer.resume();
        }
      }
    }

    // Also mute self when deafened
    if (newDeafened && !store.isMuted) {
      this.toggleMute();
    }
  }

  /** Toggle video (start/stop camera) */
  async toggleVideo(): Promise<void> {
    const store = useVoiceStore.getState();
    if (store.isVideoOn) {
      await this.closeProducer('camera');
      notificationSoundService.play('video-off');
    } else {
      await this.produceVideo(store.videoDeviceId || undefined);
      notificationSoundService.play('video-on');
    }
  }

  /** Toggle screen share. Pass sourceId and options from ScreenSharePicker for Electron. */
  async toggleScreenShare(sourceId?: string, options?: ScreenShareOptions): Promise<void> {
    const store = useVoiceStore.getState();
    console.debug('toggleScreenShare:', { isScreenSharing: store.isScreenSharing, sourceId });
    if (store.isScreenSharing) {
      await this.closeProducer('screen');
      notificationSoundService.play('screen-off');
    } else {
      await this.produceScreen(sourceId, options);
      notificationSoundService.play('screen-on');
    }
  }

  /** Pause a local producer by source name (e.g. 'screen', 'camera', 'mic') */
  pauseLocalProducer(source: string): void {
    const producer = this.producers.get(source);
    if (!producer || producer.paused) return;
    producer.pause();
    this.socket?.emit('pause-producer', { producerId: producer.id });
  }

  /** Resume a local producer by source name */
  resumeLocalProducer(source: string): void {
    const producer = this.producers.get(source);
    if (!producer?.paused) return;
    producer.resume();
    this.socket?.emit('resume-producer', { producerId: producer.id });
  }

  // ─── Solo Bandwidth Saving ──────────────────────────────────────────

  /**
   * Check participant count and enter/exit solo bandwidth saving mode.
   * When alone in a call, pauses all producers to save bandwidth.
   * Resumes automatically when someone joins.
   */
  private checkSoloBandwidthSaving(): void {
    const store = useVoiceStore.getState();
    const participantCount = Object.keys(store.participants).length;
    const wasSolo = store.isSoloBandwidthSaving;

    if (participantCount <= 1 && !wasSolo) {
      this.enterSoloBandwidthSaving();
    } else if (participantCount > 1 && wasSolo) {
      this.exitSoloBandwidthSaving();
    }
  }

  private enterSoloBandwidthSaving(): void {
    const store = useVoiceStore.getState();
    store.setSoloBandwidthSaving(true);

    // Pause all producers (stops sending data to server)
    // But do NOT stop local MediaStream tracks (keeps local preview/VAD)
    for (const [, producer] of this.producers) {
      if (!producer.paused) {
        producer.pause();
        this.socket?.emit('pause-producer', { producerId: producer.id });
      }
    }

    // Start 60-second timer for user notification
    this.soloNotificationTimer = setTimeout(() => {
      useVoiceStore.getState().setSoloBandwidthNotification(true);
    }, 60_000);
  }

  private exitSoloBandwidthSaving(): void {
    const store = useVoiceStore.getState();
    store.setSoloBandwidthSaving(false);
    store.setSoloBandwidthNotification(false);

    // Clear notification timer
    if (this.soloNotificationTimer) {
      clearTimeout(this.soloNotificationTimer);
      this.soloNotificationTimer = null;
    }

    // Resume all producers (start sending again)
    // Respect current mute state — don't resume mic if user is muted
    for (const [source, producer] of this.producers) {
      if (source === 'mic' && store.isMuted) continue; // stay paused if intentionally muted
      if (producer.paused) {
        producer.resume();
        this.socket?.emit('resume-producer', { producerId: producer.id });
      }
    }
  }

  /** Opt-in to consume a remote screen share ("Tune In" model) — up to 5 concurrent */
  async tuneInToScreenShare(producerId: string, userId: string): Promise<void> {
    const store = useVoiceStore.getState();
    // Bind to the call we start in (#2088): the store mutations after the
    // awaits below must not land on a store that leaveChannel()/reconnect has
    // reset out from under us (see the stale-context guard before store.tuneIn).
    const startChannelId = store.activeChannelId;

    // Idempotency + in-flight guard (#2088): the ShareTunePill, the Tune In
    // Everywhere control, and the
    // auto-tune engine can race on the same producer; a second consume would
    // orphan a duplicate SFU consumer that keeps forwarding RTP.
    if (producerId in store.tunedInScreenShares || this.tuneInsInFlight.has(producerId)) {
      return;
    }
    this.tuneInsInFlight.add(producerId);
    try {
      // Manual re-tune clears any auto-tune suppression for this producer
      // (spec §3.3; no-op for the auto path, which pre-filters suppressed ids).
      store.clearAutoTuneSuppression(producerId);

      // Reserve a slot against the cap synchronously (#2088). Auto-tune fires
      // one tuneInToScreenShare per new-producer announce, so a burst races
      // through here; counting only committed tune-ins lets them all pass this
      // guard before any of them reaches store.tuneIn(), overshooting the cap
      // and dragging maxVideoSlots below budget. Count committed tune-ins AND
      // in-flight reservations (this producer is already in tuneInsInFlight);
      // the Set de-dupes the brief window where a producer is both tuned-in and
      // still in-flight (the paired-audio await after store.tuneIn()).
      const reservedCount = new Set<string>([
        ...Object.keys(store.tunedInScreenShares),
        ...this.tuneInsInFlight,
      ]).size;
      if (reservedCount > MAX_TUNED_SCREEN_SHARES) {
        store.setVideoSlotError('Maximum 5 screen shares reached. Tune out of one first.');
        return;
      }

      // Ensure decrypt key is ready for E2EE screen share
      if (this.mediaEncryption && startChannelId) {
        await this.addDecryptKeyForUser(startChannelId, userId);
      }

      // Remove from available list and consume the producer
      store.removeAvailableScreenShare(producerId);
      await this.consumeProducer(producerId, userId, 'video');

      // Find the consumer that was just created for this producer
      let consumerId = '';
      for (const [cid, consumer] of this.consumers) {
        if (consumer.producerId === producerId) {
          consumerId = cid;
          break;
        }
      }

      // Stale-context guard (#2088): leaveChannel()/reconnect can reset the store
      // and tear down transports while we await key setup + consume. Recording
      // tune-in state now would re-add stale tunedInScreenShares and lower
      // maxVideoSlots for a call the user already left. Close the consumer we
      // just created (idempotent — no-op if teardown already cleared it) so the
      // SFU stops forwarding, then bail before touching the store.
      if (useVoiceStore.getState().activeChannelId !== startChannelId) {
        if (consumerId) this.closeConsumerAndNotify(consumerId);
        return;
      }

      // Track in store
      store.tuneIn(producerId, consumerId);

      // Set as dominant if first tuned-in share
      if (!useVoiceStore.getState().dominantScreenShareId) {
        store.setDominantScreenShare(producerId);
      }

      store.updateParticipant(userId, { isScreenSharing: true });

      // Consume paired screen audio if available (keep mapping for re-tune)
      const audioProducerId = this.pendingScreenAudioProducers.get(userId);
      if (audioProducerId) {
        await this.consumeProducer(audioProducerId, userId, 'audio');
      }
    } finally {
      this.tuneInsInFlight.delete(producerId);
    }
  }

  /** Opt-out of a tuned-in screen share. `suppressAutoTune` marks a MANUAL
   *  tune-out so the auto-tune engine (#2088) won't immediately re-consume. */
  async tuneOutOfScreenShare(
    producerId: string,
    opts?: { suppressAutoTune?: boolean }
  ): Promise<void> {
    const store = useVoiceStore.getState();
    const consumerId = store.tunedInScreenShares[producerId];
    const shareMeta = store.activeScreenShares[producerId];
    const isLocal = shareMeta?.isLocal === true || consumerId === LOCAL_SCREEN_CONSUMER_ID;

    if (!isLocal) {
      // Identify the producing user: metadata seam first, consumerMeta fallback
      const videoMeta = consumerId ? this.consumerMeta.get(consumerId) : undefined;
      const screenOwnerUserId = shareMeta?.userId ?? videoMeta?.producerUserId;

      // Close the screen video consumer locally AND free the SFU side (#2088 fix)
      if (consumerId) {
        this.closeConsumerAndNotify(consumerId);
      }

      // Close the paired screen-audio consumer for this specific user
      if (screenOwnerUserId) {
        this.closeScreenAudioConsumerForUser(screenOwnerUserId, store);
      }

      // Add back to available shares (remote shares only — never offer the
      // user a Tune In to their own stream)
      const producerUserId = screenOwnerUserId ?? this.findScreenShareOwner(store);
      if (producerUserId) {
        const participant = store.participants[producerUserId];
        store.addAvailableScreenShare({
          producerId,
          userId: producerUserId,
          username: shareMeta?.username ?? participant?.username ?? 'Unknown',
          displayName: shareMeta?.displayName ?? participant?.displayName,
        });
      }

      // Manual tune-out while auto-tune is ON → suppress auto re-tune for this
      // producer lifetime (cleared on producer close / manual re-tune / rejoin)
      if (opts?.suppressAutoTune && useVideoSettingsStore.getState().autoTuneInScreenShares) {
        store.suppressAutoTune(producerId);
      }
    }

    // Remove from tuned-in (handles dominant swap & slot recalculation).
    // For the LOCAL share this only hides the preview — production continues.
    store.tuneOut(producerId);
  }

  /** Auto-tune engine (#2088): consume every available, unsuppressed remote
   *  screen share while the autoTuneInScreenShares setting is ON. Reuses the
   *  manual tune-in path so E2EE key setup, paired audio, dominant selection,
   *  and the cap guard (incl. the existing videoSlotError copy) stay identical. */
  private async autoTuneSweep(): Promise<void> {
    if (!useVideoSettingsStore.getState().autoTuneInScreenShares) return;
    // Bind the sweep to the call it started in (#2088). The join trigger fires
    // it fire-and-forget, so capture the channel now and re-check it each
    // iteration: if the user leaves or switches calls mid-sweep we stop instead
    // of consuming shares (and lowering maxVideoSlots) for a room they left.
    const startChannelId = useVoiceStore.getState().activeChannelId;
    const shares = [...useVoiceStore.getState().availableScreenShares]; // snapshot
    for (const share of shares) {
      // Re-read the live preference + call context every iteration (#2088): the
      // user can flip auto-tune OFF or leave the call while an earlier share's
      // E2EE/consume round trip is still pending. Checking only once before the
      // loop would keep auto-consuming the original snapshot after the toggle is
      // off or the call is gone.
      if (!useVideoSettingsStore.getState().autoTuneInScreenShares) return;
      if (useVoiceStore.getState().activeChannelId !== startChannelId) return;
      if (useVoiceStore.getState().autoTuneSuppressedProducers[share.producerId]) continue;
      // Isolate per-share failures (#2088): a rejected E2EE key fetch or consume
      // for one share must neither abort the remaining shares nor escape the
      // fire-and-forget `void autoTuneSweep()` call sites as an unhandled
      // rejection. Mirrors the try/catch on the manual pill/Tune-Everywhere handlers.
      try {
        await this.tuneInToScreenShare(share.producerId, share.userId);
      } catch (err) {
        console.error('auto-tune sweep failed for', share.producerId, errorMessage(err));
      }
    }
  }

  /** Tune in to every currently available screen share, up to the cap.
   *  Explicit user intent — clears any auto-tune suppressions on the way. */
  async tuneInAllScreenShares(): Promise<void> {
    const store = useVoiceStore.getState();
    const shares = [...store.availableScreenShares]; // snapshot
    for (const share of shares) {
      store.clearAutoTuneSuppression(share.producerId);
      // Sequential on purpose: the cap guard inside tuneInToScreenShare reads
      // live state; parallel awaits could race past the limit. Over-cap calls
      // no-op and set the existing videoSlotError copy.
      await this.tuneInToScreenShare(share.producerId, share.userId);
    }
  }

  /** Tune out of every tuned-in REMOTE screen share. Snapshots ids first so
   *  mid-iteration store mutations can't skip entries. Never touches the
   *  local user's own share (the local-screen sentinel). */
  async tuneOutAllScreenShares(): Promise<void> {
    const store = useVoiceStore.getState();
    const producerIds = Object.entries(store.tunedInScreenShares)
      .filter(([, consumerId]) => consumerId !== LOCAL_SCREEN_CONSUMER_ID)
      .map(([producerId]) => producerId); // snapshot
    for (const producerId of producerIds) {
      await this.tuneOutOfScreenShare(producerId, { suppressAutoTune: true });
    }
  }

  /** Close a consumer locally (maps + pause/layer bookkeeping) and ask the SFU
   *  to close its side. Client-initiated closes ONLY — server-initiated close
   *  paths (producer-closed / consumer-closed / transportclose) must NOT emit
   *  back at the server. No-ops for unknown ids and the local-screen sentinel. */
  private closeConsumerAndNotify(consumerId: string): void {
    const consumer = this.consumers.get(consumerId);
    if (!consumer) return;
    consumer.close();
    this.consumers.delete(consumerId);
    this.decoderBudgetSampler.deleteConsumer(consumerId);
    this.consumerMeta.delete(consumerId);
    this.lastPreferredLayerKeyByConsumer.delete(consumerId);
    this.pauseCoordinator.clearConsumer(consumerId);
    this.socket?.emit('close-consumer', { consumerId });
  }

  /** Close the screen-audio consumer for a specific user. */
  private closeScreenAudioConsumerForUser(
    userId: string,
    store: ReturnType<typeof useVoiceStore.getState>
  ): void {
    for (const [cid, meta] of this.consumerMeta) {
      if (meta.source === 'screen-audio' && meta.producerUserId === userId) {
        this.closeConsumerAndNotify(cid);
        store.updateParticipant(userId, { screenAudioStream: undefined });
        break;
      }
    }
  }

  /** Find the user who owns an active screen share. */
  private findScreenShareOwner(
    store: ReturnType<typeof useVoiceStore.getState>
  ): string | undefined {
    for (const [uid, p] of Object.entries(store.participants)) {
      if (p.screenStream) return uid;
    }
    return undefined;
  }

  /** Pause a consumer (egress + local) for an explicit external reason (e.g. PiP ownership). */
  pauseConsumer(consumerId: string): void {
    this.pauseCoordinator.addReason(consumerId, 'manual');
  }

  /** Release an explicit external pause; the consumer resumes unless another reason holds. */
  resumeConsumer(consumerId: string): void {
    this.pauseCoordinator.removeReason(consumerId, 'manual');
  }

  // ─── Per-stream screenshare audio mute (#2162) ─────────────────────

  /**
   * Resolve the live `screen-audio` consumer for a sharer via consumerMeta.
   * Returns undefined when the sharer has no active screen audio (intent still holds).
   */
  private screenAudioConsumerIdFor(sharerUserId: string): string | undefined {
    for (const [cid, meta] of this.consumerMeta) {
      if (meta.source === 'screen-audio' && meta.producerUserId === sharerUserId) return cid;
    }
    return undefined;
  }

  /** Mute a sharer's screenshare audio: pause the server-side screen-audio consumer. */
  muteScreenShare(sharerUserId: string): void {
    useVoiceStore.getState().setScreenShareMuted(sharerUserId, true);
    const consumerId = this.screenAudioConsumerIdFor(sharerUserId);
    if (consumerId) this.pauseCoordinator.addReason(consumerId, 'stream-mute');
  }

  /** True when local audio output is silenced by self- or server-deafen. */
  private isLocalAudioOutputDeafened(): boolean {
    const store = useVoiceStore.getState();
    if (store.isDeafened) return true;
    const localUserId = useUserStore.getState().user?.id;
    return !!localUserId && store.participants[localUserId]?.serverDeafened === true;
  }

  /** Unmute a sharer's screenshare audio: resume server forwarding + local decode. */
  unmuteScreenShare(sharerUserId: string): void {
    useVoiceStore.getState().setScreenShareMuted(sharerUserId, false);
    const consumerId = this.screenAudioConsumerIdFor(sharerUserId);
    if (!consumerId) return;
    this.pauseCoordinator.removeReason(consumerId, 'stream-mute');
    // Deafen is authoritative over per-stream unmute. Removing the last
    // 'stream-mute' reason above makes the coordinator resume local decode, but
    // if the local user is (self- or server-) deafened the stream must stay
    // silent until they undeafen — so re-assert the deafen pause on local
    // decode. Mirrors toggleDeafen's 'stream-mute' skip (the symmetric
    // ordering); once the mute reason is gone, toggleDeafen's undeafen loop
    // resumes this consumer normally (#2162).
    if (this.isLocalAudioOutputDeafened()) {
      this.consumers.get(consumerId)?.pause();
    }
  }

  /**
   * True when a freshly-created consumer already carries a persisted screenshare
   * mute intent, so it must stay server-paused rather than be resumed then
   * re-paused. Callers use this to skip the unconditional consume-time resume and
   * avoid a resume→pause audio/bandwidth blip on reconnect / stream restart (#2162).
   */
  private startsPausedByScreenMute(source: string, producerUserId: string): boolean {
    return (
      source === 'screen-audio' &&
      useVoiceStore.getState().screenShareMuted[producerUserId] === true
    );
  }

  /**
   * On a newly-created `screen-audio` consumer, re-apply the viewer's persisted mute
   * intent (survives reconnect / stream restart, which mint a fresh consumerId).
   */
  private applyInitialScreenMuteReason(consumerId: string, sharerUserId: string): void {
    if (useVoiceStore.getState().screenShareMuted[sharerUserId]) {
      this.pauseCoordinator.addReason(consumerId, 'stream-mute');
    }
  }

  /**
   * Re-apply per-source initial pause intent to a freshly-created consumer,
   * dispatched by media source. Called AFTER the consume-time resume so it is
   * not clobbered (#1541 visibility for camera, #2162 mute for screen-audio).
   */
  private applyInitialConsumerPauseReasons(
    consumerId: string,
    source: string,
    producerUserId: string
  ): void {
    if (source === 'camera') {
      this.applyInitialVisibilityReason(consumerId, producerUserId);
    } else if (source === 'screen-audio') {
      this.applyInitialScreenMuteReason(consumerId, producerUserId);
    }
  }

  // ─── Visibility-pause (#1541) ──────────────────────────────────────

  private resetRemoteVideoLayeringState(): void {
    this.cameraLayeringEnabled = false;
    this.screenLayeringEnabled = false;
    this.cameraLayeringReproduceInFlight = false;
    this.screenLayeringReproduceInFlight = false;
    this.cameraLayeringReproducePending = false;
    this.screenLayeringReproducePending = false;
    this.remoteVideoPressureByUser.clear();
    this.lastPreferredLayerKeyByConsumer.clear();
    this.remoteVideoRenderStateByUser.clear();
    this.remoteScreenRenderStateByUser.clear();
    // Screen reproduce re-tune-in markers are per-call state; a leave/reset must drop
    // them AND their timers so no stale marker forces a re-consume in a later call (#1924).
    for (const { timer } of this.screenReproducePending.values()) clearTimeout(timer);
    this.screenReproducePending.clear();
  }

  private scheduleCameraLayeringReproduce(): void {
    if (this.cameraLayeringReproduceInFlight) {
      this.cameraLayeringReproducePending = true;
      return;
    }

    this.cameraLayeringReproduceInFlight = true;
    void this.drainCameraLayeringReproduceQueue(this.videoReproduceGeneration);
  }

  private async drainCameraLayeringReproduceQueue(sessionGeneration: number): Promise<void> {
    try {
      do {
        if (!this.isCurrentVideoReproduceSession(sessionGeneration)) return;
        this.cameraLayeringReproducePending = false;
        try {
          await this.fastReproduceCamera();
        } catch (err) {
          console.warn(
            '[camera-layering] failed to re-produce camera after gate change:',
            errorMessage(err)
          );
        }
      } while (
        this.isCurrentVideoReproduceSession(sessionGeneration) &&
        this.cameraLayeringReproducePending
      );
    } finally {
      if (sessionGeneration === this.videoReproduceGeneration) {
        this.cameraLayeringReproduceInFlight = false;
      }
    }
  }

  /**
   * Serialize screen re-produces (#1924). Rapid `screen-layering-gate` edges, or a
   * Support-Simulcast toggle racing a gate edge, must not overlap two
   * `fastReproduceScreen()` calls — each closes the single 'screen' producer and then
   * `produce()`s, so two concurrent runs create a duplicate producer / 'track ended'.
   * Mirrors the camera scheduler pair: while a drain is in flight, a further trigger
   * only sets the pending flag; the drain loop picks it up as one coalesced re-run.
   */
  private scheduleScreenLayeringReproduce(): void {
    if (this.screenLayeringReproduceInFlight) {
      this.screenLayeringReproducePending = true;
      return;
    }

    this.screenLayeringReproduceInFlight = true;
    void this.drainScreenLayeringReproduceQueue(this.videoReproduceGeneration);
  }

  private async drainScreenLayeringReproduceQueue(sessionGeneration: number): Promise<void> {
    try {
      do {
        if (!this.isCurrentVideoReproduceSession(sessionGeneration)) return;
        this.screenLayeringReproducePending = false;
        try {
          await this.fastReproduceScreen();
        } catch (err) {
          console.warn(
            '[screen-layering] failed to re-produce screen after gate change:',
            errorMessage(err)
          );
        }
      } while (
        this.isCurrentVideoReproduceSession(sessionGeneration) &&
        this.screenLayeringReproducePending
      );
    } finally {
      if (sessionGeneration === this.videoReproduceGeneration) {
        this.screenLayeringReproduceInFlight = false;
      }
    }
  }

  private findCameraConsumerIdForUser(userId: string): string | null {
    for (const [id, meta] of this.consumerMeta) {
      if (meta.source === 'camera' && meta.producerUserId === userId) return id;
    }
    return null;
  }

  /** First remote SCREEN consumer id for a producing user (#1924). Mirrors
   *  findCameraConsumerIdForUser; feeds screen set-preferred-layers demand. */
  private findScreenConsumerIdForUser(userId: string): string | null {
    for (const [id, meta] of this.consumerMeta) {
      if (meta.source === 'screen' && meta.producerUserId === userId) return id;
    }
    return null;
  }

  /** Remote camera consumer ids for a given producing user. */
  private cameraConsumerIdsForUser(userId: string): string[] {
    const ids: string[] = [];
    for (const [id, meta] of this.consumerMeta) {
      if (meta.source === 'camera' && meta.producerUserId === userId) ids.push(id);
    }
    return ids;
  }

  /**
   * A user's camera tiles are "hidden" only when at least one tile is known AND none is
   * visible. No tiles known yet → NOT hidden (default visible) — never pause a consumer
   * before any tile has reported, which would cause a pause→resume + keyframe churn on
   * every join.
   */
  private tileHidden(userId: string): boolean {
    const tiles = this.tileVisibilityByUser.get(userId);
    if (!tiles || tiles.size === 0) return false;
    for (const visible of tiles.values()) {
      if (visible) return false;
    }
    return true;
  }

  /** Set/clear the visibility reason for one consumer = (all tiles hidden) OR (window hidden). */
  private updateVisibilityReason(consumerId: string, userId: string): void {
    const hidden = this.tileHidden(userId) || this.documentHidden;
    if (hidden) this.pauseCoordinator.addReason(consumerId, 'visibility');
    else this.pauseCoordinator.removeReason(consumerId, 'visibility');
  }

  private remoteVideoDevicePixelRatio(): number {
    const dpr = globalThis.devicePixelRatio || 1;
    return Number.isFinite(dpr) && dpr > 0 ? Math.min(dpr, MAX_REMOTE_VIDEO_DEVICE_PIXEL_RATIO) : 1;
  }

  private layerPayloadForTileState(
    userId: string,
    state: RemoteVideoTileRenderState,
    pressureStepDown = this.remoteVideoPressureByUser.get(userId) === true
  ): RemoteVideoLayerPayload {
    const devicePixelRatio = this.remoteVideoDevicePixelRatio();
    const request = computeRemoteVideoLayerRequest({
      ...state,
      devicePixelRatio,
      pressureStepDown,
    });

    return {
      ...state,
      ...request,
      devicePixelRatio,
      pressureStepDown,
    };
  }

  private computePreferredLayerPayloadForUser(
    userId: string,
    pressureStepDown?: boolean,
    source: RemoteVideoSource = 'camera'
  ): RemoteVideoLayerPayload | null {
    const stateMap =
      source === 'screen' ? this.remoteScreenRenderStateByUser : this.remoteVideoRenderStateByUser;
    const states = stateMap.get(userId);
    if (!states || states.size === 0) return null;

    // Screen has no BWE pressure machinery in v1 — force pressureStepDown=false so a
    // user's camera pressure can never bleed into their screen demand. Camera keeps
    // the passed value (undefined → per-user pressure lookup in layerPayloadForTileState).
    const effectivePressure = source === 'screen' ? false : pressureStepDown;

    let bestVisible: RemoteVideoLayerPayload | null = null;
    let hidden: RemoteVideoLayerPayload | null = null;

    for (const state of states.values()) {
      const payload = this.layerPayloadForTileState(userId, state, effectivePressure);
      if (!payload.visible) {
        hidden ??= payload;
        continue;
      }

      if (
        !bestVisible ||
        payload.spatialLayer > bestVisible.spatialLayer ||
        (payload.spatialLayer === bestVisible.spatialLayer &&
          payload.temporalLayer > bestVisible.temporalLayer)
      ) {
        bestVisible = payload;
      }
    }

    return bestVisible ?? hidden;
  }

  private clampRemoteVideoLayer(layer: number): 0 | 1 | 2 {
    if (layer <= 0) return 0;
    if (layer >= 2) return 2;
    return 1;
  }

  private emitPreferredLayers(consumerId: string, payload: RemoteVideoLayerPayload): void {
    if (!this.socket) return;
    const key = [
      payload.spatialLayer,
      payload.temporalLayer,
      payload.visible,
      payload.cssWidth,
      payload.cssHeight,
      payload.devicePixelRatio,
      payload.role,
      payload.focusedWindow,
      payload.pressureStepDown,
    ].join(':');
    if (this.lastPreferredLayerKeyByConsumer.get(consumerId) === key) return;
    this.lastPreferredLayerKeyByConsumer.set(consumerId, key);
    this.socket.emit('set-preferred-layers', {
      consumerId,
      spatialLayer: payload.spatialLayer,
      temporalLayer: payload.temporalLayer,
      visible: payload.visible,
      cssWidth: payload.cssWidth,
      cssHeight: payload.cssHeight,
      devicePixelRatio: payload.devicePixelRatio,
      role: payload.role,
      focusedWindow: payload.focusedWindow,
      pressureStepDown: payload.pressureStepDown,
    });
  }

  private emitPreferredLayersForUser(userId: string, source: RemoteVideoSource = 'camera'): void {
    const consumerId =
      source === 'screen'
        ? this.findScreenConsumerIdForUser(userId)
        : this.findCameraConsumerIdForUser(userId);
    if (!consumerId) return;

    const payload = this.computePreferredLayerPayloadForUser(userId, undefined, source);
    if (!payload) return;

    this.emitPreferredLayers(consumerId, payload);
  }

  /**
   * Emit set-preferred-layers demand for an EXPLICIT consumer id (#1924, PiP screen).
   * A PiP window runs a socket-less voiceService in a separate BrowserWindow, so it
   * can't report render-state itself — it proxies an explicit RPC (via PipSignalingProxy)
   * to THIS main window, which owns the socket the PiP's consumer was created on. Address
   * the PiP's OWN consumer id passed in — deliberately NOT via emitPreferredLayersForUser /
   * findScreenConsumerIdForUser, which resolve to the main window's now-PAUSED screen
   * consumer (the wrong one, and paused, so its demand would never reach the SFU).
   * Screen has no BWE pressure machinery in v1, so pressureStepDown is forced false and
   * the userId arg to layerPayloadForTileState is inert.
   */
  emitPreferredLayersForConsumer(
    consumerId: string,
    renderState: RemoteVideoTileRenderState
  ): void {
    const payload = this.layerPayloadForTileState('', renderState, false);
    this.emitPreferredLayers(consumerId, payload);
  }

  private tryEmitCameraPressureLayerRequest(
    consumerId: string,
    currentLayers: ConsumerLayerSelection | undefined
  ): CameraPressureLayerRequestResult {
    const meta = this.consumerMeta.get(consumerId);
    if (meta?.source !== 'camera' || !this.socket) return 'fallback';

    const targetConsumerId = this.findCameraConsumerIdForUser(meta.producerUserId);
    if (!targetConsumerId) return 'fallback';

    const states = this.remoteVideoRenderStateByUser.get(meta.producerUserId);
    if (!states || states.size === 0) return 'fallback';

    const payload = this.computePreferredLayerPayloadForUser(meta.producerUserId, true);
    if (!payload) return 'handled';

    if (currentLayers) {
      const spatialLayer = this.clampRemoteVideoLayer(
        Math.min(payload.spatialLayer, currentLayers.spatialLayer)
      );
      const temporalLayer = this.clampRemoteVideoLayer(
        Math.min(payload.temporalLayer, currentLayers.temporalLayer)
      );
      if (
        spatialLayer >= currentLayers.spatialLayer &&
        temporalLayer >= currentLayers.temporalLayer
      ) {
        return 'handled';
      }

      this.remoteVideoPressureByUser.set(meta.producerUserId, true);
      this.emitPreferredLayers(targetConsumerId, {
        ...payload,
        spatialLayer,
        temporalLayer,
      });
      return 'emitted';
    }

    this.remoteVideoPressureByUser.set(meta.producerUserId, true);
    this.emitPreferredLayers(targetConsumerId, payload);
    return 'emitted';
  }

  private clearRemoteVideoPressureAndEmit(): void {
    const pressuredUserIds = [...this.remoteVideoPressureByUser.entries()]
      .filter(([, pressured]) => pressured)
      .map(([userId]) => userId);

    for (const userId of pressuredUserIds) {
      this.remoteVideoPressureByUser.delete(userId);
      this.emitPreferredLayersForUser(userId);
    }
  }

  /**
   * Renderer reports whether ONE camera tile (a stable per-instance `tileId`) is visible.
   * The same participant can render in several tiles at once (grid + bar + PiP); the consumer
   * is paused only when EVERY known tile is hidden, so an off-screen bar entry never freezes
   * video that's visible in the grid (#1541 Gitar review).
   */
  setRemoteVideoVisibility(userId: string, visible: boolean, tileId: string): void {
    const tiles = this.tileVisibilityByUser.get(userId) ?? new Map<string, boolean>();
    tiles.set(tileId, visible);
    this.tileVisibilityByUser.set(userId, tiles);
    for (const id of this.cameraConsumerIdsForUser(userId)) {
      this.updateVisibilityReason(id, userId);
    }
  }

  setRemoteVideoRenderState(
    userId: string,
    tileId: string,
    state: {
      visible: boolean;
      cssWidth: number;
      cssHeight: number;
      role: RemoteVideoRole;
      focusedWindow: boolean;
    },
    source: RemoteVideoSource = 'camera'
  ): void {
    const stateMap =
      source === 'screen' ? this.remoteScreenRenderStateByUser : this.remoteVideoRenderStateByUser;
    const tiles = stateMap.get(userId) ?? new Map<string, RemoteVideoTileRenderState>();
    tiles.set(tileId, {
      visible: state.visible,
      cssWidth: state.cssWidth,
      cssHeight: state.cssHeight,
      role: state.role,
      focusedWindow: state.focusedWindow,
    });
    stateMap.set(userId, tiles);

    // Camera drives the per-consumer visibility-pause coordinator (#1541). Screen
    // pause is window-hidden/tuned-out driven (handleDocumentVisibilityChange), so the
    // screen reporter contributes ONLY layer demand — never a pause reason.
    if (source === 'camera') {
      this.setRemoteVideoVisibility(userId, state.visible, tileId);
    }
    this.emitPreferredLayersForUser(userId, source);
  }

  /**
   * Deregister a camera tile on unmount — NOT a "report hidden", so a closing PiP frame
   * doesn't freeze a still-visible grid tile. Prunes the user entry when empty so the map
   * never accumulates departed users (#1541 Gitar review).
   */
  removeRemoteVideoTile(
    userId: string,
    tileId: string,
    source: RemoteVideoSource = 'camera'
  ): void {
    // Camera-only: the visibility-pause coordinator (#1541) tracks camera tiles.
    if (source === 'camera') {
      const tiles = this.tileVisibilityByUser.get(userId);
      if (tiles) {
        tiles.delete(tileId);
        if (tiles.size === 0) this.tileVisibilityByUser.delete(userId);
        for (const id of this.cameraConsumerIdsForUser(userId)) {
          this.updateVisibilityReason(id, userId);
        }
      }
    }

    const stateMap =
      source === 'screen' ? this.remoteScreenRenderStateByUser : this.remoteVideoRenderStateByUser;
    const renderStates = stateMap.get(userId);
    if (!renderStates) return;
    // #1924: snapshot the removed tile's state BEFORE the delete so a last-surface
    // screen removal can tell the SFU the viewer stopped watching (visible:false),
    // instead of dropping local demand silently — which would pin the layer/gate on
    // the last visible demand until the consumer itself closes.
    const removedState = renderStates.get(tileId);
    renderStates.delete(tileId);
    if (renderStates.size === 0) {
      // Screen-only: the last visible screen surface for this sharer just unmounted —
      // release the layer/gate (extracted to keep this function under the S3776 limit).
      if (source === 'screen') this.releaseScreenDemandOnLastUnmount(userId, removedState);
      stateMap.delete(userId);
      return;
    }
    this.emitPreferredLayersForUser(userId, source);
  }

  /**
   * #1924: when the LAST screen render surface for a sharer unmounts, tell the SFU the
   * viewer stopped watching (`visible:false`) so the layer/gate is released instead of
   * pinned on the last visible demand until the consumer closes. Screen-only — camera
   * routes hidden-ness through the pause coordinator, so its last-surface removal stays a
   * silent drop (never emit a camera set-preferred-layers here).
   */
  private releaseScreenDemandOnLastUnmount(
    userId: string,
    removedState: RemoteVideoTileRenderState | undefined
  ): void {
    if (!removedState) return;
    const consumerId = this.findScreenConsumerIdForUser(userId);
    if (!consumerId) return;
    this.emitPreferredLayers(consumerId, {
      ...this.layerPayloadForTileState(userId, removedState),
      visible: false,
    });
  }

  /** Apply the current visibility intent to a freshly-routed camera consumer. */
  private applyInitialVisibilityReason(consumerId: string, userId: string): void {
    this.updateVisibilityReason(consumerId, userId);
    this.emitPreferredLayersForUser(userId);
  }

  /** Window hidden/shown → fan the visibility reason across all remote video consumers. */
  private handleDocumentVisibilityChange(hidden: boolean): void {
    this.documentHidden = hidden;
    for (const [id, meta] of this.consumerMeta) {
      if (meta.source === 'camera') {
        this.updateVisibilityReason(id, meta.producerUserId);
      } else if (meta.source === 'screen') {
        // Tuned-in screen tiles have no per-tile observer; window-hidden alone drives them.
        if (this.documentHidden) this.pauseCoordinator.addReason(id, 'visibility');
        else this.pauseCoordinator.removeReason(id, 'visibility');
      }
    }
  }

  private registerDocumentVisibilityListener(): void {
    if (this.boundDocVisibility) return;
    this.boundDocVisibility = () => this.handleDocumentVisibilityChange(document.hidden);
    document.addEventListener('visibilitychange', this.boundDocVisibility);
  }

  private unregisterDocumentVisibilityListener(): void {
    if (this.boundDocVisibility) {
      document.removeEventListener('visibilitychange', this.boundDocVisibility);
      this.boundDocVisibility = null;
    }
    this.documentHidden = false;
    this.tileVisibilityByUser.clear();
    this.resetRemoteVideoLayeringState();
  }

  /** Get consumer IDs filtered by source (e.g. 'audio', 'camera', 'screen'). No filter returns all. */
  getConsumerIdsBySource(source?: string): string[] {
    if (!source) return Array.from(this.consumers.keys());
    const ids: string[] = [];
    for (const [id, meta] of this.consumerMeta) {
      if (meta.source === source) ids.push(id);
    }
    return ids;
  }

  /** Get the router's RTP capabilities (needed for PiP voice client Device.load()) */
  getRouterRtpCapabilities(): mediasoupTypes.RtpCapabilities | null {
    return this.routerRtpCapabilities;
  }

  /** Get all consumer metadata entries (used by PiP signaling proxy for ownership transfer) */
  getConsumerMeta(): Map<string, { source: string; producerUserId: string; producerId: string }> {
    return new Map(this.consumerMeta);
  }

  /**
   * Derive a decrypt frame key for a PiP window's own E2EE Worker (2026-08-21 PiP E2EE gap).
   *
   * A PiP BrowserWindow consumes producers independently, so it needs its own
   * decrypt transform and therefore its own frame keys — but it has no
   * e2eeService, no IndexedDB private key and no CSK unwrap. The main window
   * owns all of that already, so the PiP asks for the one derived artifact it
   * needs. The returned `CryptoKey` travels the BroadcastChannel by structured
   * clone, as a handle rather than raw bytes — but frame keys are EXTRACTABLE
   * by construction (the ratchet must export them), so this is not a
   * confidentiality boundary against same-origin script, only an avoidance of
   * materializing key bytes. See pipSignalingTypes.GetFrameKeyRequest.
   *
   * `keyVersion`/`keyId` are optional: omitted for the PiP's initial
   * pre-consume provision (answered with the channel's current pair), supplied
   * verbatim when the PiP Worker reports a typed miss for an exact epoch.
   *
   * Throws when there is no active channel or E2EE is not initialized — the PiP
   * treats that as fail-closed and does not play the stream.
   */
  async deriveFrameKeyForPip(
    senderUserId: string,
    keyVersion?: number,
    keyId?: number
  ): Promise<{ key: CryptoKey; keyVersion: number; keyId: number }> {
    const mediaEncryption = this.mediaEncryption;
    if (!mediaEncryption) throw new Error('E2EE: media encryption is not initialized');

    const channelId = useVoiceStore.getState().activeChannelId;
    if (!channelId) throw new Error('E2EE: no active voice channel');

    const material = await e2eeService.getChannelKeyMaterial(channelId);
    const resolvedVersion = keyVersion ?? material.keyVersion;
    const resolvedKeyId = keyId ?? mediaEncryption.getCurrentKeyId();

    // A miss for a version other than the one we hold must fetch THAT version's
    // CSK: deriving from the current CSK for an older version's frame yields a
    // key that decrypts to garbage. (An earlier draft of this comment described
    // failing closed here instead — it described a superseded design, not this
    // code, and restoring it would break mid-session CSK-rotation recovery for
    // PiP consumers. CodeRabbit, PR #2870.)
    const csk =
      resolvedVersion === material.keyVersion
        ? material.channelKey
        : await e2eeService.getChannelKeyByVersion(channelId, resolvedVersion);

    const key = await mediaEncryption.addDecryptKeyAtVersion(
      csk,
      senderUserId,
      resolvedVersion,
      resolvedKeyId
    );
    return { key, keyVersion: resolvedVersion, keyId: resolvedKeyId };
  }

  /** Proxy a signaling event to the media plane (used by PiP signaling proxy) */
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- T defaults to `any` to avoid forcing PiP-proxy callers to parameterize every signaling forward; callers that care about the response type set T explicitly
  forwardToServer<T = any>(event: string, data?: unknown): Promise<T> {
    return this.emitAsync<T>(event, data);
  }

  /**
   * Wait for the send transport's internal AwaitQueue to drain.
   *
   * producer.close() enqueues a stopSending() SDP renegotiation as fire-and-forget
   * on the transport's _awaitQueue. If we call sendTransport.produce() before that
   * completes, the new m= section's codec PT can collide with the recycled section
   * from the closing producer (Chromium BUNDLE PT 45 collision).
   *
   * This pushes a no-op onto the same queue and awaits it, guaranteeing all prior
   * queued operations (including stopSending) have finished before we proceed.
   */
  private async drainSendTransportQueue(
    transport: mediasoupTypes.Transport | null = this.sendTransport
  ): Promise<void> {
    if (!transport || transport.closed) return;
    try {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any -- mediasoup-client's internal `_awaitQueue` is not in the public Transport type; documented workaround for the Chromium PT 45 BUNDLE collision (see comment above method)
      await (transport as any)._awaitQueue.push(async () => {
        /* no-op — just wait for our turn */
      }, 'drainSendTransportQueue');
    } catch {
      // Transport may have closed while waiting — safe to ignore
    }
  }

  /** Close a specific producer by source */
  async closeProducer(source: string): Promise<void> {
    if (source === 'camera' || source === 'screen') {
      this.cancelVideoReproduce(source);
    }
    const producer = this.producers.get(source);

    if (producer) {
      producer.close();
      await this.drainSendTransportQueue();
      this.producers.delete(source);
      this.socket?.emit('close-producer', { producerId: producer.id });
    }

    // Always clean up local tracks and reset state, even if no producer exists.
    if (source === 'mic') this.cleanupMicState();
    else if (source === 'camera') this.cleanupCameraState();
    else if (source === 'screen') await this.cleanupScreenState();
    else if (source === 'screen-audio') this.cleanupScreenAudioState();
  }

  private cleanupMicState(): void {
    this.stopLocalVAD();
    this.stopNoiseGate();
    this.stopInputVolume();
    if (this.localMicStream) {
      for (const t of this.localMicStream.getTracks()) t.stop();
      this.localMicStream = null;
    }
  }

  private cleanupCameraState(): void {
    if (this.localCameraStream) {
      for (const t of this.localCameraStream.getTracks()) t.stop();
      this.localCameraStream = null;
    }
    const store = useVoiceStore.getState();
    store.setVideoOn(false);
    store.setActiveCameraCodec(null);
    const localUserId = useUserStore.getState().user?.id;
    if (localUserId) {
      store.updateParticipant(localUserId, { videoStream: undefined, isVideoOn: false });
    }
  }

  /**
   * Clean up local system-audio capture after a standalone `screen-audio` close
   * (e.g. a permission push revokes Speak while ScreenShare — and thus the screen
   * VIDEO track — is still allowed). The screen-audio producer is created with
   * `stopTracks: false` and shares `localScreenStream` with the screen video, so
   * closing the producer alone leaves the system-audio track live. Stop and
   * detach ONLY the audio track(s), leaving the video track (and the stream
   * itself) running so the still-permitted screen share continues. Contrast
   * cleanupScreenState, which tears the whole stream down when `screen` closes.
   */
  private cleanupScreenAudioState(): void {
    if (this.localScreenStream) {
      for (const t of this.localScreenStream.getAudioTracks()) {
        t.stop();
        this.localScreenStream.removeTrack(t);
      }
    }
    const localUserId = useUserStore.getState().user?.id;
    if (localUserId) {
      useVoiceStore.getState().updateParticipant(localUserId, { screenAudioStream: undefined });
    }
  }

  private async cleanupScreenState(guard?: {
    token: VideoReproduceToken;
    transport: mediasoupTypes.Transport;
    stream: MediaStream;
  }): Promise<void> {
    if (
      guard &&
      (!this.isCurrentVideoReproduce(guard.token, guard.transport) ||
        this.localScreenStream !== guard.stream)
    ) {
      return;
    }
    // Also close the paired screen-audio producer
    const audioProducer = this.producers.get('screen-audio');
    if (audioProducer) {
      audioProducer.close();
      await this.drainSendTransportQueue(guard?.transport);
      if (
        guard &&
        (!this.isCurrentVideoReproduce(guard.token, guard.transport) ||
          this.localScreenStream !== guard.stream ||
          this.producers.get('screen-audio') !== audioProducer)
      ) {
        return;
      }
      this.producers.delete('screen-audio');
      this.socket?.emit('close-producer', { producerId: audioProducer.id });
    }

    if (this.localScreenStream) {
      for (const t of this.localScreenStream.getTracks()) t.stop();
      this.localScreenStream = null;
    }
    const store = useVoiceStore.getState();
    store.setScreenSharing(false);
    store.setActiveScreenCodec(null);
    // #2088: deterministic local-share cleanup (spec §3.1 removal point) —
    // don't rely solely on the media-plane's producer-closed self-echo.
    const localShare = Object.values(store.activeScreenShares).find((s) => s.isLocal);
    if (localShare) {
      store.unregisterActiveScreenShare(localShare.producerId);
      if (store.tunedInScreenShares[localShare.producerId]) {
        store.tuneOut(localShare.producerId);
      }
    }
    const localUserId = useUserStore.getState().user?.id;
    if (localUserId) {
      store.updateParticipant(localUserId, {
        screenStream: undefined,
        screenAudioStream: undefined,
        isScreenSharing: false,
      });
    }
  }

  /** Update audio quality tier on existing mic producer */
  async setQualityTier(tier: AudioQualityTier): Promise<void> {
    useVoiceStore.getState().setQualityTier(tier);
    // Re-produce mic with new codec options if we have an active producer
    const producer = this.producers.get('mic');
    if (producer && this.sendTransport) {
      await this.closeProducer('mic');
      await this.produceAudio();
    }
  }

  // ─── Transport Setup ───────────────────────────────────────────────

  private async createSendTransport(): Promise<void> {
    if (!this.device) throw new Error('Device not initialized before creating send transport');
    const expectedSessionGeneration = this.videoReproduceGeneration;

    const options = await this.emitAsync<TransportOptions>('create-transport', {
      direction: 'send',
    });
    if (expectedSessionGeneration !== this.videoReproduceGeneration) {
      throw new Error('Media session changed while creating send transport');
    }

    this.sendTransport = this.device.createSendTransport({
      id: options.id,
      iceParameters: options.iceParameters,
      iceCandidates: options.iceCandidates,
      dtlsParameters: options.dtlsParameters,
      // E2EE (legacy path): encodedInsertableStreams enables createEncodedStreams().
      // NOT set for RTCRtpScriptTransform — the two mechanisms conflict (#295).
      // All channels are always encrypted, so this is unconditionally applied when supported.
      ...(currentTransformPath() === 'encoded-streams' && {
        additionalSettings: {
          encodedInsertableStreams: true,
        } as unknown as Partial<RTCConfiguration>,
      }),
    });
    this.videoReproduceSessionActive = true;

    const sendTransportId = this.sendTransport.id;
    this.sendTransport.on('connect', ({ dtlsParameters }, callback, errback) => {
      this.emitAsync('connect-transport', {
        transportId: sendTransportId,
        dtlsParameters,
      })
        .then(() => callback())
        .catch(errback);
    });

    this.sendTransport.on(
      'produce',
      async ({ kind, rtpParameters, appData }, callback, errback) => {
        try {
          const result = await this.emitAsync<{ id: string }>('produce', {
            transportId: sendTransportId,
            kind,
            rtpParameters,
            appData,
          });
          callback({ id: result.id });
        } catch (err) {
          errback(err as Error);
        }
      }
    );

    // DEBUG: Capture SDP from the send transport's PeerConnection
    // to identify the source of the payload_type=45 BUNDLE collision
    this.logTransportSdp(this.sendTransport, 'send');
  }

  /**
   * Create recv transport(s). For E2EE channels, creates separate audio and
   * video transports to avoid BUNDLE payload_type collision (#291). Each
   * transport gets its own RTCPeerConnection, so audio and video codecs
   * can never collide within a BUNDLE group.
   */
  private async createRecvTransports(): Promise<void> {
    // All channels are always encrypted — always create split audio/video transports
    // to avoid BUNDLE payload_type collision (#291).
    await Promise.all([
      this.createRecvTransportForKind('audio'),
      this.createRecvTransportForKind('video'),
    ]);
  }

  /** Create a recv transport for a specific media kind (E2EE path) */
  private async createRecvTransportForKind(mediaKind: 'audio' | 'video'): Promise<void> {
    if (!this.device) throw new Error('Device not initialized before creating recv transport');

    const options = await this.emitAsync<TransportOptions>('create-transport', {
      direction: 'recv',
    });

    const transport = this.device.createRecvTransport({
      id: options.id,
      iceParameters: options.iceParameters,
      iceCandidates: options.iceCandidates,
      dtlsParameters: options.dtlsParameters,
      // E2EE (legacy path only) — see send transport comment (#295)
      ...(currentTransformPath() === 'encoded-streams' && {
        additionalSettings: {
          encodedInsertableStreams: true,
        } as unknown as Partial<RTCConfiguration>,
      }),
    });

    if (mediaKind === 'audio') {
      this.recvTransportAudio = transport;
    } else {
      this.recvTransportVideo = transport;
    }

    transport.on('connect', ({ dtlsParameters }, callback, errback) => {
      this.emitAsync('connect-transport', {
        transportId: transport.id,
        dtlsParameters,
      })
        .then(() => callback())
        .catch(errback);
    });

    // DEBUG: Capture SDP from E2EE recv transport
    this.logTransportSdp(transport, `recv-${mediaKind}`);
  }

  /**
   * DEBUG: Log the SDP from a transport's underlying PeerConnection.
   * Helps identify the source of the payload_type=45 BUNDLE collision.
   */
  private logTransportSdp(transport: mediasoupTypes.Transport, label: string): void {
    if (!E2EE_VERBOSE) return; // Only log SDP when verbose debugging is enabled
    try {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any -- mediasoup-client's internal `_handler.pc` is needed to diagnose the BUNDLE PT 45 collision; no public API exposes the underlying RTCPeerConnection
      const handler = (transport as any)._handler;
      if (!handler?._pc) return;
      const pc = handler._pc as RTCPeerConnection;

      const logSdp = () => {
        if (pc.localDescription) {
          console.debug('[SDP] local:', label, pc.localDescription.sdp);
        }
        if (pc.remoteDescription) {
          console.debug('[SDP] remote:', label, pc.remoteDescription.sdp);
        }
      };

      pc.addEventListener('signalingstatechange', () => {
        console.debug(`[SDP] ${label} signalingState: ${pc.signalingState}`);
        if (pc.signalingState === 'stable') logSdp();
      });
    } catch {
      // Non-critical debug logging — don't break transport setup
    }
  }

  /** Get the recv transport for a given media kind (always split audio/video — all channels are E2EE) */
  private getRecvTransport(kind: mediasoupTypes.MediaKind): mediasoupTypes.Transport | null {
    return kind === 'audio' ? this.recvTransportAudio : this.recvTransportVideo;
  }

  /** Get the transport ID to pass to the server for a consume request */
  private getRecvTransportId(kind: mediasoupTypes.MediaKind): string | undefined {
    return this.getRecvTransport(kind)?.id;
  }

  // ─── Consumer Management ───────────────────────────────────────────

  /**
   * Queue a consume operation. Serializes per-transport (each transport has
   * independent SDP negotiation).
   */
  private async consumeProducer(
    producerId: string,
    senderUserId?: string,
    kind?: mediasoupTypes.MediaKind
  ): Promise<void> {
    // Validate kind for E2EE transport routing (all channels are always E2EE)
    if (kind && kind !== 'audio' && kind !== 'video') {
      console.warn('[consume] skipped — invalid kind for E2EE routing', { producerId, kind });
      return;
    }
    // Warn if kind is missing (fallback to audio queue)
    if (!kind) {
      console.warn('[consume] kind not provided — using audio queue fallback', {
        producerId,
      });
    }
    if (kind) {
      // Route to per-transport queues so audio/video negotiate in parallel
      if (kind === 'audio') {
        this.consumeQueueAudio = this.consumeQueueAudio
          .then(() => this.consumeProducerImpl(producerId, senderUserId, kind))
          .catch((err) => {
            console.error('Audio consume queue error:', errorMessage(err));
          });
        await this.consumeQueueAudio;
      } else {
        this.consumeQueueVideo = this.consumeQueueVideo
          .then(() => this.consumeProducerImpl(producerId, senderUserId, kind))
          .catch((err) => {
            console.error('Video consume queue error:', errorMessage(err));
          });
        await this.consumeQueueVideo;
      }
    } else {
      // kind unknown: fall back to audio queue
      this.consumeQueueAudio = this.consumeQueueAudio
        .then(() => this.consumeProducerImpl(producerId, senderUserId, kind))
        .catch((err) => {
          console.error('Consume queue error:', errorMessage(err));
        });
      await this.consumeQueueAudio;
    }
  }

  /** Lazy-init E2EE and apply decrypt transform for a consumed producer. */
  private async ensureE2EEForConsumer(
    consumer: mediasoupTypes.Consumer,
    producerUserId: string
  ): Promise<void> {
    // Lazy re-init: if initEncryption failed at join time but CSK is now available
    if (!this.mediaEncryption) {
      const channelId = useVoiceStore.getState().activeChannelId;
      if (channelId) {
        try {
          await this.initEncryption(channelId);
        } catch (err) {
          console.warn('E2EE: lazy re-init failed, closing consumer fail-closed', {
            error: errorMessage(err),
          });
        }
      }
    }
    if (!this.mediaEncryption) {
      throw new Error(
        'E2EE: failed to attach decrypt transform (media encryption is not initialized)'
      );
    }

    // Fire-and-forget: add decrypt key without blocking the consume queue.
    const channelId = useVoiceStore.getState().activeChannelId;
    if (channelId) {
      void this.addDecryptKeyForUser(channelId, producerUserId).catch((err) => {
        console.warn('E2EE: addDecryptKeyForUser failed, self-healing will retry', {
          producerUserId,
          error: err instanceof Error ? err.message : err,
        });
      });
    }
    this.applyDecryptTransform(consumer, producerUserId);
  }

  /** Route a consumed stream to the correct participant field in the store. */
  private routeConsumerToStore(consumer: mediasoupTypes.Consumer, result: ConsumeResponse): void {
    const stream = new MediaStream([consumer.track]);
    const store = useVoiceStore.getState();
    const source = result.source;
    const hasParticipant = !!store.participants[result.producerUserId];

    if (!hasParticipant) {
      // The producer's user-joined roster entry has not landed yet (the
      // join-vs-consume race — pronounced for DM calls, #1873). PII-safe:
      // UUID + source enum only, never names/streams (observability.md).
      console.warn('[consume] participant not in store; hydrating from consume', {
        producerUserId: result.producerUserId,
        source,
      });
    }

    const routeMap: Record<string, Partial<VoiceParticipant>> = {
      'screen-audio': { screenAudioStream: stream },
      camera: { videoStream: stream, isVideoOn: true },
      screen: { screenStream: stream, isScreenSharing: true },
    };
    const update = routeMap[source] ?? (result.kind === 'audio' ? { audioStream: stream } : null);
    if (update) {
      // upsert (not updateParticipant): a consumed track must never be dropped
      // when the participant has not been hydrated yet (#1873). A later
      // user-joined upsert backfills name/avatar without clobbering the stream.
      store.upsertParticipant(result.producerUserId, update);
    }

    console.debug('[consume] stream attached', {
      kind: result.kind,
      source,
      producerUserId: result.producerUserId,
      hasParticipant,
    });
  }

  private async consumeProducerImpl(
    producerId: string,
    senderUserId?: string,
    kind?: mediasoupTypes.MediaKind
  ): Promise<void> {
    if (!this.device) {
      console.warn('[consume] skipped — no device', { producerId });
      return;
    }

    try {
      // For E2EE, tell the server which recv transport to use for this consumer.
      // Guard: ensure recv transport exists before server call to prevent
      // to avoid leaking server-side consumers that the client can't attach.
      const transportId = kind ? this.getRecvTransportId(kind) : undefined;
      if (kind && !transportId) {
        console.warn('[consume] skipped — recv transport not ready for kind', {
          producerId,
          kind,
        });
        return;
      }

      console.debug('[consume] requesting', { producerId, senderUserId, kind, transportId });
      const result = await this.emitAsync<ConsumeResponse>('consume', {
        producerId,
        ...(transportId && { transportId }),
      });
      if (!result || 'error' in result) {
        console.warn('[consume] server returned error or empty result', { producerId, result });
        return;
      }

      console.debug('[consume] server responded', {
        consumerId: result.id,
        kind: result.kind,
        source: result.source,
        producerUserId: result.producerUserId,
      });

      // Route to the correct recv transport (split audio/video)
      const recvTransport = this.getRecvTransport(result.kind);
      if (!recvTransport) {
        console.warn('[consume] skipped — no recvTransport for kind', {
          producerId,
          kind: result.kind,
        });
        return;
      }

      // Chromium ≥149 (encoded-transform V2 line) ignores a receive transform
      // attached after the receiver is live: zero frames enter it and
      // ciphertext reaches the decoder (2026-08-21 incident, PR #2865). The
      // transform therefore attaches AT RECEIVER CREATION via onRtpReceiver —
      // after setRemoteDescription, before createAnswer — mirroring the
      // sender-side onRtpSender hook, which works on the same engines.
      // applyDecryptTransform stays as the verifier: it detects the
      // creation-time attachment, schedules the bypass probe, and remains the
      // fail-closed late-attach path for the legacy pipeline.
      const consumer = await recvTransport.consume({
        id: result.id,
        producerId: result.producerId,
        kind: result.kind,
        rtpParameters: result.rtpParameters,
        ...this.creationAttachConsumeOption(result, senderUserId),
      });

      this.consumers.set(consumer.id, consumer);
      this.consumerMeta.set(consumer.id, {
        source: result.source,
        producerUserId: result.producerUserId,
        producerId: result.producerId,
      });

      console.debug('[consume] consumer created', {
        consumerId: consumer.id,
        kind: consumer.kind,
        trackState: consumer.track.readyState,
        trackEnabled: consumer.track.enabled,
        paused: consumer.paused,
      });

      // Apply E2EE decrypt transform (all channels are always encrypted)
      const producerUserId = senderUserId || result.producerUserId;
      try {
        await this.ensureE2EEForConsumer(consumer, producerUserId);
      } catch (err) {
        this.closeConsumerAfterDecryptTransformFailure(consumer, errorMessage(err));
        return;
      }

      // Attach stream to participant
      this.routeConsumerToStore(consumer, result);

      // Clean up on close
      consumer.on('transportclose', () => {
        console.debug('[consume] transport closed for consumer', consumer.id);
        this.consumers.delete(consumer.id);
        this.decoderBudgetSampler.deleteConsumer(consumer.id);
        this.consumerMeta.delete(consumer.id);
        this.lastPreferredLayerKeyByConsumer.delete(consumer.id);
        this.pauseCoordinator.clearConsumer(consumer.id);
        this.testSuspendedConsumerIds.delete(consumer.id);
        this.testRestoreEligibleConsumerIds.delete(consumer.id);
        this.testServerPausedConsumerIds.delete(consumer.id);
        this.serverResumeOnUndeafenConsumerIds.delete(consumer.id);
      });

      if (this.testSuspensionDepth > 0 && consumer.kind === 'audio') {
        if (!consumer.paused) consumer.pause();
        this.testSuspendedConsumerIds.add(consumer.id);
        this.testRestoreEligibleConsumerIds.add(consumer.id);
        this.testServerPausedConsumerIds.add(consumer.id);
        console.debug('[consume] consumer held during test suspension', {
          consumerId: consumer.id,
        });
      } else if (this.startsPausedByScreenMute(result.source, result.producerUserId)) {
        // A screen-audio consumer the viewer has already muted is created paused
        // on the server. Unconditionally resuming it here only to re-pause it in
        // applyInitialConsumerPauseReasons() below would open a resume→pause
        // window where the SFU forwards audio for a stream the viewer explicitly
        // muted — wasted bandwidth plus a brief audible blip on reconnect / stream
        // restart. Detect that persisted mute intent up front and skip the resume,
        // leaving the consumer server-paused for the coordinator to own (#2162).
        console.debug('[consume] consumer left server-paused (persisted screenshare mute)', {
          consumerId: consumer.id,
        });
      } else {
        // Resume the consumer (was created paused on server)
        await this.emitAsync('resume-consumer', { consumerId: consumer.id });
        console.debug('[consume] consumer resumed', { consumerId: consumer.id });
      }

      // #1541: apply pending visibility intent AFTER the resume emit above. An
      // initially-hidden tile's pause-consumer must run after the unconditional
      // resume that starts the server-paused consumer — otherwise the resume
      // clobbers the pause and the off-screen tile keeps forwarding (Gitar review).
      // #1541 + #2162: re-apply per-source initial pause intent AFTER the
      // unconditional resume above (else the resume clobbers an initially-hidden
      // camera tile or a muted screenshare). Dispatched by source in a helper to
      // keep consume()'s cognitive complexity within budget.
      this.applyInitialConsumerPauseReasons(consumer.id, result.source, result.producerUserId);

      // #1924: a screen REPRODUCE / codec-swap keeps the render surface mounted (same
      // userId/tileId), so the reporter's IntersectionObserver never re-fires — but the
      // underlying screen consumer was just swapped for a fresh one seeded at spatial
      // layer 0. Re-emit the stored render-state demand so the NEW consumer inherits the
      // viewer's real size/visibility instead of stranding on layer 0.
      this.reemitScreenDemandOnConsume(result.source, result.producerUserId);
    } catch (err) {
      console.error('[consume] Failed to consume producer:', producerId, errorMessage(err));
    }
  }

  /**
   * #1924: after a NEW screen consumer is recorded, re-send the sharer's persisted screen
   * render-state demand so the fresh consumer picks up the right layer. Resolves the new
   * consumer via findScreenConsumerIdForUser inside emitPreferredLayersForUser, and
   * no-ops safely for non-screen sources and when no render-state was ever reported.
   */
  private reemitScreenDemandOnConsume(source: string, userId: string): void {
    if (source !== 'screen') return;
    this.emitPreferredLayersForUser(userId, 'screen');
  }

  // ─── Socket Listeners ──────────────────────────────────────────────

  /**
   * producer-closed SCREEN branch (#1924). Purge the closed producer's available /
   * active / tuned-in state, arm the reproduce re-tune-in marker (keyed by SHARER), and
   * clear the participant-level screen flags. Extracted from the socket handler to keep
   * it under the S3776 cognitive-complexity budget.
   */
  private handleScreenProducerClosed(
    producerId: string,
    userId: string,
    store: ReturnType<typeof useVoiceStore.getState>
  ): void {
    store.removeAvailableScreenShare(producerId);
    store.unregisterActiveScreenShare(producerId);
    store.clearAutoTuneSuppression(producerId);
    // Clean up tunedInScreenShares so UI collapses back to user frame grid
    if (producerId in store.tunedInScreenShares) {
      // Stash a re-tune-in intent keyed by the SHARER (userId) BEFORE tuneOut
      // prunes the live tuned-in/dominant state (#1924 review fix A): if this
      // sharer immediately re-announces a screen producer — a fastReproduceScreen
      // codec-floor / screen-layering-gate swap closes the old producer and
      // announces a new one — the new-producer handler re-consumes the new
      // producerId for us, bypassing the autoTuneInScreenShares opt-in and
      // restoring dominance. Bounded by a timer so a genuine stop (no re-announce
      // within the window) falls back to the normal opt-in path. Re-arm cleanly
      // if a marker for this sharer already exists.
      const existing = this.screenReproducePending.get(userId);
      if (existing) clearTimeout(existing.timer);
      const wasDominant = store.dominantScreenShareId === producerId;
      const timer = setTimeout(() => {
        this.screenReproducePending.delete(userId);
      }, SCREEN_REPRODUCE_RETUNE_WINDOW_MS);
      this.screenReproducePending.set(userId, { wasDominant, timer });
      store.tuneOut(producerId);
    }

    // Reverse-order clobber guard (#1924 review fix): a reproduce can deliver the NEW
    // screen producer (already consumed + re-tuned by the reverse-order path in
    // handleOptInScreenAnnounce) BEFORE this OLD producer-closed arrives. In that window
    // a fresh screen consumer for this sharer is already live, so unconditionally
    // clearing isScreenSharing:false / screenStream:undefined here would wipe the state
    // the reverse-order re-consume just set up. Only clear the participant-level screen
    // state when this viewer is NOT still tuned into a newer screen from the same sharer.
    // Evaluated on FRESH store state AFTER the old producer's tuneOut above, and after the
    // consumer-close loop deleted the old consumer's meta, so the just-closed producer
    // never counts toward "still tuned in".
    if (!this.isUserScreenTunedIn(userId, useVoiceStore.getState())) {
      store.updateParticipant(userId, { isScreenSharing: false, screenStream: undefined });
    }
  }

  /** Check if we're already tuned into a screen share from a specific user. */
  private isUserScreenTunedIn(
    userId: string,
    store: ReturnType<typeof useVoiceStore.getState>
  ): boolean {
    return (
      Object.values(store.tunedInScreenShares).length > 0 &&
      [...this.consumerMeta.values()].some(
        (m) => m.source === 'screen' && m.producerUserId === userId
      )
    );
  }

  /** Register a newly announced opt-in screen share and auto-tune when the
   *  receive policy is ON (#2088). Extracted from handleNewProducer (S3776). */
  private async handleOptInScreenAnnounce(
    producerId: string,
    userId: string,
    store: ReturnType<typeof useVoiceStore.getState>
  ): Promise<void> {
    const participant = store.participants[userId];
    store.addAvailableScreenShare({
      producerId,
      userId,
      username: participant?.username || 'Unknown',
      displayName: participant?.displayName,
    });
    store.registerActiveScreenShare({
      producerId,
      userId,
      username: participant?.username || 'Unknown',
      displayName: participant?.displayName,
      isLocal: false,
    });
    store.updateParticipant(userId, { isScreenSharing: true });

    // Reproduce re-tune-in (#1924 review fix A). If this sharer's previous screen
    // producer just closed while THIS viewer was tuned into it (a fastReproduceScreen
    // codec-floor / screen-layering-gate swap), re-tune-in to the NEW producerId
    // regardless of the autoTuneInScreenShares opt-in and restore dominance if the
    // closed producer was dominant. Takes precedence over the opt-in branch below and
    // returns, so a reproduce never double-consumes. The marker is one-shot: consuming
    // it (or its bounding timer) clears it, so a genuine later restart falls through to
    // the opt-in path on the next announce.
    const reproduce = this.screenReproducePending.get(userId);
    // Order-robustness (Gitar #1924 review): the marker is armed by producer-closed,
    // which normally lands before this new-producer announce (the sharer emits
    // close-producer, THEN produces, over an ordered per-socket transport). If that
    // order is ever reversed, no marker exists yet but THIS viewer is still tuned into
    // the sharer's OLD screen producer — and since a user has at most one screen, a new
    // screen from a sharer we're already watching IS a reproduce. Detect that so Fix A
    // does not depend on event ordering. Exactly one path fires per reproduce: the
    // marker path in the normal order (reverseOrderReproduce is false when a marker
    // exists); the reverse path otherwise, after which the later producer-closed arms a
    // marker that simply expires. tuneInToScreenShare is idempotent, so even a double
    // trigger cannot double-consume the same producerId.
    const reverseOrderReproduce = !reproduce && this.isUserScreenTunedIn(userId, store);
    const dominantConsumerId =
      store.dominantScreenShareId === null
        ? undefined
        : store.tunedInScreenShares[store.dominantScreenShareId];
    const reverseOrderWasDominant =
      reverseOrderReproduce &&
      dominantConsumerId !== undefined &&
      this.consumerMeta.get(dominantConsumerId)?.producerUserId === userId;
    if (reproduce || reverseOrderReproduce) {
      if (reproduce) {
        clearTimeout(reproduce.timer);
        this.screenReproducePending.delete(userId);
      }
      // The new-producer socket handler discards this promise — isolate failures the
      // same way autoTuneSweep does (no unhandled rejection).
      try {
        await this.tuneInToScreenShare(producerId, userId);
        // setDominantScreenShare no-ops unless the id is currently tuned in, so a
        // cap-exhausted / stale-context bail inside tuneInToScreenShare cannot point
        // dominance at an un-consumed producer. Reverse-order delivery reasserts the
        // old dominant sharer's replacement before producer-closed falls back to an
        // arbitrary remaining tuned-in share.
        if (reproduce?.wasDominant || reverseOrderWasDominant) {
          useVoiceStore.getState().setDominantScreenShare(producerId);
        }
      } catch (err) {
        console.error(
          'Screen reproduce re-tune-in failed for producer',
          producerId,
          errorMessage(err)
        );
      }
      return;
    }

    // New-share trigger (#2088). The cap guard inside tuneInToScreenShare
    // surfaces the existing max-stream error when capacity is exhausted.
    if (
      useVideoSettingsStore.getState().autoTuneInScreenShares &&
      !store.autoTuneSuppressedProducers[producerId]
    ) {
      // The new-producer socket handler discards this promise — isolate
      // failures the same way autoTuneSweep does (no unhandled rejection).
      try {
        await this.tuneInToScreenShare(producerId, userId);
      } catch (err) {
        console.error('Auto-tune failed for producer', producerId, errorMessage(err));
      }
    }
  }

  /** Handle a new-producer socket event — dispatches opt-in, E2EE, slot enforcement. */
  private async handleNewProducer(event: {
    producerId: string;
    userId: string;
    kind: string;
    source: string;
    requiresOptIn?: boolean;
  }): Promise<void> {
    const { producerId, userId, kind, source, requiresOptIn } = event;
    const store = useVoiceStore.getState();

    if (requiresOptIn && source === 'screen') {
      await this.handleOptInScreenAnnounce(producerId, userId, store);
      return;
    }

    if (requiresOptIn && source === 'screen-audio') {
      this.pendingScreenAudioProducers.set(userId, producerId);
      if (this.isUserScreenTunedIn(userId, store)) {
        await this.consumeProducer(producerId, userId, 'audio');
      }
      return;
    }

    if (this.mediaEncryption) {
      const channelId = store.activeChannelId;
      if (channelId) {
        await this.addDecryptKeyForUser(channelId, userId);
      }
    }

    if (source === 'camera') {
      const videoOnCount = Object.values(store.participants).filter((p) => p.isVideoOn).length;
      if (videoOnCount >= store.maxVideoSlots) {
        console.warn(
          `[new-producer] Video slot limit reached (${store.maxVideoSlots}), skipping camera consume for ${userId}`
        );
        store.updateParticipant(userId, { isVideoOn: true });
        return;
      }
    }

    await this.consumeProducer(producerId, userId, kind as mediasoupTypes.MediaKind);
    if (source === 'camera') store.updateParticipant(userId, { isVideoOn: true });
    if (source === 'screen') store.updateParticipant(userId, { isScreenSharing: true });

    this.onProducerAdded?.(producerId, userId, source);
  }

  private setupSocketListeners(): void {
    if (!this.socket) return;

    this.socket.on('new-producer', (event) => this.handleNewProducer(event));

    this.socket.on('producer-paused', ({ producerId: _producerId, userId }) => {
      useVoiceStore.getState().updateParticipant(userId, { isMuted: true });
    });

    this.socket.on('producer-resumed', ({ producerId: _producerId, userId }) => {
      useVoiceStore.getState().updateParticipant(userId, { isMuted: false });
    });

    // Self-deafen broadcast from another participant (#685) — mirrors the
    // producer-paused/resumed mute handlers above.
    this.socket.on(
      'participant-deafen-changed',
      ({ userId, isDeafened }: { userId: string; isDeafened: boolean }) => {
        useVoiceStore.getState().updateParticipant(userId, { isDeafened });
      }
    );

    this.socket.on(
      'participant-testing-changed',
      ({ userId, isTesting }: { userId: string; isTesting: boolean }) => {
        useVoiceStore.getState().updateParticipant(userId, { isTesting });
      }
    );

    this.socket.on('producer-closed', ({ producerId, userId, source }) => {
      // Find and close the corresponding consumer
      for (const [consumerId, consumer] of this.consumers) {
        if (consumer.producerId === producerId) {
          consumer.close();
          this.consumers.delete(consumerId);
          this.decoderBudgetSampler.deleteConsumer(consumerId);
          this.consumerMeta.delete(consumerId);
          this.lastPreferredLayerKeyByConsumer.delete(consumerId);
          this.pauseCoordinator.clearConsumer(consumerId);
          break;
        }
      }

      const store = useVoiceStore.getState();
      if (source === 'camera')
        store.updateParticipant(userId, { isVideoOn: false, videoStream: undefined });
      else if (source === 'screen') {
        this.handleScreenProducerClosed(producerId, userId, store);
      } else if (source === 'screen-audio') {
        store.updateParticipant(userId, { screenAudioStream: undefined });
        this.pendingScreenAudioProducers.delete(userId);
      }

      // Notify PiP proxy so open PiP windows can close their consumers
      this.onProducerClosed?.(producerId, userId);
    });

    // permissions-changed (CV-CAN-007 P1): the control plane revoked this peer's
    // mid-session voice permissions and the media plane already closed the listed
    // producers server-side, so forwarding has stopped. But that server-side close
    // does NOT stop THIS client's local capture — the producer-closed self-echo
    // above only tears down consumers/store state — so the camera/mic hardware
    // (and its indicator light) would keep running after the revocation. Close each
    // revoked source through the normal local-cleanup path (closeProducer stops the
    // underlying tracks and resets store state). Awaited sequentially so the paired
    // screen / screen-audio cleanup cannot race. Idempotent: the server producer is
    // already gone, so the redundant close-producer emit is a server-side no-op.
    this.socket.on(
      'permissions-changed',
      async ({
        closedSources,
      }: {
        channelId: string;
        permissions: string;
        closedSources: string[];
      }) => {
        if (!Array.isArray(closedSources)) return;
        for (const source of closedSources) {
          await this.closeProducer(source);
        }
        // closeProducer('mic') stops the local mic tracks/VAD via cleanupMicState
        // but never touches mute state, and the producer-closed self-echo has no
        // mic branch. Left alone the UI would still show the user as unmuted after
        // the server revoked their mic, and toggleMute() would early-return because
        // there is no mic producer to resume. Reflect the forced mic-off in the
        // store and channel sidebar so local state matches the SFU.
        if (closedSources.includes('mic')) {
          const store = useVoiceStore.getState();
          store.setMuted(true);
          this.applyOptimisticMute(store, true);
        }
      }
    );

    this.socket.on(
      'user-joined',
      async ({
        userId,
        username,
        displayName,
        avatarUrl,
        e2eeEpoch,
        isDeafened,
        isTesting,
      }: {
        userId: string;
        username: string;
        displayName?: string;
        avatarUrl?: string | null;
        e2eeEpoch?: number;
        isDeafened?: boolean;
        isTesting?: boolean;
      }) => {
        // Upsert roster fields plus the optional authoritative reconnect flags.
        // Never default absent flags or media fields: the consume path may have
        // already populated them in the join-vs-consume race (#1873).
        useVoiceStore.getState().upsertParticipant(userId, {
          username,
          displayName,
          avatarUrl: avatarUrl ?? undefined,
          ...(typeof isDeafened === 'boolean' ? { isDeafened } : {}),
          ...(typeof isTesting === 'boolean' ? { isTesting } : {}),
        });

        // Solo bandwidth saving: exit solo mode when someone joins
        this.checkSoloBandwidthSaving();

        // E2EE: add decrypt key for new participant & rotate keys (new epoch)
        if (this.mediaEncryption) {
          const channelId = useVoiceStore.getState().activeChannelId;
          if (channelId) {
            const targetEpoch =
              typeof e2eeEpoch === 'number'
                ? e2eeEpoch
                : this.mediaEncryption.getCurrentKeyId() + 1;
            await this.addDecryptKeyForUser(channelId, userId, targetEpoch);
            this.debouncedRotateE2EEKeys();
          }
        }
      }
    );

    this.socket.on('user-left', async (event: UserLeftEvent) => {
      await this.handleUserLeft(event);
    });

    // E2EE: periodic epoch sync — recover from missed join/leave events
    this.socket.on('epoch-sync', async ({ epoch }: { epoch: number }) => {
      if (!this.mediaEncryption) return;
      const localEpoch = this.mediaEncryption.getCurrentKeyId();
      if (epoch > localEpoch) {
        const gap = epoch - localEpoch;
        console.debug(
          `E2EE epoch sync: local=${localEpoch}, server=${epoch}, catching up ${gap} steps`
        );
        try {
          const channelId = useVoiceStore.getState().activeChannelId;
          if (channelId) {
            await this.addDecryptKeysForActiveParticipantsAtEpoch(channelId, epoch);
          }
          if (this.e2eeWorker) {
            this.e2eeWorker.postMessage({
              type: 'catchUpToEpoch',
              targetEpoch: epoch,
            } satisfies E2EEWorkerMessage);
          }
          await this.mediaEncryption.catchUpToEpoch(epoch);
        } catch (err) {
          console.error('E2EE: epoch catch-up failed — decrypt may fail until rejoin', {
            localEpoch,
            serverEpoch: epoch,
            error: err instanceof Error ? err.message : err,
          });
        }
      }
    });

    this.socket.on('active-speaker', ({ userId }) => {
      const store = useVoiceStore.getState();
      // Clear previous speaker
      if (store.activeSpeakerId && store.activeSpeakerId !== userId) {
        store.updateParticipant(store.activeSpeakerId, { isSpeaking: false });
      }
      if (userId) {
        store.setActiveSpeaker(userId);
        store.updateParticipant(userId, { isSpeaking: true });
      } else {
        // Silence — no one is speaking
        store.setActiveSpeaker(null);
      }
    });

    this.socket.on('room-codec-floor', ({ codecFloor }: { codecFloor: string[] | null }) => {
      const store = useVoiceStore.getState();
      const previousFloor = store.codecFloor;
      store.setCodecFloor(codecFloor);
      void this.handleCodecFloorChange(previousFloor, codecFloor).catch((err) =>
        console.warn('[codec-floor] Floor-change re-produce failed:', errorMessage(err))
      );
    });

    this.socket.on('camera-layering-gate', ({ enabled }: { enabled: boolean }) => {
      const nextEnabled = enabled === true;
      if (this.cameraLayeringEnabled === nextEnabled) return;
      this.cameraLayeringEnabled = nextEnabled;
      this.scheduleCameraLayeringReproduce();
    });

    // #1924 server-authoritative screen-layering gate. Route through the screen
    // re-produce serializer (mirroring camera) so rapid gate edges — or a gate edge
    // racing a Support-Simulcast toggle — coalesce into one queued drain instead of
    // overlapping two fastReproduceScreen() calls. fastReproduceScreen is internally
    // guarded (no-ops without an active screen producer) and rides the existing
    // stopTracks:false (#1902) + fail-closed capture-stop (CWE-212) path.
    this.socket.on('screen-layering-gate', ({ enabled }: { enabled: boolean }) => {
      const nextEnabled = enabled === true;
      if (this.screenLayeringEnabled === nextEnabled) return;
      this.screenLayeringEnabled = nextEnabled;
      this.scheduleScreenLayeringReproduce();
    });

    this.socket.on('consumer-closed', ({ consumerId }) => {
      const consumer = this.consumers.get(consumerId);
      if (consumer) {
        // If this was a screen share consumer, clean up tunedInScreenShares
        const store = useVoiceStore.getState();
        const producerId = consumer.producerId;
        if (producerId && producerId in store.tunedInScreenShares) {
          store.tuneOut(producerId);
        }

        consumer.close();
        this.consumers.delete(consumerId);
        this.decoderBudgetSampler.deleteConsumer(consumerId);
        this.consumerMeta.delete(consumerId);
        this.lastPreferredLayerKeyByConsumer.delete(consumerId);
        this.pauseCoordinator.clearConsumer(consumerId);
      }
    });

    this.socket.on('disconnect', (reason) => {
      // eslint-disable-next-line no-restricted-syntax -- reason is a string, not an Error; no err.cause propagation risk
      console.warn('Media plane disconnected:', reason);
      if (reason === 'io server disconnect') {
        // Server forcibly disconnected us — full cleanup
        this.emergencyCleanup();
      } else if (reason !== 'io client disconnect') {
        // Unexpected disconnect (transport close, ping timeout, etc.)
        // Don't play for voluntary leaves — leaveChannel already plays voice-leave
        useVoiceStore.getState().setConnectionState('reconnecting');
        notificationSoundService.play('disconnect');
        // No token handling needed here: the socket's auth CALLBACK
        // (see joinChannel) re-reads the store token on every
        // reconnection attempt (#1790).
      }
    });

    this.socket.on('connect', () => {
      // Socket.IO reconnected after a transient drop (VPN toggle, interface
      // change). The media-plane removed our participant the moment the old
      // socket dropped, so the previous media session is unrecoverable
      // server-side — a bare state flip to 'connected' would leave a dead
      // call. Rebuild the session instead (#1790).
      if (useVoiceStore.getState().connectionState === 'reconnecting') {
        void this.resumeAfterReconnect();
      }
    });

    // Socket.IO exhausted all reconnection attempts
    this.socket.io.on('reconnect_failed', () => {
      this.emergencyCleanup();
    });

    this.socket.on('error', ({ message }) => {
      // eslint-disable-next-line no-restricted-syntax -- message is a string from a server-supplied socket error payload, not an Error; no err.cause propagation risk
      console.error('Media plane error:', message);
    });

    // Server-enforced mute/deafen
    this.socket.on(
      'server-mute-changed',
      ({ userId, serverMuted }: { userId: string; serverMuted: boolean }) => {
        const store = useVoiceStore.getState();
        store.updateParticipant(userId, { serverMuted });

        const localUserId = useUserStore.getState().user?.id;
        if (userId === localUserId && serverMuted) {
          // Enforce locally — pause mic producer
          const micProducer = this.producers.get('mic');
          if (micProducer && !micProducer.paused) {
            micProducer.pause();
            this.socket?.emit('pause-producer', { producerId: micProducer.id });
          }
          store.setMuted(true);
        }
      }
    );

    this.socket.on(
      'server-deafen-changed',
      ({ userId, serverDeafened }: { userId: string; serverDeafened: boolean }) => {
        const store = useVoiceStore.getState();
        store.updateParticipant(userId, { serverDeafened, serverMuted: serverDeafened });

        const localUserId = useUserStore.getState().user?.id;
        if (userId === localUserId && serverDeafened) {
          // Enforce locally — pause all audio consumers and mic
          for (const [, consumer] of this.consumers) {
            if (consumer.kind === 'audio') {
              consumer.pause();
            }
          }
          const micProducer = this.producers.get('mic');
          if (micProducer && !micProducer.paused) {
            micProducer.pause();
            this.socket?.emit('pause-producer', { producerId: micProducer.id });
          }
          store.setMuted(true);
          store.setDeafened(true);
        }
      }
    );
  }

  // ─── Decoder Budget Profiling (IGNIS insight) ─────────────────

  /**
   * Profile decode performance of video consumers and adjust preferred SVC
   * layers to prevent decoder queue buildup.
   *
   * IGNIS formula: FPS_safe = 0.8 × (1000 / T_decode_p95)
   * Exceeding the decoder budget causes exponential queue buildup
   * ("counterintuitive latency" — pushing higher quality actually increases
   * total latency due to decoder overload).
   */
  private startDecoderBudgetProfiling(): void {
    // Profile every 5 seconds for the first 30 seconds, then every 30 seconds
    let probeCount = 0;
    const maxInitialProbes = 6; // 6 × 5s = 30s initial period

    this.decoderProfilingTimer = setInterval(() => {
      probeCount++;

      // Switch to slower interval after initial profiling
      if (probeCount === maxInitialProbes + 1 && this.decoderProfilingTimer) {
        clearInterval(this.decoderProfilingTimer);
        this.decoderProfilingTimer = setInterval(() => {
          void this.runDecoderProfilingTick();
        }, 30_000);
      }

      void this.runDecoderProfilingTick();
    }, 5_000);
  }

  /** Run at most one timer-triggered stats pass at a time. */
  private async runDecoderProfilingTick(): Promise<void> {
    if (this.decoderProfilingInFlight) return;
    this.decoderProfilingInFlight = true;
    try {
      await this.profileDecoders();
    } catch (err) {
      console.warn('IGNIS decoder profiling failed:', errorMessage(err));
    } finally {
      this.decoderProfilingInFlight = false;
    }
  }

  /**
   * Full IGNIS decoder budget profiling.
   *
   * Zone thresholds:
   *   Green:  rho < 0.80  (inside the safe decoder budget)
   *   Yellow: 0.80 <= rho < 0.925 (approaching limit)
   *   Red:    rho >= 0.925 (near-critical, 92.5% capacity)
   *
   * Formulas:
   *   Load Ratio:  rho = T_d_p95 / T_f = (T_d_p95 × FPS) / 1000
   *   Safe FPS:    FPS_safe = margin × (1000 / T_d_p95)
   *   Risk Score:  R = (FPS × T_d_p95) / 1000
   *
   * Actions:
   *   Green:  full quality, no intervention
   *   Yellow: lower temporal layer by 1
   *   Red:    lower spatial layer by 1, if already lowest → pause lowest-priority consumer
   */
  /** Handle RED zone decoder overload: reduce layers or pause lowest-priority consumer */
  private handleRedZone(
    consumer: mediasoupTypes.Consumer,
    consumerWithLayers: ConsumerWithLayers,
    currentLayers: ConsumerLayerSelection | undefined,
    rho: number,
    p95DecodeMs: number,
    currentFps: number
  ): void {
    if (currentLayers && currentLayers.spatialLayer > 0) {
      const pressureResult = this.tryEmitCameraPressureLayerRequest(consumer.id, currentLayers);
      if (pressureResult === 'emitted') {
        console.warn(
          'IGNIS RED: lowering camera layers via render policy for consumer:',
          consumer.id,
          'rho:',
          rho.toFixed(2),
          'decode p95ms:',
          p95DecodeMs.toFixed(1),
          'fps:',
          currentFps // eslint-disable-line no-restricted-syntax -- currentFps is a number from RTCStatsReport, not an Error
        );
        this.decoderBudgetSampler.deleteConsumer(consumer.id);
        return;
      }
      if (pressureResult === 'handled') return;
      consumerWithLayers.setPreferredLayers({
        spatialLayer: currentLayers.spatialLayer - 1,
        temporalLayer: currentLayers.temporalLayer,
      });
      console.warn(
        'IGNIS RED: lowering spatial layer for consumer:',
        consumer.id,
        'rho:',
        rho.toFixed(2),
        'decode p95ms:',
        p95DecodeMs.toFixed(1),
        'fps:',
        currentFps // eslint-disable-line no-restricted-syntax -- currentFps is a number from RTCStatsReport, not an Error
      );
      this.decoderBudgetSampler.deleteConsumer(consumer.id);
      return;
    }

    if (currentLayers && currentLayers.temporalLayer > 0) {
      const pressureResult = this.tryEmitCameraPressureLayerRequest(consumer.id, currentLayers);
      if (pressureResult === 'emitted') {
        console.warn(
          'IGNIS RED: lowering camera temporal demand via render policy for consumer:',
          consumer.id,
          'rho:',
          rho.toFixed(2)
        );
        this.decoderBudgetSampler.deleteConsumer(consumer.id);
        return;
      }
      if (pressureResult === 'handled') return;
      consumerWithLayers.setPreferredLayers({
        spatialLayer: currentLayers.spatialLayer,
        temporalLayer: currentLayers.temporalLayer - 1,
      });
      console.warn(
        'IGNIS RED: lowering temporal layer for consumer:',
        consumer.id,
        'rho:',
        rho.toFixed(2)
      );
      this.decoderBudgetSampler.deleteConsumer(consumer.id);
      return;
    }

    // Already at lowest layers — pause lowest-priority consumer when another
    // active video remains available to supply recovery evidence.
    if (this.pauseLowestPriorityConsumer(consumer)) {
      this.decoderBudgetSampler.deleteConsumer(consumer.id);
    }
  }

  /** Pause a consumer for decoder relief, preferring to pause camera over screen share */
  private pauseLowestPriorityConsumer(consumer: mediasoupTypes.Consumer): boolean {
    const hasAnotherActiveVideo = [...this.consumers.values()].some(
      (candidate) =>
        candidate.id !== consumer.id &&
        candidate.kind === 'video' &&
        !candidate.closed &&
        !candidate.paused
    );
    if (!hasAnotherActiveVideo) {
      console.warn(`IGNIS RED: keeping sole video consumer ${consumer.id} active at lowest layers`);
      return false;
    }

    const tunedIn = useVoiceStore.getState().tunedInScreenShares;
    const isScreenShare = Object.values(tunedIn).includes(consumer.id);

    if (!isScreenShare) {
      this.pauseCoordinator.addReason(consumer.id, 'ignis');
      console.warn(`IGNIS RED: pausing camera consumer ${consumer.id} — no layers to reduce`);
      return true;
    }

    // Try to find a still-decoding camera consumer to pause instead of the screen share
    for (const [, c] of this.consumers) {
      if (c.kind === 'video' && !c.paused && !Object.values(tunedIn).includes(c.id)) {
        this.pauseCoordinator.addReason(c.id, 'ignis');
        console.warn(
          `IGNIS RED: pausing camera consumer ${c.id} instead of screen share ${consumer.id}`
        );
        return true;
      }
    }

    this.pauseCoordinator.addReason(consumer.id, 'ignis');
    console.warn(`IGNIS RED: pausing screen share consumer ${consumer.id} — no camera to demote`);
    return true;
  }

  /** Classify decoder load into a health zone and apply layer adjustments. */
  private classifyAndHandleDecoderZone(
    consumer: mediasoupTypes.Consumer,
    rho: number,
    p95DecodeMs: number,
    currentFps: number,
    worstZone: DecoderHealthZone
  ): DecoderHealthZone {
    const consumerWithLayers = consumer as unknown as ConsumerWithLayers;
    const currentLayers = consumerWithLayers.currentLayers;

    if (rho >= 0.925) {
      this.handleRedZone(consumer, consumerWithLayers, currentLayers, rho, p95DecodeMs, currentFps);
      return 'red';
    }
    if (rho >= 0.8) {
      return this.handleYellowZone(
        consumer,
        consumerWithLayers,
        currentLayers,
        rho,
        p95DecodeMs,
        currentFps,
        worstZone
      );
    }
    return worstZone;
  }

  private handleYellowZone(
    consumer: mediasoupTypes.Consumer,
    consumerWithLayers: ConsumerWithLayers,
    currentLayers: ConsumerLayerSelection | undefined,
    rho: number,
    p95DecodeMs: number,
    currentFps: number,
    worstZone: DecoderHealthZone
  ): DecoderHealthZone {
    if (!currentLayers || currentLayers.temporalLayer <= 0) {
      return VoiceService.mergeDecoderZones(worstZone, 'yellow');
    }

    const pressureResult = this.tryEmitCameraPressureLayerRequest(consumer.id, currentLayers);
    if (pressureResult === 'emitted') {
      console.warn(
        'IGNIS YELLOW: lowering camera layers via render policy for consumer:',
        consumer.id,
        'rho:',
        rho.toFixed(2),
        'decode p95ms:',
        p95DecodeMs.toFixed(1),
        'fps:',
        currentFps // eslint-disable-line no-restricted-syntax -- currentFps is a number from RTCStatsReport, not an Error
      );
      this.decoderBudgetSampler.deleteConsumer(consumer.id);
      return VoiceService.mergeDecoderZones(worstZone, 'yellow');
    }
    if (pressureResult === 'handled') return VoiceService.mergeDecoderZones(worstZone, 'yellow');

    consumerWithLayers.setPreferredLayers({
      spatialLayer: currentLayers.spatialLayer,
      temporalLayer: currentLayers.temporalLayer - 1,
    });
    console.warn(
      'IGNIS YELLOW: lowering temporal layer for consumer:',
      consumer.id,
      'rho:',
      rho.toFixed(2),
      'decode p95ms:',
      p95DecodeMs.toFixed(1),
      'fps:',
      currentFps // eslint-disable-line no-restricted-syntax -- currentFps is a number from RTCStatsReport, not an Error
    );
    this.decoderBudgetSampler.deleteConsumer(consumer.id);
    return VoiceService.mergeDecoderZones(worstZone, 'yellow');
  }

  private static mergeDecoderZones(
    current: DecoderHealthZone,
    next: DecoderHealthZone
  ): DecoderHealthZone {
    if (current === 'red' || next === 'red') return 'red';
    if (current === 'yellow' || next === 'yellow') return 'yellow';
    return 'green';
  }

  private async selectDecoderReport(
    consumer: mediasoupTypes.Consumer
  ): Promise<SelectedDecoderStatsReport | null> {
    try {
      const stats = await consumer.getStats();
      const negotiatedSsrcs = new Set<number>();
      for (const encoding of consumer.rtpParameters.encodings ?? []) {
        if (typeof encoding.ssrc === 'number') negotiatedSsrcs.add(encoding.ssrc);
      }
      return selectInboundVideoDecoderReport(stats.entries(), negotiatedSsrcs);
    } catch {
      return null;
    }
  }

  private observeDecoderConsumer(
    consumer: mediasoupTypes.Consumer,
    selected: SelectedDecoderStatsReport | null,
    worstZone: DecoderHealthZone | null
  ): DecoderHealthZone | null {
    const decoded = this.decoderBudgetSampler.observe({
      consumerId: consumer.id,
      reportId: selected?.reportId ?? consumer.id,
      paused: consumer.paused,
      report: selected?.report,
      observedAtMs: performance.timeOrigin + performance.now(),
    });
    if (!decoded.usable) return worstZone;

    return this.classifyAndHandleDecoderZone(
      consumer,
      decoded.rho,
      decoded.p95DecodeMs,
      decoded.currentFps,
      worstZone ?? 'green'
    );
  }

  private async profileDecoders(): Promise<void> {
    let worstZone: DecoderHealthZone | null = null;

    for (const consumer of this.consumers.values()) {
      if (consumer.kind !== 'video') continue;

      let selected: SelectedDecoderStatsReport | null = null;
      if (!consumer.paused) selected = await this.selectDecoderReport(consumer);
      worstZone = this.observeDecoderConsumer(consumer, selected, worstZone);
    }

    if (!worstZone) return;
    useVoiceStore.getState().setDecoderHealth(worstZone);

    this.updateDecoderRecoveryState(worstZone);
  }

  /**
   * Track consecutive green cycles and trigger gradual recovery once the
   * hysteresis threshold is met. Extracted from profileDecoders to keep that
   * method's cognitive complexity within bounds.
   */
  private updateDecoderRecoveryState(worstZone: DecoderHealthZone): void {
    if (worstZone !== 'green') {
      this.consecutiveGreenIntervals = 0;
      return;
    }
    this.consecutiveGreenIntervals++;
    if (this.consecutiveGreenIntervals >= VoiceService.IGNIS_RECOVERY_GREEN_INTERVALS) {
      this.recoverFromDecoderThrottle();
      this.clearRemoteVideoPressureAndEmit();
      this.consecutiveGreenIntervals = 0;
    }
  }

  /**
   * Resume ONE previously-IGNIS-paused consumer (gradual step-up).
   * Called only after IGNIS_RECOVERY_GREEN_INTERVALS consecutive green cycles.
   * Resumes one consumer per green window to avoid re-saturating the decoder.
   */
  private recoverFromDecoderThrottle(): void {
    for (const id of this.pauseCoordinator.consumersWithReason('ignis')) {
      const consumer = this.consumers.get(id);
      if (!consumer) {
        this.pauseCoordinator.clearConsumer(id); // gone — prune
        this.decoderBudgetSampler.deleteConsumer(id);
        continue;
      }
      // Release IGNIS's hold. The consumer resumes locally unless another reason
      // (visibility / manual) still holds — that is the #1541 anti-clobber fix.
      this.pauseCoordinator.removeReason(id, 'ignis');
      console.warn(`IGNIS GREEN recovery: resuming consumer ${id}`);
      return; // one per green window — gradual step-up
    }
  }

  // ─── E2EE ─────────────────────────────────────────────────────

  /** Highest version observed by either current-key or history-key fetches. */
  private highestSeenE2EEKeyVersion(channelId: string): number {
    // Tolerate older test harnesses while all production implementations expose
    // the read-only reconciliation surface.
    if (typeof e2eeService.getHighestSeenKeyVersion !== 'function') return 0;
    return e2eeService.getHighestSeenKeyVersion(channelId);
  }

  /** True only while this exact channel/user/init still owns the shared slots. */
  private isCurrentE2EEInit(context: E2EEInitContext): boolean {
    return (
      context.generation === this.e2eeInitGeneration &&
      context.channelId === useVoiceStore.getState().activeChannelId &&
      context.userId === useUserStore.getState().user?.id &&
      context.expectedMediaEncryption === this.mediaEncryption &&
      context.expectedWorker === this.e2eeWorker
    );
  }

  private assertCurrentE2EEInit(context: E2EEInitContext): void {
    if (!this.isCurrentE2EEInit(context)) {
      throw new E2EEInitializationSupersededError();
    }
  }

  /**
   * Invalidate pending initializers before cleanup or a successor initializer
   * can yield. A provisional (not-yet-committed) listener is request-owned and
   * is removed here; a committed listener remains live until the shared
   * subscription is synchronously replaced or explicitly torn down.
   */
  private invalidatePendingE2EEInit(): void {
    this.e2eeInitGeneration++;
    const pending = this.pendingE2EEInitContext;
    if (pending && !pending.ownsSharedRotationSubscription) {
      pending.rotationOff?.();
      pending.rotationOff = null;
    }
    this.pendingE2EEInitContext = null;
    this.e2eeInitInFlight = null;
  }

  /** Subscribe before the first key fetch so edge-triggered rotations are buffered. */
  private beginE2EEInit(channelId: string): E2EEInitContext {
    const userId = useUserStore.getState().user?.id;
    if (!userId) throw new Error('No local userId for E2EE init');
    if (useVoiceStore.getState().activeChannelId !== channelId) {
      throw new E2EEInitializationSupersededError();
    }

    this.invalidatePendingE2EEInit();
    const context: E2EEInitContext = {
      generation: this.e2eeInitGeneration,
      channelId,
      userId,
      expectedMediaEncryption: this.mediaEncryption,
      expectedWorker: this.e2eeWorker,
      highestBufferedVersion: 0,
      rotationOff: null,
      ownsSharedRotationSubscription: false,
      committed: false,
      liveRotations: false,
    };
    this.pendingE2EEInitContext = context;

    if (typeof e2eeService.onKeyRotation === 'function') {
      context.rotationOff = e2eeService.onKeyRotation((event) => {
        if (event.channelId !== channelId || !this.isCurrentE2EEInit(context)) return;
        context.highestBufferedVersion = Math.max(context.highestBufferedVersion, event.keyVersion);
        if (context.liveRotations) {
          void this.rebaseEncryptKey(channelId, event.keyVersion);
        }
      });
    }
    // Subscribe-before-read closes the event/read race. A by-version fetch may
    // have advanced highestSeen while the current-key cache still reports less.
    context.highestBufferedVersion = Math.max(
      context.highestBufferedVersion,
      this.highestSeenE2EEKeyVersion(channelId)
    );
    return context;
  }

  private finishE2EEInit(context: E2EEInitContext): void {
    if (this.pendingE2EEInitContext === context) {
      this.pendingE2EEInitContext = null;
    }
    if (!context.ownsSharedRotationSubscription) {
      context.rotationOff?.();
      context.rotationOff = null;
    }
  }

  /** Publish locally-built crypto state in one synchronous, fenced commit. */
  private commitE2EEInit(
    context: E2EEInitContext,
    mediaEncryption: MediaEncryption,
    worker: Worker | null,
    keyVersion: number
  ): void {
    this.assertCurrentE2EEInit(context);
    const previousMediaEncryption = this.mediaEncryption;
    const previousWorker = this.e2eeWorker;
    const previousRotationOff = this.keyRotationOff;

    this.mediaEncryption = mediaEncryption;
    this.e2eeWorker = worker;
    this.latestRequestedEncryptKeyVersion = keyVersion;
    this.keyRotationOff = context.rotationOff;
    context.expectedMediaEncryption = mediaEncryption;
    context.expectedWorker = worker;
    context.committed = true;
    context.ownsSharedRotationSubscription = true;

    if (previousRotationOff && previousRotationOff !== context.rotationOff) {
      previousRotationOff();
    }
    if (previousMediaEncryption && previousMediaEncryption !== mediaEncryption) {
      try {
        previousMediaEncryption.destroy();
      } catch {
        /* replacement is already atomically committed */
      }
    }
    if (previousWorker && previousWorker !== worker) {
      this.destroyE2EEWorkerInstance(previousWorker);
    }
  }

  /** Reconcile all rotations buffered before the new subscription goes live. */
  private async reconcileE2EEInit(context: E2EEInitContext): Promise<void> {
    while (true) {
      this.assertCurrentE2EEInit(context);
      context.highestBufferedVersion = Math.max(
        context.highestBufferedVersion,
        this.highestSeenE2EEKeyVersion(context.channelId)
      );
      const installedVersion = context.expectedMediaEncryption?.getKeyVersion() ?? 0;
      if (context.highestBufferedVersion <= installedVersion) {
        // No await exists between the final read and enabling the listener, so
        // a later event is necessarily handled by the live branch.
        context.liveRotations = true;
        return;
      }

      const targetVersion = context.highestBufferedVersion;
      await this.rebaseEncryptKey(context.channelId, targetVersion);
      this.assertCurrentE2EEInit(context);
      if ((context.expectedMediaEncryption?.getKeyVersion() ?? 0) < targetVersion) {
        throw new Error('E2EE: failed to reconcile the initialized sender key version');
      }
    }
  }

  /** Core encryption setup: derive locally, then atomically publish if still current. */
  private async initEncryptionCore(
    channelId: string,
    attempt: number,
    initContext?: E2EEInitContext
  ): Promise<void> {
    if (currentTransformPath() === 'unavailable') {
      throw new Error('E2EE: no encoded transform API available');
    }
    const ownsContext = initContext === undefined;
    const context = initContext ?? this.beginE2EEInit(channelId);
    let candidateMediaEncryption: MediaEncryption | null = null;
    let candidateWorker: Worker | null = null;
    let committed = false;

    try {
      const { channelKey: channelCSK, keyVersion } =
        await e2eeService.getChannelKeyMaterial(channelId);
      this.assertCurrentE2EEInit(context);

      const encryptKey = await deriveFrameKey(channelCSK, context.userId);
      this.assertCurrentE2EEInit(context);
      // channelCSK and keyVersion are one atomic key-service snapshot, so a
      // rotation during derivation cannot stamp old material as a new version.

      candidateMediaEncryption = new MediaEncryption();
      candidateMediaEncryption.initFromKey(encryptKey, 0);
      candidateMediaEncryption.setKeyVersion(keyVersion);
      if (currentTransformPath() === 'script-transform') {
        candidateWorker = this.createE2EEWorker();
        candidateWorker.postMessage({
          type: 'init',
          encryptKey,
          currentKeyId: 0,
          keyVersion,
        } satisfies E2EEWorkerMessage);
      }

      // This is the last check before the synchronous shared-state commit.
      this.assertCurrentE2EEInit(context);
      this.commitE2EEInit(context, candidateMediaEncryption, candidateWorker, keyVersion);
      committed = true;
      await this.reconcileE2EEInit(context);
      this.assertCurrentE2EEInit(context);

      console.debug('E2EE: encryption initialized', {
        channelId,
        userId: context.userId,
        keyVersion: context.expectedMediaEncryption?.getKeyVersion() ?? keyVersion,
        attempt: attempt + 1,
        useScriptTransform: currentTransformPath() === 'script-transform',
        transformPath: currentTransformPath(),
      });
    } finally {
      if (!committed) {
        candidateMediaEncryption?.destroy();
        if (candidateWorker) this.destroyE2EEWorkerInstance(candidateWorker);
      }
      if (ownsContext) this.finishE2EEInit(context);
    }
  }

  /**
   * #1878: re-base the sender's encrypt key onto a rotated CSK after a CONFIRMED
   * fetch. Until the fetch resolves, outgoing frames still stamp the old version
   * (the encrypt key is untouched), so there is no window where the sender
   * stamps a version it can't back with a key. Captured session identities and
   * the monotonic requested version prevent stale async results from rolling back
   * or mutating a replacement session.
   */
  private beginEncryptRebase(channelId: string, keyVersion: number): EncryptRebaseContext | null {
    if (channelId !== useVoiceStore.getState().activeChannelId) return null;
    const mediaEncryption = this.mediaEncryption;
    const e2eeWorker = this.e2eeWorker;
    const userId = useUserStore.getState().user?.id;
    if (!mediaEncryption || !userId) return null;
    if (
      keyVersion <= this.latestRequestedEncryptKeyVersion ||
      keyVersion <= mediaEncryption.getKeyVersion()
    ) {
      return null;
    }
    this.latestRequestedEncryptKeyVersion = keyVersion;
    return { channelId, keyVersion, userId, mediaEncryption, e2eeWorker };
  }

  private isCurrentEncryptRebase(context: EncryptRebaseContext): boolean {
    return (
      context.channelId === useVoiceStore.getState().activeChannelId &&
      context.userId === useUserStore.getState().user?.id &&
      this.mediaEncryption === context.mediaEncryption &&
      this.e2eeWorker === context.e2eeWorker &&
      this.latestRequestedEncryptKeyVersion === context.keyVersion
    );
  }

  private failEncryptRebase(context: EncryptRebaseContext, attempt: number, err: unknown): void {
    console.error('E2EE: sender key re-base failed closed', {
      channelId: context.channelId,
      keyVersion: context.keyVersion,
      attempt: attempt + 1,
      error: errorMessage(err),
    });
    if (this.isCurrentEncryptRebase(context)) this.emergencyCleanup();
  }

  private async tryEncryptRebase(
    context: EncryptRebaseContext,
    attempt: number,
    finalAttempt: number
  ): Promise<'retry' | 'stop'> {
    if (!this.isCurrentEncryptRebase(context)) return 'stop';

    try {
      const newCsk = await e2eeService.getChannelKeyByVersion(
        context.channelId,
        context.keyVersion
      );
      if (!this.isCurrentEncryptRebase(context)) return 'stop';
      const newEncryptKey = await deriveFrameKey(newCsk, context.userId);
      if (!this.isCurrentEncryptRebase(context)) return 'stop';

      // Advance the local encrypt key + stamped version together.
      context.mediaEncryption.initFromKey(newEncryptKey, 0);
      context.mediaEncryption.setKeyVersion(context.keyVersion);
      context.e2eeWorker?.postMessage({
        type: 'init',
        encryptKey: newEncryptKey,
        currentKeyId: 0,
        keyVersion: context.keyVersion,
      } satisfies E2EEWorkerMessage);
      console.debug('E2EE: sender re-based encrypt key on CSK rotation', {
        channelId: context.channelId,
        keyVersion: context.keyVersion,
      });
      return 'stop';
    } catch (err) {
      if (!this.isCurrentEncryptRebase(context)) return 'stop';
      if (attempt < finalAttempt && isRetryableEncryptRebaseError(err)) return 'retry';
      this.failEncryptRebase(context, attempt, err);
      return 'stop';
    }
  }

  private async rebaseEncryptKey(channelId: string, keyVersion: number): Promise<void> {
    const context = this.beginEncryptRebase(channelId, keyVersion);
    if (!context) return;

    const retryDelays = [500, 1000, 2000];
    for (let attempt = 0; attempt <= retryDelays.length; attempt++) {
      const result = await this.tryEncryptRebase(context, attempt, retryDelays.length);
      if (result === 'stop') return;
      await new Promise((resolve) => setTimeout(resolve, retryDelays[attempt]));
      if (!this.isCurrentEncryptRebase(context)) return;
    }
  }

  /** Destroy only state synchronously committed by this still-current request. */
  private failCurrentE2EEInit(context: E2EEInitContext): void {
    if (!this.isCurrentE2EEInit(context)) return;
    const committed = context.committed;
    const rotationOff = context.rotationOff;
    this.invalidatePendingE2EEInit();

    if (!committed) return;
    if (this.keyRotationOff === rotationOff) {
      rotationOff?.();
      this.keyRotationOff = null;
    }
    if (this.mediaEncryption === context.expectedMediaEncryption) {
      this.mediaEncryption?.destroy();
      this.mediaEncryption = null;
    }
    if (this.e2eeWorker === context.expectedWorker) {
      this.terminateE2EEWorker();
    }
    this.latestRequestedEncryptKeyVersion = 0;
    context.rotationOff = null;
    context.ownsSharedRotationSubscription = false;
  }

  private async runE2EEInitAttempt(
    context: E2EEInitContext,
    attempt: number,
    retryDelay: number | undefined
  ): Promise<E2EEInitAttemptResult> {
    try {
      await this.initEncryptionCore(context.channelId, attempt, context);
      return { succeeded: true };
    } catch (err) {
      if (!this.isCurrentE2EEInit(context)) {
        throw new E2EEInitializationSupersededError();
      }
      console.warn('E2EE: initEncryption attempt failed', {
        channelId: context.channelId,
        attempt: attempt + 1,
        error: err instanceof Error ? err.message : err,
      });

      if (retryDelay !== undefined) {
        const delay = retryDelayForError(err, retryDelay);
        await new Promise((r) => setTimeout(r, delay));
        this.assertCurrentE2EEInit(context);
      }
      return { succeeded: false, error: err };
    }
  }

  private throwE2EEInitFailure(context: E2EEInitContext, lastError: unknown): never {
    // All retries exhausted — fail closed only if this exact request still owns
    // the shared slots. A stale loop cannot terminate a replacement session.
    this.assertCurrentE2EEInit(context);
    this.failCurrentE2EEInit(context);
    if (lastError instanceof Error) throw lastError;
    let stringified = String(lastError);
    if (typeof lastError === 'object' && lastError !== null) {
      try {
        stringified = JSON.stringify(lastError);
      } catch {
        // circular / unstringifiable object — keep the String() fallback
      }
    }
    const detail = lastError == null ? '' : ` (${stringified})`;
    throw new Error(`E2EE: failed to initialize encryption after retries${detail}`);
  }

  /** Bounded retry loop owned by one immutable channel/user/init context. */
  private async runE2EEInitWithRetry(context: E2EEInitContext): Promise<void> {
    const retryDelays = [500, 1000, 2000];
    let lastError: unknown;

    for (let attempt = 0; attempt <= retryDelays.length; attempt++) {
      const result = await this.runE2EEInitAttempt(context, attempt, retryDelays[attempt]);
      if (result.succeeded) return;
      lastError = result.error;
    }

    this.throwE2EEInitFailure(context, lastError);
  }

  /** Initialize media encryption (single-flight for one channel/user session). */
  private async initEncryption(channelId: string): Promise<void> {
    const userId = useUserStore.getState().user?.id;
    const inFlight = this.e2eeInitInFlight;
    if (
      userId &&
      inFlight?.context.generation === this.e2eeInitGeneration &&
      inFlight.context.channelId === channelId &&
      inFlight.context.userId === userId &&
      useVoiceStore.getState().activeChannelId === channelId
    ) {
      return inFlight.promise;
    }

    const context = this.beginE2EEInit(channelId);
    const promise = this.runE2EEInitWithRetry(context);
    const flight: E2EEInitFlight = { context, promise };
    this.e2eeInitInFlight = flight;
    try {
      await promise;
    } finally {
      if (this.e2eeInitInFlight === flight) this.e2eeInitInFlight = null;
      this.finishE2EEInit(context);
    }
  }

  /** Build an unshared E2EE Worker candidate for RTCRtpScriptTransform. */
  private createE2EEWorker(): Worker {
    const worker = new Worker(new URL('../../workers/e2eeWorker.ts', import.meta.url), {
      type: 'module',
    });
    worker.onmessage = (event: MessageEvent<E2EEMainMessage>) => {
      if (this.e2eeWorker !== worker) return;
      this.handleWorkerMessage(event.data);
    };
    worker.onerror = (err) => {
      if (this.e2eeWorker !== worker) return;
      console.error('E2EE Worker error:', errorMessage(err));
    };
    return worker;
  }

  /** Destroy one Worker without consulting or mutating the shared Worker slot. */
  private destroyE2EEWorkerInstance(worker: Worker): void {
    try {
      worker.postMessage({ type: 'destroy' } satisfies E2EEWorkerMessage);
    } catch {
      /* still terminate a failed Worker */
    }
    try {
      worker.terminate();
    } catch {
      /* idempotent teardown */
    }
  }

  /** Terminate and clean up the E2EE Worker */
  private terminateE2EEWorker(): void {
    const worker = this.e2eeWorker;
    if (!worker) return;
    this.e2eeWorker = null;
    this.destroyE2EEWorkerInstance(worker);
  }

  /** Handle messages from the E2EE Worker */
  private handleWorkerMessage(msg: E2EEMainMessage): void {
    switch (msg.type) {
      case 'rotationComplete':
        // Sync the main-thread epoch tracker with the Worker's authoritative state.
        // The main-thread MediaEncryption is only used for getCurrentKeyId() calls
        // (key derivation uses the epoch to pre-ratchet new decrypt keys).
        this.mediaEncryption?.setCurrentKeyId(msg.newKeyId);
        break;

      case 'requestRecovery': {
        const channelId = useVoiceStore.getState().activeChannelId;
        if (channelId) {
          console.debug(`E2EE: Worker requested recovery for ${msg.senderUserId}`);
          e2eeService.invalidateChannelKey(channelId);
          this.addDecryptKeyForUser(channelId, msg.senderUserId).catch(() => {});
        }
        break;
      }

      case 'requestFrameKey': {
        // #1878/#1895: the worker hit a typed decrypt miss for an exact
        // (sender, keyVersion, keyId). Provision the version-specific key via the
        // shared path — the same one the legacy createEncodedStreams pipeline
        // uses (#1895), so both decrypt paths recover identically.
        const channelId = useVoiceStore.getState().activeChannelId;
        if (channelId) {
          this.provisionFrameKey(channelId, msg.senderUserId, msg.keyVersion, msg.keyId);
        }
        break;
      }

      case 'requestKeyframe': {
        if (useVoiceStore.getState().activeChannelId) {
          this.socket?.emit('request-keyframe', { senderUserId: msg.senderUserId });
        }
        break;
      }

      case 'decryptStats': {
        // Floating by design; the catch keeps an unexpected throw in the
        // reattach/close branches from becoming an unhandled rejection.
        void this.evaluateBypassProbe(msg.probeId, msg.entered).catch((err) =>
          console.error('E2EE: bypass probe evaluation failed:', errorMessage(err))
        );
        break;
      }

      case 'log':
        // Forward Worker logs to renderer console
        // eslint-disable-next-line no-console -- worker log bridge; levels beyond .warn/.error/.debug need to propagate as the worker emitted them for parity with worker-side output
        console[msg.level](msg.message, msg.data || '');
        break;
    }
  }

  private async handleUserLeft({ userId, e2eeEpoch }: UserLeftEvent): Promise<void> {
    useVoiceStore.getState().removeParticipant(userId);
    this.checkSoloBandwidthSaving();
    await this.rotateE2EEAfterUserLeft(e2eeEpoch);
  }

  private async rotateE2EEAfterUserLeft(e2eeEpoch: number | undefined): Promise<void> {
    const mediaEncryption = this.mediaEncryption;
    if (!mediaEncryption) return;

    const targetEpoch = e2eeEpoch ?? mediaEncryption.getCurrentKeyId() + 1;
    const channelId = useVoiceStore.getState().activeChannelId;
    if (channelId) {
      await this.addDecryptKeysForActiveParticipantsAtEpoch(channelId, targetEpoch);
    }

    await this.catchUpToAuthoritativeLeaveEpoch(e2eeEpoch);
  }

  private async catchUpToAuthoritativeLeaveEpoch(e2eeEpoch: number | undefined): Promise<void> {
    const mediaEncryption = this.mediaEncryption;
    if (!mediaEncryption) return;

    if (e2eeEpoch === undefined) {
      this.debouncedRotateE2EEKeys();
      return;
    }

    const localEpoch = mediaEncryption.getCurrentKeyId();
    if (e2eeEpoch <= localEpoch) return;

    try {
      this.e2eeWorker?.postMessage({
        type: 'catchUpToEpoch',
        targetEpoch: e2eeEpoch,
      } satisfies E2EEWorkerMessage);
      await mediaEncryption.catchUpToEpoch(e2eeEpoch);
    } catch (err) {
      console.error('E2EE: leave epoch catch-up failed — decrypt may fail until rejoin', {
        localEpoch,
        serverEpoch: e2eeEpoch,
        error: err instanceof Error ? err.message : err,
      });
    }
  }

  /**
   * Debounced key rotation: collapses rapid join/leave bursts.
   * For Worker path: sends rotateKeys to the Worker.
   * For legacy path: delegates to MediaEncryption.debouncedRotateKeys().
   */
  private debouncedRotateE2EEKeys(): void {
    if (currentTransformPath() === 'encoded-streams') {
      // Legacy path: MediaEncryption handles its own debounce
      this.mediaEncryption?.debouncedRotateKeys();
      return;
    }

    // Modern path: debounce on main thread, send to Worker
    if (!this.e2eeWorker) return;
    this.rotationPending = true;

    if (!this.rotationDeadline) {
      this.rotationDeadline = Date.now() + VoiceService.ROTATION_MAX_CAP_MS;
    }

    if (this.rotationTimer) clearTimeout(this.rotationTimer);
    const remaining = this.rotationDeadline - Date.now();
    const delay = remaining <= 0 ? 0 : Math.min(VoiceService.ROTATION_DEBOUNCE_MS, remaining);

    this.rotationTimer = setTimeout(() => {
      this.rotationTimer = null;
      this.rotationDeadline = 0;
      if (!this.rotationPending || !this.e2eeWorker) return;
      this.rotationPending = false;
      this.e2eeWorker.postMessage({ type: 'rotateKeys' } satisfies E2EEWorkerMessage);
    }, delay);
  }

  /** Add a decryption key for a remote user, pre-ratcheted to current epoch (with retry) */
  private isCurrentDecryptKeyContext(
    channelId: string,
    localUserId: string,
    mediaEncryption: MediaEncryption,
    e2eeWorker: Worker | null
  ): boolean {
    return (
      useVoiceStore.getState().activeChannelId === channelId &&
      useUserStore.getState().user?.id === localUserId &&
      this.mediaEncryption === mediaEncryption &&
      this.e2eeWorker === e2eeWorker
    );
  }

  private assertCurrentDecryptKeyContext(
    channelId: string,
    localUserId: string,
    mediaEncryption: MediaEncryption,
    e2eeWorker: Worker | null
  ): void {
    if (!this.isCurrentDecryptKeyContext(channelId, localUserId, mediaEncryption, e2eeWorker)) {
      throw new Error('E2EE session changed during decrypt-key provisioning');
    }
  }

  /** Derive and install a decrypt key for a single user at the current epoch */
  private async deriveAndInstallDecryptKey(
    channelId: string,
    userId: string,
    attempt: number,
    targetEpoch?: number
  ): Promise<void> {
    const mediaEncryption = this.mediaEncryption;
    const e2eeWorker = this.e2eeWorker;
    const localUserId = useUserStore.getState().user?.id;
    if (!mediaEncryption) throw new Error('mediaEncryption destroyed');
    if (!localUserId) throw new Error('E2EE session changed during decrypt-key provisioning');
    this.assertCurrentDecryptKeyContext(channelId, localUserId, mediaEncryption, e2eeWorker);

    const { channelKey, keyVersion } = await e2eeService.getChannelKeyMaterial(channelId);
    this.assertCurrentDecryptKeyContext(channelId, localUserId, mediaEncryption, e2eeWorker);
    const keyId = targetEpoch ?? mediaEncryption.getCurrentKeyId();

    if (keyId > 100) {
      throw new Error(`E2EE: epoch ${keyId} exceeds ratchet limit (100), rejoin required`);
    }

    this.assertCurrentDecryptKeyContext(channelId, localUserId, mediaEncryption, e2eeWorker);
    const key = await mediaEncryption.addDecryptKeyAtVersion(channelKey, userId, keyVersion, keyId);
    this.assertCurrentDecryptKeyContext(channelId, localUserId, mediaEncryption, e2eeWorker);

    if (e2eeWorker) {
      e2eeWorker.postMessage({
        type: 'addDecryptKey',
        senderUserId: userId,
        keyVersion,
        keyId,
        key,
      } satisfies E2EEWorkerMessage);
    }

    console.debug('E2EE: decrypt key added', {
      channelId,
      targetUserId: userId,
      currentEpoch: mediaEncryption.getCurrentKeyId(),
      keyId,
      attempt: attempt + 1,
    });
  }

  private beginDecryptKeyRequest(
    channelId: string,
    userId: string,
    targetEpoch: number | undefined
  ): DecryptKeyRequestContext | null {
    const mediaEncryption = this.mediaEncryption;
    const e2eeWorker = this.e2eeWorker;
    const localUserId = useUserStore.getState().user?.id;
    if (!mediaEncryption || !localUserId) return null;

    const context = {
      channelId,
      userId,
      targetEpoch,
      localUserId,
      mediaEncryption,
      e2eeWorker,
    };
    if (!this.isCurrentDecryptKeyRequest(context)) return null;
    return context;
  }

  private isCurrentDecryptKeyRequest(context: DecryptKeyRequestContext): boolean {
    return this.isCurrentDecryptKeyContext(
      context.channelId,
      context.localUserId,
      context.mediaEncryption,
      context.e2eeWorker
    );
  }

  private async tryAddDecryptKey(
    context: DecryptKeyRequestContext,
    attempt: number,
    retryDelay: number | undefined
  ): Promise<DecryptKeyAttemptResult> {
    if (!this.isCurrentDecryptKeyRequest(context)) return { status: 'stale' };

    try {
      await this.deriveAndInstallDecryptKey(
        context.channelId,
        context.userId,
        attempt,
        context.targetEpoch
      );
      if (!this.isCurrentDecryptKeyRequest(context)) return { status: 'stale' };
      return { status: 'success' };
    } catch (err) {
      if (!this.isCurrentDecryptKeyRequest(context)) return { status: 'stale' };
      if (retryDelay === undefined) return { status: 'failed', error: err };

      const delay = isPendingKeyError(err) ? retryDelay * 2 : retryDelay;
      await new Promise((r) => setTimeout(r, delay));
      if (!this.isCurrentDecryptKeyRequest(context)) return { status: 'stale' };
      return { status: 'retry', error: err };
    }
  }

  /** Add a decryption key for a remote user, pre-ratcheted to current epoch (with retry) */
  private async addDecryptKeyForUser(
    channelId: string,
    userId: string,
    targetEpoch?: number
  ): Promise<boolean> {
    const context = this.beginDecryptKeyRequest(channelId, userId, targetEpoch);
    if (!context) return false;

    const retryDelays = [500, 1000, 2000];
    let lastError: unknown;

    for (let attempt = 0; attempt <= retryDelays.length; attempt++) {
      const result = await this.tryAddDecryptKey(context, attempt, retryDelays[attempt]);
      if (result.status === 'success') return true;
      if (result.status === 'stale') return false;
      lastError = result.error;
    }

    console.error('E2EE: failed to add decrypt key after retries', {
      channelId: context.channelId,
      targetUserId: context.userId,
      error: lastError instanceof Error ? lastError.message : lastError,
    });
    return false;
  }

  private async addDecryptKeysForActiveParticipantsAtEpoch(
    channelId: string,
    targetEpoch: number
  ): Promise<void> {
    const selfId = useUserStore.getState().user?.id;
    const userIds = Object.keys(useVoiceStore.getState().participants).filter(
      (userId) => userId !== selfId
    );
    await Promise.all(
      userIds.map((userId) => this.addDecryptKeyForUser(channelId, userId, targetEpoch))
    );
  }

  /**
   * Produce only after mediasoup creates the sender and this callback installs E2EE.
   * onRtpSender runs synchronously before createOffer() and before the server-side
   * produce event, so an attachment failure cannot publish a plaintext frame.
   */
  private produceEncrypted(
    transport: mediasoupTypes.Transport,
    options: mediasoupTypes.ProducerOptions
  ): Promise<mediasoupTypes.Producer> {
    const source = typeof options.appData?.source === 'string' ? options.appData.source : undefined;
    const codecFamily =
      options.track?.kind === 'audio' ? 'opus' : codecFamilyFromMimeType(options.codec?.mimeType);

    return transport.produce({
      ...options,
      onRtpSender: (sender) => {
        try {
          this.applyEncryptTransform(sender, codecFamily, source);
        } catch (err) {
          // mediasoup invokes this before createOffer(), but does not roll back the
          // just-created transceiver when the callback throws. Detach the track and
          // close the transport so a failed sender cannot survive into a later offer.
          try {
            sender.replaceTrack(null).catch(() => undefined);
          } catch {
            // Best effort; closing the transport below is authoritative.
          }
          transport.close();
          throw err;
        }
      },
    });
  }

  /**
   * Apply E2EE to a sender before publication.
   * Modern path: RTCRtpScriptTransform (Chromium 129+, Worker-based).
   * Legacy path: createEncodedStreams (Chromium 86-130, main-thread).
   */
  private applyEncryptTransform(
    sender: RTCRtpSender,
    codecFamily?: CodecFamily,
    source?: string
  ): void {
    // Modern path: RTCRtpScriptTransform (Chromium 129+)
    if (currentTransformPath() === 'script-transform') {
      if (!this.e2eeWorker) {
        this.failClosedEncryptTransform(source, 'E2EE Worker is not initialized');
      }
      try {
        const options: E2EETransformOptions = { role: 'encrypt', codecFamily };
        sender.transform = new RTCRtpScriptTransform(this.e2eeWorker, options);
        console.debug('E2EE: encrypt transform applied (RTCRtpScriptTransform)');
      } catch (err) {
        console.error('E2EE: RTCRtpScriptTransform failed on sender:', errorMessage(err));
        this.failClosedEncryptTransform(source, 'RTCRtpScriptTransform failed');
      }
      return;
    }

    if (currentTransformPath() === 'unavailable') {
      this.failClosedEncryptTransform(source, 'encoded transform API unavailable');
    }

    // Legacy path: createEncodedStreams (Chromium 86-130)
    if (!this.mediaEncryption) {
      this.failClosedEncryptTransform(source, 'media encryption is not initialized');
    }
    const encryption = this.mediaEncryption;
    const legacySender = sender as RtpSenderWithEncodedStreams;

    if (typeof legacySender.createEncodedStreams === 'function') {
      try {
        const { readable, writable } = legacySender.createEncodedStreams();
        let encryptDropCount = 0;
        let firstEncryptLogged = false;
        const transform = new TransformStream({
          async transform(frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame, controller) {
            try {
              await encryption.encryptFrame(frame, codecFamily);
              controller.enqueue(frame);
              if (!firstEncryptLogged) {
                firstEncryptLogged = true;
                console.debug('E2EE: first frame encrypted successfully', {
                  kind: 'type' in frame ? 'video' : 'audio',
                  dataSize: frame.data.byteLength,
                });
              }
              if (encryptDropCount > 0) {
                console.debug(`E2EE: encrypt recovered after ${encryptDropCount} dropped frames`);
                encryptDropCount = 0;
              }
            } catch (err) {
              encryptDropCount++;
              if (encryptDropCount === 1 || encryptDropCount % 100 === 0) {
                console.warn('E2EE: encrypt frame dropped', {
                  totalDropped: encryptDropCount,
                  error: err instanceof Error ? err.message : err,
                });
              }
            }
          },
        });
        readable
          .pipeThrough(transform)
          .pipeTo(writable)
          .catch((err: unknown) => {
            console.error('E2EE: encrypt pipe broken — frames stopped flowing:', errorMessage(err));
          });
        console.debug('E2EE: encrypt transform applied (createEncodedStreams)');
      } catch (err) {
        console.error('E2EE: createEncodedStreams failed on sender:', errorMessage(err));
        this.failClosedEncryptTransform(source, 'createEncodedStreams failed');
      }
    } else {
      console.warn('E2EE: no Insertable Streams API available — frames will not be encrypted');
      this.failClosedEncryptTransform(source, 'Insertable Streams API unavailable');
    }
  }

  private failClosedEncryptTransform(source: string | undefined, reason: string): never {
    // Pre-publication failures have no Producer to close. Always tear down the
    // owning capture stream so fail-closed E2EE cannot leave hardware active.
    if (source === 'camera') this.cleanupCameraState();
    else if (source === 'mic') this.cleanupMicState();
    else if (source === 'screen' || source === 'screen-audio')
      // cleanupScreenState is async (awaits the transport queue drain); this path
      // is intentionally fire-and-forget because failClosed throws synchronously
      // below. Attach a catch so a failing async teardown logs (PII-safe) instead
      // of surfacing as an unhandled promise rejection.
      void this.cleanupScreenState().catch((err) =>
        console.error('E2EE: fail-closed screen cleanup failed:', errorMessage(err))
      );
    throw new Error(`E2EE: failed to attach encrypt transform (${reason})`);
  }

  /**
   * Apply E2EE decrypt transform to a consumer.
   * Modern path: RTCRtpScriptTransform (Chromium 129+, Worker-based).
   * Legacy path: createEncodedStreams (Chromium 86-130, main-thread).
   */
  /**
   * #1878/#1895: provision the exact (keyVersion, keyId) decrypt key for a typed
   * FrameKeyMiss. Shared by the Worker `requestFrameKey` IPC handler and the
   * legacy createEncodedStreams pipeline's `requestFrameKey` callback so both
   * decrypt paths recover identically.
   *
   * Adds the derived key to the main-thread MediaEncryption — which IS the
   * instance the legacy pipeline decrypts with — and, on the Worker path, also
   * posts the key into the Worker's own instance. Fail-closed on fetch failure
   * (pending-404 / permanent-403): getChannelKeyByVersion rate-limits and the
   * Worker caps its retries, so a swallowed rejection is safe.
   */
  private provisionFrameKey(
    channelId: string,
    senderUserId: string,
    keyVersion: number,
    keyId: number
  ): void {
    const mediaEncryption = this.mediaEncryption;
    const e2eeWorker = this.e2eeWorker;
    const localUserId = useUserStore.getState().user?.id;
    if (!mediaEncryption || !localUserId) return;

    void (async () => {
      try {
        this.assertCurrentDecryptKeyContext(channelId, localUserId, mediaEncryption, e2eeWorker);
        const csk = await e2eeService.getChannelKeyByVersion(channelId, keyVersion);
        this.assertCurrentDecryptKeyContext(channelId, localUserId, mediaEncryption, e2eeWorker);
        const key = await mediaEncryption.addDecryptKeyAtVersion(
          csk,
          senderUserId,
          keyVersion,
          keyId
        );
        this.assertCurrentDecryptKeyContext(channelId, localUserId, mediaEncryption, e2eeWorker);
        if (currentTransformPath() === 'script-transform' && e2eeWorker) {
          e2eeWorker.postMessage({
            type: 'addDecryptKey',
            senderUserId,
            keyVersion,
            keyId,
            key,
          } satisfies E2EEWorkerMessage);
        }
      } catch (err) {
        // Fail-closed: getChannelKeyByVersion rate-limits and the Worker caps
        // retries, so swallowing is SAFE — but swallowing SILENTLY is not. A
        // permanently failing provision is indistinguishable from a working one
        // in the console, which is how a key-miss storm stays invisible. Log at
        // debug (this fires per missed key and pending-404 is an expected
        // transient), PII-safe: ids and the reason only.
        console.debug('E2EE: frame-key provision failed', {
          senderUserId,
          keyVersion,
          keyId,
          reason: errorMessage(err),
        });
      }
    })();
  }

  /** Build DecryptRecoveryCallbacks bound to this VoiceService instance. */
  private decryptRecoveryCallbacks(): DecryptRecoveryCallbacks {
    return {
      getActiveChannelId: () => useVoiceStore.getState().activeChannelId,
      addDecryptKeyForUser: (channelId, userId) => this.addDecryptKeyForUser(channelId, userId),
      invalidateChannelKey: (channelId) => e2eeService.invalidateChannelKey(channelId),
      requestKeyframe: (senderUserId) => {
        if (useVoiceStore.getState().activeChannelId) {
          this.socket?.emit('request-keyframe', { senderUserId });
        }
      },
      // #1895: typed-miss provisioning for the legacy createEncodedStreams path,
      // routed through the same shared provisionFrameKey as the Worker IPC path.
      requestFrameKey: (senderUserId, keyVersion, keyId) => {
        const channelId = useVoiceStore.getState().activeChannelId;
        if (channelId) {
          this.provisionFrameKey(channelId, senderUserId, keyVersion, keyId);
        }
      },
    };
  }

  private applyDecryptTransform(consumer: mediasoupTypes.Consumer, senderUserId: string): void {
    const receiver = consumer.rtpReceiver;
    if (!receiver) {
      throw new Error('E2EE: failed to attach decrypt transform (no rtpReceiver on consumer)');
    }

    // #1895: resolve SENDER's codec from consumer.rtpParameters (populated before this is called —
    // consumeProducerImpl creates the consumer from result.rtpParameters then calls applyDecryptTransform,
    // so there is no race — OQ-8). Drives H.264 NAL-aware / AV1 per-OBU / whole-frame dispatch.
    const codecFamily = codecFamilyFromRtpParameters(consumer.rtpParameters);

    // Modern path: RTCRtpScriptTransform (Chromium 129+)
    if (currentTransformPath() === 'script-transform') {
      if (!this.e2eeWorker) {
        throw new Error('E2EE: failed to attach decrypt transform (Worker is not initialized)');
      }
      if (receiver.transform) {
        // Attached at receiver creation (onRtpReceiver in the consume call).
        // Nothing to install — arm the bypass probe that verifies the engine
        // actually routes frames through it.
        this.scheduleBypassProbe(consumer.id, senderUserId, 'first', 1);
        return;
      }
      try {
        const options: E2EETransformOptions = {
          role: 'decrypt',
          senderUserId,
          codecFamily,
          probeId: consumer.id,
        };
        receiver.transform = new RTCRtpScriptTransform(this.e2eeWorker, options);
        console.debug(
          `E2EE: decrypt transform applied for ${senderUserId} (RTCRtpScriptTransform)`
        );
        this.scheduleBypassProbe(consumer.id, senderUserId, 'first', 1);
      } catch (err) {
        console.error('E2EE: RTCRtpScriptTransform failed on receiver:', errorMessage(err));
        throw new Error(
          `E2EE: failed to attach decrypt transform (RTCRtpScriptTransform failed: ${errorMessage(err)})`
        );
      }
      return;
    }

    if (currentTransformPath() === 'unavailable') {
      throw new Error('E2EE: failed to attach decrypt transform (encoded transform unavailable)');
    }

    // Legacy path: createEncodedStreams (Chromium 86-130)
    if (!this.mediaEncryption) {
      throw new Error(
        'E2EE: failed to attach decrypt transform (media encryption is not initialized)'
      );
    }
    applyLegacyDecryptPipeline(
      receiver as InsertableStreamsReceiver,
      senderUserId,
      this.mediaEncryption,
      this.decryptRecoveryCallbacks(),
      E2EE_VERBOSE,
      codecFamily
    );
  }

  // ── Receive-transform bypass probe (2026-08-21 incident) ─────────────
  // Decoder-side getStats() and the worker's entered-frame counter are paired
  // ~5s after attach. Packets arriving while zero frames entered the transform
  // means ciphertext is reaching the decoder (garbled audio / black video).
  // One re-attach is attempted; if the bypass persists, the consumer is closed
  // fail-closed — silence is strictly better than decoded ciphertext.
  // Pending probes are keyed by consumer id; every async hop re-validates the
  // consumer and worker, so stale timers after teardown are harmless no-ops.

  /** Ownership token for the one-shot legacy-fallback rebuild (see engageLegacyFallback). */
  private legacyRebuildIntent: symbol | null = null;

  private readonly bypassProbes = new Map<
    string,
    { senderUserId: string; phase: BypassProbePhase; attempt: number }
  >();

  /**
   * The onRtpReceiver fragment for a consume() call, or nothing. Extracted so
   * consumeProducerImpl stays under the complexity ceiling and the Worker
   * narrows by early return instead of an assertion.
   */
  private creationAttachConsumeOption(
    result: ConsumeResponse,
    senderUserId?: string
  ): { onRtpReceiver?: (receiver: RTCRtpReceiver) => void } {
    if (currentTransformPath() !== 'script-transform') return {};
    const worker = this.e2eeWorker;
    if (!worker) {
      // Lazy-init race: E2EE finishes initializing AFTER this consume (the
      // ensureE2EEForConsumer path). The consumer degrades to late attach,
      // which Chromium ≥149 bypasses — the probe then fails it closed and the
      // legacy fallback restores audio. Self-healing, but log it so the
      // degraded window is observable instead of silent (Gitar, PR #2866).
      console.warn(
        'E2EE: creation-time decrypt attach skipped — worker not ready at consume; late attach will be probed',
        { consumerId: result.id }
      );
      return {};
    }
    return {
      onRtpReceiver: buildDecryptCreationAttach(
        worker,
        senderUserId || result.producerUserId,
        codecFamilyFromRtpParameters(result.rtpParameters),
        result.id
      ),
    };
  }

  private scheduleBypassProbe(
    consumerId: string,
    senderUserId: string,
    phase: BypassProbePhase,
    attempt: number,
    delayMs: number = BYPASS_PROBE_DELAY_MS
  ): void {
    setTimeout(() => {
      const consumer = this.consumers.get(consumerId);
      const worker = this.e2eeWorker;
      if (!consumer || consumer.closed || !worker) return;
      this.bypassProbes.set(consumerId, { senderUserId, phase, attempt });
      worker.postMessage({
        type: 'queryDecryptStats',
        probeId: consumerId,
      } satisfies E2EEWorkerMessage);
    }, delayMs);
  }

  private async evaluateBypassProbe(consumerId: string, entered: number): Promise<void> {
    const probe = this.bypassProbes.get(consumerId);
    if (!probe) return;
    this.bypassProbes.delete(consumerId);

    const consumer = this.consumers.get(consumerId);
    if (!consumer || consumer.closed) return;

    let packetsReceived = 0;
    try {
      const stats = await consumer.rtpReceiver?.getStats();
      stats?.forEach((report: { type?: string; packetsReceived?: number }) => {
        if (report.type === 'inbound-rtp') packetsReceived += report.packetsReceived ?? 0;
      });
    } catch {
      return; // stats unavailable (torn down mid-probe) — nothing to judge
    }

    const action = decideBypassProbeAction(packetsReceived, entered, probe.phase, probe.attempt);
    switch (action) {
      case 'verified':
        console.debug('E2EE: receive transform verified', {
          consumerId,
          entered,
          packetsReceived,
        });
        return;
      case 'retry':
        this.scheduleBypassProbe(consumerId, probe.senderUserId, probe.phase, probe.attempt + 1);
        return;
      case 'slow-retry':
        // Log the transition once, then poll quietly for the consumer's life.
        if (probe.attempt === BYPASS_PROBE_MAX_ATTEMPTS) {
          console.debug('E2EE: bypass probe entering slow poll — no media yet', { consumerId });
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
          'E2EE: receive transform BYPASSED — encrypted frames are reaching the decoder; re-attaching',
          { consumerId, senderUserId: probe.senderUserId, packetsReceived }
        );
        const receiver = consumer.rtpReceiver;
        const worker = this.e2eeWorker;
        if (!receiver || !worker) {
          // Bypass already CONFIRMED — returning here would leave a consumer
          // known to be feeding ciphertext to the decoder playing on with no
          // further probe. Fail closed instead. (Gitar, PR #2870; the same
          // shape it found in the PiP mirror of this code.)
          this.closeConsumerAfterDecryptTransformFailure(
            consumer,
            'confirmed bypass could not be re-attached (receiver or worker unavailable)'
          );
          return;
        }
        try {
          // Direct replacement — NEVER null-then-set: per the encoded-transform
          // spec, assigning null enables the PASSTHROUGH algorithm, i.e. a
          // window where ciphertext flows to the decoder by design
          // (CodeRabbit, PR #2865 / CWE-693). A single assignment is the
          // defined dynamic update.
          receiver.transform = new RTCRtpScriptTransform(worker, {
            role: 'decrypt',
            senderUserId: probe.senderUserId,
            codecFamily: codecFamilyFromRtpParameters(consumer.rtpParameters),
            probeId: consumerId,
          } satisfies E2EETransformOptions);
          this.scheduleBypassProbe(consumerId, probe.senderUserId, 'reattached', 1);
        } catch (err) {
          this.closeConsumerAfterDecryptTransformFailure(
            consumer,
            `bypass re-attach failed: ${errorMessage(err)}`
          );
          // A throwing replacement is the same engine signal as a persisting
          // bypass — the modern path is broken here. Engage the fallback so
          // the session heals instead of permanently losing consumers
          // (Gitar finding, PR #2866). Chromium 150 fails the replacement
          // asynchronously (pipe error) and reaches the 'close' action, but a
          // spec-conformant synchronous InvalidStateError lands in THIS catch.
          this.engageLegacyFallback();
        }
        return;
      }
      case 'close':
        console.error(
          'E2EE: receive transform still bypassed after re-attach — closing consumer fail-closed',
          { consumerId, senderUserId: probe.senderUserId, packetsReceived }
        );
        this.closeConsumerAfterDecryptTransformFailure(
          consumer,
          'receive transform bypassed — ciphertext reaching decoder'
        );
        this.engageLegacyFallback();
        return;
    }
  }

  /**
   * Engine-level bypass confirmed even for a creation-time attachment: switch
   * this page session to the legacy createEncodedStreams pipeline (main
   * thread, parity-hardened in PR #2863) and rebuild the media session once so
   * new transports pick up encodedInsertableStreams. One shot per session —
   * if the legacy path also bypasses (unknown engine), consumers keep failing
   * closed and no rebuild loop can start.
   */
  private engageLegacyFallback(): void {
    if (!engageLegacyTransformOverride()) return;
    if (currentTransformPath() !== 'encoded-streams') {
      console.error(
        'E2EE: legacy fallback requested but createEncodedStreams is unavailable — staying fail-closed'
      );
      return;
    }
    const store = useVoiceStore.getState();
    const channelId = store.activeChannelId;
    console.error(
      'E2EE: receive transforms are bypassed on this engine — switching to the legacy encoded-streams pipeline and rebuilding the media session',
      { channelId }
    );
    if (!channelId) return;
    const joinType = store.isDMCall ? ('dm' as const) : ('channel' as const);
    const intent = Symbol('legacy-rebuild');
    this.legacyRebuildIntent = intent;
    void (async () => {
      try {
        await this.leaveChannel({ internalRebuild: true });
        // Ownership check: a user-initiated leave or join during the rebuild
        // window clears the intent — the user's action wins, no auto-rejoin.
        if (this.legacyRebuildIntent !== intent) {
          console.debug('E2EE: legacy fallback rebuild cancelled — user acted during rebuild');
          return;
        }
        if (useVoiceStore.getState().activeChannelId !== null) {
          console.debug('E2EE: legacy fallback rebuild skipped — another call is active');
          return;
        }
        await this.joinChannel(channelId, joinType, { internalRebuild: true });
      } catch (err) {
        console.error('E2EE: legacy fallback rebuild failed:', errorMessage(err));
      }
    })();
  }

  private closeConsumerAfterDecryptTransformFailure(
    consumer: mediasoupTypes.Consumer,
    reason: string
  ): void {
    console.error('E2EE: closing consumer because decrypt transform failed', {
      consumerId: consumer.id,
      producerId: consumer.producerId,
      reason,
    });
    if (!consumer.closed) consumer.close();
    this.consumers.delete(consumer.id);
    this.decoderBudgetSampler.deleteConsumer(consumer.id);
    this.consumerMeta.delete(consumer.id);
    this.lastPreferredLayerKeyByConsumer.delete(consumer.id);
    this.pauseCoordinator.clearConsumer(consumer.id);
    this.testSuspendedConsumerIds.delete(consumer.id);
    this.testRestoreEligibleConsumerIds.delete(consumer.id);
    this.testServerPausedConsumerIds.delete(consumer.id);
    this.serverResumeOnUndeafenConsumerIds.delete(consumer.id);
    this.socket?.emit('close-consumer', { consumerId: consumer.id });
  }

  // ─── Helpers ───────────────────────────────────────────────────────

  private waitForConnect(): Promise<void> {
    return new Promise((resolve, reject) => {
      const socket = this.socket;
      if (!socket) return reject(new Error('No socket'));

      if (socket.connected) return resolve();

      // Ride through a transient media-plane blip (e.g. a deploy recreating the
      // single media-plane container) instead of hard-failing the join on the first
      // connect_error. With reconnection:true, a transient failure leaves
      // socket.active === true and the Manager auto-retries the connect; we only
      // reject on a server-side denial (socket.active === false — e.g. auth), on
      // reconnection exhaustion (reconnect_failed), or on the overall timeout. Use
      // on()/off() (not once()) because connect_error fires once per failed attempt. #2176.
      let settled = false;
      const finish = (cb: () => void): void => {
        if (settled) return;
        settled = true;
        clearTimeout(timeout);
        socket.off('connect', onConnect);
        socket.off('connect_error', onConnectError);
        socket.io.off('reconnect_failed', onReconnectFailed);
        cb();
      };
      const onConnect = (): void => finish(() => resolve());
      const onConnectError = (err: Error): void => {
        // socket.active === false => the client will NOT auto-reconnect (server
        // denial, or reconnection attempts exhausted) => fail fast. Otherwise it is a
        // transient error the Manager is already retrying — keep waiting.
        if (!socket.active) finish(() => reject(err));
      };
      const onReconnectFailed = (): void =>
        finish(() => reject(new Error('Socket reconnection failed')));
      const timeout = setTimeout(
        () => finish(() => reject(new Error('Socket connection timeout'))),
        VOICE_CONNECT_TIMEOUT_MS
      );

      socket.on('connect', onConnect);
      socket.on('connect_error', onConnectError);
      socket.io.on('reconnect_failed', onReconnectFailed);
    });
  }

  private emitAsync<T>(event: string, data?: unknown): Promise<T> {
    return new Promise((resolve, reject) => {
      if (!this.socket) return reject(new Error('No socket connection'));

      const timeout = setTimeout(() => {
        reject(new Error(`Socket emit timeout: ${event}`));
      }, 10_000);

      this.socket.emit(event, data, (response: T & { error?: string; code?: string }) => {
        clearTimeout(timeout);
        if (response && typeof response === 'object' && 'error' in response) {
          // Preserve a typed `code` (e.g. #1878 'crypto_version_mismatch') on the
          // rejected error so callers can branch on it. emitAsync otherwise
          // discards every ack field but `error`; the bare Error loses the code.
          const err = new Error(response.error) as Error & { code?: string };
          if (typeof response.code === 'string') err.code = response.code;
          reject(err);
        } else {
          resolve(response);
        }
      });
    });
  }

  private async cleanup(): Promise<void> {
    // Invalidate asynchronous crypto work and drop the live rotation listener
    // before closeProducer reaches the first await.
    this.invalidatePendingE2EEInit();
    this.bypassProbes.clear(); // same stale-probe reasoning as cleanupTimersAndE2EE
    this.teardownSharedE2EEState();
    this.invalidateVideoReproduces();
    // Clear solo bandwidth saving state
    if (this.soloNotificationTimer) {
      clearTimeout(this.soloNotificationTimer);
      this.soloNotificationTimer = null;
    }

    // Close all producers — snapshot keys first because closeProducer()
    // mutates the Map (deletes entries), which breaks for..of iteration
    // and causes producers to be skipped (camera stays alive).
    const producerSources = [...this.producers.keys()];
    for (const source of producerSources) {
      await this.closeProducer(source);
    }

    // Safety net: ensure ALL local media tracks are stopped even if
    // closeProducer missed them (e.g. due to a prior error or race).
    for (const stream of [this.localMicStream, this.localCameraStream, this.localScreenStream]) {
      if (stream) for (const t of stream.getTracks()) t.stop();
    }
    this.localMicStream = null;
    this.localCameraStream = null;
    this.localScreenStream = null;

    // Close all consumers
    for (const [, consumer] of this.consumers) {
      consumer.close();
    }
    this.consumers.clear();
    this.decoderBudgetSampler.clear();
    this.consumerMeta.clear();
    this.testSuspensionDepth = 0;
    this.testSuspendedProducerIds.clear();
    this.testSuspendedConsumerIds.clear();
    this.testRestoreEligibleProducerIds.clear();
    this.testRestoreEligibleConsumerIds.clear();
    this.testServerPausedConsumerIds.clear();
    this.serverResumeOnUndeafenConsumerIds.clear();
    this.pendingScreenAudioProducers.clear();
    this.resetRemoteVideoLayeringState();
    // Reset IGNIS recovery state on channel leave — consumers are gone, so stale
    // ids must not linger and the green-cycle counter must not carry across
    // channels (#1540; mirrors the emergency cleanupTimersAndE2EE path).
    this.unregisterDocumentVisibilityListener();
    this.pauseCoordinator.reset();
    this.consecutiveGreenIntervals = 0;

    // Close transports
    this.sendTransport?.close();
    this.recvTransportAudio?.close();
    this.recvTransportVideo?.close();
    this.sendTransport = null;
    this.recvTransportAudio = null;
    this.recvTransportVideo = null;

    // Stop local VAD, noise gate, input volume, and live subscriptions
    this.stopLocalVAD();
    this.stopNoiseGate();
    this.stopInputVolume();
    this.teardownLiveSubscriptions();

    // Stop packet loss monitor
    this.stopPacketLossMonitor();

    // Stop decoder profiling
    if (this.decoderProfilingTimer) {
      clearInterval(this.decoderProfilingTimer);
      this.decoderProfilingTimer = null;
    }
    this.decoderProfilingInFlight = false;

    // Clear screen share opt-in list
    useVoiceStore.getState().clearAvailableScreenShares();

    // Reset consume queues so pending consumes from the old session don't
    // interfere with the next join.
    this.consumeQueueAudio = Promise.resolve();
    this.consumeQueueVideo = Promise.resolve();

    // Disconnect socket
    this.socket?.disconnect();
    this.socket = null;

    // Reset device and router capabilities
    this.device = null;
    this.routerRtpCapabilities = null;
  }

  // ─── OS Permission Helper (#197) ────────────────────────────────────

  /**
   * Ensure an OS-level permission is granted before proceeding.
   * On macOS, triggers the system permission prompt if status is 'not-determined'.
   * Throws a descriptive error if denied/restricted so the caller's catch block
   * can display a meaningful message.
   */
  private async ensureOsPermission(type: 'microphone' | 'camera'): Promise<void> {
    const status = await ensureOsPermissionShared(type);
    // Allow 'granted' and 'not-determined' — getUserMedia will trigger the native
    // TCC prompt for 'not-determined' (safe after plist patch).
    if (status === 'granted' || status === 'not-determined') return;

    const label = type === 'microphone' ? 'Microphone' : 'Camera';
    if (status === 'denied' || status === 'restricted') {
      throw new DOMException(
        `${label} access denied. Grant permission in System Settings > Privacy & Security.`,
        'NotAllowedError'
      );
    }
    if (status === 'unavailable') {
      throw new DOMException(
        `${label} is unavailable on this device or OS configuration.`,
        'NotAllowedError'
      );
    }
    // Catch-all for unexpected statuses
    throw new DOMException(
      `${label} access is not available (status: ${status}).`,
      'NotAllowedError'
    );
  }
}

// Export singleton
export const voiceService = new VoiceService();

// Auto-cleanup voice when auth tokens are cleared (logout, token revocation,
// refresh failure). This fires BEFORE the React tree re-renders, ensuring
// media stops immediately regardless of which code path called clearTokens().
useAuthStore.subscribe((state, prevState) => {
  if (prevState.accessToken && !state.accessToken) {
    voiceService.emergencyCleanup();
  }
});

// Stop all media when the window is closing (app quit, reload, crash).
// Ensures the OS releases mic/camera even on unclean shutdown.
if (globalThis.window) {
  globalThis.addEventListener('beforeunload', () => {
    voiceService.emergencyCleanup();
  });
}
