/**
 * PNG / APNG metadata strip (#2469).
 */

import { describe, it, expect } from 'vitest';
import { stripPng } from '@/renderer/utils/imageMetadata/png';
import { ImageParseError } from '@/renderer/utils/imageMetadata/reader';
import {
  GPS_MARKER,
  ICC_MARKER,
  IPTC_MARKER,
  PNG_SIGNATURE,
  XMP_MARKER,
  assertAbsent,
  assertContains,
  be32,
  buildApng,
  buildPng,
  bytesOf,
  cat,
  indexOfBytes,
  pngChunk,
  pngChunks,
} from './fixtures';

const typesOf = (buf: ArrayBuffer | Uint8Array): string[] => pngChunks(buf).map((c) => c.type);

describe('stripPng', () => {
  it('removes the eXIf chunk carrying GPS', () => {
    const fixture = buildPng();
    expect(typesOf(fixture.buffer)).toContain('eXIf');
    assertContains(fixture.buffer, GPS_MARKER, 'GPS payload in eXIf');

    const out = bytesOf(stripPng(fixture.buffer).data);

    expect(typesOf(out)).not.toContain('eXIf');
    assertAbsent(out, GPS_MARKER, 'GPS payload');
  });

  it('removes tEXt, iTXt and zTXt chunks', () => {
    const fixture = buildPng();
    assertContains(fixture.buffer, fixture.textValue, 'tEXt Software string');
    assertContains(fixture.buffer, XMP_MARKER, 'iTXt XMP payload');
    assertContains(fixture.buffer, IPTC_MARKER, 'zTXt payload');

    const out = bytesOf(stripPng(fixture.buffer).data);

    assertAbsent(out, fixture.textValue, 'tEXt Software string');
    assertAbsent(out, XMP_MARKER, 'iTXt XMP payload');
    assertAbsent(out, IPTC_MARKER, 'zTXt payload');
    expect(typesOf(out)).not.toContain('tEXt');
    expect(typesOf(out)).not.toContain('iTXt');
    expect(typesOf(out)).not.toContain('zTXt');
  });

  it('removes the tIME last-modified chunk', () => {
    const out = bytesOf(stripPng(buildPng().buffer).data);
    expect(typesOf(out)).not.toContain('tIME');
  });

  it('preserves IHDR, IDAT and IEND byte-identically', () => {
    const fixture = buildPng();
    const out = bytesOf(stripPng(fixture.buffer).data);

    assertContains(out, fixture.ihdr, 'the whole IHDR chunk');
    assertContains(out, fixture.idat, 'the whole IDAT chunk');
    assertContains(out, fixture.iend, 'the whole IEND chunk');
    // Order matters as much as presence.
    expect(typesOf(out)).toEqual(['IHDR', 'iCCP', 'IDAT', 'IEND']);
  });

  it('keeps the 8-byte PNG signature', () => {
    const out = bytesOf(stripPng(buildPng().buffer).data);
    expect(Array.from(out.subarray(0, 8))).toEqual(PNG_SIGNATURE);
  });

  it('PRESERVES the iCCP colour profile (deliberate decision)', () => {
    const fixture = buildPng();
    assertContains(fixture.buffer, ICC_MARKER, 'iCCP profile payload');

    const out = bytesOf(stripPng(fixture.buffer).data);

    assertContains(out, fixture.iccp, 'the entire iCCP chunk');
    expect(typesOf(out)).toContain('iCCP');
  });

  it('reports stripped=false and an unchanged file when there is no metadata', () => {
    const clean = cat(
      PNG_SIGNATURE,
      pngChunk('IHDR', cat(be32(2), be32(2), [0x08, 0x02, 0x00, 0x00, 0x00])),
      pngChunk('IDAT', [0x78, 0x9c, 0x62, 0x00, 0x00, 0x00, 0x02, 0x00, 0x01]),
      pngChunk('IEND', [])
    );

    const result = stripPng(clean);

    expect(result.stripped).toBe(false);
    expect(Array.from(bytesOf(result.data))).toEqual(Array.from(clean));
  });

  it('throws ImageParseError when IEND is missing', () => {
    const fixture = buildPng();
    const iendAt = indexOfBytes(fixture.buffer, fixture.iend);
    expect(iendAt).toBeGreaterThan(0);

    expect(() => stripPng(fixture.buffer.slice(0, iendAt))).toThrow(/no IEND/);
  });

  it('throws ImageParseError on a bad signature', () => {
    const bytes = buildPng().buffer.slice();
    bytes[1] = 0x00;
    expect(() => stripPng(bytes)).toThrow(ImageParseError);
  });

  it('throws ImageParseError on a chunk length that overruns the file', () => {
    const bytes = buildPng().buffer.slice();
    // IHDR is the first chunk, its length field starts at offset 8.
    new DataView(bytes.buffer, bytes.byteOffset).setUint32(8, 0x0fff_ffff, false);
    expect(() => stripPng(bytes)).toThrow(ImageParseError);
  });
});

describe('stripPng on APNG', () => {
  it('removes metadata while acTL, fcTL and fdAT survive', () => {
    const fixture = buildApng();
    assertContains(fixture.buffer, GPS_MARKER, 'GPS payload in eXIf');
    expect(typesOf(fixture.buffer)).toContain('acTL');

    const out = bytesOf(stripPng(fixture.buffer).data);
    const types = typesOf(out);

    assertAbsent(out, GPS_MARKER, 'GPS payload');
    expect(types).not.toContain('eXIf');
    expect(types).not.toContain('tEXt');
    expect(types).toContain('acTL');
    expect(types.filter((t) => t === 'fcTL')).toHaveLength(3);
    expect(types.filter((t) => t === 'fdAT')).toHaveLength(2);
  });

  it('leaves the acTL frame count unchanged', () => {
    const fixture = buildApng();
    const before = pngChunks(fixture.buffer).find((c) => c.type === 'acTL');
    expect(before?.data.length).toBe(8);

    const out = bytesOf(stripPng(fixture.buffer).data);
    const after = pngChunks(out).find((c) => c.type === 'acTL');

    const frames = (chunk: typeof before): number =>
      new DataView(chunk!.data.buffer, chunk!.data.byteOffset).getUint32(0, false);
    expect(frames(after)).toBe(fixture.frameCount);
    expect(frames(after)).toBe(frames(before));
    // The whole chunk, CRC included, is copied verbatim.
    assertContains(out, fixture.actl, 'the entire acTL chunk');
  });
});
