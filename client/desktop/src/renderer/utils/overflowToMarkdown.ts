/**
 * composeMarkdownOverflow — deterministic-input utility that converts a long
 * Markdown message into a preview snippet + a Blob suitable for upload via the
 * Tier 2 E2EE attachment path.
 *
 * Note: not technically pure (`new Date()` is read for the filename timestamp),
 * but the output is fully determined by the inputs at the moment of call.
 *
 * Used by MessageInput.handleSend when content.length exceeds MAX_CONTENT_LENGTH.
 * The recipient renders the previewText inline and fetches the fileBlob on
 * [Expand] for full inline rendering.
 *
 * See spec §5.1 (data flow — send) and §5.4 for validation pairing.
 */

export const OVERFLOW_PREVIEW_CHARS = 200;

export interface MarkdownOverflowComposition {
  /** First OVERFLOW_PREVIEW_CHARS UTF-16 code units of content + '…' (surrogate-safe) */
  previewText: string;
  /** Full content as a text/markdown Blob — UTF-8 encoded by the Blob spec */
  fileBlob: Blob;
  /** Auto-generated filename: message-<ISO8601-no-colons>.md */
  filename: string;
}

/**
 * Slices `s` to at most `n` UTF-16 code units, then drops the trailing code
 * unit if it is a high surrogate (0xD800–0xDBFF). Without this guard, slicing
 * at an emoji boundary would orphan the high surrogate; the orphan renders as
 * U+FFFD (replacement char), causing visible corruption in the preview UI.
 */
function safeSlicePreservingSurrogates(s: string, n: number): string {
  const sliced = s.slice(0, n);
  if (sliced.length === 0) return sliced;
  const last = sliced.codePointAt(sliced.length - 1) ?? 0;
  // If the last char is a high surrogate (0xD800–0xDBFF), it would be orphaned
  // without its low-surrogate partner. Drop it to avoid U+FFFD render corruption.
  if (last >= 0xd800 && last <= 0xdbff) {
    return sliced.slice(0, -1);
  }
  return sliced;
}

export function composeMarkdownOverflow(content: string): MarkdownOverflowComposition {
  const previewText = safeSlicePreservingSurrogates(content, OVERFLOW_PREVIEW_CHARS) + '…';
  const fileBlob = new Blob([content], { type: 'text/markdown' });
  const isoTimestamp = new Date().toISOString().replaceAll(':', '-');
  const filename = `message-${isoTimestamp}.md`;
  return { previewText, fileBlob, filename };
}
