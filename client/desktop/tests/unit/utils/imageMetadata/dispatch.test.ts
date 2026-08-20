/**
 * Dispatch and fail-closed policy for stripFileMetadata (#2469).
 *
 * These are the properties that decide whether an unstripped file can reach the
 * upload path at all, so they matter more than any single parser's coverage.
 */

import { describe, it, expect } from 'vitest';
import { stripFileMetadata, ImageParseError } from '@/renderer/utils/imageMetadata';
import {
  GPS_MARKER,
  PIXEL_MARKER,
  asc,
  assertAbsent,
  assertContains,
  buildBmp,
  buildGif,
  buildHeic,
  buildJpeg,
  buildPng,
  buildTiff,
  buildWebpExtended,
  bytesOf,
  cat,
} from './fixtures';

const bufferOf = (bytes: Uint8Array): ArrayBuffer =>
  bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer;

describe('stripFileMetadata format detection', () => {
  it.each([
    ['jpeg', () => buildJpeg().buffer, 'image/jpeg'],
    ['png', () => buildPng().buffer, 'image/png'],
    ['gif', () => buildGif().buffer, 'image/gif'],
    ['webp', () => buildWebpExtended().buffer, 'image/webp'],
    ['heic', () => buildHeic().buffer, 'image/heic'],
    ['tiff', () => buildTiff(true).buffer, 'image/tiff'],
  ])('detects %s from its magic bytes', (format, build, mime) => {
    const result = stripFileMetadata(bufferOf(build()), mime);

    expect(result.format).toBe(format);
    expect(result.stripped).toBe(true);
  });

  it('parses a JPEG as JPEG even when the declared MIME says image/png', () => {
    const fixture = buildJpeg();
    assertContains(fixture.buffer, GPS_MARKER, 'GPS payload in APP1 EXIF');

    const result = stripFileMetadata(bufferOf(fixture.buffer), 'image/png');

    // Magic wins: `file.type` comes from the OS and a rename changes it.
    expect(result.format).toBe('jpeg');
    expect(result.stripped).toBe(true);
    assertAbsent(result.data, GPS_MARKER, 'GPS payload');
  });

  it('parses a PNG as PNG even when the declared MIME says image/jpeg', () => {
    const fixture = buildPng();
    assertContains(fixture.buffer, GPS_MARKER, 'GPS payload in eXIf');

    const result = stripFileMetadata(bufferOf(fixture.buffer), 'image/jpeg');

    expect(result.format).toBe('png');
    assertAbsent(result.data, GPS_MARKER, 'GPS payload');
  });

  it('ignores MIME case', () => {
    const result = stripFileMetadata(bufferOf(buildBmp()), 'IMAGE/BMP');
    expect(result.format).toBe('bmp');
  });
});

describe('stripFileMetadata pass-through', () => {
  it('recognises BMP and returns it unchanged with stripped=false', () => {
    const bmp = buildBmp();
    const input = bufferOf(bmp);

    const result = stripFileMetadata(input, 'image/bmp');

    expect(result.format).toBe('bmp');
    expect(result.stripped).toBe(false);
    expect(result.data).toBe(input);
    expect(Array.from(bytesOf(result.data))).toEqual(Array.from(bmp));
  });

  it('passes a non-image buffer through as format=unknown', () => {
    const pdf = cat(asc('%PDF-1.7\n'), PIXEL_MARKER, asc('\n%%EOF'));
    const input = bufferOf(pdf);

    const result = stripFileMetadata(input, 'application/pdf');

    expect(result.format).toBe('unknown');
    expect(result.stripped).toBe(false);
    expect(result.data).toBe(input);
  });

  it('passes an ISO-BMFF video brand through rather than guessing', () => {
    // ftyp at offset 4, brand 'isom' — MP4, not a still image.
    const mp4 = cat([0x00, 0x00, 0x00, 0x18], asc('ftyp'), asc('isom'), [0, 0, 0, 0], asc('isom'));

    const result = stripFileMetadata(bufferOf(mp4), 'video/mp4');

    expect(result.format).toBe('unknown');
    expect(result.stripped).toBe(false);
  });
});

describe('stripFileMetadata fails closed', () => {
  it('THROWS on a garbage buffer that declares image/jpeg', () => {
    const garbage = cat(asc('this is definitely not a JPEG'), PIXEL_MARKER);

    expect(() => stripFileMetadata(bufferOf(garbage), 'image/jpeg')).toThrow(ImageParseError);
  });

  it('throws on an empty buffer that declares an image type', () => {
    expect(() => stripFileMetadata(new ArrayBuffer(0), 'image/png')).toThrow(ImageParseError);
  });

  it('throws for every MIME type the module claims to handle', () => {
    const garbage = bufferOf(cat(asc('nope'), PIXEL_MARKER));

    for (const mime of [
      'image/jpeg',
      'image/jpg',
      'image/png',
      'image/apng',
      'image/webp',
      'image/gif',
      'image/heic',
      'image/heif',
      'image/tiff',
      'image/bmp',
    ]) {
      expect(() => stripFileMetadata(garbage, mime), `MIME ${mime} must fail closed`).toThrow(
        ImageParseError
      );
    }
  });

  it('propagates a parse failure rather than returning the original bytes', () => {
    // Correct PNG magic, then a chunk length that runs past the end of the file.
    const fixture = buildPng();
    const bytes = fixture.buffer.slice();
    new DataView(bytes.buffer, bytes.byteOffset).setUint32(8, 0x0fff_ffff, false);

    expect(() => stripFileMetadata(bufferOf(bytes), 'image/png')).toThrow(ImageParseError);
  });

  it('still throws when the declared MIME does not claim the format', () => {
    // A truncated JPEG mislabelled as a PDF: magic still says JPEG, so it is
    // parsed, and the parse failure is not swallowed.
    const truncated = buildJpeg().buffer.slice(0, 10);

    expect(() => stripFileMetadata(bufferOf(truncated), 'application/pdf')).toThrow(
      ImageParseError
    );
  });
});
