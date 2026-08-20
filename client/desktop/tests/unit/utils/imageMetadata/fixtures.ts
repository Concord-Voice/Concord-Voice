/**
 * Synthesised image fixtures for the metadata-strip tests (#2469).
 *
 * Every fixture is built in-test from bytes — no committed binaries — so the
 * exact metadata payload under test is visible in this file.
 *
 * THE ANTI-VACUOUS RULE
 * ---------------------
 * A fixture builder with a bug produces a buffer that never carried the
 * metadata at all, and the "it was removed" assertion then passes trivially
 * while proving nothing. Every removal test in this directory therefore calls
 * `assertContains` on the PRE-STRIP buffer before calling `assertAbsent` on the
 * result. The pair is the proof; either half alone is theatre.
 */

import { expect } from 'vitest';

// Distinctive byte runs. Each is improbable in structural container bytes, so
// finding one is unambiguous evidence the payload it stands for is present.
export const GPS_MARKER = [0x47, 0x50, 0x53, 0xde, 0xad, 0xbe, 0xef]; // "GPS" + deadbeef
export const ICC_MARKER = [0x49, 0x43, 0x43, 0xc0, 0x1d, 0x1f, 0x1e]; // "ICC" + colour
export const XMP_MARKER = [0x58, 0x4d, 0x50, 0xba, 0xdb, 0xee, 0xf0]; // "XMP" + badbeef0
export const IPTC_MARKER = [0x49, 0x50, 0x54, 0xc0, 0xde, 0xca, 0xfe]; // "IPT" + codecafe
export const PIXEL_MARKER = [0x50, 0x49, 0x58, 0xa5, 0x5a, 0xa5, 0x5a]; // "PIX" + a55a
export const PIXEL_MARKER_B = [0x50, 0x58, 0x42, 0x5a, 0xa5, 0x5a, 0xa5]; // "PXB" + 5aa5

// ---------------------------------------------------------------------------
// byte plumbing
// ---------------------------------------------------------------------------

export type Bytes = number[] | Uint8Array;

export function toU8(value: Bytes): Uint8Array {
  return value instanceof Uint8Array ? value : new Uint8Array(value);
}

export function cat(...parts: Bytes[]): Uint8Array {
  let total = 0;
  for (const p of parts) {
    total += p.length;
  }
  const out = new Uint8Array(total);
  let at = 0;
  for (const p of parts) {
    out.set(toU8(p), at);
    at += p.length;
  }
  return out;
}

export const be16 = (n: number): number[] => [(n >>> 8) & 0xff, n & 0xff];
export const be32 = (n: number): number[] => [
  (n >>> 24) & 0xff,
  (n >>> 16) & 0xff,
  (n >>> 8) & 0xff,
  n & 0xff,
];
export const le16 = (n: number): number[] => [n & 0xff, (n >>> 8) & 0xff];
export const le32 = (n: number): number[] => [
  n & 0xff,
  (n >>> 8) & 0xff,
  (n >>> 16) & 0xff,
  (n >>> 24) & 0xff,
];
export const num16 = (n: number, little: boolean): number[] => (little ? le16(n) : be16(n));
export const num32 = (n: number, little: boolean): number[] => (little ? le32(n) : be32(n));

/** ASCII bytes. A trailing `\0` in the string produces a real NUL byte. */
export const asc = (text: string): number[] => Array.from(text, (c) => c.charCodeAt(0));

export function bytesOf(buf: ArrayBuffer | Uint8Array): Uint8Array {
  return buf instanceof Uint8Array ? buf : new Uint8Array(buf);
}

export function hex(needle: Bytes): string {
  return Array.from(toU8(needle), (b) => b.toString(16).padStart(2, '0')).join(' ');
}

export function indexOfBytes(haystack: ArrayBuffer | Uint8Array, needle: Bytes, from = 0): number {
  const hay = bytesOf(haystack);
  const pin = toU8(needle);
  if (pin.length === 0) {
    return -1;
  }
  outer: for (let i = from; i + pin.length <= hay.length; i++) {
    for (let j = 0; j < pin.length; j++) {
      if (hay[i + j] !== pin[j]) {
        continue outer;
      }
    }
    return i;
  }
  return -1;
}

export function countOccurrences(haystack: ArrayBuffer | Uint8Array, needle: Bytes): number {
  const pin = toU8(needle);
  let count = 0;
  let at = indexOfBytes(haystack, pin);
  while (at >= 0) {
    count++;
    at = indexOfBytes(haystack, pin, at + pin.length);
  }
  return count;
}

/**
 * The pre-strip half of the anti-vacuous pair: proves the fixture really does
 * carry the payload whose removal is about to be asserted.
 */
export function assertContains(buf: ArrayBuffer | Uint8Array, needle: Bytes, what: string): void {
  const at = indexOfBytes(buf, needle);
  expect(
    at,
    `FIXTURE BUG: expected ${what} [${hex(needle)}] to be PRESENT before stripping`
  ).toBeGreaterThanOrEqual(0);
}

