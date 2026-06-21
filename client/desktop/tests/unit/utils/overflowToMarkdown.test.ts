import { describe, it, expect } from 'vitest';
import {
  composeMarkdownOverflow,
  OVERFLOW_PREVIEW_CHARS,
} from '@/renderer/utils/overflowToMarkdown';

describe('composeMarkdownOverflow', () => {
  it('produces preview text of OVERFLOW_PREVIEW_CHARS chars + ellipsis', () => {
    // ASCII-only content: surrogate guard never fires, so length is exactly
    // OVERFLOW_PREVIEW_CHARS + 1 (the ellipsis character).
    const content = 'a'.repeat(6000);
    const { previewText } = composeMarkdownOverflow(content);
    expect(previewText.length).toBe(OVERFLOW_PREVIEW_CHARS + 1);
    expect(previewText.endsWith('…')).toBe(true);
    expect(previewText.startsWith('a'.repeat(10))).toBe(true);
  });

  it('preserves the full content in fileBlob', async () => {
    const content = '# heading\n\n' + 'a'.repeat(6000);
    const { fileBlob } = composeMarkdownOverflow(content);
    expect(fileBlob.type).toBe('text/markdown');
    expect(fileBlob.size).toBe(new TextEncoder().encode(content).byteLength);
    const text = await fileBlob.text();
    expect(text).toBe(content);
  });

  it('generates a filename of shape message-<ISO8601-no-colons>.md', () => {
    const content = 'a'.repeat(6000);
    const { filename } = composeMarkdownOverflow(content);
    // ISO with colons replaced by hyphens
    expect(filename).toMatch(/^message-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}(\.\d{3})?Z\.md$/);
  });

  it('handles unicode content correctly (no byte/char confusion)', () => {
    // 3000 emoji chars = 6000 UTF-16 code units (each emoji is a surrogate pair,
    // so each contributes 2 code units)
    const content = '😀'.repeat(3000);
    const { previewText, fileBlob } = composeMarkdownOverflow(content);
    // 200 UTF-16 code units = exactly 100 emoji (200/2), so the slice lands on a
    // clean pair boundary and the surrogate guard does not fire.
    expect(previewText).toBe(content.slice(0, OVERFLOW_PREVIEW_CHARS) + '…');
    // Full content in the blob, byte-exact
    expect(fileBlob.size).toBe(new TextEncoder().encode(content).byteLength);
  });

  it('does NOT trim short content (caller is responsible for cap check)', () => {
    // Even if called with short content, the utility just composes.
    // (In practice MessageInput only calls this on overflow, but the unit
    // should not silently no-op — it always produces a preview+blob+filename.)
    const content = 'short';
    const { previewText, fileBlob } = composeMarkdownOverflow(content);
    // 'short' is < OVERFLOW_PREVIEW_CHARS, so preview is full content + ellipsis
    expect(previewText).toBe('short…');
    expect(fileBlob.size).toBe(5);
  });

  it('avoids orphan high surrogate when slice boundary lands mid-pair', () => {
    // 199 ASCII chars + 1000 emoji. The 200th UTF-16 code unit is the high
    // surrogate of the first emoji (position 199); position 200 is its low
    // surrogate. content.slice(0, 200) would orphan the high surrogate. The
    // guard must drop it.
    const content = 'a'.repeat(199) + '😀'.repeat(1000);
    const { previewText } = composeMarkdownOverflow(content);

    // Preview is 199 'a's + ellipsis (the emoji is dropped because its high
    // surrogate would be orphaned — the guard removes it). No U+FFFD.
    expect(previewText.length).toBe(200); // 199 chars + '…'
    expect(previewText).toBe('a'.repeat(199) + '…');
    expect(previewText).not.toContain('\uFFFD'); // no U+FFFD replacement char
    // The full content is still preserved in the blob — only the preview is shortened.
  });
});
