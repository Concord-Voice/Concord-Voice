/**
 * GIF metadata strip (#2469).
 */

import { describe, it, expect } from 'vitest';
import { stripGif } from '@/renderer/utils/ui/imageMetadata/gif';
import { ImageParseError } from '@/renderer/utils/ui/imageMetadata/reader';
import {
  GPS_MARKER,
  XMP_MARKER,
  asc,
  assertAbsent,
  assertContains,
  assertPreserved,
  buildGif,
  buildGifWithRareBlocks,
  bytesOf,
  countOccurrences,
  indexOfBytes,
} from './fixtures';

/** The unique 9-byte prefix of an image descriptor identifies one frame. */
const frameKey = (frame: Uint8Array): Uint8Array => frame.subarray(0, 9);

describe('stripGif', () => {
  it('removes the Comment Extension', () => {
    const fixture = buildGif();
    assertContains(fixture.buffer, fixture.commentText, 'comment text');
    assertContains(fixture.buffer, [0x21, 0xfe], 'the Comment Extension introducer');

    const out = bytesOf(stripGif(fixture.buffer).data);

    assertAbsent(out, fixture.commentText, 'comment text');
    assertAbsent(out, [0x21, 0xfe], 'the Comment Extension introducer');
  });

  it('removes GPS carried in the comment', () => {
    const fixture = buildGif();
    assertContains(fixture.buffer, GPS_MARKER, 'GPS payload');

    assertAbsent(bytesOf(stripGif(fixture.buffer).data), GPS_MARKER, 'GPS payload');
  });

  it('removes an XMP Application Extension', () => {
    const fixture = buildGif();
    assertContains(fixture.buffer, asc('XMP DataXMP'), 'the XMP application identifier');
    assertContains(fixture.buffer, XMP_MARKER, 'XMP payload');

    const out = bytesOf(stripGif(fixture.buffer).data);

    assertAbsent(out, asc('XMP DataXMP'), 'the XMP application identifier');
    assertAbsent(out, XMP_MARKER, 'XMP payload');
  });

  it('PRESERVES the NETSCAPE2.0 Application Extension (the loop count)', () => {
    const fixture = buildGif();
    assertContains(fixture.buffer, fixture.netscape, 'the NETSCAPE2.0 extension');

    const out = bytesOf(stripGif(fixture.buffer).data);

    assertPreserved(out, fixture.netscape, 'the NETSCAPE2.0 extension');
    // Loop-count sub-block intact: 0x03 0x01 <u16 loops>.
    const at = indexOfBytes(out, asc('NETSCAPE2.0'));
    expect(Array.from(out.subarray(at + 11, at + 16))).toEqual([0x03, 0x01, 0x00, 0x00, 0x00]);
  });

  it('preserves every Graphic Control Extension', () => {
    const fixture = buildGif();
    expect(countOccurrences(fixture.buffer, fixture.gce)).toBe(2);

    const out = bytesOf(stripGif(fixture.buffer).data);

    expect(countOccurrences(out, fixture.gce)).toBe(2);
  });

  it('preserves the image descriptors and leaves the frame count unchanged', () => {
    const fixture = buildGif();
    const before =
      countOccurrences(fixture.buffer, frameKey(fixture.frameA)) +
      countOccurrences(fixture.buffer, frameKey(fixture.frameB));
    expect(before).toBe(2);

    const out = bytesOf(stripGif(fixture.buffer).data);

    assertContains(out, fixture.frameA, 'frame A, descriptor and pixel sub-blocks');
    assertContains(out, fixture.frameB, 'frame B, descriptor and pixel sub-blocks');
    expect(
      countOccurrences(out, frameKey(fixture.frameA)) +
        countOccurrences(out, frameKey(fixture.frameB))
    ).toBe(2);
  });

  it('preserves the header, logical screen descriptor and global colour table', () => {
    const fixture = buildGif();
    const out = bytesOf(stripGif(fixture.buffer).data);

    expect(Array.from(out.subarray(0, fixture.head.length))).toEqual(Array.from(fixture.head));
  });

  it('keeps the trailer as the final byte', () => {
    const out = bytesOf(stripGif(buildGif().buffer).data);
    expect(out[out.length - 1]).toBe(0x3b);
  });

  it('reports stripped=true when metadata was present', () => {
    expect(stripGif(buildGif().buffer).stripped).toBe(true);
  });

  it('reads a frame that carries a LOCAL colour table', () => {
    const fixture = buildGifWithRareBlocks();

    const out = bytesOf(stripGif(fixture.buffer).data);

    assertContains(out, fixture.frame, 'the frame, local colour table included');
  });

  it('removes a Plain Text Extension', () => {
    const fixture = buildGifWithRareBlocks();
    assertContains(fixture.buffer, fixture.plainText, 'plain-text payload');

    assertAbsent(bytesOf(stripGif(fixture.buffer).data), fixture.plainText, 'plain-text payload');
  });

  it('preserves an extension with an unrecognised label rather than guessing', () => {
    const fixture = buildGifWithRareBlocks();

    const out = bytesOf(stripGif(fixture.buffer).data);

    assertContains(out, fixture.unknownExt, 'the unrecognised extension block');
  });

  it('throws ImageParseError when the trailer is missing', () => {
    const fixture = buildGif({ trailer: false });
    expect(fixture.buffer[fixture.buffer.length - 1]).not.toBe(0x3b);

    expect(() => stripGif(fixture.buffer)).toThrow(/no trailer/);
  });

  it('throws ImageParseError on a bad signature', () => {
    const bytes = buildGif().buffer.slice();
    bytes.set(asc('GIF88a'), 0);
    expect(() => stripGif(bytes)).toThrow(/bad signature/);
  });

  it('throws ImageParseError on an unrecognised block introducer', () => {
    const fixture = buildGif();
    const bytes = fixture.buffer.slice();
    bytes[fixture.head.length] = 0x7f; // neither 0x21, 0x2c nor 0x3b
    expect(() => stripGif(bytes)).toThrow(ImageParseError);
  });
});
