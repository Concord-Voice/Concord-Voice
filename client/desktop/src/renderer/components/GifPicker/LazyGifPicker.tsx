import { Suspense, lazy } from 'react';

const GifPickerLazy = lazy(() => import('./GifPicker'));

interface LazyGifPickerProps {
  onSelect: (gifSlug: string) => void;
  onClose: () => void;
  position: { x: number; y: number; anchorCenterX: number };
}

const LazyGifPicker: React.FC<LazyGifPickerProps> = (props) => (
  <Suspense fallback={<div className="gif-picker-loading" />}>
    <GifPickerLazy {...props} />
  </Suspense>
);

export default LazyGifPicker;
