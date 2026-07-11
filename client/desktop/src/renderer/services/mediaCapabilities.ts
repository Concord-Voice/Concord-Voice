/**
 * Media Capabilities Service — probes webcam capabilities and codec support.
 *
 * Detects supported resolutions/framerates for webcams and available video
 * codecs (with hardware acceleration status) for use in quality settings.
 */

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface WebcamCapability {
  width: number;
  height: number;
  frameRate: number;
  label: string; // e.g. "720p30"
}

export interface CodecCapability {
  mimeType: string; // e.g. "video/VP9"
  sdpFmtpLine?: string;
  hwAvailable?: boolean; // true = GPU has a HW encoder, false = it does not, undefined = detection unavailable
  supported: boolean;
  profileId: string | null; // "640034" for H264 High, "2" for VP9 P2, null for VP8/AV1
  profileLabel: string | null; // "High", "Main", "Baseline", "HDR", null for no-profile codecs
  isHdr: boolean; // true for HDR-only profiles (VP9 P2). AV1 is HDR-capable but works in SDR too.
}

export interface GpuInfo {
  vendor: string;
  device: string;
  encodeProfiles: string[];
}

// ---------------------------------------------------------------------------
// Profile parsing
// ---------------------------------------------------------------------------

function parseProfile(
  mimeType: string,
  sdpFmtpLine?: string
): {
  id: string | null;
  label: string | null;
  isHdr: boolean;
} {
  const mime = mimeType.toLowerCase();

  // AV1 Main profile supports 10-bit HDR but also works in SDR — keep isHdr false
  // so it appears in the SDR codec list. HDR capability is shown via getCodecInfo().hdr.
  if (mime === 'video/av1') {
    return { id: null, label: null, isHdr: false };
  }

  if (!sdpFmtpLine) return { id: null, label: null, isHdr: false };

  if (mime === 'video/h264') {
    const match = /profile-level-id=([0-9a-fA-F]{6})/.exec(sdpFmtpLine);
    if (!match) return { id: null, label: null, isHdr: false };
    const plid = match[1].toLowerCase();
    const profile = plid.substring(0, 4);
    const labels: Record<string, string> = {
      '42e0': 'Baseline',
      '4d00': 'Main',
      '6400': 'High',
    };
    return { id: plid, label: labels[profile] ?? plid, isHdr: false };
  }

  if (mime === 'video/vp9') {
    const match = /profile-id=(\d+)/.exec(sdpFmtpLine);
    if (!match) return { id: '0', label: null, isHdr: false };
    const pid = match[1];
    return { id: pid, label: pid === '2' ? 'HDR' : null, isHdr: pid === '2' };
  }

  return { id: null, label: null, isHdr: false };
}

/**
 * Build a WebCodecs codec string for a HARDWARE-encode capability probe.
 * Levels target ~720p (the probe resolution in probeHardwareEncode) so
 * isConfigSupported's level check does not false-negative. Returns null for
 * codecs we do not probe (rtx/red/ulpfec are already filtered upstream).
 */
export function buildWebCodecsCodecString(
  mimeType: string,
  profileId: string | null,
  isHdr: boolean
): string | null {
  const mime = mimeType.toLowerCase();
  if (mime === 'video/vp8') return 'vp8';
  if (mime === 'video/vp9') return isHdr ? 'vp09.02.31.10' : 'vp09.00.31.08';
  if (mime === 'video/av1') return 'av01.0.05M.08';
  if (mime === 'video/h264') {
    // Preserve the profile_idc + constraint-flags bytes from the SDP
    // profile-level-id, but PIN the level to 3.1 (0x1f). The SDP level_idc can be
    // higher than the 720p probe needs (e.g. 640034 = level 5.2), which would
    // false-negative on a HW encoder that caps below the advertised level — the
    // exact "false software" outcome this probe exists to avoid. Mirrors the fixed
    // ~3.1 level used for the other codecs above.
    const base =
      profileId && /^[0-9a-fA-F]{6}$/.test(profileId)
        ? profileId.toLowerCase().substring(0, 4)
        : '4200';
    return `avc1.${base}1f`;
  }
  if (mime === 'video/h265' || mime === 'video/hevc') return 'hev1.1.6.L93.B0';
  return null;
}

// WebCodecs HW-encode probe. 1280x720 is the WebRTC default resolution and is
// universally hardware-supported where the codec is supported at all; the codec
// strings above carry levels that cover it.
const HW_PROBE_WIDTH = 1280;
const HW_PROBE_HEIGHT = 720;