/**
 * The post-strip half for content that must SURVIVE. Distinct from
 * `assertContains` only in its message: reusing the fixture-precondition
 * helper on an output makes a real truncation report itself as "FIXTURE BUG"
 * and sends the next reader hunting a fixture that is fine.
 */
export function assertPreserved(buf: ArrayBuffer | Uint8Array, needle: Bytes, what: string): void {
  const at = indexOfBytes(buf, needle);
  expect(
    at,
    `expected ${what} [${hex(needle)}] to SURVIVE stripping, but it is gone`
  ).toBeGreaterThanOrEqual(0);
}

/** The post-strip half for content that must go. Pairs with `assertContains`. */
export function assertAbsent(buf: ArrayBuffer | Uint8Array, needle: Bytes, what: string): void {
  const at = indexOfBytes(buf, needle);
  expect(at, `expected ${what} [${hex(needle)}] to be GONE after stripping`).toBe(-1);
}

export function assertBytesEqual(a: ArrayBuffer | Uint8Array, b: ArrayBuffer | Uint8Array): void {
  expect(Array.from(bytesOf(a))).toEqual(Array.from(bytesOf(b)));
}

// ---------------------------------------------------------------------------
// JPEG
// ---------------------------------------------------------------------------

const JPEG_SOS_PAYLOAD = [0x01, 0x01, 0x00, 0x00, 0x3f, 0x00];
const JPEG_SCAN = cat(PIXEL_MARKER, [0x12, 0x34, 0xff, 0x00, 0x56, 0x78, 0x9a]);

function jpegSegment(marker: number, payload: Bytes): Uint8Array {
  const p = toU8(payload);
  return cat([0xff, marker], be16(p.length + 2), p);
}

/**
 * A big-endian EXIF APP1 payload with IFD0 = { Make, Orientation=6, GPSIFD },
 * and a GPS sub-IFD whose value area holds GPS_MARKER.
 *
 * Layout (offsets are from the start of the TIFF header, as EXIF requires):
 *   0   'MM' 0x002A  ifd0=8
 *   8   IFD0: 3 entries, next=0            → ends at 50
 *   50  Make string, 11 bytes + 1 pad      → 62
 *   62  GPS IFD: 1 entry, next=0           → 80
 *   80  GPS_MARKER (the "coordinates")
 */
export function exifApp1Payload(orientation = 6): Uint8Array {
  const MAKE = asc('CONCORDCAM\0'); // 11
  const makeAt = 50;
  const gpsIfdAt = 62;
  const gpsDataAt = 80;

  const ifd0 = cat(
    be16(3),
    // Make (0x010F) ASCII count 11 → 11 > 4, so the field holds an offset
    be16(0x010f),
    be16(2),
    be32(MAKE.length),
    be32(makeAt),
    // Orientation (0x0112) SHORT count 1 → inline, left-aligned
    be16(0x0112),
    be16(3),
    be32(1),
    be16(orientation),
    be16(0),
    // GPSInfoIFD (0x8825) LONG count 1 → inline offset
    be16(0x8825),
    be16(4),
    be32(1),
    be32(gpsIfdAt),
    be32(0) // no next IFD
  );

  const gpsIfd = cat(
    be16(1),
    be16(0x0004), // GPSLongitude, carried here as UNDEFINED for the fixture
    be16(7),
    be32(GPS_MARKER.length),
    be32(gpsDataAt),
    be32(0)
  );

  const tiff = cat(
    asc('MM'),
    be16(42),
    be32(8),
    ifd0,
    MAKE,
    [0x00], // pad to an even offset
    gpsIfd,
    GPS_MARKER
  );

  // Sanity: the offsets above must match the emitted layout, or the fixture
  // would be describing a file it did not build.
  expect(indexOfBytes(tiff, MAKE)).toBe(makeAt);
  expect(indexOfBytes(tiff, GPS_MARKER)).toBe(gpsDataAt);
  expect(tiff.length).toBe(gpsDataAt + GPS_MARKER.length);

  return cat(asc('Exif\0\0'), tiff);
}

export interface JpegFixture {
  buffer: Uint8Array;
  /** SOS header + scan data + EOI — must survive byte-identically. */
  tail: Uint8Array;
  app2: Uint8Array;
  app0: Uint8Array;
  sof0: Uint8Array;
  comment: number[];
}

