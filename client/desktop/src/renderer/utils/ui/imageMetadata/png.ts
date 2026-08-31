/**
 * PNG and APNG metadata strip (#2469).
 *
 * A PNG is an 8-byte signature followed by length-prefixed chunks, each with its
 * own CRC. Removing whole chunks needs no CRC recomputation — the CRC covers the
 * chunk's own type and data, not the file.
 *
 * Dropped: eXIf (EXIF, including GPS), tEXt / iTXt / zTXt (arbitrary text, the
 * usual home of camera software strings and XMP), and tIME (last-modified).
 *
 * Preserved: everything else, and two deliberately.
 *
 *   iCCP  colour profile, on the same reasoning as JPEG's APP2. A profile is not
 *         identifying data and dropping it shifts colour.
 *   acTL / fcTL / fdAT  APNG animation control and frames. This is why the
 *         module walks chunks rather than re-encoding: a canvas round-trip
 *         would flatten an APNG to its first frame.
 */

import { ByteReader, ImageParseError, concat } from './reader';

const SIGNATURE = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];

const STRIPPED = new Set(['eXIf', 'tEXt', 'iTXt', 'zTXt', 'tIME']);

export function stripPng(bytes: Uint8Array): { data: ArrayBuffer; stripped: boolean } {
  const r = new ByteReader(bytes);
  for (const expected of SIGNATURE) {
    if (r.u8() !== expected) {
      throw new ImageParseError('not a PNG: bad signature');
    }
  }

  const out: Uint8Array[] = [bytes.subarray(0, 8)];
  let stripped = false;
  let sawIEND = false;

  while (!r.eof()) {
    r.step('PNG chunk');
    const start = r.position;
    const length = r.u32be();
    const type = r.ascii(4);

    // length covers data only; the chunk is 4 (len) + 4 (type) + length + 4 (crc).
    // The CRC is read for its bounds check and then discarded: a chunk copied
    // verbatim keeps whatever CRC it arrived with, and a chunk we drop needs none.
    r.skip(length);
    r.u32be();
    const end = r.position;

    if (STRIPPED.has(type)) {
      stripped = true;
      continue;
    }

    out.push(bytes.subarray(start, end));

    if (type === 'IEND') {
      sawIEND = true;
      break;
    }
  }

  if (!sawIEND) {
    throw new ImageParseError('truncated PNG: no IEND chunk');
  }

  return { data: concat(out), stripped };
}
