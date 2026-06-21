import { renderHook, act } from '@testing-library/react';
import { useImageUpload } from '@/renderer/hooks/useImageUpload';
import { API_BASE } from '@/renderer/config';
import { vi } from 'vitest';

describe('useImageUpload', () => {
  const defaultOptions = {
    maxSize: 1024 * 1024,
    allowedTypes: ['image/png', 'image/jpeg', 'image/gif', 'image/webp'],
    onError: vi.fn(),
  };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('initializes with null values when no initialUrl', () => {
    const { result } = renderHook(() => useImageUpload(defaultOptions));
    expect(result.current.preview).toBeNull();
    expect(result.current.imageUrl).toBeNull();
    expect(result.current.removed).toBe(false);
    expect(result.current.pendingFile).toBeNull();
    expect(result.current.showCrop).toBe(false);
  });

  it('initializes with initialUrl when provided', () => {
    const { result } = renderHook(() =>
      useImageUpload({ ...defaultOptions, initialUrl: '/api/v1/media/avatars/123' })
    );
    // #1586: preview is absolutized for display; imageUrl stays the raw wire value.
    expect(result.current.preview).toBe(`${API_BASE}/api/v1/media/avatars/123`);
    expect(result.current.imageUrl).toBe('/api/v1/media/avatars/123');
  });

  it('passes through a data: initialUrl unchanged for preview and wire value (#1586)', () => {
    const dataUrl = 'data:image/png;base64,AAAA';
    const { result } = renderHook(() =>
      useImageUpload({ ...defaultOptions, initialUrl: dataUrl })
    );
    expect(result.current.preview).toBe(dataUrl);
    expect(result.current.imageUrl).toBe(dataUrl);
  });

  describe('handleChange', () => {
    it('rejects files with invalid type', () => {
      const { result } = renderHook(() => useImageUpload(defaultOptions));
      const file = new File(['data'], 'test.txt', { type: 'text/plain' });
      const event = {
        target: { files: [file] },
      } as unknown as React.ChangeEvent<HTMLInputElement>;

      act(() => result.current.handleChange(event));

      expect(defaultOptions.onError).toHaveBeenCalledWith(
        'Only PNG, JPEG, GIF, and WebP images are allowed'
      );
      expect(result.current.showCrop).toBe(false);
    });

    it('rejects files exceeding max size', () => {
      const { result } = renderHook(() => useImageUpload(defaultOptions));
      const largeFile = new File([new ArrayBuffer(2 * 1024 * 1024)], 'big.png', {
        type: 'image/png',
      });
      Object.defineProperty(largeFile, 'size', { value: 2 * 1024 * 1024 });
      const event = {
        target: { files: [largeFile] },
      } as unknown as React.ChangeEvent<HTMLInputElement>;

      act(() => result.current.handleChange(event));

      expect(defaultOptions.onError).toHaveBeenCalledWith('Image must be smaller than 1MB');
      expect(result.current.showCrop).toBe(false);
    });

    it('accepts valid files and opens crop editor', () => {
      const { result } = renderHook(() => useImageUpload(defaultOptions));
      const file = new File(['data'], 'photo.png', { type: 'image/png' });
      const event = {
        target: { files: [file] },
      } as unknown as React.ChangeEvent<HTMLInputElement>;

      act(() => result.current.handleChange(event));

      expect(defaultOptions.onError).toHaveBeenCalledWith(undefined);
      expect(result.current.pendingFile).toBe(file);
      expect(result.current.showCrop).toBe(true);
    });

    it('does nothing when no file selected', () => {
      const { result } = renderHook(() => useImageUpload(defaultOptions));
      const event = {
        target: { files: [] },
      } as unknown as React.ChangeEvent<HTMLInputElement>;

      act(() => result.current.handleChange(event));

      expect(defaultOptions.onError).not.toHaveBeenCalled();
      expect(result.current.showCrop).toBe(false);
    });
  });

  describe('handleCropConfirm', () => {
    it('sets preview and imageUrl, clears pending state', () => {
      const { result } = renderHook(() => useImageUpload(defaultOptions));

      act(() => result.current.handleCropConfirm('/api/v1/media/avatars/456'));

      expect(result.current.preview).toBe(`${API_BASE}/api/v1/media/avatars/456`);
      expect(result.current.imageUrl).toBe('/api/v1/media/avatars/456');
      expect(result.current.removed).toBe(false);
      expect(result.current.pendingFile).toBeNull();
      expect(result.current.showCrop).toBe(false);
    });
  });

  describe('handleCropCancel', () => {
    it('clears pending state without changing preview', () => {
      const { result } = renderHook(() =>
        useImageUpload({ ...defaultOptions, initialUrl: '/existing.png' })
      );

      // Open crop
      const file = new File(['data'], 'photo.png', { type: 'image/png' });
      act(() =>
        result.current.handleChange({
          target: { files: [file] },
        } as unknown as React.ChangeEvent<HTMLInputElement>)
      );
      expect(result.current.showCrop).toBe(true);

      // Cancel
      act(() => result.current.handleCropCancel());

      expect(result.current.showCrop).toBe(false);
      expect(result.current.pendingFile).toBeNull();
      expect(result.current.preview).toBe(`${API_BASE}/existing.png`);
    });
  });

  describe('handleRemove', () => {
    it('clears preview and imageUrl, sets removed flag', () => {
      const { result } = renderHook(() =>
        useImageUpload({ ...defaultOptions, initialUrl: '/existing.png' })
      );

      act(() => result.current.handleRemove());

      expect(result.current.preview).toBeNull();
      expect(result.current.imageUrl).toBeNull();
      expect(result.current.removed).toBe(true);
    });
  });

  describe('reset', () => {
    it('resets all state to new initial URL', () => {
      const { result } = renderHook(() => useImageUpload(defaultOptions));

      // Modify state
      act(() => result.current.handleCropConfirm('/uploaded.png'));
      expect(result.current.imageUrl).toBe('/uploaded.png');

      // Reset
      act(() => result.current.reset('/new-initial.png'));

      expect(result.current.preview).toBe(`${API_BASE}/new-initial.png`);
      expect(result.current.imageUrl).toBe('/new-initial.png');
      expect(result.current.removed).toBe(false);
      expect(result.current.pendingFile).toBeNull();
      expect(result.current.showCrop).toBe(false);
    });

    it('resets to null when called without argument', () => {
      const { result } = renderHook(() =>
        useImageUpload({ ...defaultOptions, initialUrl: '/existing.png' })
      );

      act(() => result.current.reset());

      expect(result.current.preview).toBeNull();
      expect(result.current.imageUrl).toBeNull();
    });
  });

  describe('handleKeyDown', () => {
    it('triggers click on Enter key', () => {
      const { result } = renderHook(() => useImageUpload(defaultOptions));
      const preventDefaultMock = vi.fn();
      const event = {
        key: 'Enter',
        preventDefault: preventDefaultMock,
      } as unknown as React.KeyboardEvent;

      act(() => result.current.handleKeyDown(event));

      expect(preventDefaultMock).toHaveBeenCalled();
    });

    it('triggers click on Space key', () => {
      const { result } = renderHook(() => useImageUpload(defaultOptions));
      const preventDefaultMock = vi.fn();
      const event = {
        key: ' ',
        preventDefault: preventDefaultMock,
      } as unknown as React.KeyboardEvent;

      act(() => result.current.handleKeyDown(event));

      expect(preventDefaultMock).toHaveBeenCalled();
    });

    it('does nothing on other keys', () => {
      const { result } = renderHook(() => useImageUpload(defaultOptions));
      const preventDefaultMock = vi.fn();
      const event = {
        key: 'Tab',
        preventDefault: preventDefaultMock,
      } as unknown as React.KeyboardEvent;

      act(() => result.current.handleKeyDown(event));

      expect(preventDefaultMock).not.toHaveBeenCalled();
    });
  });

  it('shows 2MB in error message for larger size limits', () => {
    const onError = vi.fn();
    const { result } = renderHook(() =>
      useImageUpload({
        ...defaultOptions,
        maxSize: 2 * 1024 * 1024,
        onError,
      })
    );

    const largeFile = new File(['data'], 'big.png', { type: 'image/png' });
    Object.defineProperty(largeFile, 'size', { value: 3 * 1024 * 1024 });

    act(() =>
      result.current.handleChange({
        target: { files: [largeFile] },
      } as unknown as React.ChangeEvent<HTMLInputElement>)
    );

    expect(onError).toHaveBeenCalledWith('Image must be smaller than 2MB');
  });

  it('provides a stable fileInputRef', () => {
    const { result, rerender } = renderHook(() => useImageUpload(defaultOptions));
    const ref1 = result.current.fileInputRef;
    rerender();
    expect(result.current.fileInputRef).toBe(ref1);
  });
});
