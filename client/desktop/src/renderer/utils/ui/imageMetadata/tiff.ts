/**
 * TIFF metadata strip (#2469).
 *
 * TIFF is the one format here that needs a genuine rewrite rather than a walk
 * with deletions, because the container IS the metadata format: a TIFF is a
 * header pointing at a chain of Image File Directories, and EXIF is literally an
 * IFD embedded in one. There is no "metadata section" to excise — pixel data is
 * addressed by offsets held in the same directory as GPS.
 *
 * So IFD0 is rebuilt from an allowlist of structural tags, the strip or tile
 * data is copied verbatim, and the offsets pointing at it are recomputed for the
 * new layout. Every subsequent IFD is dropped: additional pages are rare in a
 * chat attachment, and each would need the same treatment.
 *
 * Orientation (274) is in the allowlist for the same reason JPEG rebuilds a
 * minimal APP1 — dropping it renders portrait photos sideways.
 */

import { ByteReader, ImageParseError, concat } from './reader';

/** Tags that describe how to decode the image. Everything else is dropped. */
const STRUCTURAL_TAGS = new Set([
  256, // ImageWidth
  257, // ImageLength
  258, // BitsPerSample
  259, // Compression
  262, // PhotometricInterpretation
  273, // StripOffsets            (rewritten)
  274, // Orientation
  277, // SamplesPerPixel
  278, // RowsPerStrip
  279, // StripByteCounts
  282, // XResolution
  283, // YResolution
  284, // PlanarConfiguration
  296, // ResolutionUnit
  317, // Predictor
  320, // ColorMap
  324, // TileOffsets             (rewritten)
  325, // TileByteCounts
  338, // ExtraSamples
  339, // SampleFormat
]);

const STRIP_OFFSETS = 273;
const TILE_OFFSETS = 324;
const STRIP_BYTE_COUNTS = 279;
const TILE_BYTE_COUNTS = 325;

const TYPE_SIZES: Record<number, number> = {
  1: 1, // BYTE
  2: 1, // ASCII
  3: 2, // SHORT
  4: 4, // LONG
  5: 8, // RATIONAL
  6: 1, // SBYTE
  7: 1, // UNDEFINED
  8: 2, // SSHORT
  9: 4, // SLONG
  10: 8, // SRATIONAL
  11: 4, // FLOAT
  12: 8, // DOUBLE
};

interface Entry {
  tag: number;
  type: number;
  count: number;
  /** Raw value bytes, already resolved from the inline field or its offset. */
  value: Uint8Array;
}

function entryByteLength(type: number, count: number): number {
  const size = TYPE_SIZES[type];
  if (!size) {
    throw new ImageParseError(`unknown TIFF field type ${type}`);
  }
  return size * count;
}

interface TiffReader {
  r: ByteReader;
  little: boolean;
  u16: () => number;
  u32: () => number;
}

/** Validates the TIFF header and positions the reader at IFD0. */
function openTiff(bytes: Uint8Array): TiffReader {
  const r = new ByteReader(bytes);
  const endian = r.ascii(2);
  const little = endian === 'II';
  if (!little && endian !== 'MM') {
    throw new ImageParseError('not a TIFF: bad byte-order mark');
  }
  const u16 = () => (little ? r.u16le() : r.u16be());
  const u32 = () => (little ? r.u32le() : r.u32be());

  if (u16() !== 42) {
    throw new ImageParseError('not a TIFF: bad magic');
  }
  const ifd0At = u32();
  if (ifd0At < 8 || ifd0At >= bytes.byteLength) {
    throw new ImageParseError(`IFD0 offset ${ifd0At} out of range`);
  }
  r.seek(ifd0At);
  return { r, little, u16, u32 };
}

/**
 * Reads IFD0, keeping only structural tags.
 *
 * The allowlist check runs BEFORE the value is interpreted. Every entry is a
 * fixed 12 bytes, so a dropped tag needs no size computation — and computing one
 * first meant a private or vendor field type (legal in TIFF, common in raw-ish
 * files) threw and rejected the whole upload over a tag being discarded anyway.
 */
function readIfd0(t: TiffReader, bytes: Uint8Array): { kept: Entry[]; droppedAny: boolean } {
  const { r, u16, u32 } = t;
  const count = u16();
  const kept: Entry[] = [];
  let droppedAny = false;

  for (let i = 0; i < count; i++) {
    r.step('IFD entry');
    const tag = u16();
    const type = u16();
    const n = u32();

    if (!STRUCTURAL_TAGS.has(tag)) {
      // GPSIFD (34853), ExifIFD (34665), Make, Model, Software, DateTime,
      // Artist, Copyright, XMP, IPTC, Photoshop — all land here.
      droppedAny = true;
      r.skip(4); // the value/offset field; entry width is fixed
      continue;
    }

    const size = entryByteLength(type, n);
    let value: Uint8Array;
    if (size <= 4) {
      value = r.bytesOf(4).subarray(0, size);
    } else {
      const at = u32();
      if (at + size > bytes.byteLength) {
        throw new ImageParseError(`tag ${tag} value at ${at}+${size} overruns file`);
      }
      value = bytes.slice(at, at + size);
    }

    kept.push({ tag, type, count: n, value });
  }

  return { kept, droppedAny };
}