export function buildJpeg(options: { orientation?: number } = {}): JpegFixture {
  const app0 = jpegSegment(
    0xe0,
    cat(asc('JFIF\0'), [0x01, 0x02, 0x00], be16(72), be16(72), [0, 0])
  );
  const app1Exif = jpegSegment(0xe1, exifApp1Payload(options.orientation ?? 6));
  const app1Xmp = jpegSegment(
    0xe1,
    cat(asc('http://ns.adobe.com/xap/1.0/\0'), XMP_MARKER, asc('<x:xmpmeta/>'))
  );
  const app2 = jpegSegment(0xe2, cat(asc('ICC_PROFILE\0'), [0x01, 0x01], ICC_MARKER));
  const app13 = jpegSegment(0xed, cat(asc('Photoshop 3.0\0'), IPTC_MARKER, asc('8BIM')));
  const comment = asc('CAM-SERIAL-12345 taken at home');
  const com = jpegSegment(0xfe, comment);
  const sof0 = jpegSegment(0xc0, [0x08, 0x00, 0x02, 0x00, 0x02, 0x01, 0x01, 0x11, 0x00]);

  const tail = cat(
    [0xff, 0xda],
    be16(JPEG_SOS_PAYLOAD.length + 2),
    JPEG_SOS_PAYLOAD,
    JPEG_SCAN,
    [0xff, 0xd9]
  );

  return {
    buffer: cat([0xff, 0xd8], app0, app1Exif, app1Xmp, app2, app13, com, sof0, tail),
    tail,
    app2,
    app0,
    sof0,
    comment,
  };
}

/**
 * A JPEG that terminates at EOI without ever reaching SOS, and that exercises
 * the two marker shapes the main fixture does not: 0xFF fill bytes before a
 * marker, and a standalone (length-less) marker.
 */
export function buildJpegWithFillAndStandalone(): {
  buffer: Uint8Array;
  app0: Uint8Array;
  sof0: Uint8Array;
  comment: number[];
} {
  const app0 = jpegSegment(
    0xe0,
    cat(asc('JFIF\0'), [0x01, 0x02, 0x00], be16(72), be16(72), [0, 0])
  );
  const comment = asc('CAM-SERIAL-12345');
  const com = jpegSegment(0xfe, comment);
  const sof0 = jpegSegment(0xc0, [0x08, 0x00, 0x02, 0x00, 0x02, 0x01, 0x01, 0x11, 0x00]);
  return {
    buffer: cat(
      [0xff, 0xd8],
      app0,
      com,
      [0xff, 0xd0], // RST0 — standalone, no length field
      [0xff, 0xff], // fill bytes before the next marker
      sof0,
      [0xff, 0xd9] // EOI reached without an SOS segment
    ),
    app0,
    sof0,
    comment,
  };
}

/** A JPEG with no metadata segments at all — nothing to strip. */
export function buildBareJpeg(): Uint8Array {
  const sof0 = jpegSegment(0xc0, [0x08, 0x00, 0x02, 0x00, 0x02, 0x01, 0x01, 0x11, 0x00]);
  const tail = cat(
    [0xff, 0xda],
    be16(JPEG_SOS_PAYLOAD.length + 2),
    JPEG_SOS_PAYLOAD,
    JPEG_SCAN,
    [0xff, 0xd9]
  );
  return cat([0xff, 0xd8], sof0, tail);
}

// ---------------------------------------------------------------------------
// PNG / APNG
// ---------------------------------------------------------------------------

const CRC_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) {
      c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    }
    table[n] = c >>> 0;
  }
  return table;
})();

export function crc32(data: Uint8Array): number {
  let c = 0xffffffff;
  for (let i = 0; i < data.length; i++) {
    c = CRC_TABLE[(c ^ data[i]) & 0xff] ^ (c >>> 8);
  }
  return (c ^ 0xffffffff) >>> 0;
}

export const PNG_SIGNATURE = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];

/** A real PNG chunk, CRC included, so the fixtures are structurally valid. */
export function pngChunk(type: string, data: Bytes): Uint8Array {
  const body = cat(asc(type), data);
  return cat(be32(toU8(data).length), body, be32(crc32(body)));
}

export interface PngChunkInfo {
  type: string;
  start: number;
  end: number;
  data: Uint8Array;
}

/** Independent chunk walker for assertions — deliberately not the code under test. */
export function pngChunks(buf: ArrayBuffer | Uint8Array): PngChunkInfo[] {
  const bytes = bytesOf(buf);
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const out: PngChunkInfo[] = [];
  let at = 8;
  while (at + 8 <= bytes.length) {
    const length = view.getUint32(at, false);
    const type = String.fromCharCode(...bytes.subarray(at + 4, at + 8));
    const end = at + 12 + length;
    if (end > bytes.length) {
      break;
    }
    out.push({ type, start: at, end, data: bytes.slice(at + 8, at + 8 + length) });
    at = end;
  }
  return out;
}

const IHDR_DATA = cat(be32(2), be32(2), [0x08, 0x02, 0x00, 0x00, 0x00]);
const IDAT_DATA = cat(PIXEL_MARKER, [0x78, 0x9c, 0x62, 0x60, 0x00, 0x00, 0x00, 0x02, 0x00, 0x01]);

export interface PngFixture {
  buffer: Uint8Array;
  ihdr: Uint8Array;
  idat: Uint8Array;
  iend: Uint8Array;
  iccp: Uint8Array;
  textValue: number[];
}

