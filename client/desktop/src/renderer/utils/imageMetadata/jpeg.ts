/**
 * JPEG metadata strip (#2469).
 *
 * A JPEG is SOI followed by a chain of marker segments, then SOS after which the
 * entropy-coded scan data runs to EOI. Metadata lives in APPn segments:
 *
 *   APP1  EXIF (GPS, camera, timestamps) and XMP
 *   APP2  ICC colour profile — PRESERVED, see below
 *   APP13 Photoshop/IPTC
 *   COM   free-text comment
 *
 * ICC is deliberately kept. It is a colour profile, not identifying data, and
 * dropping it visibly shifts colour on wide-gamut photos: a quality regression
 * that buys no privacy. The strip targets identifying metadata, not every
 * ancillary segment.
 *
 * Orientation is the subtle one. EXIF tag 274 lives in the same APP1 block as
 * GPS, so removing the block wholesale renders every portrait phone photo
 * sideways — which reads as a bug, not a privacy feature. Rather than decoding
 * and rotating pixels (which is what the canvas approach would force, and what
 * this module exists to avoid), a minimal APP1 carrying ONLY Orientation is
 * rebuilt: no GPS IFD, no Exif sub-IFD, no Make/Model/DateTime/Software.
 */

import { ByteReader, ImageParseError, concat } from './reader';

const MARKER = 0xff;
const SOI = 0xd8;
const EOI = 0xd9;
const SOS = 0xda;
const APP1 = 0xe1;
const APP2 = 0xe2;
const APP13 = 0xed;
const COM = 0xfe;

/** Markers with no length field, so no payload to skip. */
function isStandalone(marker: number): boolean {
  // RSTn (D0-D7), SOI, EOI, and TEM (01).
  return (marker >= 0xd0 && marker <= 0xd9) || marker === 0x01;
}

function isStripped(marker: number): boolean {
  // APP2 carries the ICC colour profile and is explicitly NOT stripped. Naming
  // it here rather than only in prose means the decision is checked by the
  // compiler: if someone later adds APP2 to the stripped set, this guard
  // contradicts them at the same line.
  if (marker === APP2) {
    return false;
  }
  return marker === APP1 || marker === APP13 || marker === COM;
}

const EXIF_ID = [0x45, 0x78, 0x69, 0x66, 0x00, 0x00]; // "Exif\0\0"
const TAG_ORIENTATION = 0x0112;

/**
 * Reads the Orientation tag out of an EXIF APP1 payload, or null when absent,
 * unreadable, or already the default. A malformed EXIF block is not fatal to the
 * strip — the block is being discarded either way — so this returns null rather
 * than throwing, and the image simply loses an orientation hint it may not have
 * had.
 */
interface TiffCursor {
  reader: ByteReader;
  little: boolean;
}

/** Opens the TIFF header inside an EXIF APP1 payload, or null if it is not one. */
function openExifTiff(payload: Uint8Array): TiffCursor | null {
  for (let i = 0; i < EXIF_ID.length; i++) {
    if (payload[i] !== EXIF_ID[i]) {
      return null; // XMP or another APP1 flavour, not EXIF.
    }
  }
  const tiff = payload.subarray(EXIF_ID.length);
  const reader = new ByteReader(tiff, 4096);
  const endian = reader.ascii(2);
  const little = endian === 'II';
  if (!little && endian !== 'MM') {
    return null;
  }
  const magic = little ? reader.u16le() : reader.u16be();
  if (magic !== 42) {
    return null;
  }
  const ifd0 = little ? reader.u32le() : reader.u32be();
  if (ifd0 < 8 || ifd0 >= tiff.byteLength) {
    return null;
  }
  reader.seek(ifd0);
  return { reader, little };
}

/** Scans IFD0 for the Orientation tag. Returns null when absent or default. */
function scanForOrientation({ reader, little }: TiffCursor): number | null {
  const count = little ? reader.u16le() : reader.u16be();
  for (let i = 0; i < count; i++) {
    reader.step('IFD entry');
    const tagId = little ? reader.u16le() : reader.u16be();
    reader.skip(6); // type (2) + count (4)
    const value = little ? reader.u16le() : reader.u16be();
    reader.skip(2); // remainder of the 4-byte value field
    if (tagId === TAG_ORIENTATION) {
      return value >= 1 && value <= 8 && value !== 1 ? value : null;
    }
  }
  return null;
}

/**
 * Reads the Orientation tag out of an EXIF APP1 payload, or null when absent,
 * unreadable, or already the default. A malformed EXIF block is not fatal to the
 * strip — the block is being discarded either way — so this returns null rather
 * than throwing, and the image simply loses an orientation hint it may not have
 * had.
 */
function readOrientation(payload: Uint8Array): number | null {
  try {
    const cursor = openExifTiff(payload);
    return cursor ? scanForOrientation(cursor) : null;
  } catch {
    return null;
  }
}

/**
 * Builds a minimal big-endian EXIF APP1 containing exactly one IFD0 entry:
 * Orientation.
 */
