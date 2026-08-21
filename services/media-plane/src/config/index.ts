import type { RouterRtpCodecCapability } from 'mediasoup/types';

// Node 24+ native .env loading, replacing the third-party `dotenv` package.
// `process.loadEnvFile()` reads ./.env and THROWS when the file is absent,
// whereas `dotenv.config()` soft-failed. Swallow only ENOENT so a missing
// .env stays non-fatal (the deployed services supply env directly) while a
// real fault — unreadable file, bad permissions — still surfaces.
try {
  process.loadEnvFile();
} catch (err) {
  if ((err as NodeJS.ErrnoException)?.code !== 'ENOENT') throw err;
}

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

/**
 * Sanity ceiling for the mediasoup worker pool (#2178 review). A worker is a
 * single-threaded C++ subprocess; no host this deploys to has anywhere near this
 * many cores, so a value above it is a typo, not a configuration.
 */
const MAX_MEDIASOUP_WORKERS = 32;

/**
 * Parse the mediasoup worker count fail-closed (#2178). Deliberately NOT
 * `parsePositiveIntEnv` above: a silent fallback is right for an operational
 * limit, but a bad worker count is unrecoverable.
 *
 * Clamped at BOTH ends, matching the discipline `[internal]rules/media-plane.md`
 * states for every other capacity value ("Both resolvers clamp both ends: the
 * upper bound stops a tier value raising the global ceiling; the lower bound
 * stops a 0/negative value from disabling the cap").
 *
 * Below the floor — `NUM_WORKERS=0` builds an empty worker pool, `init()` still
 * resolves, Express still listens and `/health` still answers 200. Then every
 * voice join reaches `MediasoupService.getWorker()` (lib/mediasoup.ts), whose
 * `workers[0]` is undefined, and the resulting TypeError is caught by the join
 * handler and returned as an ordinary join-error ack. The process does NOT
 * crash: it survives indefinitely, answering /health 200 while failing every
 * join, so nothing restarts it. That silent-black-hole shape is exactly why the
 * guard belongs at config load.
 *
 * Above the ceiling — `NUM_WORKERS=64` (a fat-fingered 6+4, or a value copied
 * from a bare-metal sizing note) forks 64 subprocesses inside a 4 GB / 3-CPU
 * cgroup, producing an OOM-kill → `restart: unless-stopped` → respawn loop with
 * no startup signal at all. That is the more likely typo of the two.
 *
 * Both compose files now set this explicitly, so an operator typo is caught at
 * startup by the same `FATAL:` + `process.exit(1)` mechanism as the JWT_SECRET
 * and ALLOWED_ORIGINS guards below.
 */
function parseWorkerCountEnv(raw: string | undefined, fallback: number): number {
  if (raw === undefined) return fallback;
  // Strict: bare Number.parseInt would accept '3.5' (-> 3) and '4abc' (-> 4).
  const trimmed = raw.trim();
  const workers = /^\d+$/.test(trimmed) ? Number.parseInt(trimmed, 10) : Number.NaN;
  if (!Number.isSafeInteger(workers) || workers < 1 || workers > MAX_MEDIASOUP_WORKERS) {
    console.error(
      `FATAL: NUM_WORKERS=${JSON.stringify(raw)} is not a valid worker count. ` +
        `It must be an integer between 1 and ${MAX_MEDIASOUP_WORKERS} ` +
        `(leave it unset for the default of ${fallback}). ` +
        'A zero or unparseable count builds an empty mediasoup worker pool that ' +
        'starts and answers /health, then fails every voice join without crashing; ' +
        'an oversized count forks more subprocesses than the container cgroup can hold.'
    );
    process.exit(1);
  }
  return workers;
}

function parseOpsMetricsIntervalMs(raw: string | undefined): number {
  const match = /^(\d+)(ms|s|m)$/.exec((raw || '15s').trim());
  if (!match) return Number.NaN;
  const value = Number.parseInt(match[1], 10);
  const multiplier = match[2] === 'ms' ? 1 : match[2] === 's' ? 1000 : 60_000;
  return value * multiplier;
}

const opsMetricsEnabled = process.env.OPS_METRICS_ENABLED?.trim().toLowerCase() === 'true';
const opsMetricsNodeId = opsMetricsEnabled ? (process.env.OPS_METRICS_NODE_ID || '').trim() : '';
const opsMetricsSharedSecret = opsMetricsEnabled
  ? (process.env.OPS_METRICS_SHARED_SECRET || '').trim()
  : '';