/**
 * Probe whether the GPU exposes a genuine HARDWARE encoder for a codec string.
 * `hardwareAcceleration: 'prefer-hardware'` is the ONLY diagnostic variant —
 * Chromium's isConfigSupported filters out software codecs and forces a
 * hardware-only probe (verified in third_party/blink/.../webcodecs/video_encoder.cc).
 * Returns undefined when WebCodecs is unavailable or the probe throws — NEVER
 * coerce that to false, which would falsely claim "software".
 */
async function probeHardwareEncode(codec: string): Promise<boolean | undefined> {
  if (typeof VideoEncoder === 'undefined' || typeof VideoEncoder.isConfigSupported !== 'function') {
    return undefined;
  }
  try {
    const result = await VideoEncoder.isConfigSupported({
      codec,
      width: HW_PROBE_WIDTH,
      height: HW_PROBE_HEIGHT,
      hardwareAcceleration: 'prefer-hardware',
    });
    return result.supported === true;
  } catch {
    return undefined;
  }
}

/** Build a unique key like "video/H264:640034" or "video/VP8" */
export function codecKey(cap: CodecCapability): string {
  return cap.profileId ? `${cap.mimeType}:${cap.profileId}` : cap.mimeType;
}

/** Extract mimeType from a codec key */
export function codecKeyMime(key: string): string {
  return key.split(':')[0];
}

// ---------------------------------------------------------------------------
// Codec description data
// ---------------------------------------------------------------------------

export interface CodecInfo {
  name: string;
  quality: string;
  efficiency: string;
  compressionRatio: string;
  hdr: boolean;
  notes: string;
}

export function getCodecInfo(key: string): CodecInfo {
  const [mime, profileId] = key.split(':');
  const m = mime.toLowerCase();

  if (m === 'video/av1')
    return {
      name: 'AV1',
      quality: 'Excellent',
      efficiency: 'Very High',
      compressionRatio: '~50% better than H.264',
      hdr: true,
      notes: 'HDR/10-bit capable. Best for screen sharing. Requires modern GPU for HW encode.',
    };
  if (m === 'video/vp9') {
    if (profileId === '2')
      return {
        name: 'VP9 (HDR)',
        quality: 'Excellent',
        efficiency: 'High',
        compressionRatio: '~40% better than H.264',
        hdr: true,
        notes: '10-bit color depth. SVC capable. Requires HDR display for full benefit.',
      };
    return {
      name: 'VP9',
      quality: 'Very Good',
      efficiency: 'High',
      compressionRatio: '~40% better than H.264',
      hdr: false,
      notes: 'SVC capable. Good balance of quality and compatibility.',
    };
  }
  if (m === 'video/h264') {
    if (profileId?.startsWith('6400'))
      return {
        name: 'H.264 (High)',
        quality: 'Very Good',
        efficiency: 'Good',
        compressionRatio: '~25% better than Baseline',
        hdr: false,
        notes: '8x8 transforms, best H.264 compression. Ideal for high-res.',
      };
    if (profileId?.startsWith('4d00'))
      return {
        name: 'H.264 (Main)',
        quality: 'Good',
        efficiency: 'Moderate',
        compressionRatio: '~15% better than Baseline',
        hdr: false,
        notes: 'B-frames & CABAC entropy coding. Good general-purpose profile.',
      };
    return {
      name: 'H.264 (Baseline)',
      quality: 'Baseline',
      efficiency: 'Moderate',
      compressionRatio: 'Reference',
      hdr: false,
      notes: 'Maximum compatibility, lowest latency. No B-frames.',
    };
  }
  if (m === 'video/vp8')
    return {
      name: 'VP8',
      quality: 'Basic',
      efficiency: 'Low',
      compressionRatio: 'Similar to H.264 Baseline',
      hdr: false,
      notes: 'Universal fallback. Always works.',
    };

  return {
    name: mime.replace('video/', ''),
    quality: 'Unknown',
    efficiency: 'Unknown',
    compressionRatio: 'Unknown',
    hdr: false,
    notes: '',
  };
}

// ---------------------------------------------------------------------------
// Probe resolutions
// ---------------------------------------------------------------------------

const PROBE_RESOLUTIONS: { width: number; height: number; label: string }[] = [
  { width: 3840, height: 2160, label: '4K' },
  { width: 2560, height: 1440, label: '1440p' },
  { width: 1920, height: 1080, label: '1080p' },
  { width: 1280, height: 720, label: '720p' },
  { width: 640, height: 360, label: '360p' },
];

const PROBE_FRAMERATES = [60, 30, 15];

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

const cachedWebcamCaps: Map<string, WebcamCapability[]> = new Map();
let cachedCodecCaps: CodecCapability[] | null = null;

/**
 * Enumerate supported resolutions and frame rates for a given webcam.
 * Results are cached per deviceId to avoid repeated probing.
 */
