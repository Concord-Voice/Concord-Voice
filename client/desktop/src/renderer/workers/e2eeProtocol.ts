/**
 * E2EE Worker ↔ Main Thread communication protocol.
 * Shared types imported by both the Worker and voiceService.
 */

// ─── Main Thread → Worker ────────────────────────────────────────────

export type E2EEWorkerMessage =
  // #1878: init carries the authoritative CSK keyVersion so the worker stamps
  // it into every outgoing v3 frame trailer (binds encrypt version at init /
  // reconnect / post-rotation — never a stale 0 when the channel is higher).
  | { type: 'init'; encryptKey: CryptoKey; currentKeyId: number; keyVersion: number }
  // #1878: addDecryptKey carries the CSK keyVersion so the worker keys its
  // decrypt map by senderId:keyVersion:keyId (matches the v3 frame trailer).
  | {
      type: 'addDecryptKey';
      senderUserId: string;
      keyVersion: number;
      keyId: number;
      key: CryptoKey;
    }
  | { type: 'rotateKeys' }
  | { type: 'catchUpToEpoch'; targetEpoch: number }
  | { type: 'destroy' };

// ─── Worker → Main Thread ────────────────────────────────────────────

export type E2EEMainMessage =
  | { type: 'rotationComplete'; newKeyId: number }
  | { type: 'requestKeyframe'; senderUserId: string }
  | {
      type: 'requestRecovery';
      senderUserId: string;
      dropCount: number;
    }
  // #1878: typed decrypt-miss → on-demand key request for an exact
  // (senderUserId, keyVersion, keyId). Distinct from requestRecovery, which
  // handles the OperationError/persistent-failure self-heal case.
  | {
      type: 'requestFrameKey';
      senderUserId: string;
      keyVersion: number;
      keyId: number;
    }
  | {
      type: 'log';
      level: 'debug' | 'info' | 'warn' | 'error';
      message: string;
      data?: Record<string, unknown>;
    };

// ─── RTCRtpScriptTransform options ───────────────────────────────────

/** Codec families the per-codec crypto dispatch recognizes (spec §6). */
export type CodecFamily = 'opus' | 'vp8' | 'vp9' | 'av1' | 'h264';

/**
 * Map an RTP mimeType (e.g. 'video/AV1', 'audio/opus') to a CodecFamily.
 * Case-insensitive. An unknown/missing mimeType returns undefined so the
 * dispatch falls through to whole-frame (the byte-transparent default) — an
 * unknown codec is NEVER assumed to be AV1.
 */
export function codecFamilyFromMimeType(mimeType: string | undefined): CodecFamily | undefined {
  switch ((mimeType ?? '').toLowerCase()) {
    case 'video/av1':
      return 'av1';
    case 'video/vp9':
      return 'vp9';
    case 'video/vp8':
      return 'vp8';
    case 'audio/opus':
      return 'opus';
    case 'video/h264':
      return 'h264';
    default:
      return undefined;
  }
}

/** The crypto scheme a codec family uses (single-sourced for both decrypt paths). */
export function selectCryptoScheme(codec: CodecFamily | undefined): 'per-obu' | 'whole-frame' {
  return codec === 'av1' ? 'per-obu' : 'whole-frame';
}

/** Minimal shape of the mediasoup RTP parameters we read the codec from. */
interface RtpParametersLike {
  codecs?: ReadonlyArray<{ mimeType?: string }>;
}

/**
 * Non-media codec subtype suffixes that must be skipped when resolving the
 * media codec from an RTP parameter set. mediasoup may list RTX, RED, ULPFEC,
 * FlexFEC, CN (comfort-noise), or telephone-event entries ahead of the actual
 * media codec; blindly reading codecs[0] would resolve the wrong family and
 * cause a permanent encrypt/decrypt codec mismatch (black screen / silence).
 * Case-insensitive match against the slash-separated subtype.
 */
const NON_MEDIA_SUFFIXES = ['/rtx', '/red', '/ulpfec', '/flexfec', '/cn', '/telephone-event'];

function isNonMediaCodec(mimeType: string | undefined): boolean {
  if (!mimeType) return false;
  const lower = mimeType.toLowerCase();
  return NON_MEDIA_SUFFIXES.some((s) => lower.endsWith(s));
}

/**
 * Resolve the CodecFamily from a producer/consumer's rtpParameters, skipping
 * non-media entries (RTX, RED, ULPFEC, FlexFEC, CN, telephone-event) that may
 * appear before the actual media codec in the codecs list. Returns the
 * CodecFamily of the FIRST media codec found, or undefined if none. This
 * ensures encrypt (producer.rtpParameters) and decrypt (consumer.rtpParameters)
 * always resolve the SAME media codec even if codecs[0] is an RTX/FEC entry.
 */
export function codecFamilyFromRtpParameters(
  params: RtpParametersLike | undefined
): CodecFamily | undefined {
  for (const codec of params?.codecs ?? []) {
    if (!isNonMediaCodec(codec.mimeType)) {
      return codecFamilyFromMimeType(codec.mimeType);
    }
  }
  return undefined;
}

export interface E2EETransformOptions {
  role: 'encrypt' | 'decrypt';
  senderUserId?: string;
  // #1895: codec family of the stream this transform processes — encrypt uses
  // the LOCAL send codec, decrypt uses the SENDER's codec. Drives per-codec
  // crypto dispatch (AV1 per-OBU vs whole-frame). Undefined → whole-frame.
  codecFamily?: CodecFamily;
}
