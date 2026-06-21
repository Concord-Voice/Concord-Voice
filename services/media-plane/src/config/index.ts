import dotenv from 'dotenv';
import type { RouterRtpCodecCapability } from 'mediasoup/types';

dotenv.config();

// ---------------------------------------------------------------------------
// Audio quality tiers — enforced client-side via producer codecOptions.
// The router advertises the maximum (510 kbps stereo); each producer selects
// its tier by setting maxAverageBitrate, DTX, FEC, and stereo in SDP fmtp.
// ---------------------------------------------------------------------------
export const AUDIO_QUALITY_TIERS = {
  minimum: {
    label: 'Minimum',
    description: 'Optimized for pure survival over quality',
    maxBitrate: 16_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 60,
    premium: false,
  },
  low: {
    label: 'Low',
    description: 'Prioritizes keeping you in the conversation',
    maxBitrate: 32_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 40,
    premium: false,
  },
  moderate: {
    label: 'Moderate',
    description: 'The industry standard sweet spot',
    maxBitrate: 64_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 20,
    premium: false,
  },
  standard: {
    label: 'Standard',
    description: 'The Concord default, maximum clarity',
    maxBitrate: 96_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 20,
    premium: false,
  },
  high: {
    label: 'High',
    description: 'Virtually transparent clarity',
    maxBitrate: 192_000,
    opusDtx: false,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 10,
    premium: true,
  },
  hifi: {
    label: 'Hi-Fi',
    description: 'Maximum fidelity for power users',
    maxBitrate: 256_000,
    opusDtx: false,
    opusFec: false,
    opusStereo: true,
    preferredFrameSize: 10,
    premium: true,
  },
  studio: {
    label: 'Studio',
    description: 'The absolute ceiling, acoustically transparent 48kHz/16-bit',
    maxBitrate: 510_000,
    opusDtx: false,
    opusFec: false,
    opusStereo: true,
    preferredFrameSize: 10,
    premium: true,
  },
} as const;

export type AudioQualityTier = keyof typeof AUDIO_QUALITY_TIERS;

// ---------------------------------------------------------------------------
// RTCP feedback common to all video codecs
// ---------------------------------------------------------------------------
const VIDEO_RTCP_FEEDBACK = [
  { type: 'nack' as const },
  { type: 'nack' as const, parameter: 'pli' },
  { type: 'ccm' as const, parameter: 'fir' },
  { type: 'goog-remb' as const },
  { type: 'transport-cc' as const },
];

