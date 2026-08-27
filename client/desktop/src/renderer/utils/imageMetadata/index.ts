/**
 * Strip identifying metadata from image bytes before they are encrypted and
 * uploaded (#2469).
 *
 * Tier-2 attachments are end-to-end encrypted, so the server cannot strip
 * anything: a GPS-tagged photo reaches every recipient with its coordinates
 * intact, and once sent it cannot be remediated server-side. Only the sending
 * client can remove it, which is why this runs before `encryptFile`.
 *
 * Two properties matter more than breadth of format support:
 *
 *   Dispatch on MAGIC BYTES, not the declared MIME. `file.type` comes from the
 *   OS and is attacker-influenced by renaming, so trusting it would run the
 *   wrong parser on a mislabelled file.
 *
 *   FAIL CLOSED. A buffer that sniffs as a known image container but will not
 *   parse throws, and the upload is rejected. It never falls back to sending the
 *   original — a fallback would silently void the entire control, which is the
 *   defect class found on the attachment path in #2843 and the message path in
 *   #2832.
 */

import { ImageParseError, startsWith, tag } from './reader';
import { stripJpeg } from './jpeg';
import { stripPng } from './png';
import { stripWebp } from './riff';
import { stripGif } from './gif';
import { stripHeic } from './bmff';
import { stripTiff } from './tiff';

export { ImageParseError } from './reader';

export interface StripResult {
  /** Stripped bytes, or the original buffer when there was nothing to remove. */
  data: ArrayBuffer;
  /** Whether any metadata was actually removed. */
  stripped: boolean;
  /** Detected container, for tests and diagnostics. `unknown` when passed through. */
  format: string;
}

type Stripper = (bytes: Uint8Array) => { data: ArrayBuffer; stripped: boolean };

/**
 * MIME types this module claims to handle. A buffer whose declared type is in
 * here MUST parse — it cannot silently pass through — which is what makes a
 * renamed or corrupt file an error rather than an unprotected upload.
 */
const HANDLED_IMAGE_MIMES = new Set([
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
  'image/avif',
]);

/** How many leading bytes `sniffHandledImage` needs.
 *
 *  `detect` reads magic bytes and, for ISO-BMFF, a brand at offset 8..12 — far
 *  less than this. 64 KiB is deliberate headroom so the sniff never has to grow
 *  a second read, and it is the ONLY plaintext this path touches for a file it
 *  turns out not to handle. */
export const SNIFF_BYTES = 65_536;

/** Whether the leading bytes look like an image format this module strips.
 *
 *  Dispatch on THIS, never on the declared MIME type. A JPEG uploaded as
 *  `application/octet-stream` must still be stripped — dispatching on the
 *  declared type would silently stop stripping it, which is a privacy
 *  regression against #2469 rather than a mere miscategorisation. */
export function sniffHandledImage(head: Uint8Array): boolean {
  const detected = detect(head);
  return detected !== null && detected.strip !== null;
}

function detect(bytes: Uint8Array): { format: string; strip: Stripper | null } | null {
  if (startsWith(bytes, [0xff, 0xd8, 0xff])) {
    return { format: 'jpeg', strip: stripJpeg };
  }
  if (startsWith(bytes, [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])) {
    // APNG is a PNG with extra chunks; the same walker handles both.
    return { format: 'png', strip: stripPng };
  }
  if (startsWith(bytes, tag('GIF87a')) || startsWith(bytes, tag('GIF89a'))) {
    return { format: 'gif', strip: stripGif };
  }
  if (startsWith(bytes, tag('RIFF')) && startsWith(bytes, tag('WEBP'), 8)) {
    return { format: 'webp', strip: stripWebp };
  }
  if (startsWith(bytes, tag('ftyp'), 4)) {
    // ISO-BMFF. Brand distinguishes still images from video; video is PR 2.
    const brand = String.fromCodePoint(...bytes.subarray(8, 12));
    // AVIF is ISO-BMFF too and carries EXIF in the same meta/iinf/iloc
    // structure, so the HEIC path handles it unchanged.
    const stillImageBrands = [
      'heic',
      'heix',
      'hevc',
      'heim',
      'heis',
      'hevm',
      'mif1',
      'msf1',
      'avif',
      'avis',
    ];
    if (stillImageBrands.includes(brand)) {
      return { format: 'heic', strip: stripHeic };
    }
    return null; // MP4/MOV and friends — not handled here, see PR 2
  }
  if (startsWith(bytes, [0x49, 0x49, 0x2a, 0x00]) || startsWith(bytes, [0x4d, 0x4d, 0x00, 0x2a])) {
    return { format: 'tiff', strip: stripTiff };
  }
  if (startsWith(bytes, tag('BM'))) {
    // BMP has no metadata container at all. Recognised so it is a deliberate
    // no-op rather than falling through to the unparseable branch.
    return { format: 'bmp', strip: null };
  }
  return null;
}

export function stripFileMetadata(data: ArrayBuffer, mimeType: string): StripResult {
  const bytes = new Uint8Array(data);
  const declared = (mimeType || '').toLowerCase();
  const detected = detect(bytes);

  if (!detected) {
    if (HANDLED_IMAGE_MIMES.has(declared)) {
      // Claims to be a format we strip, but its bytes say otherwise. Rejecting
      // is the point: uploading it would mean uploading unstripped bytes under
      // a type we promised to handle.
      throw new ImageParseError(
        `file declares ${declared} but its contents do not match any handled image format`
      );
    }
    // PDFs, documents, audio, video, archives. Not in scope.
    return { data, stripped: false, format: 'unknown' };
  }

  if (!detected.strip) {
    return { data, stripped: false, format: detected.format };
  }

  // Parse failures propagate. The caller must not upload the original.
  const { data: out, stripped } = detected.strip(bytes);
  return { data: out, stripped, format: detected.format };
}
