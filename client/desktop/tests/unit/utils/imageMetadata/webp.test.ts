/**
 * WebP (RIFF) metadata strip (#2469).
 */

import { describe, it, expect } from 'vitest';
import { stripWebp } from '@/renderer/utils/imageMetadata/riff';
import { ImageParseError } from '@/renderer/utils/imageMetadata/reader';
import {
  GPS_MARKER,
  ICC_MARKER,
  VP8X_EXIF_FLAG,
  VP8X_ICC_FLAG,
  VP8X_XMP_FLAG,
  XMP_MARKER,
  asc,
  assertAbsent,
  assertContains,
  buildWebpExtended,
  buildWebpSimple,
  bytesOf,
  cat,
  indexOfBytes,
  le32,
  riffChunk,
} from './fixtures';

const riffSizeField = (buf: Uint8Array): number =>
  new DataView(buf.buffer, buf.byteOffset, buf.byteLength).getUint32(4, true);

describe('stripWebp', () => {
  it('removes the EXIF chunk carrying GPS', () => {
    const fixture = buildWebpExtended();
    assertContains(fixture.buffer, asc('EXIF'), 'the EXIF chunk FourCC');
    assertContains(fixture.buffer, GPS_MARKER, 'GPS payload in the EXIF chunk');

    const out = bytesOf(stripWebp(fixture.buffer).data);

    assertAbsent(out, GPS_MARKER, 'GPS payload');
    assertAbsent(out, asc('EXIF'), 'the EXIF chunk FourCC');
  });

  it('removes the XMP chunk', () => {
    const fixture = buildWebpExtended();
    assertContains(fixture.buffer, XMP_MARKER, 'XMP payload');
    assertContains(fixture.buffer, asc('XMP '), 'the XMP chunk FourCC');

    const out = bytesOf(stripWebp(fixture.buffer).data);

    assertAbsent(out, XMP_MARKER, 'XMP payload');
    assertAbsent(out, asc('XMP '), 'the XMP chunk FourCC');
  });

  it('clears the VP8X EXIF (0x08) and XMP (0x04) advertisement bits', () => {
    const fixture = buildWebpExtended();
    const flagsBefore = fixture.buffer[fixture.vp8xFlagsAt];
    // Pre-assert the bits were actually set, or "cleared" would be meaningless.
    expect(flagsBefore & VP8X_EXIF_FLAG).toBe(VP8X_EXIF_FLAG);
    expect(flagsBefore & VP8X_XMP_FLAG).toBe(VP8X_XMP_FLAG);

    const out = bytesOf(stripWebp(fixture.buffer).data);
    const vp8xAt = indexOfBytes(out, asc('VP8X'));
    expect(vp8xAt).toBeGreaterThanOrEqual(0);
    const flagsAfter = out[vp8xAt + 8];

    expect(flagsAfter & VP8X_EXIF_FLAG).toBe(0);
    expect(flagsAfter & VP8X_XMP_FLAG).toBe(0);
  });

  it('leaves the VP8X ICC bit set and the ICCP chunk in place', () => {
    const fixture = buildWebpExtended();
    expect(fixture.buffer[fixture.vp8xFlagsAt] & VP8X_ICC_FLAG).toBe(VP8X_ICC_FLAG);
    assertContains(fixture.buffer, ICC_MARKER, 'ICCP profile payload');

    const out = bytesOf(stripWebp(fixture.buffer).data);
    const vp8xAt = indexOfBytes(out, asc('VP8X'));

    expect(out[vp8xAt + 8] & VP8X_ICC_FLAG).toBe(VP8X_ICC_FLAG);
    assertContains(out, fixture.iccp, 'the entire ICCP chunk');
  });

  it('does not mutate the input buffer while clearing the VP8X flags', () => {
    const fixture = buildWebpExtended();
    const before = Array.from(fixture.buffer);

    stripWebp(fixture.buffer);

    expect(Array.from(fixture.buffer)).toEqual(before);
  });

  it('rewrites the RIFF size field for the shortened file', () => {
    const fixture = buildWebpExtended();
    expect(riffSizeField(fixture.buffer)).toBe(fixture.buffer.length - 8);

    const out = bytesOf(stripWebp(fixture.buffer).data);

    expect(riffSizeField(out)).toBe(out.length - 8);
    expect(out.length).toBeLessThan(fixture.buffer.length);
  });

  it('preserves the VP8 pixel chunk', () => {
    const fixture = buildWebpExtended();
    const out = bytesOf(stripWebp(fixture.buffer).data);
    assertContains(out, fixture.vp8, 'the entire VP8 pixel chunk');
  });

  it('reports stripped=true when metadata was present', () => {
    expect(stripWebp(buildWebpExtended().buffer).stripped).toBe(true);
  });

  it('returns a simple-format WebP unchanged with stripped=false', () => {
    const simple = buildWebpSimple();

    const result = stripWebp(simple);

    expect(result.stripped).toBe(false);
    expect(Array.from(bytesOf(result.data))).toEqual(Array.from(simple));
  });

  it('throws when the container is not RIFF', () => {
    const bytes = buildWebpExtended().buffer.slice();
    bytes[0] = 0x52 + 1;
    expect(() => stripWebp(bytes)).toThrow(/not a RIFF/);
  });

  it('throws when the RIFF form is not WEBP', () => {
    const bytes = buildWebpExtended().buffer.slice();
    bytes.set(asc('AVI '), 8);
    expect(() => stripWebp(bytes)).toThrow(/not WEBP/);
  });

  it('throws when a chunk declares a size past the end of the file', () => {
    const fixture = buildWebpExtended();
    const bytes = fixture.buffer.slice();
    const exifAt = indexOfBytes(bytes, asc('EXIF'));
    bytes.set(le32(0x0fff_ffff), exifAt + 4);
    expect(() => stripWebp(bytes)).toThrow(ImageParseError);
  });

  it('handles an odd-length chunk without disturbing the following chunk', () => {
    const fixture = buildWebpExtended();
    // ICCP was built with an odd declared size, so RIFF padding is in play.
    const declared = new DataView(
      fixture.iccp.buffer,
      fixture.iccp.byteOffset,
      fixture.iccp.byteLength
    ).getUint32(4, true);
    expect(declared % 2).toBe(1);
    expect(fixture.iccp.length).toBe(8 + declared + 1);
    const out = bytesOf(stripWebp(fixture.buffer).data);

    // Both survivors are intact and in order.
    const iccpAt = indexOfBytes(out, fixture.iccp);
    const vp8At = indexOfBytes(out, fixture.vp8);
    expect(iccpAt).toBeGreaterThan(0);
    expect(vp8At).toBe(iccpAt + fixture.iccp.length);
  });
});

describe('stripWebp output framing', () => {
  it('emits a RIFF/WEBP header followed only by the surviving chunks', () => {
    const fixture = buildWebpExtended();
    const out = bytesOf(stripWebp(fixture.buffer).data);

    expect(Array.from(out.subarray(0, 4))).toEqual(asc('RIFF'));
    expect(Array.from(out.subarray(8, 12))).toEqual(asc('WEBP'));

    // VP8X rebuilt independently: same payload, ICC bit only.
    const vp8x = riffChunk('VP8X', [VP8X_ICC_FLAG, 0, 0, 0, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00]);
    const expected = cat(vp8x, fixture.iccp, fixture.vp8);
    expect(Array.from(out.subarray(12))).toEqual(Array.from(expected));
  });
});
