/**
 * HEIC / ISO-BMFF metadata strip (#2469).
 *
 * The core property is that the strip is OFFSET-STABLE: the output is the same
 * length as the input, because `iloc` holds absolute file offsets and moving any
 * byte would invalidate them.
 */

import { describe, it, expect } from 'vitest';
import { stripHeic } from '@/renderer/utils/imageMetadata/bmff';
import { ImageParseError } from '@/renderer/utils/imageMetadata/reader';
import {
  GPS_MARKER,
  PIXEL_MARKER,
  asc,
  assertAbsent,
  assertContains,
  buildHeic,
  buildHeicWithoutExif,
  bytesOf,
} from './fixtures';

describe('stripHeic', () => {
  it('zeroes the Exif item payload, removing GPS', () => {
    const fixture = buildHeic();
    assertContains(fixture.buffer, GPS_MARKER, 'GPS payload in the Exif item');
    assertContains(fixture.buffer, asc('CONCORDCAM'), 'camera make in the Exif item');

    const out = bytesOf(stripHeic(fixture.buffer).data);

    assertAbsent(out, GPS_MARKER, 'GPS payload');
    assertAbsent(out, asc('CONCORDCAM'), 'camera make');
    const zeroed = out.subarray(fixture.exifStart, fixture.exifStart + fixture.exifLength);
    expect(Array.from(zeroed)).toEqual(new Array(fixture.exifLength).fill(0));
  });

  it('produces output of exactly the same length as the input (offset stability)', () => {
    const fixture = buildHeic();

    const out = bytesOf(stripHeic(fixture.buffer).data);

    expect(out.byteLength).toBe(fixture.buffer.byteLength);
  });

  it('sets the iloc extent length for the Exif item to 0', () => {
    const fixture = buildHeic();
    const before = new DataView(
      fixture.buffer.buffer,
      fixture.buffer.byteOffset,
      fixture.buffer.byteLength
    ).getUint32(fixture.exifLengthFieldAt, false);
    expect(before).toBe(fixture.exifLength);

    const out = bytesOf(stripHeic(fixture.buffer).data);
    const after = new DataView(out.buffer, out.byteOffset, out.byteLength).getUint32(
      fixture.exifLengthFieldAt,
      false
    );

    expect(after).toBe(0);
  });

  it('leaves every byte outside the Exif payload and its length field identical', () => {
    const fixture = buildHeic();
    const before = fixture.buffer;

    const out = bytesOf(stripHeic(before).data);

    const inExif = (i: number): boolean =>
      i >= fixture.exifStart && i < fixture.exifStart + fixture.exifLength;
    const inLengthField = (i: number): boolean =>
      i >= fixture.exifLengthFieldAt && i < fixture.exifLengthFieldAt + 4;

    const differing: number[] = [];
    for (let i = 0; i < before.length; i++) {
      if (out[i] !== before[i] && !inExif(i) && !inLengthField(i)) {
        differing.push(i);
      }
    }
    expect(differing).toEqual([]);
  });

  it('leaves the pixel item payload untouched', () => {
    const fixture = buildHeic();

    const out = bytesOf(stripHeic(fixture.buffer).data);

    assertContains(out, PIXEL_MARKER, 'the pixel item payload');
    expect(
      Array.from(out.subarray(fixture.imageStart, fixture.imageStart + fixture.imageLength))
    ).toEqual(
      Array.from(
        fixture.buffer.subarray(fixture.imageStart, fixture.imageStart + fixture.imageLength)
      )
    );
  });

  it('does not mutate the input buffer', () => {
    const fixture = buildHeic();
    const snapshot = Array.from(fixture.buffer);

    stripHeic(fixture.buffer);

    expect(Array.from(fixture.buffer)).toEqual(snapshot);
  });

  it('reports stripped=true when an Exif item was present', () => {
    expect(stripHeic(buildHeic().buffer).stripped).toBe(true);
  });

  it('reports stripped=false and unchanged bytes when there is no metadata item', () => {
    const clean = buildHeicWithoutExif();

    const result = stripHeic(clean);

    expect(result.stripped).toBe(false);
    expect(Array.from(bytesOf(result.data))).toEqual(Array.from(clean));
  });

  it('handles an mdat box with a 64-bit extended size', () => {
    const fixture = buildHeic({ extendedMdat: true });
    assertContains(fixture.buffer, GPS_MARKER, 'GPS payload in the Exif item');

    const out = bytesOf(stripHeic(fixture.buffer).data);

    assertAbsent(out, GPS_MARKER, 'GPS payload');
    expect(out.byteLength).toBe(fixture.buffer.byteLength);
  });

  it('handles iloc version 1 with 8-byte offset and length fields', () => {
    const fixture = buildHeic({ ilocVersion: 1, offsetSize: 8, lengthSize: 8 });
    assertContains(fixture.buffer, GPS_MARKER, 'GPS payload in the Exif item');
    expect(fixture.exifLengthFieldSize).toBe(8);

    const out = bytesOf(stripHeic(fixture.buffer).data);
    const dv = new DataView(out.buffer, out.byteOffset, out.byteLength);

    assertAbsent(out, GPS_MARKER, 'GPS payload');
    expect(out.byteLength).toBe(fixture.buffer.byteLength);
    expect(dv.getUint32(fixture.exifLengthFieldAt, false)).toBe(0);
    expect(dv.getUint32(fixture.exifLengthFieldAt + 4, false)).toBe(0);
  });

  it('throws when a box declares a size that overruns its parent', () => {
    const fixture = buildHeic();
    const bytes = fixture.buffer.slice();
    // The meta box is the second top-level box; inflate its declared size.
    const metaAt = 0 + new DataView(bytes.buffer, bytes.byteOffset).getUint32(0, false);
    new DataView(bytes.buffer, bytes.byteOffset).setUint32(metaAt, 0x0fff_ffff, false);
    expect(() => stripHeic(bytes)).toThrow(ImageParseError);
  });

  it('throws when an iloc extent points outside the file', () => {
    const fixture = buildHeic();
    const bytes = fixture.buffer.slice();
    // extent_offset sits immediately before the extent_length field.
    new DataView(bytes.buffer, bytes.byteOffset).setUint32(
      fixture.exifLengthFieldAt - 4,
      0x0fff_ffff,
      false
    );
    expect(() => stripHeic(bytes)).toThrow(/overruns/);
  });
});
