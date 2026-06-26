/**
 * Hardened, fail-closed AV1 low-overhead-bitstream OBU parser (#1895).
 *
 * Runs on UNTRUSTED, SFU/attacker-influenceable bytes on BOTH sides (the sender
 * re-parses its own encoder output; the receiver re-parses post-SFU-rewrite
 * bytes). It is the single highest-risk new surface for media frame crypto v4
 * (spec §9 / §10.3 — CWE-125 out-of-bounds read, CWE-20 input validation).
 *
 * Safety contract (LOAD-BEARING — do NOT relax):
 *   - Every read is bounds-checked against `data.length` before access.
 *   - leb128 is capped at 8 continuation bytes; a single OBU size > 2^31 is
 *     rejected; a size exceeding the remaining bytes is rejected.
 *   - The OBU count is capped (MAX_OBUS) to bound parser work.
 *   - The `forbidden` bit (header bit 7) MUST be 0.
 *   - On ANY malformation the parser returns `null` (fail-closed). It NEVER
 *     throws on malformed input and NEVER reads past the end of `data`.
 *
 * No `any`, no non-null assertions (`!`) — per `[internal]rules/e2ee.md`.
 */

/** AV1 OBU type constants (4-bit `obu_type`). */
export const AV1_OBU_SEQUENCE_HEADER = 1;
export const AV1_OBU_TEMPORAL_DELIMITER = 2;
export const AV1_OBU_FRAME_HEADER = 3;
export const AV1_OBU_TILE_GROUP = 4;
export const AV1_OBU_METADATA = 5;
export const AV1_OBU_FRAME = 6;
export const AV1_OBU_PADDING = 15;

/**
 * Maximum OBUs parsed per temporal unit — bounds parser work, rejects pathological
 * aggregation. Set to 256 to accommodate SVC and multi-tile temporal units, which
 * can legitimately contain many OBUs across spatial/temporal layers and tile groups.
 * A frame exceeding this cap returns null (fail-closed) — the caller drops it.
 */
const MAX_OBUS = 256;
/** Maximum leb128 continuation bytes (AV1 spec leb128 is at most 8 bytes). */
const MAX_LEB128_BYTES = 8;
/** Reject a single OBU claiming a payload larger than this (2^31 − 1). */
const MAX_OBU_PAYLOAD = 0x7fffffff;

export interface ParsedObu {
  /** 4-bit OBU type (header bits 6-3). */
  obuType: number;
  /** Byte offset (in `data`) of the first payload byte. */
  payloadOffset: number;
  /** Byte length of the payload (excludes header, extension, and size fields). */
  payloadLen: number;
}

/** Parsed OBU header fields, or `null` on malformation. */
interface ObuHeader {
  obuType: number;
  hasSizeField: number;
  /** Byte count consumed (1 for header alone, 2 when extension byte is present). */
  bytesConsumed: number;
}

/**
 * Parse a single OBU header byte (and optional extension byte). Returns `null`
 * if the forbidden bit is set or the extension byte is missing when required
 * (spec §9.2 / §9.8). Pure — no side effects, no throws.
 */
function parseObuHeader(data: Uint8Array, offset: number, len: number): ObuHeader | null {
  if (offset >= len) return null;
  const headerByte = data[offset];
  if ((headerByte & 0x80) !== 0) return null; // forbidden bit set (spec §9.8)
  const obuType = (headerByte >>> 3) & 0x0f;
  const extensionFlag = (headerByte >>> 2) & 0x01;
  const hasSizeField = (headerByte >>> 1) & 0x01;
  let bytesConsumed = 1;
  if (extensionFlag === 1) {
    if (offset + 1 >= len) return null; // truncated extension (spec §9.2)
    bytesConsumed = 2; // temporal_id(3) | spatial_id(2) | reserved(3)
  }
  return { obuType, hasSizeField, bytesConsumed };
}

/**
 * Decode an AV1 leb128 size field starting at `offset` inside `data`. Returns
 * `{ value, bytesConsumed }` or `null` on malformation (truncated, continuation
 * bit set for >8 bytes, or decoded value >MAX_OBU_PAYLOAD). Pure — no throws.
 */
function parseLeb128(
  data: Uint8Array,
  offset: number,
  len: number
): { value: number; bytesConsumed: number } | null {
  let value = 0;
  let shift = 0;
  for (let i = 0; i < MAX_LEB128_BYTES; i++) {
    if (offset + i >= len) return null; // truncated leb128 (spec §9.6)
    const b = data[offset + i];
    // Use multiplication (not <<) so the 5th byte can't overflow Int32.
    value += (b & 0x7f) * 2 ** shift;
    shift += 7;
    if ((b & 0x80) === 0) {
      if (value > MAX_OBU_PAYLOAD) return null; // single OBU size > 2^31 (spec §9.6)
      return { value, bytesConsumed: i + 1 };
    }
    if (value > MAX_OBU_PAYLOAD) return null; // early reject oversized
  }
  return null; // continuation bit still set after 8 bytes
}

/**
 * Parse a low-overhead AV1 bitstream into its OBU list. Returns `null` on ANY
 * malformation (fail-closed); callers MUST treat `null` as "drop this frame".
 */
export function parseAv1Obus(data: Uint8Array): ParsedObu[] | null {
  const len = data.length;
  if (len === 0) return null;

  const result: ParsedObu[] = [];
  let offset = 0;

  while (offset < len) {
    if (result.length >= MAX_OBUS) return null; // OBU count cap (spec §9.7)

    // ── OBU header (1 byte + optional extension) ─────────────────────
    const header = parseObuHeader(data, offset, len);
    if (header === null) return null;
    offset += header.bytesConsumed;

    // ── leb128 size field ────────────────────────────────────────────
    let payloadLen: number;
    if (header.hasSizeField === 1) {
      const leb = parseLeb128(data, offset, len);
      if (leb === null) return null;
      offset += leb.bytesConsumed;
      payloadLen = leb.value;
    } else {
      // No size field → payload runs to the end of `data`. The AV1 spec permits
      // this ONLY for the last OBU; if more bytes follow we treat it as the end.
      payloadLen = len - offset;
    }

    const payloadOffset = offset;
    if (payloadOffset + payloadLen > len) return null; // size overruns buffer (spec §9.10)

    result.push({ obuType: header.obuType, payloadOffset, payloadLen });
    offset = payloadOffset + payloadLen;
  }

  return result.length > 0 ? result : null;
}