/** Copies the strip or tile data this IFD points at. */
function readPixelBlocks(kept: Entry[], bytes: Uint8Array, little: boolean): Uint8Array[] {
  const offsetsEntry = kept.find((e) => e.tag === STRIP_OFFSETS || e.tag === TILE_OFFSETS);
  const countsEntry = kept.find((e) => e.tag === STRIP_BYTE_COUNTS || e.tag === TILE_BYTE_COUNTS);
  if (!offsetsEntry || !countsEntry) {
    throw new ImageParseError('TIFF has no strip or tile offsets; cannot relocate pixel data');
  }

  const dataOffsets = readNumbers(offsetsEntry, little);
  const dataCounts = readNumbers(countsEntry, little);
  if (dataOffsets.length !== dataCounts.length) {
    throw new ImageParseError('TIFF offset and byte-count arrays differ in length');
  }

  const blocks: Uint8Array[] = [];
  for (let i = 0; i < dataOffsets.length; i++) {
    const at = dataOffsets[i];
    const len = dataCounts[i];
    if (at + len > bytes.byteLength) {
      throw new ImageParseError(`pixel block ${i} at ${at}+${len} overruns file`);
    }
    blocks.push(bytes.slice(at, at + len));
  }
  return blocks;
}

function readNumbers(e: Entry, little: boolean): number[] {
  const dv = new DataView(e.value.buffer, e.value.byteOffset, e.value.byteLength);
  const out: number[] = [];
  for (let i = 0; i < e.count; i++) {
    out.push(e.type === 3 ? dv.getUint16(i * 2, little) : dv.getUint32(i * 4, little));
  }
  return out;
}

/** Rewrites the offsets entry in place for the new file layout. */
function rewriteOffsets(entry: Entry, newOffsets: number[], little: boolean): void {
  const dv = new DataView(entry.value.buffer, entry.value.byteOffset);
  for (let i = 0; i < newOffsets.length; i++) {
    if (entry.type === 3) {
      if (newOffsets[i] > 0xffff) {
        throw new ImageParseError('relocated offset exceeds SHORT range');
      }
      dv.setUint16(i * 2, newOffsets[i], little);
    } else {
      dv.setUint32(i * 4, newOffsets[i], little);
    }
  }
}

/** Assembles header | IFD0 | external values | pixel data. */
function buildTiffFile(kept: Entry[], blocks: Uint8Array[], little: boolean): ArrayBuffer {
  const HEADER = 8;
  const ifdSize = 2 + kept.length * 12 + 4;
  const externals = kept.filter((e) => e.value.byteLength > 4);

  let cursor = HEADER + ifdSize;
  const externalAt = new Map<number, number>();
  for (const e of externals) {
    externalAt.set(e.tag, cursor);
    cursor += e.value.byteLength + (e.value.byteLength % 2); // word-align
  }

  const newOffsets: number[] = [];
  for (const b of blocks) {
    newOffsets.push(cursor);
    cursor += b.byteLength;
  }

  const offsetsEntry = kept.find((e) => e.tag === STRIP_OFFSETS || e.tag === TILE_OFFSETS);
  if (offsetsEntry) {
    rewriteOffsets(offsetsEntry, newOffsets, little);
  }

  const header = new Uint8Array(HEADER);
  const hv = new DataView(header.buffer);
  header[0] = little ? 0x49 : 0x4d;
  header[1] = little ? 0x49 : 0x4d;
  hv.setUint16(2, 42, little);
  hv.setUint32(4, HEADER, little); // IFD0 immediately follows the header

  const ifd = new Uint8Array(ifdSize);
  const iv = new DataView(ifd.buffer);
  iv.setUint16(0, kept.length, little);
  kept.forEach((e, i) => {
    const at = 2 + i * 12;
    iv.setUint16(at, e.tag, little);
    iv.setUint16(at + 2, e.type, little);
    iv.setUint32(at + 4, e.count, little);
    if (e.value.byteLength > 4) {
      iv.setUint32(at + 8, externalAt.get(e.tag) as number, little);
    } else {
      ifd.set(e.value, at + 8); // inline, left-aligned
    }
  });
  iv.setUint32(2 + kept.length * 12, 0, little); // no next IFD

  const parts: Uint8Array[] = [header, ifd];
  for (const e of externals) {
    parts.push(e.value);
    if (e.value.byteLength % 2) {
      parts.push(new Uint8Array(1)); // pad
    }
  }
  parts.push(...blocks);

  return concat(parts);
}

export function stripTiff(bytes: Uint8Array): { data: ArrayBuffer; stripped: boolean } {
  const t = openTiff(bytes);
  const { kept, droppedAny } = readIfd0(t, bytes);

  if (!droppedAny) {
    return { data: bytes.slice().buffer, stripped: false };
  }

  kept.sort((a, b) => a.tag - b.tag); // TIFF requires ascending tag order
  const blocks = readPixelBlocks(kept, bytes, t.little);
  return { data: buildTiffFile(kept, blocks, t.little), stripped: true };
}
