/**
 * TIFF metadata strip (#2469).
 *
 * TIFF is rebuilt rather than walked, so the interesting property is that the
 * pixel data is still addressable through the RECOMPUTED StripOffsets.
 */

import { describe, it, expect } from 'vitest';
import { stripTiff } from '@/renderer/utils/imageMetadata/tiff';
import { ImageParseError } from '@/renderer/utils/imageMetadata/reader';
import {
  GPS_MARKER,
  PIXEL_MARKER,
  assertAbsent,
  assertContains,
  buildTiff,
  bytesOf,
  indexOfBytes,
  readTiffIfd0,
  tiffNumbers,
} from './fixtures';

const ORDERS: Array<[string, boolean]> = [
  ['II (little-endian)', true],
  ['MM (big-endian)', false],
];

describe.each(ORDERS)('stripTiff — %s', (_label, little) => {
  it('removes the GPS IFD pointer and its payload', () => {
    const fixture = buildTiff(little);
    assertContains(fixture.buffer, GPS_MARKER, 'GPS sub-IFD payload');
    expect(readTiffIfd0(fixture.buffer).entries.map((e) => e.tag)).toContain(34853);

    const out = bytesOf(stripTiff(fixture.buffer).data);

    assertAbsent(out, GPS_MARKER, 'GPS sub-IFD payload');
    expect(readTiffIfd0(out).entries.map((e) => e.tag)).not.toContain(34853);
  });

  it('removes the Exif IFD pointer and its payload', () => {
    const fixture = buildTiff(little);
    assertContains(fixture.buffer, fixture.exifMarker, 'Exif sub-IFD payload');
    expect(readTiffIfd0(fixture.buffer).entries.map((e) => e.tag)).toContain(34665);

    const out = bytesOf(stripTiff(fixture.buffer).data);

    assertAbsent(out, fixture.exifMarker, 'Exif sub-IFD payload');
    expect(readTiffIfd0(out).entries.map((e) => e.tag)).not.toContain(34665);
  });

  it('removes Make, Model, Software and DateTime', () => {
    const fixture = buildTiff(little);
    assertContains(fixture.buffer, fixture.make, 'Make');
    assertContains(fixture.buffer, fixture.model, 'Model');
    assertContains(fixture.buffer, fixture.software, 'Software');
    assertContains(fixture.buffer, fixture.datetime, 'DateTime');

    const out = bytesOf(stripTiff(fixture.buffer).data);

    assertAbsent(out, fixture.make, 'Make');
    assertAbsent(out, fixture.model, 'Model');
    assertAbsent(out, fixture.software, 'Software');
    assertAbsent(out, fixture.datetime, 'DateTime');
    const tags = readTiffIfd0(out).entries.map((e) => e.tag);
    expect(tags).not.toContain(271);
    expect(tags).not.toContain(272);
    expect(tags).not.toContain(305);
    expect(tags).not.toContain(306);
  });

  it('PRESERVES Orientation (274) with its value', () => {
    const fixture = buildTiff(little);
    const source = readTiffIfd0(fixture.buffer);
    const sourceOrientation = source.entries.find((e) => e.tag === 274);
    expect(tiffNumbers(sourceOrientation!, source.little)).toEqual([6]);

    const out = readTiffIfd0(stripTiff(fixture.buffer).data);
    const orientation = out.entries.find((e) => e.tag === 274);

    expect(orientation).toBeDefined();
    expect(tiffNumbers(orientation!, out.little)).toEqual([6]);
  });

  it('preserves the structural decode tags', () => {
    const fixture = buildTiff(little);
    const out = readTiffIfd0(stripTiff(fixture.buffer).data);
    const tags = out.entries.map((e) => e.tag);

    for (const tag of [256, 257, 258, 259, 262, 273, 274, 277, 278, 279]) {
      expect(tags).toContain(tag);
    }
  });

  it('keeps the byte order of the source file', () => {
    const fixture = buildTiff(little);
    const out = bytesOf(stripTiff(fixture.buffer).data);

    expect(Array.from(out.subarray(0, 2))).toEqual(little ? [0x49, 0x49] : [0x4d, 0x4d]);
    expect(readTiffIfd0(out).little).toBe(little);
  });

  it('emits IFD0 entries in ascending tag order', () => {
    const fixture = buildTiff(little);
    const tags = readTiffIfd0(stripTiff(fixture.buffer).data).entries.map((e) => e.tag);

    expect(tags).toEqual([...tags].sort((a, b) => a - b));
  });

  it('recovers the pixel strips byte-identically through the recomputed StripOffsets', () => {
    const fixture = buildTiff(little);
    const out = bytesOf(stripTiff(fixture.buffer).data);
    const parsed = readTiffIfd0(out);

    const offsets = tiffNumbers(
      parsed.entries.find((e) => e.tag === 273)!,
      parsed.little
    );
    const counts = tiffNumbers(
      parsed.entries.find((e) => e.tag === 279)!,
      parsed.little
    );
    expect(offsets).toHaveLength(fixture.strips.length);
    expect(counts).toEqual(fixture.strips.map((s) => s.length));

    // The offsets must have actually moved — otherwise this test would pass on
    // a parser that simply forgot to rewrite them.
    const sourceParsed = readTiffIfd0(fixture.buffer);
    const sourceOffsets = tiffNumbers(
      sourceParsed.entries.find((e) => e.tag === 273)!,
      sourceParsed.little
    );
    expect(offsets).not.toEqual(sourceOffsets);

    fixture.strips.forEach((strip, i) => {
      expect(offsets[i] + counts[i]).toBeLessThanOrEqual(out.length);
      expect(Array.from(out.subarray(offsets[i], offsets[i] + counts[i]))).toEqual(
        Array.from(strip)
      );
    });
  });

  it('places the pixel data where the marker search also finds it', () => {
    const fixture = buildTiff(little);
    const out = bytesOf(stripTiff(fixture.buffer).data);

    assertContains(out, PIXEL_MARKER, 'strip 0 pixel data');
    expect(indexOfBytes(out, fixture.strips[0])).toBeGreaterThan(0);
    expect(indexOfBytes(out, fixture.strips[1])).toBeGreaterThan(0);
  });

  it('reports stripped=true when tags were dropped', () => {
    expect(stripTiff(buildTiff(little).buffer).stripped).toBe(true);
  });

  it('shrinks the file', () => {
    const fixture = buildTiff(little);
    const out = bytesOf(stripTiff(fixture.buffer).data);
    expect(out.length).toBeLessThan(fixture.buffer.length);
  });
});

