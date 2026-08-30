/**
 * Codec-neutral media mini-header plus AV1's per-OBU IV builder.
 *
 * Layout (cleartext, unauthenticated — same trust posture as the v3 trailer;
 * used ONLY for key selection + IV, spec §5.2):
 *   offset size field        notes
 *     0     2   magic        0xDE 0xAD — leads, cheap reject before any crypto
 *     2    12   IV           fresh 96-bit CSPRNG value
 *    14     2   keyId        BE — ratchet keyId within (sender, keyVersion)
 *    16     4   keyVersion   BE — channel CSK version → getChannelKeyByVersion
 *    20     2   reserved     zero — reserved for future flags (spec §5.2 OQ-4)
 *                            → total 22 bytes
 *
 * No explicit length field: the ciphertext length is derived by the receiver as
 * (cleartext leb128 OBU size − 22 mini-header − 16 GCM tag). The IV MUST NEVER be
 * logged ([internal]rules/observability.md Core principle #1).
 */

const MAGIC_0 = 0xde;
const MAGIC_1 = 0xad;

export const MEDIA_FRAME_MINI_HEADER_SIZE = 22;
/** AV1 v4 compatibility alias; the wire layout remains byte-identical in v5. */
export const OBU_MINI_HEADER_SIZE = MEDIA_FRAME_MINI_HEADER_SIZE;

export interface MediaFrameMiniHeader {
  iv: Uint8Array; // 12 bytes
  keyId: number; // 0..65535
  keyVersion: number; // 0..2^32-1
}

/** AV1 v4 compatibility alias. */
export type ObuMiniHeader = MediaFrameMiniHeader;

/**
 * Build a fresh 12-byte GCM IV for one encrypted OBU. The full nonce is random:
 * no deterministic prefix may reset when a MediaEncryption session is recreated
 * under the same CSK-derived frame key.
 */
export function buildObuIv(): Uint8Array {
  const iv = new Uint8Array(12);
  crypto.getRandomValues(iv);
  return iv;
}

/** Encode a 22-byte codec-neutral media mini-header (magic-first). */
export function encodeMediaFrameMiniHeader(h: MediaFrameMiniHeader): Uint8Array {
  const out = new Uint8Array(MEDIA_FRAME_MINI_HEADER_SIZE);
  out[0] = MAGIC_0;
  out[1] = MAGIC_1;
  out.set(h.iv, 2); // [2..14)
  const view = new DataView(out.buffer);
  view.setUint16(14, h.keyId & 0xffff, false); // [14..16) keyId BE
  view.setUint32(16, h.keyVersion >>> 0, false); // [16..20) keyVersion BE
  // bytes [20..22) reserved (zero) — keeps the header a fixed 22 and leaves room
  // for a future version/flags pair without a wire break (spec §5.2 OQ-4).
  return out;
}

/** AV1 v4 compatibility alias. */
export const encodeObuMiniHeader = encodeMediaFrameMiniHeader;

/**
 * Decode a 22-byte mini-header from the START of `buf`. Returns `null` (no
 * throw, no OOB) when the buffer is too short or the magic is absent — the
 * fail-closed reject the decrypt paths rely on for a non-encrypted region.
 */
export function decodeMediaFrameMiniHeader(buf: Uint8Array): MediaFrameMiniHeader | null {
  if (buf.length < MEDIA_FRAME_MINI_HEADER_SIZE) return null;
  if (buf[0] !== MAGIC_0 || buf[1] !== MAGIC_1) return null;
  const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const iv = buf.slice(2, 14);
  const keyId = view.getUint16(14, false);
  const keyVersion = view.getUint32(16, false);
  return { iv, keyId, keyVersion };
}

/** AV1 v4 compatibility alias. */
export const decodeObuMiniHeader = decodeMediaFrameMiniHeader;
