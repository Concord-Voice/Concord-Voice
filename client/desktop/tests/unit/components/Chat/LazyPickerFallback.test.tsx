import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { Suspense, type ReactElement } from 'react';
import { describe, it, expect, vi } from 'vitest';
import LazyEmojiPicker from '@/renderer/components/EmojiPicker/LazyEmojiPicker';
import LazyGifPicker from '@/renderer/components/GifPicker/LazyGifPicker';

/**
 * #2071: the emoji/GIF pickers are position:fixed popovers, but a Suspense
 * `fallback` renders in the composer's NORMAL FLOW. A sized fallback box
 * (`.emoji-picker-loading` 280px / `.gif-picker-loading` 160px) there inflates
 * the message area for the one frame React.lazy always commits the fallback —
 * even with the chunk preloaded — producing the reported "expand then snap back"
 * jump. The fallback MUST stay layout-neutral (null).
 *
 * The wrappers use no hooks, so we call each directly to execute (and thus cover)
 * the changed return and assert the exact fix — the outer element is a Suspense
 * whose fallback is null. No render/lazy-resolution needed (jsdom has no layout
 * engine to show the jump anyway).
 */
describe('lazy picker Suspense fallback is layout-neutral (#2071)', () => {
  it('EmojiPicker wrapper returns a Suspense with a null (layout-neutral) fallback', () => {
    const el = LazyEmojiPicker({
      mode: 'popover',
      position: { x: 0, y: 0 },
      onSelect: vi.fn(),
      onClose: vi.fn(),
    }) as ReactElement;
    expect(el.type).toBe(Suspense);
    expect(el.props.fallback).toBeNull();
  });

  it('GifPicker wrapper returns a Suspense with a null (layout-neutral) fallback', () => {
    const el = LazyGifPicker({
      position: { x: 0, y: 0, anchorCenterX: 0 },
      onSelect: vi.fn(),
      onClose: vi.fn(),
    }) as ReactElement;
    expect(el.type).toBe(Suspense);
    expect(el.props.fallback).toBeNull();
  });

  // Source regression lock (belt-and-suspenders): fails fast if either fallback
  // reverts to an in-flow div — the same source-assertion pattern used by the
  // csp and design-tokens tests.
  const WRAPPERS = [
    '../../../../src/renderer/components/EmojiPicker/LazyEmojiPicker.tsx',
    '../../../../src/renderer/components/GifPicker/LazyGifPicker.tsx',
  ];
  it.each(WRAPPERS)('%s uses fallback={null}, never an in-flow loading box', (rel) => {
    const src = readFileSync(resolve(__dirname, rel), 'utf8');
    expect(src).toContain('fallback={null}');
    expect(src).not.toMatch(/fallback=\{<div/);
  });
});