export async function enumerateWebcamCapabilities(deviceId: string): Promise<WebcamCapability[]> {
  const cached = cachedWebcamCaps.get(deviceId);
  if (cached) {
    return cached;
  }

  const capabilities: WebcamCapability[] = [];

  for (const res of PROBE_RESOLUTIONS) {
    for (const fps of PROBE_FRAMERATES) {
      try {
        const stream = await navigator.mediaDevices.getUserMedia({
          video: {
            deviceId: { exact: deviceId },
            width: { exact: res.width },
            height: { exact: res.height },
            frameRate: { exact: fps },
          },
        });

        // Success — this combination is supported
        capabilities.push({
          width: res.width,
          height: res.height,
          frameRate: fps,
          label: `${res.label}${fps}`,
        });

        // Clean up the test stream
        for (const t of stream.getTracks()) t.stop();
      } catch {
        // Not supported at this resolution/fps — skip
      }
    }
  }

  cachedWebcamCaps.set(deviceId, capabilities);
  return capabilities;
}

/**
 * Detect supported video codecs and their hardware acceleration status.
 * Uses RTCRtpSender.getCapabilities and navigator.mediaCapabilities.
 * Profile-aware: returns separate entries for H264 Baseline/Main/High and VP9 P0/P2.
 */
export async function detectCodecCapabilities(): Promise<CodecCapability[]> {
  if (cachedCodecCaps) return cachedCodecCaps;

  const capabilities: CodecCapability[] = [];
  const senderCaps = RTCRtpSender.getCapabilities('video');
  if (!senderCaps) return capabilities;

  // De-duplicate by mimeType + profile (not mimeType alone)
  const seen = new Set<string>();
  const uniqueCodecs = senderCaps.codecs.filter((c) => {
    if (c.mimeType.includes('rtx') || c.mimeType.includes('red') || c.mimeType.includes('ulpfec'))
      return false;
    const { id } = parseProfile(c.mimeType, c.sdpFmtpLine);
    const key = `${c.mimeType}:${id ?? ''}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });

  // Probe hardware-encode capability per unique WebCodecs codec string.
  // isConfigSupported({hardwareAcceleration:'prefer-hardware'}) is true ONLY when
  // the GPU exposes a genuine hardware encoder for that codec — the reliable
  // signal the old getGPUInfo VEA list and WebRTC powerEfficient hint both lacked.
  const parsed = uniqueCodecs.map((codec) => {
    const p = parseProfile(codec.mimeType, codec.sdpFmtpLine);
    return { codec, ...p, codecString: buildWebCodecsCodecString(codec.mimeType, p.id, p.isHdr) };
  });

  const hwByCodecString = new Map<string, boolean | undefined>();
  const uniqueStrings = [
    ...new Set(parsed.map((x) => x.codecString).filter((s): s is string => s !== null)),
  ];
  await Promise.all(
    uniqueStrings.map(async (codecString) => {
      hwByCodecString.set(codecString, await probeHardwareEncode(codecString));
    })
  );

  const caps: CodecCapability[] = parsed.map(({ codec, id, label, isHdr, codecString }) => ({
    mimeType: codec.mimeType,
    sdpFmtpLine: codec.sdpFmtpLine,
    hwAvailable: codecString === null ? undefined : hwByCodecString.get(codecString),
    supported: true,
    profileId: id,
    profileLabel: label,
    isHdr,
  }));

  // WebCodecs support is stable within a session, so cache once we have codecs
  // (getCapabilities()===null still returns [] above without caching).
  if (caps.length > 0) {
    cachedCodecCaps = caps;
  }
  return caps;
}

/** Clear cached capabilities (e.g. on device change) */
export function clearCapabilitiesCache(): void {
  cachedWebcamCaps.clear();
  cachedCodecCaps = null;
}

/**
 * Pre-warm the WebRTC engine so the first voice join doesn't pay the cold-start
 * penalty (~2s for ICE agent initialization, network interface enumeration, and
 * DTLS handshake setup). After warming, subsequent RTCPeerConnection creations
 * (inside mediasoup-client) complete in ~100-200ms instead of ~2s.
 *
 * Also preloads the voice service chunk (~300KB mediasoup-client + socket.io).
 */
export function prewarmWebRTC(): void {
  try {
    const pc = new RTCPeerConnection();
    // Creating and closing a PeerConnection initializes the ICE agent,
    // enumerates network interfaces, and warms the DTLS certificate cache.
    pc.close();
  } catch {
    // WebRTC not available — non-critical
  }

  // Preload the voice service chunk so it's cached when user clicks Join
  import('./voiceService').catch(() => {
    // Non-critical — will load on demand when user joins voice
  });
}
