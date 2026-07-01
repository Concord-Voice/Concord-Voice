import { renderHook, act } from '@testing-library/react';
import { vi } from 'vitest';
import { useImageUpload } from '@/renderer/hooks/useImageUpload';
import { FREE_ENTITLEMENT } from '@/renderer/stores/subscriptionStore';
import {
  MACH_ICON_SIZE,
  MACH_BANNER_SIZE,
  maxServerIconSizeForTier,
  maxServerBannerSizeForTier,
} from '@/renderer/components/Servers/serverConstants';

const allowedTypes = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
const mib = 1024 * 1024;

function uploadImageWithLimit(size: number, maxSize: number) {
  const onError = vi.fn();
  const { result } = renderHook(() =>
    useImageUpload({
      maxSize,
      allowedTypes,
      onError,
    })
  );
  const file = new File(['image'], 'image.png', { type: 'image/png' });
  Object.defineProperty(file, 'size', { value: size });

  act(() => {
    result.current.handleChange({
      target: { files: [file] },
    } as unknown as React.ChangeEvent<HTMLInputElement>);
  });

  return { result, onError };
}

describe('image upload limit resolution', () => {
  it('allows premium profile image sizes through the real upload hook', () => {
    const premiumAvatarBytes = 8 * mib;
    const { result, onError } = uploadImageWithLimit(7 * mib, premiumAvatarBytes);

    expect(onError).toHaveBeenCalledWith(undefined);
    expect(result.current.showCrop).toBe(true);
  });

  it('keeps free profile uploads capped at the free entitlement', () => {
    const { result, onError } = uploadImageWithLimit(6 * mib, FREE_ENTITLEMENT.maxAvatarBytes);

    expect(onError).toHaveBeenCalledWith('Image must be smaller than 5MB');
    expect(result.current.showCrop).toBe(false);
  });

  it('resolves Mach server image limits to 8 MiB for the upload hook', () => {
    expect(maxServerIconSizeForTier('mach')).toBe(MACH_ICON_SIZE);
    expect(maxServerBannerSizeForTier('mach')).toBe(MACH_BANNER_SIZE);

    const { result, onError } = uploadImageWithLimit(7 * mib, maxServerIconSizeForTier('mach'));

    expect(onError).toHaveBeenCalledWith(undefined);
    expect(result.current.showCrop).toBe(true);
  });

  it('falls back to Groundspeed server image limits for stale or free server cache entries', () => {
    const maxIconBytes = maxServerIconSizeForTier(undefined);
    const maxBannerBytes = maxServerBannerSizeForTier('groundspeed');

    expect(maxIconBytes).toBe(5 * mib);
    expect(maxBannerBytes).toBe(5 * mib);

    const { result, onError } = uploadImageWithLimit(6 * mib, maxIconBytes);

    expect(onError).toHaveBeenCalledWith('Image must be smaller than 5MB');
    expect(result.current.showCrop).toBe(false);
  });
});
