import { Suspense, lazy } from 'react';
import type { EmojiPickerProps } from './types';

const importEmojiPicker = () => import('./EmojiPicker');
const EmojiPickerLazy = lazy(importEmojiPicker);

/**
 * Warm the EmojiPicker chunk ahead of its first open. Called on composer mount
 * (#2071) so the first open doesn't incur a chunk download/parse reflow of the
 * composer region — the mechanism behind the one-off "expand then snap back"
 * jump. Idempotent: the module cache makes repeat calls no-ops.
 */
export function preloadEmojiPicker(): void {
  // Best-effort warm-up; swallow chunk-load errors so they don't surface as
  // unhandled rejections. The real open path still handles loading via
  // React.lazy/Suspense.
  importEmojiPicker().catch(() => {});
}

/**
 * Lazy-loaded EmojiPicker wrapper. The picker is always rendered conditionally
 * (behind showEmojiPicker state), so the chunk loads on first open.
 */
const LazyEmojiPicker: React.FC<EmojiPickerProps> = (props) => (
  // ponytail: fallback MUST be null. The picker is a position:fixed popover, but
  // the Suspense fallback renders in the composer's normal flow — a sized
  // `.emoji-picker-loading` box (height:280px) there inflates the message area
  // for the one frame React.lazy always commits the fallback (even with the
  // chunk preloaded), causing the reported expand-then-snap jump (#2071).
  <Suspense fallback={null}>
    <EmojiPickerLazy {...props} />
  </Suspense>
);

export default LazyEmojiPicker;
