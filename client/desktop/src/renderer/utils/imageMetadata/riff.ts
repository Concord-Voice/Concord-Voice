/**
 * WebP metadata strip (#2469).
 *
 * WebP is a RIFF container: "RIFF" | size | "WEBP" | chunks. Metadata only
 * exists in the extended form, which opens with a VP8X chunk whose flag byte
 * advertises which optional chunks follow.
 *
 * Dropping the EXIF and XMP chunks is not enough on its own: the VP8X flag bits
 * must be cleared too, or a decoder is told to expect chunks that are no longer
 * there. The RIFF size field then has to be rewritten for the shorter file.
 *
 * A simple-format WebP (VP8 or VP8L, no VP8X) has nowhere to carry metadata, so
 * it is returned untouched with stripped=false. That is a correct no-op, not a
 * gap in coverage.
 *
 * ICCP is preserved, consistent with JPEG's APP2 and PNG's iCCP.
 */

import { ByteReader, ImageParseError, concat } from './reader';

const VP8X_EXIF_FLAG = 0x08;
const VP8X_XMP_FLAG = 0x04;

export function stripWebp(bytes: Uint8Array): { data: ArrayBuffer; stripped: boolean } {
  const r = new ByteReader(bytes);
  if (r.ascii(4) !== 'RIFF') {
    throw new ImageParseError('not a RIFF container');
  }
  r.u32le(); // declared size; recomputed below rather than trusted
  if (r.ascii(4) !== 'WEBP') {
    throw new ImageParseError('RIFF container is not WEBP');
  }

  const chunks: Uint8Array[] = [];
  let stripped = false;
  let vp8xAt: number | null = null;

  while (r.remaining() >= 8) {
    r.step('RIFF chunk');
    const start = r.position;
    const type = r.ascii(4);
    const size = r.u32le();
    // RIFF chunks are padded to even length.
    const padded = size + (size % 2);
    if (r.remaining() < padded) {
      throw new ImageParseError(`truncated ${type} chunk: needs ${padded}, has ${r.remaining()}`);
    }
    r.skip(padded);
    const end = r.position;

    if (type === 'EXIF' || type === 'XMP ') {
      stripped = true;
      continue;
    }

    if (type === 'VP8X') {
      vp8xAt = chunks.length;
    }
    chunks.push(bytes.slice(start, end));
  }

  if (!stripped) {
    // Nothing removed: return the original bytes rather than a rebuilt copy, so
    // a file with no metadata is byte-identical after a round trip.
    return { data: bytes.slice().buffer, stripped: false };
  }

  if (vp8xAt !== null) {
    // Clear the EXIF and XMP advertisement bits. Byte layout: type(4) size(4)
    // then the flag byte at payload offset 0.
    const vp8x = chunks[vp8xAt];
    vp8x[8] &= ~(VP8X_EXIF_FLAG | VP8X_XMP_FLAG);
  }

  let payloadSize = 4; // the "WEBP" FourCC counts toward the RIFF size
  for (const c of chunks) {
    payloadSize += c.byteLength;
  }

  const header = new Uint8Array(12);
  header.set([0x52, 0x49, 0x46, 0x46], 0); // "RIFF"
  new DataView(header.buffer).setUint32(4, payloadSize, true);
  header.set([0x57, 0x45, 0x42, 0x50], 8); // "WEBP"

  return { data: concat([header, ...chunks]), stripped: true };
}