export function buildPng(): PngFixture {
  const ihdr = pngChunk('IHDR', IHDR_DATA);
  const iccp = pngChunk('iCCP', cat(asc('ConcordProfile\0'), [0x00], ICC_MARKER));
  const exif = pngChunk('eXIf', cat(asc('MM'), be16(42), be32(8), GPS_MARKER));
  const textValue = asc('Software\0ConcordCam CAM-SERIAL-12345');
  const text = pngChunk('tEXt', textValue);
  const itxt = pngChunk('iTXt', cat(asc('XML:com.adobe.xmp\0\0\0\0\0'), XMP_MARKER));
  const ztxt = pngChunk('zTXt', cat(asc('Comment\0'), [0x00], IPTC_MARKER));
  const time = pngChunk('tIME', cat(be16(2026), [8, 20, 1, 2, 3]));
  const idat = pngChunk('IDAT', IDAT_DATA);
  const iend = pngChunk('IEND', []);

  return {
    buffer: cat(PNG_SIGNATURE, ihdr, iccp, exif, text, itxt, ztxt, time, idat, iend),
    ihdr,
    idat,
    iend,
    iccp,
    textValue,
  };
}

export interface ApngFixture {
  buffer: Uint8Array;
  frameCount: number;
  actl: Uint8Array;
}

export function buildApng(): ApngFixture {
  const frameCount = 3;
  const ihdr = pngChunk('IHDR', IHDR_DATA);
  const actl = pngChunk('acTL', cat(be32(frameCount), be32(0)));
  const fctl = (seq: number): Uint8Array =>
    pngChunk(
      'fcTL',
      cat(be32(seq), be32(2), be32(2), be32(0), be32(0), be16(10), be16(100), [0x00, 0x00])
    );
  const fdat = (seq: number): Uint8Array => pngChunk('fdAT', cat(be32(seq), IDAT_DATA));
  const exif = pngChunk('eXIf', cat(asc('MM'), be16(42), be32(8), GPS_MARKER));
  const text = pngChunk('tEXt', asc('Software\0ConcordCam'));

  return {
    buffer: cat(
      PNG_SIGNATURE,
      ihdr,
      actl,
      fctl(0),
      pngChunk('IDAT', IDAT_DATA),
      fctl(1),
      fdat(2),
      fctl(3),
      fdat(4),
      exif,
      text,
      pngChunk('IEND', [])
    ),
    frameCount,
    actl,
  };
}

// ---------------------------------------------------------------------------
// WebP (RIFF)
// ---------------------------------------------------------------------------

export function riffChunk(type: string, payload: Bytes): Uint8Array {
  const p = toU8(payload);
  const pad = p.length % 2 ? [0x00] : [];
  return cat(asc(type), le32(p.length), p, pad);
}

export const VP8X_ICC_FLAG = 0x20;
export const VP8X_EXIF_FLAG = 0x08;
export const VP8X_XMP_FLAG = 0x04;

export interface WebpFixture {
  buffer: Uint8Array;
  vp8xFlagsAt: number;
  vp8: Uint8Array;
  iccp: Uint8Array;
}

export function buildWebpExtended(): WebpFixture {
  // VP8X payload: flags(1) reserved(3) canvasWidth-1(3 LE) canvasHeight-1(3 LE)
  const flags = VP8X_ICC_FLAG | VP8X_EXIF_FLAG | VP8X_XMP_FLAG;
  const vp8x = riffChunk('VP8X', [flags, 0, 0, 0, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00]);
  // Deliberately odd payload so the RIFF pad path is exercised.
  const iccp = riffChunk('ICCP', cat(asc('Concord'), ICC_MARKER, [0x01]));
  const vp8 = riffChunk('VP8 ', cat(PIXEL_MARKER, [0x9d, 0x01, 0x2a, 0x02, 0x00, 0x02, 0x00]));
  const exif = riffChunk('EXIF', cat(asc('MM'), be16(42), be32(8), GPS_MARKER, [0x01]));
  const xmp = riffChunk('XMP ', cat(asc('<x:xmpmeta '), XMP_MARKER));

  const body = cat(vp8x, iccp, vp8, exif, xmp);
  const buffer = cat(asc('RIFF'), le32(4 + body.length), asc('WEBP'), body);
  return { buffer, vp8xFlagsAt: 12 + 8, vp8, iccp };
}

export function buildWebpSimple(): Uint8Array {
  const vp8 = riffChunk('VP8 ', cat(PIXEL_MARKER, [0x9d, 0x01, 0x2a, 0x02, 0x00, 0x02, 0x00]));
  return cat(asc('RIFF'), le32(4 + vp8.length), asc('WEBP'), vp8);
}

// ---------------------------------------------------------------------------
// GIF
// ---------------------------------------------------------------------------

/** Length-prefixed sub-block chain, terminating zero byte included. */
export function subBlocks(data: Bytes): Uint8Array {
  const bytes = toU8(data);
  const parts: Bytes[] = [];
  for (let at = 0; at < bytes.length; at += 255) {
    const slice = bytes.subarray(at, Math.min(at + 255, bytes.length));
    parts.push([slice.length], slice);
  }
  parts.push([0x00]);
  return cat(...parts);
}

