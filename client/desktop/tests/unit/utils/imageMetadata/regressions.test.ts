import { describe, it, expect } from 'vitest';
import { stripJpeg } from '@/renderer/utils/ui/imageMetadata/jpeg';
import { stripHeic } from '@/renderer/utils/ui/imageMetadata/bmff';
import { stripFileMetadata, ImageParseError } from '@/renderer/utils/ui/imageMetadata';
import {
  GPS_MARKER,
  PIXEL_MARKER,
  assertAbsent,
  assertContains,
  assertPreserved,
  asc,
  be16,
  be32,
  bmffBox,
  bmffFullBox,
  buildJpeg,
  bytesOf,
  cat,
  indexOfBytes,
} from './fixtures';

/**
 * Regressions found by an independent read of the parsers (#2469).
 *
 * Each of these passed the format's own happy-path tests while leaking. They are
 * kept as named regressions rather than folded into the per-format suites so the
 * specific failure mode stays legible to whoever breaks one.
 */
describe('imageMetadata regressions', () => {
  it('JPEG: discards data appended after EOI', () => {
    // Cameras append thumbnails here, and it is the classic polyglot slot. The
    // original implementation copied from SOS to end-of-buffer, so a second EXIF
    // block after EOI survived every strip while a decoder ignored it entirely.
    const base = buildJpeg().buffer;
    const trailer = cat(asc('TRAILING-SECONDARY-IMAGE'), GPS_MARKER);
    const withTrailer = cat(base, trailer);

    expect(indexOfBytes(bytesOf(withTrailer), GPS_MARKER)).toBeGreaterThanOrEqual(0);

    const out = bytesOf(stripJpeg(withTrailer).data);

    expect(indexOfBytes(out, GPS_MARKER)).toBe(-1);
    expect(out.length).toBeLessThan(bytesOf(withTrailer).length);
  });

  it('HEIC: refuses an iloc whose length_size is 0 rather than reporting success', () => {
    // length_size = 0 stores no extent length, so the span of the metadata item
    // is unknown. Zeroing a zero-length range removes nothing — and the original
    // implementation still returned stripped: true, which is strictly worse than
    // refusing: the caller uploads a GPS-bearing file believing it was cleaned.
    const exif = cat(be32(0), asc('MM'), be16(42), be32(8), GPS_MARKER, asc('CONCORDCAM'));
    const image = cat(asc('PIXELS'), [1, 2, 3]);
    const ftyp = bmffBox('ftyp', cat(asc('heic'), be32(0), asc('mif1')));
    const hdlr = bmffFullBox('hdlr', 0, cat(be32(0), asc('pict'), new Uint8Array(12), [0]));
    const infe = (id: number, t: string) =>
      bmffFullBox('infe', 2, cat(be16(id), be16(0), asc(t), [0]));
    const iinf = bmffFullBox('iinf', 0, cat(be16(2), infe(1, 'Exif'), infe(2, 'hvc1')));

    // offset_size = 4 (high nibble), length_size = 0 (low nibble).
    const mkIloc = (eo: number, io: number) =>
      bmffFullBox(
        'iloc',
        0,
        cat(
          [0x40],
          [0x00],
          be16(2),
          be16(1),
          be16(0),
          be16(1),
          be32(eo),
          be16(2),
          be16(0),
          be16(1),
          be32(io)
        )
      );
    const ilocSize = mkIloc(0, 0).length;
    const metaSize = 8 + 4 + hdlr.length + iinf.length + ilocSize;
    const exifStart = ftyp.length + metaSize + 8;
    const iloc = mkIloc(exifStart, exifStart + exif.length);
    const meta = bmffFullBox('meta', 0, cat(hdlr, iinf, iloc));
    const buf = cat(ftyp, meta, bmffBox('mdat', cat(exif, image)));

    expect(() => stripHeic(buf)).toThrow(ImageParseError);
    expect(() => stripHeic(buf)).toThrow(/length_size=0/);
  });

  it('JPEG: walks every scan in a progressive image', () => {
    // A progressive JPEG carries several scans with DHT/DQT/SOS headers between
    // them, and those payloads are NOT byte-stuffed. The original fix for the
    // trailing-data leak scanned raw for FF D9, so a quantization table
    // containing those two bytes ended the walk early and TRUNCATED the photo.
    //
    // This fixture puts FF D9 inside a DQT payload between two scans, and an
    // EXIF APP1 after the first scan. Both halves matter: the image must survive
    // intact, and metadata after a scan must still be stripped.
    const sos = (payload: number[]) => cat([0xff, 0xda], be16(payload.length + 2), payload);
    const dqt = (payload: number[]) => cat([0xff, 0xdb], be16(payload.length + 2), payload);
    const app1 = (payload: number[]) => cat([0xff, 0xe1], be16(payload.length + 2), payload);

    const buf = cat(
      [0xff, 0xd8], // SOI
      sos([0x01, 0x01, 0x00]),
      [0x11, 0x22, 0xff, 0x00, 0x33], // scan 1: a stuffed FF, then data
      dqt([0x00, 0xff, 0xd9, 0x44]), // DQT payload CONTAINS a bare FF D9
      app1(cat(asc('Exif'), [0x00, 0x00], GPS_MARKER)), // metadata after a scan
      sos([0x01, 0x01, 0x00]),
      cat(PIXEL_MARKER, [0xff, 0x00]), // scan 2, ending in a stuffed FF
      [0xff, 0xd9] // EOI
    );

    assertContains(buf, GPS_MARKER, 'fixture EXIF');
    assertContains(buf, PIXEL_MARKER, 'fixture second scan');

    const out = bytesOf(stripJpeg(buf).data);

    // The second scan survived — i.e. the walk did not stop at the false FF D9.
    assertPreserved(out, PIXEL_MARKER, 'second scan data');
    // And metadata appearing after the first scan was still removed.
    assertAbsent(out, GPS_MARKER, 'EXIF after the first scan');
    // The image ends at EOI.
    expect(Array.from(out.subarray(out.length - 2))).toEqual([0xff, 0xd9]);
  });

  it('HEIC: refuses an iloc extent that is not file-offset based', () => {
    // construction_method 1 (idat-relative) and 2 (item-relative) mean the extent
    // offset is NOT absolute. Treating it as absolute zeroes an unrelated byte
    // range — potentially pixel data — while the real EXIF survives, and then
    // reports stripped: true. Same "destroy the wrong bytes, claim success" shape
    // as an unknown length_size.
    const exif = cat(be32(0), asc('MM'), be16(42), be32(8), GPS_MARKER);
    const ftyp = bmffBox('ftyp', cat(asc('heic'), be32(0), asc('mif1')));
    const hdlr = bmffFullBox('hdlr', 0, cat(be32(0), asc('pict'), new Uint8Array(12), [0]));
    const infe = (id: number, t: string) =>
      bmffFullBox('infe', 2, cat(be16(id), be16(0), asc(t), [0]));
    const iinf = bmffFullBox('iinf', 0, cat(be16(1), infe(1, 'Exif')));

    // version 1 so construction_method is present; low nibble = 1 (idat-relative).
    const iloc = bmffFullBox(
      'iloc',
      1,
      cat(
        [0x44], // offset_size 4, length_size 4
        [0x00],
        be16(1), // one item
        be16(1), // item id
        be16(0x0001), // reserved(12) + construction_method(4) = 1
        be16(0), // data reference index
        be16(1), // one extent
        be32(0),
        be32(exif.length)
      )
    );
    const meta = bmffFullBox('meta', 0, cat(hdlr, iinf, iloc));
    const buf = cat(ftyp, meta, bmffBox('mdat', exif));

    expect(() => stripHeic(buf)).toThrow(ImageParseError);
    expect(() => stripHeic(buf)).toThrow(/construction_method=1/);
  });

  it('dispatch: routes AVIF through the ISO-BMFF path', () => {
    // AVIF is ISO-BMFF and carries EXIF in the same meta/iinf/iloc structure, but
    // its major brand is 'avif' and the original brand list omitted it, so an
    // AVIF fell through to the pass-through branch entirely unstripped.
    const avif = cat(be32(24), asc('ftyp'), asc('avif'), be32(0), asc('mif1'), asc('miaf'));
    const buf = avif.buffer.slice(
      avif.byteOffset,
      avif.byteOffset + avif.byteLength
    ) as ArrayBuffer;

    const result = stripFileMetadata(buf, 'image/avif');

    expect(result.format).toBe('heic');
  });
});
