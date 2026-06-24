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
import { useAuthStore } from '../stores/authStore';
import { useUserStore } from '../stores/userStore';
import { useUpdateStatusStore } from '../stores/updateStatusStore';
import {
  useVoiceStore,
  AUDIO_QUALITY_TIERS,
  type AudioQualityTier,
  type VoiceParticipant,
} from '../stores/voiceStore';
import { useAudioSettingsStore, type AudioPriority } from '../stores/audioSettingsStore';
import {
  useVideoSettingsStore,
  VIDEO_QUALITY_PRESETS,
  type VideoPriority,
  type ScreenShareOptions,
} from '../stores/videoSettingsStore';
import { apiFetch } from './apiClient';
import {
  MEDIA_E2EE_FRAME_CRYPTO_VERSION,
  MediaEncryption,
  deriveFrameKey,
} from './mediaEncryption';
import { e2eeService } from './e2eeService';
import { isPendingKeyError } from './e2eeErrors';
import {
  useOsPermissionStore,
  ensureOsPermission as ensureOsPermissionShared,
} from '../stores/osPermissionStore';
import type {
  E2EEWorkerMessage,
  E2EEMainMessage,
  E2EETransformOptions,
} from '../workers/e2eeProtocol';
import { notificationSoundService } from './notificationSoundService';
import { selectCodecFromCascade, type CodecLookup } from './voiceCodecSelection';
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
} from './voiceE2eeTransforms';
import { errorMessage } from '../utils/redactError';

// Toggle for verbose E2EE/SDP diagnostics — set to true when debugging
// frame drops, BUNDLE collisions, or key rotation issues. When false,
// only errors, warnings, and key lifecycle events are logged.
const E2EE_VERBOSE = false;
const MAX_REMOTE_VIDEO_DEVICE_PIXEL_RATIO = 8;

// Detect which Insertable Streams API is available at module load.
//
// Priority: createEncodedStreams (legacy) > RTCRtpScriptTransform (modern).
//
// Reason: encodedInsertableStreams + RTCRtpScriptTransform CONFLICT in Chromium 135 —
// the internal pipeline created by encodedInsertableStreams blocks the decoder output
// even when RTCRtpScriptTransform processes frames in the Worker, producing silence.
// RTCRtpScriptTransform WITHOUT encodedInsertableStreams receives no frames at all.
// createEncodedStreams + encodedInsertableStreams is the proven path (#295).
//
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

interface RemoteVideoLayerPayload extends RemoteVideoTileRenderState, RemoteVideoLayerRequest {
  devicePixelRatio: number;
  pressureStepDown: boolean;
}

type CameraPressureLayerRequestResult = 'emitted' | 'handled' | 'fallback';

interface TestSuspensionRestorePolicy {
  keepAudioOutPaused: boolean;
  keepProducersPaused: boolean;
  keepMicPaused: boolean;
}
const HAS_ENCODED_STREAMS =
  typeof RTCRtpSender !== 'undefined' &&
  typeof (RTCRtpSender.prototype as RtpSenderWithEncodedStreams).createEncodedStreams ===
    'function';
const USE_SCRIPT_TRANSFORM = !HAS_ENCODED_STREAMS && typeof RTCRtpScriptTransform !== 'undefined';