export const GIF_NETSCAPE_EXT = cat(
  [0x21, 0xff, 0x0b],
  asc('NETSCAPE2.0'),
  [0x03, 0x01, 0x00, 0x00, 0x00]
);

const GIF_GCE = cat([0x21, 0xf9, 0x04, 0x08], le16(10), [0x00, 0x00]);

function gifImageDescriptor(left: number, lzw: Bytes): Uint8Array {
  return cat(
    [0x2c],
    le16(left),
    le16(0),
    le16(2),
    le16(2),
    [0x00], // no local colour table
    [0x02], // LZW minimum code size
    subBlocks(lzw)
  );
}

export interface GifFixture {
  buffer: Uint8Array;
  frameA: Uint8Array;
  frameB: Uint8Array;
  gce: Uint8Array;
  netscape: Uint8Array;
  commentText: number[];
  /** The bytes before the first block — header, LSD, and global colour table. */
  head: Uint8Array;
}

export function buildGif(options: { trailer?: boolean } = {}): GifFixture {
  const gct = cat([0xff, 0xff, 0xff], [0x00, 0x00, 0x00], [0xff, 0x00, 0x00], [0x00, 0x00, 0xff]);
  // packed: GCT present (0x80), colour resolution 0, not sorted, size N=1 → 4 entries
  const head = cat(asc('GIF89a'), le16(2), le16(2), [0x81, 0x00, 0x00], gct);

  const xmpExt = cat(
    [0x21, 0xff, 0x0b],
    asc('XMP DataXMP'),
    subBlocks(cat(XMP_MARKER, GPS_MARKER, asc('geo:51.5033,-0.1196')))
  );
  const commentText = asc('CAM-SERIAL-12345 shot at ');
  const comment = cat([0x21, 0xfe], subBlocks(cat(commentText, GPS_MARKER)));

  const frameA = gifImageDescriptor(0, cat(PIXEL_MARKER, [0x44, 0x01, 0x00]));
  const frameB = gifImageDescriptor(1, cat(PIXEL_MARKER_B, [0x55, 0x02, 0x00]));

  const parts: Bytes[] = [
    head,
    GIF_NETSCAPE_EXT,
    xmpExt,
    comment,
    GIF_GCE,
    frameA,
    GIF_GCE,
    frameB,
  ];
  if (options.trailer !== false) {
    parts.push([0x3b]);
  }

  return {
    buffer: cat(...parts),
    frameA,
    frameB,
    gce: GIF_GCE,
    netscape: GIF_NETSCAPE_EXT,
    commentText,
    head,
  };
}

/**
 * A GIF exercising the block shapes the main fixture omits: a frame with a
 * LOCAL colour table, a Plain Text extension (dropped), and an extension with
 * an unrecognised label (preserved, because guessing is worse than keeping).
 */
export function buildGifWithRareBlocks(): {
  buffer: Uint8Array;
  head: Uint8Array;
  frame: Uint8Array;
  unknownExt: Uint8Array;
  plainText: number[];
} {
  const head = cat(asc('GIF89a'), le16(2), le16(2), [0x00, 0x00, 0x00]); // no global table
  const plainText = asc('PLAIN-TEXT-CAM-SERIAL');
  const plainTextExt = cat(
    [0x21, 0x01, 0x0c],
    [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0], // 12-byte fixed header
    subBlocks(plainText)
  );
  const unknownExt = cat([0x21, 0x77], subBlocks(cat(PIXEL_MARKER_B, [0x01])));
  const lct = cat([0x11, 0x22, 0x33], [0x44, 0x55, 0x66], [0x77, 0x88, 0x99], [0xaa, 0xbb, 0xcc]);
  const frame = cat(
    [0x2c],
    le16(0),
    le16(0),
    le16(2),
    le16(2),
    [0x81], // local colour table present, size N=1 → 4 entries
    lct,
    [0x02],
    subBlocks(cat(PIXEL_MARKER, [0x44, 0x01, 0x00]))
  );
  return {
    buffer: cat(head, plainTextExt, unknownExt, frame, [0x3b]),
    head,
    frame,
    unknownExt,
    plainText,
  };
}

/**
 * A GIF whose comment extension is a chain of `count` one-byte sub-blocks.
 * Used to trip the ByteReader iteration budget on a crafted chain.
 */
export function buildGifWithSubBlockChain(count: number): Uint8Array {
  const head = cat(asc('GIF89a'), le16(1), le16(1), [0x00, 0x00, 0x00]);
  const chain = new Uint8Array(count * 2 + 1);
  for (let i = 0; i < count; i++) {
    chain[i * 2] = 1;
    chain[i * 2 + 1] = 0x41;
  }
  chain[count * 2] = 0x00;
  return cat(head, [0x21, 0xfe], chain, [0x3b]);
}

