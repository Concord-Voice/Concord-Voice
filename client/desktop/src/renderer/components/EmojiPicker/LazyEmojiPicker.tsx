import { Suspense, lazy } from 'react';
import type { EmojiPickerProps } from './types';

const EmojiPickerLazy = lazy(() => import('./EmojiPicker'));

/**
 * Lazy-loaded EmojiPicker wrapper. The picker is always rendered conditionally
 * (behind showEmojiPicker state), so the chunk loads on first open.
 */
const LazyEmojiPicker: React.FC<EmojiPickerProps> = (props) => (
  <Suspense fallback={<div className="emoji-picker-loading" />}>
    <EmojiPickerLazy {...props} />
  </Suspense>
);

export default LazyEmojiPicker;
