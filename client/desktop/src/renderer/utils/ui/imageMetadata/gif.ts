/**
 * GIF metadata strip (#2469).
 *
 * A GIF is a header, a logical screen descriptor, an optional global colour
 * table, then a sequence of blocks terminated by 0x3B. Metadata rides in
 * extension blocks introduced by 0x21:
 *
 *   0xFE  Comment Extension        — dropped
 *   0xFF  Application Extension    — dropped, EXCEPT NETSCAPE2.0
 *   0xF9  Graphic Control          — preserved (frame delay, disposal)
 *   0x01  Plain Text               — dropped (renderable text nobody emits)
 *
 * NETSCAPE2.0 is the exception that matters. It carries the animation loop
 * count, so a rule of "drop all application extensions" would silently make
 * every looping GIF play once — the same class of mistake as stripping ICC or
 * flattening an APNG. XMP arrives as an Application Extension too (identifier
 * "XMP Data"), and that one is genuinely metadata.
 */

import { ByteReader, ImageParseError, concat } from './reader';

const EXTENSION = 0x21;
const IMAGE_DESCRIPTOR = 0x2c;
const TRAILER = 0x3b;

const COMMENT_EXT = 0xfe;
const APPLICATION_EXT = 0xff;
const GRAPHIC_CONTROL_EXT = 0xf9;
const PLAIN_TEXT_EXT = 0x01;

const NETSCAPE = 'NETSCAPE2.0';

/** Walks a chain of length-prefixed sub-blocks to its terminating zero byte. */
function skipSubBlocks(r: ByteReader): void {
  for (;;) {
    r.step('GIF sub-block');
    const size = r.u8();
    if (size === 0) {
      return;
    }
    r.skip(size);
  }
}

/** Reads the header, logical screen descriptor, and global colour table. */
function readGifHeader(r: ByteReader): void {
  const signature = r.ascii(6);
  if (signature !== 'GIF87a' && signature !== 'GIF89a') {
    throw new ImageParseError('not a GIF: bad signature');
  }
  r.skip(4); // logical screen width + height
  const packed = r.u8();
  r.skip(2); // background colour index + pixel aspect ratio
  if (packed & 0x80) {
    // Global colour table: 3 bytes per entry, 2^(N+1) entries.
    r.skip(3 * (1 << ((packed & 0x07) + 1)));
  }
}

/** Advances past one image descriptor and its LZW sub-block chain. */
function skipImageDescriptor(r: ByteReader): void {
  r.skip(8); // left, top, width, height
  const packed = r.u8();
  if (packed & 0x80) {
    r.skip(3 * (1 << ((packed & 0x07) + 1))); // local colour table
  }
  r.skip(1); // LZW minimum code size
  skipSubBlocks(r);
}

/**
 * Advances past one extension block and reports whether it should be KEPT.
 *
 * NETSCAPE2.0 is the exception that matters: it carries the animation loop
 * count, so a rule of "drop all application extensions" would silently make
 * every looping GIF play once. An unrecognised label is preserved rather than
 * guessed at — more likely a format we do not model than metadata worth removing.
 */
function readExtension(r: ByteReader): boolean {
  const label = r.u8();

  if (label === APPLICATION_EXT) {
    const blockSize = r.u8();
    const identifier = r.ascii(blockSize);
    skipSubBlocks(r);
    return identifier === NETSCAPE; // XMP Data, ImageMagick, Adobe → dropped
  }

  if (label === COMMENT_EXT || label === PLAIN_TEXT_EXT) {
    if (label === PLAIN_TEXT_EXT) {
      r.skip(r.u8()); // fixed-size header before the sub-block chain
    }
    skipSubBlocks(r);
    return false;
  }

  if (label === GRAPHIC_CONTROL_EXT) {
    r.skip(r.u8()); // fixed-size payload
    skipSubBlocks(r);
    return true;
  }

  skipSubBlocks(r);
  return true;
}

export function stripGif(bytes: Uint8Array): { data: ArrayBuffer; stripped: boolean } {
  const r = new ByteReader(bytes);
  readGifHeader(r);

  const out: Uint8Array[] = [bytes.subarray(0, r.position)];
  let stripped = false;
  let sawTrailer = false;

  while (!r.eof()) {
    r.step('GIF block');
    const blockStart = r.position;
    const introducer = r.u8();

    if (introducer === TRAILER) {
      out.push(new Uint8Array([TRAILER]));
      sawTrailer = true;
      break;
    }

    if (introducer === IMAGE_DESCRIPTOR) {
      skipImageDescriptor(r);
      out.push(bytes.subarray(blockStart, r.position));
      continue;
    }

    if (introducer !== EXTENSION) {
      throw new ImageParseError(
        `unexpected GIF block 0x${introducer.toString(16)} at ${blockStart}`
      );
    }

    if (readExtension(r)) {
      out.push(bytes.subarray(blockStart, r.position));
    } else {
      stripped = true;
    }
  }

  if (!sawTrailer) {
    throw new ImageParseError('truncated GIF: no trailer');
  }

  return { data: concat(out), stripped };
}