const opsMetricsIntervalMs = opsMetricsEnabled
  ? parseOpsMetricsIntervalMs(process.env.OPS_METRICS_INTERVAL)
  : 15_000;
const opsMetricsRole = opsMetricsEnabled
  ? (process.env.OPS_METRICS_ROLE || 'local').trim().toLowerCase()
  : 'local';

export const config = {
  environment: process.env.ENVIRONMENT || 'development',
  port: Number.parseInt(process.env.PORT || '3000', 10),

  // Free-tier per-room concurrent camera-producer cap (#1539). P0 capacity
  // guardrail: bounds SFU egress fan-out so one free user can't saturate a box.
  // Tier-aware since #1542: premium rooms resolve to PREMIUM_VIDEO_PUBLISHER_CAP
  // in roomManager; this is the free-room value.
  freeVideoPublisherCap: parsePositiveIntEnv(process.env.FREE_VIDEO_PUBLISHER_CAP, 8),

  // Free per-room concurrent screenshare-producer cap (#1542; raised 1→8 for
  // Discord parity — Discord caps stream quality, not concurrency). The premium
  // value (16) is a code constant in roomManager (PREMIUM_SCREEN_PRODUCER_CAP),
  // resolved per room tier by resolveScreenProducerCap — same shape as the
  // camera cap above.
  freeScreenProducerCap: parsePositiveIntEnv(process.env.FREE_SCREEN_PRODUCER_CAP, 8),

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

  // Aggregate-only operations snapshots (#1689). Dormant unless explicitly
  // enabled; publisher construction lives in index.ts.
  opsMetrics: {
    enabled: opsMetricsEnabled,
    nodeId: opsMetricsNodeId,
    sharedSecret: opsMetricsSharedSecret,
    intervalMs: opsMetricsIntervalMs,
    role: opsMetricsRole,
  },

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
    // Worker count (#2178). Fail-closed at load — see parseWorkerCountEnv. The
    // default describes an unconstrained host; each compose file sets its own
    // value (base "4", production "3" = its cpus limit).
    numWorkers: parseWorkerCountEnv(process.env.NUM_WORKERS, 4),

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

  // Peer addresses or CIDR blocks whose X-Real-IP / X-Forwarded-For may be
  // trusted by the #2032 admission gate (`lib/admissionGate.ts`). nginx fronts
  // the media-plane over a docker bridge, so entries are matched by RANGE — a
  // bridge address is not a stable literal. Mirrors the control-plane's
  // TRUSTED_PROXY_CIDRS; both compose files set a literal.
  //
  // An empty list makes the gate INERT (allow unconditionally, track nothing)
  // rather than attributing every client to the proxy and sharing one handshake
  // budget deployment-wide. Hence no fatal-exit guard: the misconfiguration
  // degrades to the pre-gate behaviour and is logged once at startup, where a
  // startup exit would take voice down.
  trustedProxies: (process.env.TRUSTED_PROXIES ?? '')
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

if (config.opsMetrics.enabled) {
  const problems: string[] = [];
  if (!/^cvn_[a-z2-7]{16}$/.test(config.opsMetrics.nodeId)) {
    problems.push('OPS_METRICS_NODE_ID must be an opaque assigned cvn_ token');
  }
  if (Buffer.byteLength(config.opsMetrics.sharedSecret, 'utf8') < 32) {
    problems.push('OPS_METRICS_SHARED_SECRET must be at least 32 bytes');
  }
  if (
    !Number.isFinite(config.opsMetrics.intervalMs) ||
    config.opsMetrics.intervalMs < 5000 ||
    config.opsMetrics.intervalMs > 300_000
  ) {
    problems.push('OPS_METRICS_INTERVAL must be from 5s through 5m');
  }
  if (config.opsMetrics.role === 'aggregator') {
    problems.push('OPS_METRICS_ROLE=aggregator is reserved until #1504');
  } else if (config.opsMetrics.role !== 'local') {
    problems.push('OPS_METRICS_ROLE must be local');
  }
  if (problems.length > 0) {
    console.error(`FATAL: invalid operations metrics config: ${problems.join('; ')}`);
    process.exit(1);
  }
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