if (E2EE_VERBOSE) {
  console.debug('E2EE API detection:', {
    hasEncodedStreams: HAS_ENCODED_STREAMS,
    hasScriptTransform: typeof RTCRtpScriptTransform !== 'undefined',
    selectedPath: (() => {
      if (HAS_ENCODED_STREAMS) return 'createEncodedStreams (legacy)';
      if (USE_SCRIPT_TRANSFORM) return 'RTCRtpScriptTransform (Worker)';
      return 'NONE — E2EE unavailable';
    })(),
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
  store.tuneIn(producerId, 'local-screen');
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
  private localCameraStream: MediaStream | null = null;
  private localScreenStream: MediaStream | null = null;

  // Pending screen-audio producers from remote users (userId → producerId)
  // Consumed when the local user tunes into the corresponding screen share
  private readonly pendingScreenAudioProducers: Map<string, string> = new Map();

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
    this.stopPacketLossMonitor();
    this.lastPacketsLost = 0;
    this.lastPacketsSent = 0;

    this.packetLossTimer = setInterval(async () => {
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

  /**
   * Find a video codec from the loaded device's send capabilities.
   * Accepts a codec key like "video/H264:640034" or plain mimeType "video/VP9".
   * When a profile is specified, matches exactly. When mimeType-only, returns
   * the last match (highest quality — router lists profiles in ascending order).
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
          return String(params['profile-level-id'] ?? '')
            .toLowerCase()
            .startsWith(profileId.toLowerCase().substring(0, 4));
        }
        if (mimeLower === 'video/vp9') {
          return String(params['profile-id'] ?? '0') === profileId;
        }
        return true;
      });
    }

    // No profile specified — return best (last = highest quality in router order)
    return matches[matches.length - 1];
  }

  /**
   * Check if a codec is compatible with the current room codec floor.
   * Extracts mimeType from codec key — floor operates at mimeType level.
   */
  private isInCodecFloor(key: string): boolean {
    const floor = useVoiceStore.getState().codecFloor;
    if (!floor) return true;
    const mime = key.split(':')[0].toLowerCase();
    return floor.includes(mime);
  }

  /**
   * Check if a codec has hardware-accelerated encoding available.
   * Extracts mimeType from codec key — HW detection is mimeType-level.
   */
  private isHwAccelerated(key: string): boolean {
    const caps = useVideoSettingsStore.getState().codecCapabilities;
    const mime = key.split(':')[0].toLowerCase();
    return caps.some((c) => c.mimeType.toLowerCase() === mime && c.powerEfficient);
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
      '720p': { w: 1280, h: 720 },
      '1080p': { w: 1920, h: 1080 },
      '1440p': { w: 2560, h: 1440 },
      '4K': { w: 3840, h: 2160 },
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
    return this.calculateRecommendedBitrate(res.w, res.h, effectiveFps, codecMime);
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
    return maxBitrate || 2_500_000;
  }

  /**
   * Pick the best codec for camera video. Single-encoding remains the default;
   * when the room gate enables camera layering, buildCameraEncodingPlan supplies
   * SVC or simulcast encodings for compatible codecs.
   *
   * Cascade: user pref → AV1 → HEVC → H264 High → VP9:2 (HDR) → H264 → VP9 → VP8
   * Two-pass: HW-accelerated first, then SW fallback.
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

    const layeringCodec = this.pickCameraLayeringCodec(codec);
    const plan = buildCameraEncodingPlan({
      codec: layeringCodec,
      maxBitrate: preset.maxBitrate,
      scalabilityMode: vs.scalabilityMode,
      priority: base,
    });
    return { codec: layeringCodec, encodings: plan.encodings };
  }

  private pickCameraLayeringCodec(
    fallbackCodec?: mediasoupTypes.RtpCodecCapability
  ): mediasoupTypes.RtpCodecCapability | undefined {
    // Layering rooms use the approved SVC-first ladder; user preference only
    // participates through the general-cascade fallback below.
    const candidates = ['video/AV1', 'video/VP9', 'video/H264:640034', 'video/H264', 'video/VP8'];
    for (const key of candidates) {
      if (!this.isInCodecFloor(key)) continue;
      const codec = this.findSendCodec(key);
      if (codec) return codec;
    }
    return fallbackCodec;
  }

  /**
   * Pick the best codec for screen sharing. Screen currently publishes one
   * encoding; camera layering is negotiated separately by the media-plane gate.
   *
   * Cascade: user pref → AV1 → HEVC → H264 High → VP9:2 (HDR) → H264 → VP9 → VP8
   * Two-pass: HW-accelerated first, then SW fallback.
   */
  private pickScreenCodec(): {
    codec?: mediasoupTypes.RtpCodecCapability;
    encodings: mediasoupTypes.RtpEncodingParameters[];
    effectiveBitrate: number;
  } {
    const vs = useVideoSettingsStore.getState();
    const prio = vs.screenSharePriority;
    const userBitrate = vs.screenShareBitrate; // 0 = auto
    const bitrate = userBitrate || this.calculateScreenBitrate();

    const codec = selectCodecFromCascade({
      preferred: vs.preferredVideoCodec,
      hwAccel: vs.hardwareAcceleration,
      hdrEncoding: vs.hdrEncoding,
      ...this.codecLookup(),
    });

    const base: Partial<mediasoupTypes.RtpEncodingParameters> =
      prio === 'off' ? {} : { priority: prio, networkPriority: prio };

    return {
      codec,
      encodings: [{ ...base, maxBitrate: bitrate }],
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
      this.liveReplaceCameraTrack();
    }
    if (state.preferredVideoCodec !== prev.preferredVideoCodec) {
      this.liveReproduceCamera();
      if (this.producers.get('screen')) {
        this.fastReproduceScreen();
      }
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
    const producer = this.producers.get('camera');
    if (!producer) return;

    const vs = useVideoSettingsStore.getState();
    const preset = VIDEO_QUALITY_PRESETS[vs.cameraPreset] || VIDEO_QUALITY_PRESETS['system'];
    const isSystemDefault = vs.cameraPreset === 'system' || preset.width === 0;

    producer.pause();

    try {
      if (this.localCameraStream) {
        for (const t of this.localCameraStream.getTracks()) t.stop();
      }

      // Try preferred constraints, fall back to bare minimum on OverconstrainedError
      let stream: MediaStream;
      try {
        stream = await navigator.mediaDevices.getUserMedia({
          video: {
            ...(isSystemDefault
              ? {}
              : {
                  width: { ideal: preset.width },
                  height: { ideal: preset.height },
                  frameRate: { ideal: preset.frameRate },
                }),
          },
        });
      } catch (err) {
        if (
          err instanceof DOMException &&
          err.name === 'OverconstrainedError' &&
          !isSystemDefault
        ) {
          console.warn(
            'Camera overconstrained during track replace, falling back:',
            errorMessage(err)
          );
          stream = await navigator.mediaDevices.getUserMedia({ video: true });
        } else {
          throw err;
        }
      }
      this.localCameraStream = stream;
      const track = stream.getVideoTracks()[0];

      await producer.replaceTrack({ track });

      // Update participant store with new stream
      const localUserId = useUserStore.getState().user?.id;
      if (localUserId) {
        useVoiceStore.getState().updateParticipant(localUserId, { videoStream: stream });
      }
    } catch (err) {
      console.warn('liveReplaceCameraTrack failed:', errorMessage(err));
    }

    producer.resume();
  }

  // --- Re-produce: close + re-create producer with new codec options ---

  private async liveReproduceAudio(): Promise<void> {
    if (!this.producers.get('mic') || !this.sendTransport) return;
    await this.closeProducer('mic');
    await this.produceAudio();
  }

  private async liveReproduceCamera(): Promise<void> {
    if (!this.producers.get('camera') || !this.sendTransport) return;
    if (this.localCameraStream) {
      for (const t of this.localCameraStream.getTracks()) t.stop();
      this.localCameraStream = null;
    }
    await this.closeProducer('camera');
    await this.produceVideo();
  }

  // --- Codec floor: fast re-produce (reuses existing track, no media re-acquisition) ---

  /**
   * Get the codec key of the codec currently used by a producer.
   * Returns profile-aware keys like "video/h264:640034" or "video/vp9:2".
   */
  private getProducerCodecMimeType(source: string): string | null {
    const producer = this.producers.get(source);
    if (!producer?.rtpSender) return null;
    const params = producer.rtpSender.getParameters();
    const codec = params.codecs?.[0];
    if (!codec) return null;
    const mime = codec.mimeType.toLowerCase();
    if (codec.sdpFmtpLine) {
      if (mime === 'video/h264') {
        const m = /profile-level-id=([0-9a-f]{6})/i.exec(codec.sdpFmtpLine);
        if (m) return `${mime}:${m[1].toLowerCase()}`;
      }
      if (mime === 'video/vp9') {
        const m = codec.sdpFmtpLine.match(/profile-id=(\d+)/);
        if (m && m[1] !== '0') return `${mime}:${m[1]}`;
      }
    }
    return mime;
  }

  /**
   * Fast re-produce camera: close producer and re-produce with best floor-compatible
   * codec, reusing the existing video track for a subsecond switch.
   */
  private async fastReproduceCamera(): Promise<void> {
    const producer = this.producers.get('camera');
    if (!producer || !this.sendTransport || !this.localCameraStream) return;

    const track = this.localCameraStream.getVideoTracks()[0];
    if (track?.readyState !== 'live') {
      await this.liveReproduceCamera();
      return;
    }

    // Close old producer (does NOT stop the track)
    producer.close();
    // Drain transport queue so stopSending SDP renegotiation finishes before produce()
    await this.drainSendTransportQueue();
    this.producers.delete('camera');
    this.socket?.emit('close-producer', { producerId: producer.id });

    // Pick new codec respecting the floor
    const { codec, encodings } = this.pickCameraCodec();
    const cameraBitrate = this.cameraStartBitrate(encodings);

    const newProducer = await this.sendTransport.produce({
      track,
      encodings,
      codec,
      codecOptions: { videoGoogleStartBitrate: this.computeStartBitrate(cameraBitrate) },
      appData: { source: 'camera' },
    });

    this.applyDegradationPreference(newProducer);
    this.producers.set('camera', newProducer);

    if (this.mediaEncryption) {
      this.applyEncryptTransform(newProducer);
    }

    newProducer.on('transportclose', () => {
      this.producers.delete('camera');
      if (this.localCameraStream) {
        for (const t of this.localCameraStream.getTracks()) t.stop();
        this.localCameraStream = null;
      }
      const s = useVoiceStore.getState();
      s.setVideoOn(false);
      const uid = useUserStore.getState().user?.id;
      if (uid) s.updateParticipant(uid, { videoStream: undefined, isVideoOn: false });
    });

    useVoiceStore.getState().setActiveCameraCodec(codec?.mimeType?.toLowerCase() ?? null);
    const hwTag = codec?.mimeType && this.isHwAccelerated(codec.mimeType) ? 'HW' : 'SW';
    console.debug(
      `[codec-floor] Fast re-produced camera with ${codec?.mimeType ?? 'default'} (${hwTag})`
    );
  }

  /**
   * Fast re-produce screen: close producer and re-produce with best floor-compatible
   * codec, reusing the existing screen track.
   */
  private async fastReproduceScreen(): Promise<void> {
    const oldProducer = this.producers.get('screen');
    if (!oldProducer || !this.sendTransport || !this.localScreenStream) return;

    const track = this.localScreenStream.getVideoTracks()[0];
    if (track?.readyState !== 'live') {
      console.warn('[codec-floor] Screen track is dead, cannot re-acquire without user gesture');
      return;
    }

    const oldProducerId = oldProducer.id;

    // Close old producer (does NOT stop the track)
    oldProducer.close();
    // Drain transport queue so stopSending SDP renegotiation finishes before produce()
    await this.drainSendTransportQueue();
    this.producers.delete('screen');
    this.socket?.emit('close-producer', { producerId: oldProducerId });

    // Pick new codec respecting the floor
    const { codec, encodings, effectiveBitrate: screenBitrate } = this.pickScreenCodec();

    const newProducer = await this.sendTransport.produce({
      track,
      encodings,
      codec,
      codecOptions: { videoGoogleStartBitrate: this.computeStartBitrate(screenBitrate) },
      appData: { source: 'screen' },
    });

    this.applyDegradationPreference(newProducer);
    this.producers.set('screen', newProducer);

    if (this.mediaEncryption) {
      this.applyEncryptTransform(newProducer);
    }

    // Re-wire track ended handler
    track.onended = () => {
      this.closeProducer('screen');
    };

    // Update tuned-in mapping since producer ID changed
    const store = useVoiceStore.getState();
    if (store.tunedInScreenShares[oldProducerId]) {
      store.tuneOut(oldProducerId);
      store.tuneIn(newProducer.id, 'local-screen');
    }
    if (store.dominantScreenShareId === oldProducerId) {
      store.setDominantScreenShare(newProducer.id);
    }

    newProducer.on('transportclose', () => {
      this.producers.delete('screen');
      if (this.localScreenStream) {
        for (const t of this.localScreenStream.getTracks()) t.stop();
        this.localScreenStream = null;
      }
      const s = useVoiceStore.getState();
      s.setScreenSharing(false);
      const uid = useUserStore.getState().user?.id;
      if (uid) s.updateParticipant(uid, { screenStream: undefined, isScreenSharing: false });
    });

    await this.reProduceScreenAudio();

    useVoiceStore.getState().setActiveScreenCodec(codec?.mimeType?.toLowerCase() ?? null);
    const hwTag = codec?.mimeType && this.isHwAccelerated(codec.mimeType) ? 'HW' : 'SW';
    console.debug(
      `[codec-floor] Fast re-produced screen with ${codec?.mimeType ?? 'default'} (${hwTag})`
    );
  }

  /** Re-produce screen audio if an active screen-audio producer and live audio track exist. */
  private async reProduceScreenAudio(): Promise<void> {
    const oldAudioProducer = this.producers.get('screen-audio');
    if (!oldAudioProducer || !this.localScreenStream || !this.sendTransport) return;

    const audioTrack = this.localScreenStream.getAudioTracks()[0];
    if (audioTrack?.readyState !== 'live') return;

    const oldAudioId = oldAudioProducer.id;
    oldAudioProducer.close();
    await this.drainSendTransportQueue();
    this.producers.delete('screen-audio');
    this.socket?.emit('close-producer', { producerId: oldAudioId });

    try {
      const newAudioProducer = await this.sendTransport.produce({
        track: audioTrack,
        codecOptions: { opusStereo: true, opusDtx: false },
        appData: { source: 'screen-audio' },
      });
      this.producers.set('screen-audio', newAudioProducer);
      if (this.mediaEncryption) {
        this.applyEncryptTransform(newAudioProducer);
      }
      newAudioProducer.on('transportclose', () => {
        this.producers.delete('screen-audio');
      });
    } catch (err) {
      console.warn('Failed to re-produce screen audio:', errorMessage(err));
    }
  }

  /**
   * Handle a codec floor update. Check each active video producer and
   * re-produce if a better floor-compatible codec is available or the
   * current codec is no longer in the floor.
   */
  private async handleCodecFloorChange(
    _previousFloor: string[] | null,
    _newFloor: string[] | null
  ): Promise<void> {
    for (const source of ['camera', 'screen'] as const) {
      await this.reProduceIfBetterCodec(source);
    }
  }

  /** Re-produce a video source if the codec cascade now selects a different codec. */
  private async reProduceIfBetterCodec(source: 'camera' | 'screen'): Promise<void> {
    const producer = this.producers.get(source);
    if (!producer) return;

    const currentMime = this.getProducerCodecMimeType(source);
    if (!currentMime) return;

    const bestPick = source === 'camera' ? this.pickCameraCodec() : this.pickScreenCodec();
    const bestMime = bestPick.codec?.mimeType?.toLowerCase() ?? null;
    if (!bestMime || bestMime === currentMime) return;

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
      await this.fastReproduceCamera();
    } else {
      await this.fastReproduceScreen();
    }
  }

  // Decoder budget profiling (IGNIS)
  private decoderProfilingTimer: ReturnType<typeof setInterval> | null = null;

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
  private readonly remoteVideoPressureByUser = new Map<string, boolean>();
  private readonly lastPreferredLayerKeyByConsumer = new Map<string, string>();
  private readonly remoteVideoRenderStateByUser = new Map<
    string,
    Map<string, RemoteVideoTileRenderState>
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
   * the utils/dm.peerName helper, which is store-agnostic per the
   * SonarCloud architecture rule), or a fallback string.
   */
  private async synthesizeDMChannel(channelId: string): Promise<{
    id: string;
    name: string;
    server_id: string;
    audio_quality_tier: string | null;
  }> {
    const { useDMStore: importedDMStore } = await import('../stores/dmStore');
    const { useUserStore: importedUserStore } = await import('../stores/userStore');
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
    const { peerName: synthesizePeerName } = await import('../utils/dm');
    return synthesizePeerName(conversation.participants, currentUserId);
  }

  /** Authorize and join a voice channel */
  async joinChannel(channelId: string, joinType: 'channel' | 'dm' = 'channel'): Promise<void> {
    const store = useVoiceStore.getState();
    if (store.activeChannelId) {
      await this.leaveChannel();
    }

    store.setConnectionState('connecting');

    try {
      // Step 1: Authorize via control plane (uses apiFetch for automatic token refresh)
      const endpoint =
        joinType === 'dm'
          ? `/api/v1/dm/conversations/${channelId}/voice/join`
          : `/api/v1/channels/${channelId}/voice/join`;
      const res = await apiFetch(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      });

      if (!res.ok) {
        const err = await res.json().catch(() => ({}));
        throw new Error(err.error || `Voice join failed: ${res.status}`);
      }

      const joinData: JoinResponse = await res.json();
      if (!joinData.allowed) throw new Error('Not allowed to join this channel');

      const { media_server_url } = joinData;
      // For DM voice joins the response omits `channel` and includes
      // `conversation` instead. Synthesize a channel-shaped object from
      // the conversation + dmStore peer/group-name lookup per spec §7.9
      // (#1209 plan task F4 — VoiceView channelName synthesis). Extracted
      // into synthesizeDMChannel to keep joinChannel's cognitive complexity
      // under the 15-statement bound (was 19 before extraction).
      const channel =
        joinData.channel ??
        (joinType === 'dm' ? await this.synthesizeDMChannel(channelId) : undefined);
      if (!channel) {
        throw new Error('Voice join response missing channel info');
      }

      store.setActiveChannel(channel.id, channel.name, channel.server_id);

      this.applyJoinMetadata(store, joinData, joinType, channelId);

      // Start outgoing ringback for DM calls
      if (joinType === 'dm') notificationSoundService.playLoop('call-outgoing');

      this.resolveQualityTier(store, channel.audio_quality_tier ?? undefined);

      // Pre-acquire mic stream in parallel with socket connection + room join.
      // getUserMedia can take 500ms+ (device enumeration, permission prompt).
      // Starting it now overlaps that latency with the network handshake below.
      const micPromise = this.acquireMicStream();

      // Step 2: Connect Socket.IO to media plane
      // Get the (possibly refreshed) access token for Socket.IO auth
      const token = useAuthStore.getState().accessToken;
      if (!token) throw new Error('Not authenticated');

      const user = useUserStore.getState().user;
      this.socket = io(media_server_url, {
        auth: {
          token,
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
        },
        transports: ['websocket'],
        reconnection: true,
        reconnectionAttempts: 10,
        reconnectionDelay: 1000,
      });

      await this.waitForConnect();
      this.setupSocketListeners();
      this.registerDocumentVisibilityListener();

      // Step 3: Join room on media plane
      const roomJoined = await this.emitAsync<RoomJoinedResponse>('join-room', {
        roomId: channel.id,
        rtpCapabilities: undefined, // Will be set after device.load
        mediaFrameCryptoVersion: MEDIA_E2EE_FRAME_CRYPTO_VERSION,
      });
      if (roomJoined.mediaFrameCryptoVersion !== MEDIA_E2EE_FRAME_CRYPTO_VERSION) {
        throw new Error('Media frame crypto version mismatch');
      }

      // Step 4: Load device with router capabilities
      this.device = new Device();
      this.routerRtpCapabilities = roomJoined.rtpCapabilities;
      await this.device.load({ routerRtpCapabilities: roomJoined.rtpCapabilities });

      // Send actual RTP capabilities to server (join-room sent undefined)
      if (!this.socket) throw new Error('Socket disconnected before RTP capabilities update');
      this.socket.emit('update-rtp-capabilities', {
        rtpCapabilities: this.device.rtpCapabilities,
      });

      // Step 5: Create transports in parallel (no dependency between send/recv)
      await Promise.all([this.createSendTransport(), this.createRecvTransports()]);

      // Step 6: Initialize E2EE (all channels are always encrypted)
      await this.setupE2EEForChannel(channel.id, roomJoined);

      // Step 7: Set participants before consuming producers
      store.setParticipants(this.buildParticipantList(roomJoined));

      // Apply enforcement flags after participants are populated
      this.applyEnforcementToParticipant(store, joinData);

      // Step 8: Produce audio (using pre-acquired mic stream)
      const preAcquiredStream = await micPromise;
      await this.produceAudio(undefined, preAcquiredStream);
      this.setupLiveSubscriptions();

      // Step 9: Consume existing producers
      await this.consumeExistingProducers(roomJoined.existingProducers);

      store.setConnectionState('connected');
      notificationSoundService.stopAllLoops();
      notificationSoundService.play(joinType === 'dm' ? 'call-connected' : 'voice-join');

      // Step 10: Start decoder budget profiling (IGNIS insight)
      // Profiles decode performance and adjusts SVC layers to avoid queue buildup
      this.startDecoderBudgetProfiling();
    } catch (err) {
      await this.handleJoinFailure(err);
      throw err;
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
    // #1878: a typed `crypto_version_mismatch` ack means the room negotiated a
    // newer media-frame crypto version than this client speaks — the client is
    // too old to join and must update. Surface the persistent "update required"
    // banner (same path as updater cert/publisher failures) instead of leaving
    // the user with a generic, non-actionable join failure.
    if (this.isCryptoVersionMismatch(err)) {
      useUpdateStatusStore
        .getState()
        .setSecurityError(
          'media-crypto-version',
          'This voice call requires a newer app version to join.'
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
  async leaveChannel(): Promise<void> {
    const store = useVoiceStore.getState();
    const channelId = store.activeChannelId;
    const isDMCall = store.isDMCall;
    const localUserId = useUserStore.getState().user?.id;

    if (this.socket?.connected) {
      this.socket.emit('leave-room');
    }
    await this.cleanup();

    notificationSoundService.stopAllLoops();
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

  /** Stop local streams, close producers/consumers/transports. */
  private cleanupMediaAndTransports(): void {
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

  /** Stop timers, subscriptions, and E2EE state. */
  private cleanupTimersAndE2EE(): void {
    this.stopLocalVAD();
    this.stopPacketLossMonitor();
    this.teardownLiveSubscriptions();
    if (this.decoderProfilingTimer) {
      clearInterval(this.decoderProfilingTimer);
      this.decoderProfilingTimer = null;
    }
    this.unregisterDocumentVisibilityListener();
    this.pauseCoordinator.reset();
    this.consecutiveGreenIntervals = 0;
    // #1878: drop the CSK key-rotation subscription so a torn-down session can't
    // re-base a destroyed encryption instance.
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
  }

  // ─── Audio Producer ────────────────────────────────────────────────

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

    const producer = await this.sendTransport.produce({
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

    // Apply E2EE encrypt transform if encrypted
    if (this.mediaEncryption) {
      this.applyEncryptTransform(producer);
    }

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
    fallbackChain: MediaStreamConstraints[]
  ): Promise<MediaStream | null> {
    for (const constraints of fallbackChain) {
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
    if (!this.sendTransport || !this.device) return;

    // Video slot enforcement
    const voiceState = useVoiceStore.getState();
    const videoOnCount = Object.values(voiceState.participants).filter((p) => p.isVideoOn).length;
    if (videoOnCount >= voiceState.maxVideoSlots) {
      voiceState.setVideoSlotError(
        `Maximum video streams reached (${voiceState.maxVideoSlots}). ` +
          'Wait for someone to turn off video or a screen share to end.'
      );
      return;
    }

    try {
      await this.ensureOsPermission('camera');

      const videoSettings = useVideoSettingsStore.getState();
      const preset =
        VIDEO_QUALITY_PRESETS[videoSettings.cameraPreset] || VIDEO_QUALITY_PRESETS['system'];
      const isSystemDefault = videoSettings.cameraPreset === 'system' || preset.width === 0;

      const fallbackChain = this.buildCameraFallbackChain(deviceId, preset, isSystemDefault);
      this.localCameraStream = await this.acquireCameraWithFallback(fallbackChain);

      if (!this.localCameraStream) {
        voiceState.setVideoSlotError(
          'Could not access camera. Check that your camera is connected and not in use by another application.'
        );
        return;
      }
      const track = this.localCameraStream.getVideoTracks()[0];

      const { codec, encodings } = this.pickCameraCodec();
      const cameraBitrate = this.cameraStartBitrate(encodings);

      const producer = await this.sendTransport.produce({
        track,
        encodings,
        codec,
        codecOptions: { videoGoogleStartBitrate: this.computeStartBitrate(cameraBitrate) },
        appData: { source: 'camera' },
      });

      this.applyDegradationPreference(producer);
      this.producers.set('camera', producer);

      if (this.mediaEncryption) {
        this.applyEncryptTransform(producer);
      }

      useVoiceStore.getState().setActiveCameraCodec(codec?.mimeType?.toLowerCase() ?? null);

      const store = useVoiceStore.getState();
      store.setVideoOn(true);
      const localUserId = useUserStore.getState().user?.id;
      if (localUserId && this.localCameraStream) {
        store.updateParticipant(localUserId, {
          videoStream: this.localCameraStream,
          isVideoOn: true,
        });
      }

      producer.on('transportclose', () => {
        this.producers.delete('camera');
        if (this.localCameraStream) {
          for (const t of this.localCameraStream.getTracks()) t.stop();
          this.localCameraStream = null;
        }
        const s = useVoiceStore.getState();
        s.setVideoOn(false);
        const uid = useUserStore.getState().user?.id;
        if (uid) s.updateParticipant(uid, { videoStream: undefined, isVideoOn: false });
      });
    } catch (err) {
      console.error('Failed to start camera:', errorMessage(err));
      if (this.localCameraStream) {
        for (const t of this.localCameraStream.getTracks()) t.stop();
        this.localCameraStream = null;
      }
      voiceState.setVideoSlotError(VoiceService.mapCameraError(err));
    }
  }

  /** Parse a screen resolution string into width/height. */
  private static parseScreenResolution(resolution: string): { w: number; h: number } {
    const resMap: Record<string, { w: number; h: number }> = {
      source: { w: 3840, h: 2160 },
      '720p': { w: 1280, h: 720 },
      '1080p': { w: 1920, h: 1080 },
      '1440p': { w: 2560, h: 1440 },
      '4K': { w: 3840, h: 2160 },
    };
    const customParsed = /^(\d+)x(\d+)$/.exec(resolution);
    return customParsed
      ? { w: Number(customParsed[1]), h: Number(customParsed[2]) }
      : resMap[resolution] || resMap['source'];
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
      const audioProducer = await this.sendTransport.produce({
        track: audioTracks[0],
        codecOptions: { opusStereo: true, opusDtx: false },
        appData: { source: 'screen-audio' },
      });

      this.producers.set('screen-audio', audioProducer);

      if (this.mediaEncryption) {
        this.applyEncryptTransform(audioProducer);
      }

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
    const screenRes = VoiceService.parseScreenResolution(resolution);

    let stream: MediaStream;
    try {
      stream = await this.captureScreen(sourceId, screenRes, screenFps);
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
      const { codec, encodings, effectiveBitrate: screenBitrate } = this.pickScreenCodec();

      const producer = await this.sendTransport.produce({
        track,
        encodings,
        codec,
        codecOptions: { videoGoogleStartBitrate: this.computeStartBitrate(screenBitrate) },
        appData: { source: 'screen' },
      });

      this.applyDegradationPreference(producer);
      this.producers.set('screen', producer);

      if (this.mediaEncryption) {
        this.applyEncryptTransform(producer);
      }

      useVoiceStore.getState().setActiveScreenCodec(codec?.mimeType?.toLowerCase() ?? null);

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
        s.setScreenSharing(false);
        const uid = useUserStore.getState().user?.id;
        if (uid) s.updateParticipant(uid, { screenStream: undefined, isScreenSharing: false });
      });
    } catch (err) {
      for (const t of stream.getTracks()) t.stop();
      this.localScreenStream = null;
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

    // Enforce 5-stream limit
    if (Object.keys(store.tunedInScreenShares).length >= 5) {
      store.setVideoSlotError('Maximum 5 screen shares reached. Tune out of one first.');
      return;
    }

    // Ensure decrypt key is ready for E2EE screen share
    if (this.mediaEncryption) {
      const channelId = store.activeChannelId;
      if (channelId) await this.addDecryptKeyForUser(channelId, userId);
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

    // Track in store
    store.tuneIn(producerId, consumerId);

    // Set as dominant if first tuned-in share
    if (!store.dominantScreenShareId) {
      store.setDominantScreenShare(producerId);
    }

    store.updateParticipant(userId, { isScreenSharing: true });

    // Consume paired screen audio if available (keep mapping for re-tune)
    const audioProducerId = this.pendingScreenAudioProducers.get(userId);
    if (audioProducerId) {
      await this.consumeProducer(audioProducerId, userId, 'audio');
    }
  }

  /** Opt-out of a tuned-in screen share */
  async tuneOutOfScreenShare(producerId: string): Promise<void> {
    const store = useVoiceStore.getState();
    const consumerId = store.tunedInScreenShares[producerId];

    // Identify the producing user from the video consumer's metadata
    const videoMeta = consumerId ? this.consumerMeta.get(consumerId) : undefined;
    const screenOwnerUserId = videoMeta?.producerUserId;

    // Close the screen video consumer
    if (consumerId) {
      const consumer = this.consumers.get(consumerId);
      if (consumer) {
        consumer.close();
        this.consumers.delete(consumerId);
        this.consumerMeta.delete(consumerId);
        this.lastPreferredLayerKeyByConsumer.delete(consumerId);
        this.pauseCoordinator.clearConsumer(consumerId);
      }
    }

    // Close the paired screen-audio consumer for this specific user
    if (screenOwnerUserId) {
      this.closeScreenAudioConsumerForUser(screenOwnerUserId, store);
    }

    // Find the producing user to update their participant state
    const producerUserId = screenOwnerUserId ?? this.findScreenShareOwner(store);

    // Add back to available shares
    if (producerUserId) {
      const participant = store.participants[producerUserId];
      store.addAvailableScreenShare({
        producerId,
        userId: producerUserId,
        username: participant?.username || 'Unknown',
        displayName: participant?.displayName,
      });
    }

    // Remove from tuned-in (this also handles dominant swap & slot recalculation)
    store.tuneOut(producerId);
  }

  /** Close the screen-audio consumer for a specific user. */
  private closeScreenAudioConsumerForUser(
    userId: string,
    store: ReturnType<typeof useVoiceStore.getState>
  ): void {
    for (const [cid, meta] of this.consumerMeta) {
      if (meta.source === 'screen-audio' && meta.producerUserId === userId) {
        const consumer = this.consumers.get(cid);
        if (consumer) {
          consumer.close();
          this.consumers.delete(cid);
          this.consumerMeta.delete(cid);
          this.lastPreferredLayerKeyByConsumer.delete(cid);
          this.pauseCoordinator.clearConsumer(cid);
          this.socket?.emit('close-consumer', { consumerId: cid });
        }
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

  // ─── Visibility-pause (#1541) ──────────────────────────────────────

  private resetRemoteVideoLayeringState(): void {
    this.cameraLayeringEnabled = false;
    this.cameraLayeringReproducePending = false;
    this.remoteVideoPressureByUser.clear();
    this.lastPreferredLayerKeyByConsumer.clear();
    this.remoteVideoRenderStateByUser.clear();
  }

  private scheduleCameraLayeringReproduce(): void {
    if (this.cameraLayeringReproduceInFlight) {
      this.cameraLayeringReproducePending = true;
      return;
    }

    this.cameraLayeringReproduceInFlight = true;
    void this.drainCameraLayeringReproduceQueue();
  }

  private async drainCameraLayeringReproduceQueue(): Promise<void> {
    try {
      do {
        this.cameraLayeringReproducePending = false;
        try {
          await this.fastReproduceCamera();
        } catch (err) {
          console.warn(
            '[camera-layering] failed to re-produce camera after gate change:',
            errorMessage(err)
          );
        }
      } while (this.cameraLayeringReproducePending);
    } finally {
      this.cameraLayeringReproduceInFlight = false;
    }
  }

  private findCameraConsumerIdForUser(userId: string): string | null {
    for (const [id, meta] of this.consumerMeta) {
      if (meta.source === 'camera' && meta.producerUserId === userId) return id;
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
    pressureStepDown?: boolean
  ): RemoteVideoLayerPayload | null {
    const states = this.remoteVideoRenderStateByUser.get(userId);
    if (!states || states.size === 0) return null;

    let bestVisible: RemoteVideoLayerPayload | null = null;
    let hidden: RemoteVideoLayerPayload | null = null;

    for (const state of states.values()) {
      const payload = this.layerPayloadForTileState(userId, state, pressureStepDown);
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

  private emitPreferredLayersForUser(userId: string): void {
    const consumerId = this.findCameraConsumerIdForUser(userId);
    if (!consumerId) return;

    const payload = this.computePreferredLayerPayloadForUser(userId);
    if (!payload) return;

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
    }
  ): void {
    const tiles =
      this.remoteVideoRenderStateByUser.get(userId) ??
      new Map<string, RemoteVideoTileRenderState>();
    tiles.set(tileId, {
      visible: state.visible,
      cssWidth: state.cssWidth,
      cssHeight: state.cssHeight,
      role: state.role,
      focusedWindow: state.focusedWindow,
    });
    this.remoteVideoRenderStateByUser.set(userId, tiles);

    this.setRemoteVideoVisibility(userId, state.visible, tileId);
    this.emitPreferredLayersForUser(userId);
  }

  /**
   * Deregister a camera tile on unmount — NOT a "report hidden", so a closing PiP frame
   * doesn't freeze a still-visible grid tile. Prunes the user entry when empty so the map
   * never accumulates departed users (#1541 Gitar review).
   */
  removeRemoteVideoTile(userId: string, tileId: string): void {
    const tiles = this.tileVisibilityByUser.get(userId);
    if (tiles) {
      tiles.delete(tileId);
      if (tiles.size === 0) this.tileVisibilityByUser.delete(userId);
      for (const id of this.cameraConsumerIdsForUser(userId)) {
        this.updateVisibilityReason(id, userId);
      }
    }

    const renderStates = this.remoteVideoRenderStateByUser.get(userId);
    if (!renderStates) return;
    renderStates.delete(tileId);
    if (renderStates.size === 0) {
      this.remoteVideoRenderStateByUser.delete(userId);
      return;
    }
    this.emitPreferredLayersForUser(userId);
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
  private async drainSendTransportQueue(): Promise<void> {
    const transport = this.sendTransport;
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

  private async cleanupScreenState(): Promise<void> {
    // Also close the paired screen-audio producer
    const audioProducer = this.producers.get('screen-audio');
    if (audioProducer) {
      audioProducer.close();
      await this.drainSendTransportQueue();
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

    const options = await this.emitAsync<TransportOptions>('create-transport', {
      direction: 'send',
    });

    this.sendTransport = this.device.createSendTransport({
      id: options.id,
      iceParameters: options.iceParameters,
      iceCandidates: options.iceCandidates,
      dtlsParameters: options.dtlsParameters,
      // E2EE (legacy path): encodedInsertableStreams enables createEncodedStreams().
      // NOT set for RTCRtpScriptTransform — the two mechanisms conflict (#295).
      // All channels are always encrypted, so this is unconditionally applied when supported.
      ...(HAS_ENCODED_STREAMS && {
        additionalSettings: {
          encodedInsertableStreams: true,
        } as unknown as Partial<RTCConfiguration>,
      }),
    });

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
      ...(HAS_ENCODED_STREAMS && {
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

      const consumer = await recvTransport.consume({
        id: result.id,
        producerId: result.producerId,
        kind: result.kind,
        rtpParameters: result.rtpParameters,
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
      } else {
        // Resume the consumer (was created paused on server)
        await this.emitAsync('resume-consumer', { consumerId: consumer.id });
        console.debug('[consume] consumer resumed', { consumerId: consumer.id });
      }

      // #1541: apply pending visibility intent AFTER the resume emit above. An
      // initially-hidden tile's pause-consumer must run after the unconditional
      // resume that starts the server-paused consumer — otherwise the resume
      // clobbers the pause and the off-screen tile keeps forwarding (Gitar review).
      if (result.source === 'camera') {
        this.applyInitialVisibilityReason(consumer.id, result.producerUserId);
      }
    } catch (err) {
      console.error('[consume] Failed to consume producer:', producerId, errorMessage(err));
    }
  }

  // ─── Socket Listeners ──────────────────────────────────────────────

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
      const participant = store.participants[userId];
      store.addAvailableScreenShare({
        producerId,
        userId,
        username: participant?.username || 'Unknown',
        displayName: participant?.displayName,
      });
      store.updateParticipant(userId, { isScreenSharing: true });
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
        store.updateParticipant(userId, { isScreenSharing: false, screenStream: undefined });
        store.removeAvailableScreenShare(producerId);
        // Clean up tunedInScreenShares so UI collapses back to user frame grid
        if (producerId in store.tunedInScreenShares) {
          store.tuneOut(producerId);
        }
      } else if (source === 'screen-audio') {
        store.updateParticipant(userId, { screenAudioStream: undefined });
        this.pendingScreenAudioProducers.delete(userId);
      }

      // Notify PiP proxy so open PiP windows can close their consumers
      this.onProducerClosed?.(producerId, userId);
    });

    this.socket.on(
      'user-joined',
      async ({
        userId,
        username,
        displayName,
        avatarUrl,
        e2eeEpoch,
      }: {
        userId: string;
        username: string;
        displayName?: string;
        avatarUrl?: string | null;
        e2eeEpoch?: number;
      }) => {
        // upsert (not addParticipant) with ROSTER fields only: if the consume
        // path already created a record for this user (join-vs-consume race,
        // #1873), a plain insert — or an upsert carrying isVideoOn/isMuted:false
        // — would clobber the media/enforcement state the consume path set.
        // upsertParticipant's create-branch supplies the false defaults for a
        // genuinely-new participant; the merge-branch preserves existing media.
        useVoiceStore.getState().upsertParticipant(userId, {
          username,
          displayName,
          avatarUrl: avatarUrl ?? undefined,
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
      this.handleCodecFloorChange(previousFloor, codecFloor);
    });

    this.socket.on('camera-layering-gate', ({ enabled }: { enabled: boolean }) => {
      const nextEnabled = enabled === true;
      if (this.cameraLayeringEnabled === nextEnabled) return;
      this.cameraLayeringEnabled = nextEnabled;
      this.scheduleCameraLayeringReproduce();
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
      }
    });

    this.socket.on('connect', () => {
      if (useVoiceStore.getState().connectionState === 'reconnecting') {
        useVoiceStore.getState().setConnectionState('connected');
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

    this.decoderProfilingTimer = setInterval(async () => {
      probeCount++;

      // Switch to slower interval after initial profiling
      if (probeCount === maxInitialProbes + 1 && this.decoderProfilingTimer) {
        clearInterval(this.decoderProfilingTimer);
        this.decoderProfilingTimer = setInterval(() => this.profileDecoders(), 30_000);
      }

      await this.profileDecoders();
    }, 5_000);
  }

  /**
   * Full IGNIS decoder budget profiling.
   *
   * Zone thresholds:
   *   Green:  rho < 0.67  (decode time < 67% of frame interval)
   *   Yellow: rho < 0.80  (approaching limit)
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
      return;
    }

    // Already at lowest layers — pause lowest-priority consumer
    this.pauseLowestPriorityConsumer(consumer);
  }

  /** Pause a consumer for decoder relief, preferring to pause camera over screen share */
  private pauseLowestPriorityConsumer(consumer: mediasoupTypes.Consumer): void {
    const tunedIn = useVoiceStore.getState().tunedInScreenShares;
    const isScreenShare = Object.values(tunedIn).includes(consumer.id);

    if (!isScreenShare) {
      this.pauseCoordinator.addReason(consumer.id, 'ignis');
      console.warn(`IGNIS RED: pausing camera consumer ${consumer.id} — no layers to reduce`);
      return;
    }

    // Try to find a still-decoding camera consumer to pause instead of the screen share
    for (const [, c] of this.consumers) {
      if (c.kind === 'video' && !c.paused && !Object.values(tunedIn).includes(c.id)) {
        this.pauseCoordinator.addReason(c.id, 'ignis');
        console.warn(
          `IGNIS RED: pausing camera consumer ${c.id} instead of screen share ${consumer.id}`
        );
        return;
      }
    }

    this.pauseCoordinator.addReason(consumer.id, 'ignis');
    console.warn(`IGNIS RED: pausing screen share consumer ${consumer.id} — no camera to demote`);
  }

  /** Extract video decoder stats from a stats report. Returns null if insufficient data. */
  private static extractDecoderStats(
    report: Record<string, unknown>
  ): { rho: number; p95DecodeMs: number; currentFps: number } | null {
    const totalDecodeTime = report.totalDecodeTime as number | undefined;
    const framesDecoded = report.framesDecoded as number | undefined;
    const currentFps = report.framesPerSecond as number | undefined;

    if (!totalDecodeTime || !framesDecoded || framesDecoded === 0 || !currentFps) return null;

    const avgDecodeMs = (totalDecodeTime / framesDecoded) * 1000;
    const p95DecodeMs = avgDecodeMs * 1.5;
    const rho = (p95DecodeMs * currentFps) / 1000;
    return { rho, p95DecodeMs, currentFps };
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

  private async profileDecoders(): Promise<void> {
    let worstZone: DecoderHealthZone = 'green';

    for (const [, consumer] of this.consumers) {
      if (consumer.kind !== 'video') continue;

      try {
        const stats = await consumer.getStats();
        for (const report of stats.values()) {
          if (report.type !== 'inbound-rtp' || report.kind !== 'video') continue;
          const decoded = VoiceService.extractDecoderStats(report as Record<string, unknown>);
          if (!decoded) continue;
          worstZone = this.classifyAndHandleDecoderZone(
            consumer,
            decoded.rho,
            decoded.p95DecodeMs,
            decoded.currentFps,
            worstZone
          );
        }
      } catch {
        // Stats not available — skip
      }
    }

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

  /** Initialize media encryption for an encrypted channel (fail-closed with retry) */
  /** Core encryption setup: derive key, create Worker or legacy MediaEncryption */
  private async initEncryptionCore(channelId: string, attempt: number): Promise<void> {
    const channelCSK = await e2eeService.getChannelKey(channelId);
    const userId = useUserStore.getState().user?.id;
    if (!userId) throw new Error('No local userId for E2EE init');

    const encryptKey = await deriveFrameKey(channelCSK, userId);
    // #1878: bind the encrypt key to the channel's authoritative CSK version so
    // every outgoing v3 frame stamps it. getChannelKey above has already cached
    // the version, so this is the value the SFU and every remote sender share.
    // Never leave the encrypt version at a stale 0 when the channel is higher.
    const keyVersion = e2eeService.getChannelKeyVersion(channelId);

    if (USE_SCRIPT_TRANSFORM) {
      this.initE2EEWorker();
      if (!this.e2eeWorker) throw new Error('E2EE Worker failed to initialize');
      this.e2eeWorker.postMessage({
        type: 'init',
        encryptKey,
        currentKeyId: 0,
        keyVersion,
      } satisfies E2EEWorkerMessage);
      this.mediaEncryption = new MediaEncryption();
      this.mediaEncryption.initFromKey(encryptKey, 0);
      this.mediaEncryption.setKeyVersion(keyVersion);
    } else {
      this.mediaEncryption = new MediaEncryption();
      await this.mediaEncryption.init(channelCSK, userId);
      this.mediaEncryption.setKeyVersion(keyVersion);
    }

    // #1878: subscribe to authoritative CSK rotations so the sender re-bases its
    // encrypt key onto the new version after a confirmed fetch (see
    // subscribeKeyRotation). Re-subscribe per init (the prior sub is cleared in
    // cleanupTimersAndE2EE); a no-op if onKeyRotation is unavailable.
    this.subscribeKeyRotation();

    console.debug('E2EE: encryption initialized', {
      channelId,
      userId,
      keyVersion,
      attempt: attempt + 1,
      useScriptTransform: USE_SCRIPT_TRANSFORM,
    });
  }

  /**
   * #1878 (the crux): subscribe to authoritative CSK rotations so the sender
   * re-bases its encrypt key onto the new version. The ordering IS the
   * rewrap-window seam: keep stamping the OLD version until the NEW CSK fetch is
   * CONFIRMED, then derive + install the new encrypt key and advance the stamped
   * version atomically. On fetch failure: no-op (stay on the old version; the
   * next rotation or epoch-sync retries). Stores the unsubscribe handle; cleared
   * in cleanupTimersAndE2EE.
   */
  private subscribeKeyRotation(): void {
    // Replace any prior subscription (re-init / reconnect).
    this.keyRotationOff?.();
    this.keyRotationOff = null;
    // Tolerate test/legacy harnesses where onKeyRotation is unavailable.
    if (typeof e2eeService.onKeyRotation !== 'function') return;
    this.keyRotationOff = e2eeService.onKeyRotation(({ channelId, keyVersion }) => {
      // Only the ACTIVE channel's rotation re-bases the live encrypt key.
      if (channelId !== useVoiceStore.getState().activeChannelId) return;
      void this.rebaseEncryptKey(channelId, keyVersion);
    });
  }

  /**
   * #1878: re-base the sender's encrypt key onto a rotated CSK after a CONFIRMED
   * fetch. Until the fetch resolves, outgoing frames still stamp the old version
   * (the encrypt key is untouched), so there is no window where the sender
   * stamps a version it can't back with a key.
   */
  private async rebaseEncryptKey(channelId: string, keyVersion: number): Promise<void> {
    try {
      const newCsk = await e2eeService.getChannelKeyByVersion(channelId, keyVersion);
      // Re-check the active channel after the await (it may have changed).
      if (channelId !== useVoiceStore.getState().activeChannelId) return;
      const userId = useUserStore.getState().user?.id;
      if (!userId || !this.mediaEncryption) return;
      const newEncryptKey = await deriveFrameKey(newCsk, userId);
      if (channelId !== useVoiceStore.getState().activeChannelId || !this.mediaEncryption) return;
      // Advance the local encrypt key + stamped version together.
      this.mediaEncryption.initFromKey(newEncryptKey, 0);
      this.mediaEncryption.setKeyVersion(keyVersion);
      this.e2eeWorker?.postMessage({
        type: 'init',
        encryptKey: newEncryptKey,
        currentKeyId: 0,
        keyVersion,
      } satisfies E2EEWorkerMessage);
      console.debug('E2EE: sender re-based encrypt key on CSK rotation', {
        channelId,
        keyVersion,
      });
    } catch {
      // Confirmed-fetch failed → stay on the old version. The next rotation or
      // epoch-sync retries; receivers self-heal via requestFrameKey meanwhile.
    }
  }

  /** Initialize media encryption for an encrypted channel (fail-closed with retry) */
  private async initEncryption(channelId: string): Promise<void> {
    const retryDelays = [500, 1000, 2000];
    let lastError: unknown;

    for (let attempt = 0; attempt <= retryDelays.length; attempt++) {
      try {
        await this.initEncryptionCore(channelId, attempt);
        return;
      } catch (err) {
        lastError = err;
        console.warn('E2EE: initEncryption attempt failed', {
          channelId,
          attempt: attempt + 1,
          error: err instanceof Error ? err.message : err,
        });

        if (attempt < retryDelays.length) {
          const delay = retryDelayForError(err, retryDelays[attempt]);
          await new Promise((r) => setTimeout(r, delay));
        }
      }
    }

    // All retries exhausted — fail-closed
    this.mediaEncryption = null;
    this.terminateE2EEWorker();
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

  /** Create the E2EE Worker for RTCRtpScriptTransform */
  private initE2EEWorker(): void {
    if (this.e2eeWorker) return;
    this.e2eeWorker = new Worker(new URL('../workers/e2eeWorker.ts', import.meta.url), {
      type: 'module',
    });
    this.e2eeWorker.onmessage = (event: MessageEvent<E2EEMainMessage>) => {
      this.handleWorkerMessage(event.data);
    };
    this.e2eeWorker.onerror = (err) => {
      console.error('E2EE Worker error:', errorMessage(err));
    };
  }

  /** Terminate and clean up the E2EE Worker */
  private terminateE2EEWorker(): void {
    if (this.e2eeWorker) {
      this.e2eeWorker.postMessage({ type: 'destroy' } satisfies E2EEWorkerMessage);
      this.e2eeWorker.terminate();
      this.e2eeWorker = null;
    }
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
        // #1878: the worker hit a typed decrypt miss for an exact
        // (sender, keyVersion, keyId). Fetch the authoritative CSK at that
        // version, derive the receiver key, and post it back. Fail-closed on
        // fetch failure (pending-404 / permanent-403): the worker rate-limits
        // and caps its own retries, so a no-op here is safe.
        const channelId = useVoiceStore.getState().activeChannelId;
        if (!channelId || !this.mediaEncryption) break;
        const mediaEncryption = this.mediaEncryption;
        e2eeService
          .getChannelKeyByVersion(channelId, msg.keyVersion)
          .then(async (csk) => {
            const key = await mediaEncryption.addDecryptKeyAtVersion(
              csk,
              msg.senderUserId,
              msg.keyVersion,
              msg.keyId
            );
            this.e2eeWorker?.postMessage({
              type: 'addDecryptKey',
              senderUserId: msg.senderUserId,
              keyVersion: msg.keyVersion,
              keyId: msg.keyId,
              key,
            } satisfies E2EEWorkerMessage);
          })
          .catch(() => {
            /* fail-closed: worker rate-limits + caps retries */
          });
        break;
      }

      case 'requestKeyframe': {
        if (useVoiceStore.getState().activeChannelId) {
          this.socket?.emit('request-keyframe', { senderUserId: msg.senderUserId });
        }
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
    if (!USE_SCRIPT_TRANSFORM) {
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
  /** Derive and install a decrypt key for a single user at the current epoch */
  private async deriveAndInstallDecryptKey(
    channelId: string,
    userId: string,
    attempt: number,
    targetEpoch?: number
  ): Promise<void> {
    if (!this.mediaEncryption) throw new Error('mediaEncryption destroyed');
    const channelCSK = await e2eeService.getChannelKey(channelId);
    if (!this.mediaEncryption) throw new Error('mediaEncryption destroyed');
    const keyId = targetEpoch ?? this.mediaEncryption.getCurrentKeyId();

    if (keyId > 100) {
      throw new Error(`E2EE: epoch ${keyId} exceeds ratchet limit (100), rejoin required`);
    }

    const key = await this.mediaEncryption.addDecryptKeyAtEpoch(channelCSK, userId, keyId);

    if (this.e2eeWorker) {
      // #1878: stamp the bound CSK version so the worker keys its decrypt map by
      // senderId:keyVersion:keyId (matches what addDecryptKeyAtEpoch registered
      // on the main-thread instance via its currentKeyVersion). Task 5 makes the
      // sender re-base advance this version authoritatively.
      this.e2eeWorker.postMessage({
        type: 'addDecryptKey',
        senderUserId: userId,
        keyVersion: this.mediaEncryption.getKeyVersion(),
        keyId,
        key,
      } satisfies E2EEWorkerMessage);
    }

    console.debug('E2EE: decrypt key added', {
      channelId,
      targetUserId: userId,
      currentEpoch: this.mediaEncryption.getCurrentKeyId(),
      keyId,
      attempt: attempt + 1,
    });
  }

  /** Add a decryption key for a remote user, pre-ratcheted to current epoch (with retry) */
  private async addDecryptKeyForUser(
    channelId: string,
    userId: string,
    targetEpoch?: number
  ): Promise<boolean> {
    if (!this.mediaEncryption) return false;

    const retryDelays = [500, 1000, 2000];
    let lastError: unknown;

    for (let attempt = 0; attempt <= retryDelays.length; attempt++) {
      try {
        await this.deriveAndInstallDecryptKey(channelId, userId, attempt, targetEpoch);
        return true;
      } catch (err) {
        lastError = err;
        if (attempt < retryDelays.length) {
          const isPending = isPendingKeyError(err);
          const delay = isPending ? retryDelays[attempt] * 2 : retryDelays[attempt];
          await new Promise((r) => setTimeout(r, delay));
        }
      }
    }

    console.error('E2EE: failed to add decrypt key after retries', {
      channelId,
      targetUserId: userId,
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
   * Apply E2EE encrypt transform to a producer.
   * Modern path: RTCRtpScriptTransform (Chromium 129+, Worker-based).
   * Legacy path: createEncodedStreams (Chromium 86-130, main-thread).
   */
  private applyEncryptTransform(producer: mediasoupTypes.Producer): void {
    const sender = producer.rtpSender;
    if (!sender) {
      this.failClosedEncryptTransform(producer, 'no rtpSender on producer');
    }

    // Modern path: RTCRtpScriptTransform (Chromium 129+)
    if (USE_SCRIPT_TRANSFORM && this.e2eeWorker) {
      try {
        const options: E2EETransformOptions = { role: 'encrypt' };
        sender.transform = new RTCRtpScriptTransform(this.e2eeWorker, options);
        console.debug('E2EE: encrypt transform applied (RTCRtpScriptTransform)');
      } catch (err) {
        console.error('E2EE: RTCRtpScriptTransform failed on sender:', errorMessage(err));
        this.failClosedEncryptTransform(producer, 'RTCRtpScriptTransform failed');
      }
      return;
    }

    // Legacy path: createEncodedStreams (Chromium 86-130)
    if (!this.mediaEncryption) {
      this.failClosedEncryptTransform(producer, 'media encryption is not initialized');
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
              await encryption.encryptFrame(frame);
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
        this.failClosedEncryptTransform(producer, 'createEncodedStreams failed');
      }
    } else {
      console.warn('E2EE: no Insertable Streams API available — frames will not be encrypted');
      this.failClosedEncryptTransform(producer, 'Insertable Streams API unavailable');
    }
  }

  private failClosedEncryptTransform(producer: mediasoupTypes.Producer, reason: string): never {
    const source =
      typeof producer.appData?.source === 'string' ? producer.appData.source : undefined;
    producer.close();
    if (source) this.producers.delete(source);
    this.socket?.emit('close-producer', { producerId: producer.id });
    throw new Error(`E2EE: failed to attach encrypt transform (${reason})`);
  }

  /**
   * Apply E2EE decrypt transform to a consumer.
   * Modern path: RTCRtpScriptTransform (Chromium 129+, Worker-based).
   * Legacy path: createEncodedStreams (Chromium 86-130, main-thread).
   */
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
    };
  }

  private applyDecryptTransform(consumer: mediasoupTypes.Consumer, senderUserId: string): void {
    const receiver = consumer.rtpReceiver;
    if (!receiver) {
      throw new Error('E2EE: failed to attach decrypt transform (no rtpReceiver on consumer)');
    }

    // Modern path: RTCRtpScriptTransform (Chromium 129+)
    if (USE_SCRIPT_TRANSFORM && this.e2eeWorker) {
      try {
        const options: E2EETransformOptions = { role: 'decrypt', senderUserId };
        receiver.transform = new RTCRtpScriptTransform(this.e2eeWorker, options);
        console.debug(
          `E2EE: decrypt transform applied for ${senderUserId} (RTCRtpScriptTransform)`
        );
      } catch (err) {
        console.error('E2EE: RTCRtpScriptTransform failed on receiver:', errorMessage(err));
        throw new Error(
          `E2EE: failed to attach decrypt transform (RTCRtpScriptTransform failed: ${errorMessage(err)})`
        );
      }
      return;
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
      E2EE_VERBOSE
    );
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
      if (!this.socket) return reject(new Error('No socket'));

      if (this.socket.connected) return resolve();

      const timeout = setTimeout(() => {
        reject(new Error('Socket connection timeout'));
      }, 10_000);

      this.socket.once('connect', () => {
        clearTimeout(timeout);
        resolve();
      });

      this.socket.once('connect_error', (err) => {
        clearTimeout(timeout);
        reject(err);
      });
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

    // Destroy E2EE
    this.terminateE2EEWorker();
    if (this.rotationTimer) {
      clearTimeout(this.rotationTimer);
      this.rotationTimer = null;
    }
    this.mediaEncryption?.destroy();
    this.mediaEncryption = null;

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
