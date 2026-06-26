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
  powerEfficient: boolean; // raw MediaCapabilities hint; false means "not confirmed"
  hwAvailable?: boolean; // true = confirmed HW, false = populated profiles exclude it, undefined = unknown
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

// Multiple resolutions for HW acceleration probing — some GPU drivers only report
// powerEfficient at certain resolutions. Try from most-commonly-accelerated to highest.
const HW_PROBE_CONFIGS = [
  { width: 1920, height: 1080, bitrate: 4_000_000, framerate: 30 },
  { width: 1280, height: 720, bitrate: 2_000_000, framerate: 30 },
  { width: 3840, height: 2160, bitrate: 10_000_000, framerate: 30 },
];

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

const cachedWebcamCaps: Map<string, WebcamCapability[]> = new Map();
let cachedCodecCaps: CodecCapability[] | null = null;

function mimeSet(values: string[] | undefined): Set<string> {
  return new Set((values ?? []).map((value) => value.toLowerCase()));
}

function resolveHardwareAvailability(
  mimeType: string,
  encodeMimes: Set<string>,
  powerEfficient: boolean,
  profilesPopulated: boolean
): boolean | undefined {
  if (encodeMimes.has(mimeType.toLowerCase())) return true;
  if (powerEfficient) return true;
  if (profilesPopulated) return false;
  return undefined;
}

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

  // Probe HW acceleration once per mimeType using multiple resolutions.
  // navigator.mediaCapabilities.encodingInfo() only accepts mimeType-level contentType
  // (not codec profiles), so per-mimeType is the correct granularity.
  // Some GPU drivers only report powerEfficient at certain resolutions, so we probe
  // at 1080p, 720p, and 4K — short-circuiting on the first positive result.
  const hwByMime = new Map<string, boolean>();
  const uniqueMimes = [...new Set(uniqueCodecs.map((c) => c.mimeType))];
  await Promise.all(
    uniqueMimes.map(async (mime) => {
      let powerEfficient = false;
      if ('mediaCapabilities' in navigator) {
        for (const probe of HW_PROBE_CONFIGS) {
          try {
            const info = await navigator.mediaCapabilities.encodingInfo({
              type: 'webrtc',
              video: {
                contentType: mime,
                width: probe.width,
                height: probe.height,
                bitrate: probe.bitrate,
                framerate: probe.framerate,
              },
            });
            if (info.powerEfficient) {
              powerEfficient = true;
              break; // HW confirmed — no need to probe further
            }
          } catch {
            // MediaCapabilities may not support WebRTC type on all browsers
          }
        }
      }
      hwByMime.set(mime, powerEfficient);
    })
  );

  const gpuInfo = await globalThis.electron?.getGPUInfo?.().catch(() => null);
  const profileSignalUnavailable = gpuInfo === undefined;
  const encodeMimes = mimeSet(gpuInfo?.encodeProfiles);
  const profilesPopulated = encodeMimes.size > 0;

  const caps = uniqueCodecs.map((codec) => {
    const { id, label, isHdr } = parseProfile(codec.mimeType, codec.sdpFmtpLine);
    const powerEfficient = hwByMime.get(codec.mimeType) ?? false;
    const hwAvailable = resolveHardwareAvailability(
      codec.mimeType,
      encodeMimes,
      powerEfficient,
      profilesPopulated
    );
    return {
      mimeType: codec.mimeType,
      sdpFmtpLine: codec.sdpFmtpLine,
      powerEfficient,
      hwAvailable,
      supported: true,
      profileId: id,
      profileLabel: label,
      isHdr,
    };
  });
  if (
    caps.length > 0 &&
    (profileSignalUnavailable ||
      profilesPopulated ||
      caps.every((cap) => cap.hwAvailable === true && cap.powerEfficient))
  ) {
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
