import { describe, it, expect } from 'vitest';
import { isRenderableMarkdown, MAX_RENDERABLE_MD_BYTES } from '@/renderer/utils/renderableMarkdown';

describe('isRenderableMarkdown', () => {
  const enc = new TextEncoder();

  it('accepts plain ASCII Markdown', () => {
    const bytes = enc.encode('# Heading\n\nParagraph with **bold**.');
    expect(isRenderableMarkdown(bytes)).toBe(true);
  });

  it('accepts UTF-8 multibyte content (emoji, CJK, accented latin)', () => {
    const bytes = enc.encode('# 你好\n\n*Café* — emoji: 😀');
    expect(isRenderableMarkdown(bytes)).toBe(true);
  });

  it('accepts whitespace control chars (\\t \\n \\r)', () => {
    const bytes = enc.encode('line one\nline two\tindented\r\nline three');
    expect(isRenderableMarkdown(bytes)).toBe(true);
  });

  it('accepts empty content', () => {
    const bytes = enc.encode('');
    expect(isRenderableMarkdown(bytes)).toBe(true);
  });

  it('rejects control chars (NULL byte)', () => {
    const bytes = new Uint8Array([0x23, 0x20, 0x00, 0x68, 0x65, 0x6c, 0x6c, 0x6f]); // "# \0hello"
    expect(isRenderableMarkdown(bytes)).toBe(false);
  });

  it('rejects control chars (ESC = 0x1B)', () => {
    const bytes = new Uint8Array([0x23, 0x20, 0x1b, 0x68, 0x65, 0x6c, 0x6c, 0x6f]); // "# \x1Bhello"
    expect(isRenderableMarkdown(bytes)).toBe(false);
  });

  it('rejects control chars (DEL = 0x7F)', () => {
    const bytes = new Uint8Array([0x23, 0x20, 0x7f, 0x68, 0x65, 0x6c, 0x6c, 0x6f]); // "# \x7Fhello"
    expect(isRenderableMarkdown(bytes)).toBe(false);
  });

  it('rejects invalid UTF-8 byte sequences', () => {
    // 0xFE and 0xFF are never valid in UTF-8
    const bytes = new Uint8Array([0x23, 0x20, 0xfe, 0xff, 0x68]);
    expect(isRenderableMarkdown(bytes)).toBe(false);
  });

  it('rejects content exceeding MAX_RENDERABLE_MD_BYTES', () => {
    const oversized = new Uint8Array(MAX_RENDERABLE_MD_BYTES + 1);
    // Fill with safe ASCII printable bytes to isolate the size check
    oversized.fill(0x61);
    expect(isRenderableMarkdown(oversized)).toBe(false);
  });

  it('accepts content at exactly MAX_RENDERABLE_MD_BYTES', () => {
    const atCap = new Uint8Array(MAX_RENDERABLE_MD_BYTES);
    atCap.fill(0x61);
    expect(isRenderableMarkdown(atCap)).toBe(true);
  });
});
