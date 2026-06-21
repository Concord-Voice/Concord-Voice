/**
 * isRenderableMarkdown — validates that a decrypted attachment byte buffer is
 * safe to pass to the inline Markdown renderer.
 *
 * Two defenses:
 *   1. Size cap — refuses to inline-render content above MAX_RENDERABLE_MD_BYTES,
 *      preventing renderer-DoS from a pathological multi-MB Markdown file.
 *   2. UTF-8 + control-char check — refuses to render bytes that are not
 *      valid UTF-8 OR contain control chars outside \t \n \r, defending against
 *      MIME-confusion attacks where a binary blob is uploaded with
 *      mime_type='text/markdown'.
 *
 * Callers that receive `false` should fall back to a generic file-chip render
 * with a "Preview unavailable" or "Too large to preview" label, keeping the
 * download path intact.
 *
 * See spec §5.4 and §7.2 (MIME confusion + render-size DoS mitigations).
 */

export const MAX_RENDERABLE_MD_BYTES = 262144; // 256 KiB

// Reject ASCII control chars except whitespace (\t = 0x09, \n = 0x0A, \r = 0x0D).
// Range: 0x00–0x08, 0x0B, 0x0C, 0x0E–0x1F, 0x7F.
const CONTROL_CHAR_REGEX = /[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]/;

export function isRenderableMarkdown(decrypted: Uint8Array): boolean {
  if (decrypted.byteLength > MAX_RENDERABLE_MD_BYTES) return false;
  try {
    const text = new TextDecoder('utf-8', { fatal: true }).decode(decrypted);
    if (CONTROL_CHAR_REGEX.test(text)) return false;
    return true;
  } catch {
    return false;
  }
}