// ---------------------------------------------------------------------------
// HEIC (ISO-BMFF)
// ---------------------------------------------------------------------------

export function bmffBox(type: string, payload: Bytes): Uint8Array {
  const p = toU8(payload);
  return cat(be32(p.length + 8), asc(type), p);
}

export function bmffFullBox(type: string, version: number, payload: Bytes): Uint8Array {
  return bmffBox(type, cat([version, 0x00, 0x00, 0x00], payload));
}

export interface HeicFixture {
  buffer: Uint8Array;
  /** File offset of the Exif item's payload. */
  exifStart: number;
  exifLength: number;
  /** File offset of the 4-byte iloc extent_length field for the Exif item. */
  exifLengthFieldAt: number;
  imageStart: number;
  imageLength: number;
  /** Width of that field in bytes — 4 or 8, per the iloc size nibbles. */
  exifLengthFieldSize: number;
}

export interface HeicOptions {
  /** Emit `mdat` with a 64-bit extended size field (box size marker 1). */
  extendedMdat?: boolean;
  ilocVersion?: 0 | 1 | 2;
  offsetSize?: 4 | 8;
  lengthSize?: 4 | 8;
}

export const be64 = (n: number): number[] => [
  ...be32(Math.floor(n / 0x1_0000_0000)),
  ...be32(n >>> 0),
];

export function buildHeic(options: HeicOptions = {}): HeicFixture {
  const version = options.ilocVersion ?? 0;
  const offsetSize = options.offsetSize ?? 4;
  const lengthSize = options.lengthSize ?? 4;
  const sized = (width: number, value: number): number[] =>
    width === 8 ? be64(value) : be32(value);
  const idField = (id: number): number[] => (version < 2 ? be16(id) : be32(id));

  const EXIF_ITEM = 1;
  const IMAGE_ITEM = 2;

  const exifPayload = cat(
    be32(0), // exif_tiff_header_offset
    asc('MM'),
    be16(42),
    be32(8),
    GPS_MARKER,
    asc('CONCORDCAM')
  );
  const imagePayload = cat(PIXEL_MARKER, [0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09]);

  const ftyp = bmffBox('ftyp', cat(asc('heic'), be32(0), asc('mif1'), asc('heic')));
  const hdlr = bmffFullBox('hdlr', 0, cat(be32(0), asc('pict'), new Uint8Array(12), [0x00]));
  const pitm = bmffFullBox('pitm', 0, be16(IMAGE_ITEM));
  const infe = (id: number, type: string): Uint8Array =>
    bmffFullBox('infe', 2, cat(be16(id), be16(0), asc(type), [0x00]));
  const iinf = bmffFullBox(
    'iinf',
    0,
    cat(be16(2), infe(EXIF_ITEM, 'Exif'), infe(IMAGE_ITEM, 'hvc1'))
  );

  const ilocItem = (id: number, offset: number, length: number): Uint8Array =>
    cat(
      idField(id),
      version >= 1 ? be16(0) : [], // construction method
      be16(0), // data reference index
      be16(1), // extent count
      sized(offsetSize, offset),
      sized(lengthSize, length)
    );

  const makeIloc = (exifOff: number, exifLen: number, imgOff: number, imgLen: number): Uint8Array =>
    bmffFullBox(
      'iloc',
      version,
      cat(
        [(offsetSize << 4) | lengthSize],
        [0x00], // base_offset_size = 0, index_size = 0
        version < 2 ? be16(2) : be32(2),
        ilocItem(EXIF_ITEM, exifOff, exifLen),
        ilocItem(IMAGE_ITEM, imgOff, imgLen)
      )
    );

  // iloc's length does not depend on the offsets it carries, so the layout can
  // be computed from a placeholder and then re-emitted with the real offsets.
  const ilocSize = makeIloc(0, 0, 0, 0).length;
  const metaSize = 8 + 4 + hdlr.length + pitm.length + iinf.length + ilocSize;
  const mdatHeaderSize = options.extendedMdat ? 16 : 8;
  const mdatStart = ftyp.length + metaSize;
  const exifStart = mdatStart + mdatHeaderSize;
  const imageStart = exifStart + exifPayload.length;

  const iloc = makeIloc(exifStart, exifPayload.length, imageStart, imagePayload.length);
  const meta = bmffFullBox('meta', 0, cat(hdlr, pitm, iinf, iloc));
  expect(meta.length).toBe(metaSize);

  const mdatPayload = cat(exifPayload, imagePayload);
  const mdat = options.extendedMdat
    ? cat(be32(1), asc('mdat'), be64(mdatPayload.length + 16), mdatPayload)
    : bmffBox('mdat', mdatPayload);
  const buffer = cat(ftyp, meta, mdat);

  // iloc box start → +8 box header, +4 version/flags, +1 sizes, +1 sizes2,
  // + item_count, then the first entry: id, [construction method], data
  // reference index, extent count, extent offset — and then the length field.
  const ilocFileStart = ftyp.length + 8 + 4 + hdlr.length + pitm.length + iinf.length;
  const exifLengthFieldAt =
    ilocFileStart +
    8 +
    4 +
    1 +
    1 +
    (version < 2 ? 2 : 4) +
    (version < 2 ? 2 : 4) +
    (version >= 1 ? 2 : 0) +
    2 +
    2 +
    offsetSize;

  // Sanity: the computed offsets must describe the buffer that was actually built.
  expect(indexOfBytes(buffer, GPS_MARKER)).toBe(exifStart + 12);
  expect(indexOfBytes(buffer, PIXEL_MARKER)).toBe(imageStart);
  const dv = new DataView(buffer.buffer, buffer.byteOffset, buffer.byteLength);
  expect(dv.getUint32(exifLengthFieldAt + lengthSize - 4, false)).toBe(exifPayload.length);
  if (lengthSize === 8) {
    expect(dv.getUint32(exifLengthFieldAt, false)).toBe(0);
  }

  return {
    buffer,
    exifStart,
    exifLength: exifPayload.length,
    exifLengthFieldAt,
    exifLengthFieldSize: lengthSize,
    imageStart,
    imageLength: imagePayload.length,
  };
}

