import { Suspense, lazy } from 'react';

const importGifPicker = () => import('./GifPicker');
const GifPickerLazy = lazy(importGifPicker);

/**
 * Warm the GifPicker chunk ahead of its first open. Called on composer mount
 * (#2071) so the first open doesn't incur a chunk download/parse reflow of the
 * composer region — the mechanism behind the one-off "expand then snap back"
 * jump. Idempotent: the module cache makes repeat calls no-ops.
 */
export function preloadGifPicker(): void {
  // Best-effort warm-up; swallow chunk-load errors so they don't surface as
  // unhandled rejections. The real open path still handles loading via
  // React.lazy/Suspense.
  importGifPicker().catch(() => {});
}

interface LazyGifPickerProps {
  onSelect: (gifSlug: string) => void;
  onClose: () => void;
  position: { x: number; y: number; anchorCenterX: number };
}

const LazyGifPicker: React.FC<LazyGifPickerProps> = (props) => (
  // ponytail: fallback MUST be null. The picker is a position:fixed popover, but
  // the Suspense fallback renders in the composer's normal flow — a sized
  // `.gif-picker-loading` box (height:160px) there inflates the message area for
  // the one frame React.lazy always commits the fallback (even with the chunk
  // preloaded), causing the reported expand-then-snap jump (#2071).
  <Suspense fallback={null}>
    <GifPickerLazy {...props} />
  </Suspense>
);

export default LazyGifPicker;