describe('stripTiff error handling', () => {
  it('throws on a bad byte-order mark', () => {
    const bytes = buildTiff(true).buffer.slice();
    bytes[0] = 0x58;
    bytes[1] = 0x58;
    expect(() => stripTiff(bytes)).toThrow(/byte-order mark/);
  });

  it('throws on a bad magic number', () => {
    const bytes = buildTiff(true).buffer.slice();
    bytes[2] = 0x2b;
    expect(() => stripTiff(bytes)).toThrow(/bad magic/);
  });

  it('throws when the IFD0 offset is out of range', () => {
    const bytes = buildTiff(true).buffer.slice();
    new DataView(bytes.buffer, bytes.byteOffset).setUint32(4, 0x0fff_ffff, true);
    expect(() => stripTiff(bytes)).toThrow(/IFD0 offset/);
  });

  it('throws when a KEPT tag value overruns the file', () => {
    // Corrupt tag 273 (StripOffsets) — structural, so its value IS dereferenced.
    //
    // This deliberately targets a kept tag. Corrupting a DROPPED tag (Make,
    // Software, GPSIFD…) must NOT throw: those entries are discarded without
    // reading their value, so an out-of-range offset on one is unreachable and
    // rejecting the whole upload over it would fail closed in the wrong place —
    // legitimate files carry private tags with vendor field types. The
    // sibling test below pins that half.
    const fixture = buildTiff(true);
    const bytes = fixture.buffer.slice();
    const dv = new DataView(bytes.buffer, bytes.byteOffset);

    const count = dv.getUint16(8, true);
    let entryAt = -1;
    for (let i = 0; i < count; i++) {
      const at = 8 + 2 + i * 12;
      if (dv.getUint16(at, true) === 273) {
        entryAt = at;
        break;
      }
    }
    expect(entryAt).toBeGreaterThan(0);

    dv.setUint32(entryAt + 8, 0x0fff_ffff, true);
    expect(() => stripTiff(bytes)).toThrow(ImageParseError);
  });

  it('does NOT throw when a DROPPED tag value overruns the file', () => {
    // Tag 271 (Make) is discarded, so its offset is never dereferenced. Before
    // #2469's fix the size/offset of every entry was resolved before the
    // allowlist check, so a private or vendor field type on a tag being thrown
    // away rejected the entire upload.
    const fixture = buildTiff(true);
    const bytes = fixture.buffer.slice();
    const dv = new DataView(bytes.buffer, bytes.byteOffset);

    const count = dv.getUint16(8, true);
    let entryAt = -1;
    for (let i = 0; i < count; i++) {
      const at = 8 + 2 + i * 12;
      if (dv.getUint16(at, true) === 271) {
        entryAt = at;
        break;
      }
    }
    expect(entryAt).toBeGreaterThan(0);

    dv.setUint32(entryAt + 8, 0x0fff_ffff, true);
    expect(() => stripTiff(bytes)).not.toThrow();
  });

  it('throws on an unknown TIFF field type', () => {
    const bytes = buildTiff(true).buffer.slice();
    const dv = new DataView(bytes.buffer, bytes.byteOffset);
    const entryAt = 8 + 2; // first entry, tag 256
    expect(dv.getUint16(entryAt, true)).toBe(256);
    dv.setUint16(entryAt + 2, 99, true);
    expect(() => stripTiff(bytes)).toThrow(/unknown TIFF field type 99/);
  });

  it('throws when the surviving IFD has no strip or tile offsets', () => {
    const bytes = buildTiff(true).buffer.slice();
    const dv = new DataView(bytes.buffer, bytes.byteOffset);
    const entryAt = 8 + 2 + 7 * 12;
    expect(dv.getUint16(entryAt, true)).toBe(273);
    dv.setUint16(entryAt, 700, true); // renumber StripOffsets to a dropped tag
    expect(() => stripTiff(bytes)).toThrow(/cannot relocate pixel data/);
  });

  it('throws when a pixel block offset overruns the file', () => {
    const fixture = buildTiff(true);
    const bytes = fixture.buffer.slice();
    const parsed = readTiffIfd0(bytes);
    const stripOffsets = parsed.entries.find((e) => e.tag === 273)!;
    const dv = new DataView(bytes.buffer, bytes.byteOffset);
    const entryAt = 8 + 2 + 7 * 12;
    expect(dv.getUint16(entryAt, true)).toBe(273);
    const valuesAt = dv.getUint32(entryAt + 8, true);
    expect(stripOffsets.count).toBe(2);
    dv.setUint32(valuesAt, 0x0fff_ffff, true);
    expect(() => stripTiff(bytes)).toThrow(/overruns file/);
  });
});