/** A HEIC with no metadata items — only a pixel item. */
export function buildHeicWithoutExif(): Uint8Array {
  const ftyp = bmffBox('ftyp', cat(asc('heic'), be32(0), asc('mif1')));
  const hdlr = bmffFullBox('hdlr', 0, cat(be32(0), asc('pict'), new Uint8Array(12), [0x00]));
  const iinf = bmffFullBox(
    'iinf',
    0,
    cat(be16(1), bmffFullBox('infe', 2, cat(be16(1), be16(0), asc('hvc1'), [0x00])))
  );
  const iloc = bmffFullBox(
    'iloc',
    0,
    cat([0x44], [0x00], be16(1), be16(1), be16(0), be16(1), be32(0), be32(0))
  );
  const meta = bmffFullBox('meta', 0, cat(hdlr, iinf, iloc));
  const mdat = bmffBox('mdat', PIXEL_MARKER);
  return cat(ftyp, meta, mdat);
}

// ---------------------------------------------------------------------------
// TIFF
// ---------------------------------------------------------------------------

const TIFF_TYPE_SIZES: Record<number, number> = { 1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1 };

export interface TiffEntryInfo {
  tag: number;
  type: number;
  count: number;
  /** The resolved value bytes, from the inline field or the offset it holds. */
  value: Uint8Array;
}

/** Independent IFD0 reader for assertions — deliberately not the code under test. */
export function readTiffIfd0(buf: ArrayBuffer | Uint8Array): {
  little: boolean;
  entries: TiffEntryInfo[];
} {
  const bytes = bytesOf(buf);
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const little = bytes[0] === 0x49;
  expect(dv.getUint16(2, little)).toBe(42);
  const ifdAt = dv.getUint32(4, little);
  const count = dv.getUint16(ifdAt, little);
  const entries: TiffEntryInfo[] = [];
  for (let i = 0; i < count; i++) {
    const at = ifdAt + 2 + i * 12;
    const tag = dv.getUint16(at, little);
    const type = dv.getUint16(at + 2, little);
    const n = dv.getUint32(at + 4, little);
    const size = (TIFF_TYPE_SIZES[type] ?? 0) * n;
    const value =
      size <= 4
        ? bytes.slice(at + 8, at + 8 + size)
        : (() => {
            const off = dv.getUint32(at + 8, little);
            return bytes.slice(off, off + size);
          })();
    entries.push({ tag, type, count: n, value });
  }
  return { little, entries };
}

export function tiffNumbers(entry: TiffEntryInfo, little: boolean): number[] {
  const dv = new DataView(entry.value.buffer, entry.value.byteOffset, entry.value.byteLength);
  const out: number[] = [];
  for (let i = 0; i < entry.count; i++) {
    out.push(entry.type === 3 ? dv.getUint16(i * 2, little) : dv.getUint32(i * 4, little));
  }
  return out;
}

export interface TiffFixture {
  buffer: Uint8Array;
  little: boolean;
  strips: Uint8Array[];
  make: number[];
  model: number[];
  software: number[];
  datetime: number[];
  exifMarker: number[];
}

const TIFF_EXIF_MARKER = [0x45, 0x58, 0x46, 0xc0, 0xff, 0xee, 0x11]; // "EXF" + coffee11