// ---------------------------------------------------------------------------
// Router media codecs — full codec suite for voice, video & screenshare.
//
// Audio:
//   Opus       — Primary codec, quality controlled per-producer (24-510 kbps)
//   Multiopus  — 5.1 / 7.1 surround (premium)
//
// Video (all with NACK, PLI, FIR, REMB, transport-cc):
//   VP8        — Universal fallback, wide HW decode support
//   VP9        — Default for video calls (SVC L3T3 capable), HDR via Profile 2
//   H264       — Constrained Baseline (compat), Main (5.0), High (5.2 = 4K60)
//   AV1        — Best compression, SVC capable, royalty-free, HDR via Main profile
//
// Strategy (codec preference enforced client-side in voiceService.ts):
//   Camera video     → VP9 SVC L3T3 (3 spatial + 3 temporal layers)
//   Camera fallback  → H264 simulcast (180p / 360p / full) → VP8 simulcast
//   Screen sharing   → AV1 SVC L3T3 (best compression for static content)
//   Screen fallback  → VP9 SVC → H264
//   Premium 4K60     → H264 High Level 5.2 (universal HW), AV1 SVC (better compression)
//   HDR              → VP9 Profile 2 (10-bit), AV1 Main (10-bit capable)
//
// IGNIS insight: 4:2:0 subsampling is sufficient for most use cases. Only
// text-heavy screenshare benefits from 4:4:4. Default to 4:2:0.
// ---------------------------------------------------------------------------
// Codec capabilities for the mediasoup router.
//
// PT assignment is left to mediasoup's pool allocator (dynamic range 96-127,
// pool order [100..127, 96..99]).  Manual preferredPayloadType was removed
// because the pool uses shift()-based RTX allocation — NOT codec_PT+1 — which
// caused collisions when manual PTs overlapped with auto-assigned RTX PTs.
//
// With the current 8 active codecs (1 audio + 7 video), the allocator assigns:
//   Opus→100, VP8→101 RTX→102, VP9P0→103 RTX→104, VP9P2→105 RTX→106,
//   H264Base→107 RTX→108, H264Main→109 RTX→110, H264High→111 RTX→112,
//   AV1→113 RTX→114  (15 of 32 dynamic PTs used)
//
// Codec selection priority is handled client-side, not by array order here.
const mediaCodecs: RouterRtpCodecCapability[] = [
  // ── AUDIO ──────────────────────────────────────────────────────────────

  // Opus stereo — router advertises max capability; per-producer narrows
  {
    kind: 'audio',
    mimeType: 'audio/opus',
    clockRate: 48000,
    channels: 2,
    parameters: {
      useinbandfec: 1, // Forward Error Correction (critical for voice)
      usedtx: 0, // DTX off by default; voice tier enables per-producer
      stereo: 1,
      'sprop-stereo': 1,
      maxplaybackrate: 48000, // Full-band (20 kHz effective)
      maxaveragebitrate: 510000,
      minptime: 10, // 10 ms minimum packet time
    },
  },

  // NOTE: Multiopus (5.1/7.1 surround) is not usable in browser-based clients.
  // getUserMedia/getDisplayMedia capture mono/stereo only, and WebRTC audio
  // playback has no path to route multichannel output to surround devices.
  // mediasoup supports it for native clients, but Electron/Chromium cannot
  // produce or consume multichannel audio via WebRTC. Commented out to avoid
  // unnecessary SDP bloat. Re-enable if a native audio pipeline is added.
  //
  // { kind: 'audio', mimeType: 'audio/multiopus', clockRate: 48000, channels: 6,
  //   parameters: { channel_mapping: '0,4,1,2,3,5', num_streams: 4, coupled_streams: 2 } },
  //
  // { kind: 'audio', mimeType: 'audio/multiopus', clockRate: 48000, channels: 8,
  //   parameters: { channel_mapping: '0,6,1,2,3,4,5,7', num_streams: 5, coupled_streams: 3 } },

  // ── VIDEO: VP8 — Universal fallback ────────────────────────────────────
  {
    kind: 'video',
    mimeType: 'video/VP8',
    clockRate: 90000,
    parameters: {},
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // ── VIDEO: VP9 Profile 0 — Default (SVC L3T3, 8-bit 4:2:0) ───────────
  {
    kind: 'video',
    mimeType: 'video/VP9',
    clockRate: 90000,
    parameters: {
      'profile-id': 0,
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // ── VIDEO: VP9 Profile 2 — HDR capable (10-bit 4:2:0) ─────────────────
  {
    kind: 'video',
    mimeType: 'video/VP9',
    clockRate: 90000,
    parameters: {
      'profile-id': 2,
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // ── VIDEO: H264 Constrained Baseline — Max compatibility ───────────────
  {
    kind: 'video',
    mimeType: 'video/H264',
    clockRate: 90000,
    parameters: {
      'packetization-mode': 1,
      'profile-level-id': '42e01f', // Constrained Baseline, Level 3.1
      'level-asymmetry-allowed': 1,
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // ── VIDEO: H264 Main — Better compression ─────────────────────────────
  {
    kind: 'video',
    mimeType: 'video/H264',
    clockRate: 90000,
    parameters: {
      'packetization-mode': 1,
      'profile-level-id': '4d0032', // Main profile, Level 5.0
      'level-asymmetry-allowed': 1,
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // ── VIDEO: H264 High — 4K60 capable (Level 5.2) ───────────────────────
  {
    kind: 'video',
    mimeType: 'video/H264',
    clockRate: 90000,
    parameters: {
      'packetization-mode': 1,
      'profile-level-id': '640034', // High profile, Level 5.2
      'level-asymmetry-allowed': 1,
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },

  // NOTE: H265/HEVC is NOT usable. mediasoup lacks support (as of 3.13.x),
  // Chrome/Firefox refuse to ship it in WebRTC, and HEVC carries patent
  // royalty obligations (MPEG LA, HEVC Advance, Access Advance) that
  // require legal review before any implementation. AV1 covers the same
  // use case royalty-free with better compression.

  // ── VIDEO: AV1 — Best compression, SVC capable ────────────────────────
  // Chrome 90+ encode (software libaom), Chrome 113+ improved real-time.
  // HW encode: NVIDIA RTX 40+, Intel Arc, AMD RX 7000+.
  // Ideal for screen sharing (mostly static, huge compression gains).
  {
    kind: 'video',
    mimeType: 'video/AV1',
    clockRate: 90000,
    parameters: {
      profile: 0, // Main profile (8-bit)
    },
    rtcpFeedback: VIDEO_RTCP_FEEDBACK,
  },
];

// ---------------------------------------------------------------------------
// Main configuration
// ---------------------------------------------------------------------------
const DEV_JWT_SECRET = 'dev_jwt_secret_change_in_production';

/**
 * Parse a positive-integer env var with a safe fallback. Returns `fallback`
 * when the value is missing, non-numeric, or < 1 — a config typo must never
 * crash the SFU or zero-out an operational limit. (Non-secret operational int:
 * no production fatal-exit guard needed, unlike the JWT-secret pattern.)
 */
function parsePositiveIntEnv(raw: string | undefined, fallback: number): number {
  const n = Number.parseInt(raw ?? '', 10);
  return Number.isFinite(n) && n >= 1 ? n : fallback;
}

export const config = {
  environment: process.env.ENVIRONMENT || 'development',
  port: Number.parseInt(process.env.PORT || '3000', 10),

  // Free-tier per-room concurrent camera-producer cap (#1539). P0 capacity
  // guardrail: bounds SFU egress fan-out so one free user can't saturate a box.
  // TODO(#1294): paid tier raises this to the absolute ceiling once the
  // entitlement resolver lands; until then all rooms use this free default.
  freeVideoPublisherCap: parsePositiveIntEnv(process.env.FREE_VIDEO_PUBLISHER_CAP, 8),

  // Audio last-N (#1544): free default forwarded-speaker cap. Tier-aware paid
  // value (16) deferred behind the #1294 seam, exactly like freeVideoPublisherCap.
  freeAudioLastN: parsePositiveIntEnv(process.env.FREE_AUDIO_LAST_N, 8),
  // Leave-hysteresis hold (ms) before a silent speaker is paused — clamped to a
  // sane range so a bad env value can't disable hysteresis or pin speakers forever.
  audioLastNHoldMs: Math.max(
    500,
    Math.min(10_000, parsePositiveIntEnv(process.env.AUDIO_LAST_N_HOLD_MS, 2500))
  ),

  // Redis — room state, cross-instance coordination
  // Default includes dev password to match docker-compose and control plane defaults
  redisUrl: process.env.REDIS_URL || 'redis://:concord_dev_redis@localhost:6379',

  // NATS — inter-service messaging (control plane ↔ media plane)
  natsUrl: process.env.NATS_URL || 'nats://localhost:4222',

  // Control plane — authorization checks, voice join validation
  controlPlaneUrl: process.env.CONTROL_PLANE_URL || 'http://localhost:8080',

  // JWT — shared secret for Socket.IO auth (must match control plane)
  jwtSecret: process.env.JWT_SECRET || DEV_JWT_SECRET,

  // ICE servers (STUN/TURN) are managed by the control plane and returned
  // to clients in the POST /voice/join response with ephemeral HMAC credentials.
  // See: services/control-plane/pkg/config/turn.go

  // WebRTC settings
  rtc: {
    announcedIp: process.env.ANNOUNCED_IP || '127.0.0.1',
    minPort: Number.parseInt(process.env.RTC_MIN_PORT || '40000', 10),
    maxPort: Number.parseInt(process.env.RTC_MAX_PORT || '49999', 10),
  },

  // mediasoup settings
  mediasoup: {
    numWorkers: Number.parseInt(process.env.NUM_WORKERS || '4', 10),

    worker: {
      logLevel: (process.env.MEDIASOUP_LOG_LEVEL || 'warn') as mediasoup.types.WorkerLogLevel,
      logTags: ['info', 'ice', 'dtls', 'rtp', 'srtp', 'rtcp'] as mediasoup.types.WorkerLogTag[],
      rtcMinPort: Number.parseInt(process.env.RTC_MIN_PORT || '40000', 10),
      rtcMaxPort: Number.parseInt(process.env.RTC_MAX_PORT || '49999', 10),
    },

    // Header extensions are NOT declared here — mediasoup advertises its built-in
    // supported set (incl. `urn:ietf:params:rtp-hdrext:ssrc-audio-level`, RFC 6464)
    // implicitly via getSupportedRtpCapabilities(). That audio-level extension is
    // load-bearing: under E2EE it is the ONLY cleartext loudness signal the
    // AudioLevelObserver (active-speaker detection / audio last-N, #1544) can read.
    // Do NOT add a headerExtensions filter that drops it. Locked by
    // tests/audioLevelHeaderExtension.test.ts (#1543).
    router: {
      mediaCodecs,
    },

    webRtcTransport: {
      listenIps: [
        {
          ip: '0.0.0.0',
          announcedIp: process.env.ANNOUNCED_IP || '127.0.0.1',
        },
      ],
      // IGNIS insight: generous bitrate headroom — let congestion control find optimal rate
      initialAvailableOutgoingBitrate: 1_000_000, // 1 Mbps initial estimate
      maxIncomingBitrate: 50_000_000, // 50 Mbps cap (supports 4K60)
      enableUdp: true,
      enableTcp: true, // TCP fallback for restrictive firewalls
      preferUdp: true, // UDP preferred for lower latency
    },
  },

  // CORS — allowed origins for Socket.IO (env-configurable for LAN/staging)
  allowedOrigins: (process.env.ALLOWED_ORIGINS || 'http://localhost:3001,http://localhost:3002')
    .split(',')
    .map((s: string) => s.trim())
    .filter(Boolean),

  // Active speaker detection
  audioLevelObserver: {
    maxEntries: 1, // Report top 1 speaker
    threshold: -60, // dBV threshold (lower = more sensitive)
    interval: 300, // Check every 300ms for snappier feedback
  },
};

// Production safety guard — mirrors control-plane's config.validate()
if (config.environment === 'production' && config.jwtSecret === DEV_JWT_SECRET) {
  console.error(
    'FATAL: JWT_SECRET must be set to a secure value in production. ' +
      'The default dev secret is not allowed.'
  );
  process.exit(1);
}

// Production safety guard — reject wildcard '*' in ALLOWED_ORIGINS when paired
// with Socket.IO's credentials: true. CWE-942: a wildcard credentialed origin
// allows cross-origin Socket.IO hijack against authenticated sessions. The
// runtime origin gate (originGate.ts) and control-plane CORS both pass '*'
// through for parity; this is a config-load guard to reject the foot-gun before
// the server starts accepting connections.
if (config.environment === 'production' && config.allowedOrigins.includes('*')) {
  console.error(
    "FATAL: ALLOWED_ORIGINS may not contain '*' in production. " +
      'Socket.IO is configured with credentials: true; a wildcard origin would ' +
      'permit cross-origin hijack against authenticated sessions. ' +
      'Set ALLOWED_ORIGINS to an explicit comma-separated list of allowed origins.'
  );
  process.exit(1);
}

// Re-export mediasoup types for convenience
import * as mediasoup from 'mediasoup';
export type * as mediasoupTypes from 'mediasoup';
