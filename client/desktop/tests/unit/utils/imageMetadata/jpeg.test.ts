/**
 * JPEG metadata strip (#2469).
 *
 * Every removal assertion is preceded by the matching presence assertion on the
 * pre-strip fixture, so a fixture that never carried the payload fails loudly
 * instead of passing vacuously.
 */

import { describe, it, expect } from 'vitest';
import { stripJpeg } from '@/renderer/utils/imageMetadata/jpeg';
import { ImageParseError } from '@/renderer/utils/imageMetadata/reader';
import {
  GPS_MARKER,
  ICC_MARKER,
  IPTC_MARKER,
  XMP_MARKER,
  asc,
  assertAbsent,
  assertContains,
  be16,
  bytesOf,
  buildBareJpeg,
  buildJpeg,
  buildJpegWithFillAndStandalone,
  cat,
  indexOfBytes,
} from './fixtures';

describe('stripJpeg', () => {
  it('removes GPS carried in an APP1 EXIF block', () => {
    const fixture = buildJpeg();
    assertContains(fixture.buffer, GPS_MARKER, 'GPS payload in APP1 EXIF');
    assertContains(fixture.buffer, asc('CONCORDCAM'), 'EXIF Make string');

    const out = bytesOf(stripJpeg(fixture.buffer).data);

    assertAbsent(out, GPS_MARKER, 'GPS payload');
    assertAbsent(out, asc('CONCORDCAM'), 'EXIF Make string');
  });

  it('reports stripped=true when metadata was present', () => {
    expect(stripJpeg(buildJpeg().buffer).stripped).toBe(true);
  });

  it('reports stripped=false for a JPEG with no metadata segments', () => {
    const bare = buildBareJpeg();
    const result = stripJpeg(bare);
    expect(result.stripped).toBe(false);
    expect(Array.from(bytesOf(result.data))).toEqual(Array.from(bare));
  });

  it('copies the SOS header and scan data byte-identically', () => {
    const fixture = buildJpeg();
    assertContains(fixture.buffer, fixture.tail, 'SOS header + scan data + EOI');

    const out = bytesOf(stripJpeg(fixture.buffer).data);

    // The scan must be the literal tail of the output, not merely present.
    expect(indexOfBytes(out, fixture.tail)).toBe(out.length - fixture.tail.length);
  });

  it('removes the APP13 Photoshop/IPTC block', () => {
    const fixture = buildJpeg();
    assertContains(fixture.buffer, IPTC_MARKER, 'APP13 IPTC payload');

    assertAbsent(bytesOf(stripJpeg(fixture.buffer).data), IPTC_MARKER, 'APP13 IPTC payload');
  });

  it('removes the COM comment segment', () => {
    const fixture = buildJpeg();
    assertContains(fixture.buffer, fixture.comment, 'COM comment text');

    assertAbsent(bytesOf(stripJpeg(fixture.buffer).data), fixture.comment, 'COM comment text');
  });

  it('removes an XMP-flavoured APP1 block', () => {
    const fixture = buildJpeg();
    assertContains(fixture.buffer, XMP_MARKER, 'APP1 XMP payload');

    assertAbsent(bytesOf(stripJpeg(fixture.buffer).data), XMP_MARKER, 'APP1 XMP payload');
  });

  it('PRESERVES the APP2 ICC colour profile (deliberate decision)', () => {
    const fixture = buildJpeg();
    assertContains(fixture.buffer, ICC_MARKER, 'APP2 ICC payload');

    const out = bytesOf(stripJpeg(fixture.buffer).data);

    // The whole segment, header included — not just its payload bytes.
    assertContains(out, fixture.app2, 'the entire APP2 segment');
  });

  it('preserves non-metadata segments (APP0 JFIF, SOF0)', () => {
    const fixture = buildJpeg();
    const out = bytesOf(stripJpeg(fixture.buffer).data);

    assertContains(out, fixture.app0, 'APP0 JFIF segment');
    assertContains(out, fixture.sof0, 'SOF0 segment');
  });

  it('rebuilds a minimal Orientation-only APP1 so Orientation 6 survives', () => {
    const fixture = buildJpeg({ orientation: 6 });
    assertContains(fixture.buffer, [0x01, 0x12, 0x00, 0x03], 'EXIF Orientation tag 0x0112');

    const out = bytesOf(stripJpeg(fixture.buffer).data);

    // MM-ordered IFD0 entry: tag 0x0112, type SHORT, count 1, value 6.
    const orientationEntry = [0x01, 0x12, 0x00, 0x03, 0x00, 0x00, 0x00, 0x01, 0x00, 0x06];
    assertContains(out, orientationEntry, 'rebuilt Orientation entry');

    const expectedApp1 = cat(
      [0xff, 0xe1],
      be16(34),
      asc('Exif\0\0'),
      asc('MM'),
      be16(42),
      [0x00, 0x00, 0x00, 0x08],
      be16(1),
      orientationEntry,
      [0x00, 0x00],
      [0x00, 0x00, 0x00, 0x00]
    );
    // Rebuilt APP1 sits immediately after SOI, before every other segment.
    expect(indexOfBytes(out, expectedApp1)).toBe(2);
  });

  it('carries no GPS or Make into the rebuilt Orientation APP1', () => {
    const fixture = buildJpeg({ orientation: 6 });
    const out = bytesOf(stripJpeg(fixture.buffer).data);

    // Exactly one Exif header in the output: the rebuilt one.
    expect(indexOfBytes(out, asc('Exif\0\0'))).toBe(6);
    expect(indexOfBytes(out, asc('Exif\0\0'), 7)).toBe(-1);
  });

  it('emits no Orientation APP1 when the source Orientation is the default 1', () => {
    const fixture = buildJpeg({ orientation: 1 });
    const out = bytesOf(stripJpeg(fixture.buffer).data);

    assertAbsent(out, asc('Exif\0\0'), 'any EXIF block');
    assertAbsent(out, GPS_MARKER, 'GPS payload');
  });

  it('preserves standalone markers and 0xFF fill bytes, and stops at EOI', () => {
    const fixture = buildJpegWithFillAndStandalone();
    assertContains(fixture.buffer, fixture.comment, 'COM comment text');

    const out = bytesOf(stripJpeg(fixture.buffer).data);

    assertAbsent(out, fixture.comment, 'COM comment text');
    // SOI, APP0, RST0 (standalone), SOF0 (reached through fill bytes), EOI.
    expect(Array.from(out)).toEqual(
      Array.from(cat([0xff, 0xd8], fixture.app0, [0xff, 0xd0], fixture.sof0, [0xff, 0xd9]))
    );
  });

  it('throws ImageParseError on input truncated inside a segment', () => {
    const truncated = buildJpeg().buffer.slice(0, 10);
    expect(() => stripJpeg(truncated)).toThrow(ImageParseError);
  });

  it('throws ImageParseError when the file ends before EOI', () => {
    const fixture = buildJpeg();
    // Cut immediately after the APP0 segment: structurally clean, but no EOI.
    const end = indexOfBytes(fixture.buffer, fixture.app0) + fixture.app0.length;
    expect(() => stripJpeg(fixture.buffer.slice(0, end))).toThrow(/no EOI/);
  });

  it('throws when the buffer does not begin with SOI', () => {
    const bytes = buildJpeg().buffer.slice();
    bytes[1] = 0x00;
    expect(() => stripJpeg(bytes)).toThrow(ImageParseError);
  });

  it('throws on a segment declaring an impossible length', () => {
    const bytes = buildJpeg().buffer.slice();
    const app0At = indexOfBytes(bytes, [0xff, 0xe0]);
    bytes[app0At + 2] = 0x00;
    bytes[app0At + 3] = 0x01; // length 1 — below the mandatory 2
    expect(() => stripJpeg(bytes)).toThrow(/invalid segment length/);
  });
});