export function buildTiff(little: boolean): TiffFixture {
  const n16 = (v: number): number[] => num16(v, little);
  const n32 = (v: number): number[] => num32(v, little);

  const make = asc('CONCORDCAM\0'); // 11
  const model = asc('CVX-1000\0'); // 9
  const software = asc('ConcordVoice/1\0'); // 15
  const datetime = asc('2026:08:20 01:02:03\0'); // 20
  const strip0 = cat(PIXEL_MARKER, [0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18]); // 16
  const strip1 = cat(PIXEL_MARKER_B, [0x20, 0x21, 0x22, 0x23, 0x24]); // 12

  // A GPS sub-IFD: one ASCII entry, then the "coordinate" payload it points near.
  const gpsBlob = cat(
    n16(1),
    n16(0x0001),
    n16(2),
    n32(2),
    asc('N\0'),
    [0x00, 0x00],
    n32(0),
    GPS_MARKER
  );
  const exifBlob = cat(
    n16(1),
    n16(0x9000), // ExifVersion
    n16(7),
    n32(4),
    asc('0231'),
    n32(0),
    TIFF_EXIF_MARKER
  );

  const ENTRY_COUNT = 16;
  const HEADER = 8;
  const ifdSize = 2 + ENTRY_COUNT * 12 + 4;

  // Allocate the external value area, word-aligned, in emission order.
  const body: Uint8Array[] = [];
  let cursor = HEADER + ifdSize;
  const alloc = (data: Uint8Array): number => {
    const at = cursor;
    body.push(data);
    cursor += data.length;
    if (data.length % 2) {
      body.push(new Uint8Array(1));
      cursor += 1;
    }
    return at;
  };

  const makeAt = alloc(toU8(make));
  const modelAt = alloc(toU8(model));
  const stripOffsetsValue = new Uint8Array(8);
  const stripOffsetsAt = alloc(stripOffsetsValue);
  const stripCountsAt = alloc(cat(n32(strip0.length), n32(strip1.length)));
  const softwareAt = alloc(toU8(software));
  const datetimeAt = alloc(toU8(datetime));
  const exifIfdAt = alloc(exifBlob);
  const gpsIfdAt = alloc(gpsBlob);
  const strip0At = alloc(strip0);
  const strip1At = alloc(strip1);

  // Now that the pixel strips have addresses, fill the StripOffsets value.
  stripOffsetsValue.set(cat(n32(strip0At), n32(strip1At)));

  const inline = (value: number[]): number[] => {
    const padded = [...value];
    while (padded.length < 4) {
      padded.push(0x00);
    }
    return padded;
  };

  const entry = (tag: number, type: number, count: number, field: number[]): number[] => [
    ...n16(tag),
    ...n16(type),
    ...n32(count),
    ...inline(field),
  ];

  const ifd = cat(
    n16(ENTRY_COUNT),
    entry(256, 3, 1, n16(2)), // ImageWidth
    entry(257, 3, 1, n16(2)), // ImageLength
    entry(258, 3, 1, n16(8)), // BitsPerSample
    entry(259, 3, 1, n16(1)), // Compression = none
    entry(262, 3, 1, n16(1)), // PhotometricInterpretation
    entry(271, 2, make.length, n32(makeAt)), // Make
    entry(272, 2, model.length, n32(modelAt)), // Model
    entry(273, 4, 2, n32(stripOffsetsAt)), // StripOffsets
    entry(274, 3, 1, n16(6)), // Orientation = 6
    entry(277, 3, 1, n16(1)), // SamplesPerPixel
    entry(278, 3, 1, n16(1)), // RowsPerStrip
    entry(279, 4, 2, n32(stripCountsAt)), // StripByteCounts
    entry(305, 2, software.length, n32(softwareAt)), // Software
    entry(306, 2, datetime.length, n32(datetimeAt)), // DateTime
    entry(34665, 4, 1, n32(exifIfdAt)), // ExifIFD
    entry(34853, 4, 1, n32(gpsIfdAt)), // GPSInfoIFD
    n32(0) // no next IFD
  );
  expect(ifd.length).toBe(ifdSize);

  const header = cat(little ? asc('II') : asc('MM'), n16(42), n32(HEADER));
  const buffer = cat(header, ifd, ...body);

  // Sanity: the allocator's offsets must match the emitted buffer.
  expect(indexOfBytes(buffer, strip0)).toBe(strip0At);
  expect(indexOfBytes(buffer, strip1)).toBe(strip1At);
  expect(indexOfBytes(buffer, GPS_MARKER)).toBe(gpsIfdAt + gpsBlob.length - GPS_MARKER.length);

  return {
    buffer,
    little,
    strips: [strip0, strip1],
    make,
    model,
    software,
    datetime,
    exifMarker: TIFF_EXIF_MARKER,
  };
}

// ---------------------------------------------------------------------------
// BMP
// ---------------------------------------------------------------------------

export function buildBmp(): Uint8Array {
  const pixels = cat(PIXEL_MARKER, [0x00, 0x00, 0x00, 0x00, 0x00]);
  const headerSize = 14 + 40;
  return cat(
    asc('BM'),
    le32(headerSize + pixels.length),
    le16(0),
    le16(0),
    le32(headerSize),
    le32(40),
    le32(2),
    le32(2),
    le16(1),
    le16(24),
    le32(0),
    le32(pixels.length),
    le32(2835),
    le32(2835),
    le32(0),
    le32(0),
    pixels
  );
}