function buildOrientationApp1(orientation: number): Uint8Array {
  // Exif\0\0 | MM | 42 | IFD0 offset 8 | count 1 | entry(12) | next IFD 0
  const tiff = new Uint8Array(2 + 2 + 4 + 2 + 12 + 4);
  const dv = new DataView(tiff.buffer);
  tiff[0] = 0x4d; // 'M'
  tiff[1] = 0x4d; // 'M'
  dv.setUint16(2, 42, false);
  dv.setUint32(4, 8, false); // IFD0 begins immediately after the header
  dv.setUint16(8, 1, false); // one entry
  dv.setUint16(10, TAG_ORIENTATION, false);
  dv.setUint16(12, 3, false); // type SHORT
  dv.setUint32(14, 1, false); // count
  dv.setUint16(18, orientation, false); // value, left-aligned in the 4-byte field
  dv.setUint16(20, 0, false);
  dv.setUint32(22, 0, false); // no next IFD

  const payload = new Uint8Array(EXIF_ID.length + tiff.byteLength);
  payload.set(EXIF_ID, 0);
  payload.set(tiff, EXIF_ID.length);

  const segment = new Uint8Array(4 + payload.byteLength);
  segment[0] = MARKER;
  segment[1] = APP1;
  new DataView(segment.buffer).setUint16(2, payload.byteLength + 2, false);
  segment.set(payload, 4);
  return segment;
}

/**
 * Reads the next marker byte, tolerating the 0xFF fill padding a marker may be
 * preceded by. Throws when the stream is not positioned at a marker at all.
 */
function readMarker(r: ByteReader): number {
  const b = r.u8();
  if (b !== MARKER) {
    throw new ImageParseError(`expected marker at ${r.position - 1}, found 0x${b.toString(16)}`);
  }
  let marker = r.u8();
  while (marker === MARKER) {
    r.step('JPEG fill byte'); // padding is legal but must not be unbounded
    marker = r.u8();
  }
  return marker;
}

/**
 * Advances past entropy-coded scan data and returns the offset of the next real
 * marker's 0xFF.
 *
 * This CANNOT be a raw scan for FF D9. A progressive JPEG has several scans with
 * DHT/DQT/SOS headers between them, and those payloads are NOT byte-stuffed — a
 * quantization table containing FF D9 would end the walk early and truncate the
 * photo. Inside the entropy stream the rules are different and well-defined:
 *
 *   FF 00        a stuffed literal 0xFF — part of the data
 *   FF FF        fill padding before a marker
 *   FF D0..D7    a restart marker — part of the scan
 *   FF <other>   the next real marker; the scan ends here
 *
 * So the walk must interpret the stream rather than search it.
 */
function skipEntropyData(bytes: Uint8Array, from: number): number {
  let i = from;
  while (i + 1 < bytes.byteLength) {
    if (bytes[i] !== MARKER) {
      i++;
      continue;
    }
    const next = bytes[i + 1];
    if (next === 0x00 || (next >= 0xd0 && next <= 0xd7)) {
      i += 2; // stuffed byte or restart marker — still scan data
      continue;
    }
    if (next === MARKER) {
      i++; // fill padding
      continue;
    }
    return i; // a real marker
  }
  throw new ImageParseError('truncated JPEG: scan data has no terminating marker');
}

/** Mutable state threaded through the segment walk. */
interface StripState {
  out: Uint8Array[];
  stripped: boolean;
  orientation: number | null;
}

/**
 * Handles one length-bearing segment. Returns true when the walk is finished
 * (SOS reached, scan data emitted).
 */
function handleSegment(
  r: ByteReader,
  bytes: Uint8Array,
  marker: number,
  state: StripState
): boolean {
  const lengthAt = r.position;
  const length = r.u16be();
  if (length < 2) {
    throw new ImageParseError(`invalid segment length ${length} at ${lengthAt}`);
  }
  const payload = r.bytesOf(length - 2);

  if (marker === SOS) {
    // Copy the SOS header and its entropy data, then RESUME the segment walk.
    //
    // Stopping here was wrong for progressive JPEGs, which carry several scans:
    // it truncated at the first false FF D9 in an unstuffed table payload, and
    // it copied verbatim any APPn metadata appearing after the first scan. The
    // walk now continues, so later scans are preserved and later metadata is
    // stripped like any other segment.
    state.out.push(bytes.subarray(lengthAt - 2, r.position));
    const nextMarker = skipEntropyData(bytes, r.position);
    state.out.push(bytes.subarray(r.position, nextMarker));
    r.seek(nextMarker);
    return false;
  }

  if (isStripped(marker)) {
    state.stripped = true;
    if (marker === APP1 && state.orientation === null) {
      state.orientation = readOrientation(payload);
    }
    return false; // drop the segment
  }

  // Preserved verbatim, APP2/ICC included.
  state.out.push(bytes.subarray(lengthAt - 2, r.position));
  return false;
}

export function stripJpeg(bytes: Uint8Array): { data: ArrayBuffer; stripped: boolean } {
  const r = new ByteReader(bytes);
  if (r.u8() !== MARKER || r.u8() !== SOI) {
    throw new ImageParseError('not a JPEG: missing SOI');
  }

  const state: StripState = { out: [bytes.subarray(0, 2)], stripped: false, orientation: null };

  for (;;) {
    r.step('JPEG segment');
    if (r.remaining() < 2) {
      throw new ImageParseError('truncated JPEG: no EOI');
    }

    const marker = readMarker(r);

    if (marker === EOI) {
      state.out.push(new Uint8Array([MARKER, EOI]));
      // Everything past EOI is dropped by not copying it. That is where a second
      // EXIF block, an appended thumbnail, or a polyglot payload lives: a decoder
      // ignores it, so it survives every visual check while carrying GPS.
      if (r.position < bytes.byteLength) {
        state.stripped = true;
      }
      break;
    }

    if (isStandalone(marker)) {
      state.out.push(new Uint8Array([MARKER, marker]));
      continue;
    }

    if (handleSegment(r, bytes, marker, state)) {
      break;
    }
  }

  if (state.orientation !== null) {
    // Insert directly after SOI so it precedes any other segment, as decoders expect.
    state.out.splice(1, 0, buildOrientationApp1(state.orientation));
  }

  return { data: concat(state.out), stripped: state.stripped };
}
