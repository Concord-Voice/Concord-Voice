import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';

vi.mock('@/renderer/stores/voiceStore', () => ({
  useVoiceStore: Object.assign(vi.fn(), {
    getState: vi.fn(() => ({ videoDeviceId: 'camera-1' })),
  }),
}));

import { useCameraTest } from '@/renderer/hooks/useCameraTest';
import { useVoiceStore } from '@/renderer/stores/voiceStore';

const mockTrackStop = vi.fn();
let mockGetUserMedia: ReturnType<typeof vi.fn>;

beforeEach(() => {
  vi.clearAllMocks();
  mockGetUserMedia = vi.fn().mockResolvedValue({
    getTracks: () => [{ stop: mockTrackStop }],
  });
  Object.defineProperty(navigator, 'mediaDevices', {
    value: { getUserMedia: mockGetUserMedia },
    configurable: true,
    writable: true,
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('useCameraTest', () => {
  it('returns correct initial state', () => {
    const { result } = renderHook(() => useCameraTest());
    expect(result.current.isTesting).toBe(false);
    expect(result.current.error).toBeNull();
    expect(result.current.stream).toBeNull();
  });

  it('starts camera preview on toggle', async () => {
    const { result } = renderHook(() => useCameraTest());
    await act(async () => {
      await result.current.toggleTest();
    });
    expect(mockGetUserMedia).toHaveBeenCalledWith({
      video: { deviceId: { exact: 'camera-1' } },
    });
    expect(result.current.isTesting).toBe(true);
    expect(result.current.stream).not.toBeNull();
  });

  it('uses video: true when no device selected', async () => {
    (useVoiceStore as any).getState.mockReturnValueOnce({ videoDeviceId: null });
    const { result } = renderHook(() => useCameraTest());
    await act(async () => {
      await result.current.toggleTest();
    });
    expect(mockGetUserMedia).toHaveBeenCalledWith({ video: true });
  });

  it('stops preview on second toggle', async () => {
    const { result } = renderHook(() => useCameraTest());
    await act(async () => {
      await result.current.toggleTest();
    });
    await act(async () => {
      await result.current.toggleTest();
    });
    expect(result.current.isTesting).toBe(false);
    expect(result.current.stream).toBeNull();
    expect(mockTrackStop).toHaveBeenCalled();
  });

  it('stopTest cleans up tracks', async () => {
    const { result } = renderHook(() => useCameraTest());
    await act(async () => {
      await result.current.toggleTest();
    });
    act(() => {
      result.current.stopTest();
    });
    expect(mockTrackStop).toHaveBeenCalled();
    expect(result.current.isTesting).toBe(false);
  });

  it('sets denied error for NotAllowedError', async () => {
    mockGetUserMedia.mockRejectedValueOnce(new DOMException('blocked', 'NotAllowedError'));
    const { result } = renderHook(() => useCameraTest());
    await act(async () => {
      await result.current.toggleTest();
    });
    expect(result.current.error).toBe('Camera access denied');
    expect(result.current.isTesting).toBe(false);
  });

  it('sets generic error for other failures', async () => {
    mockGetUserMedia.mockRejectedValueOnce(new Error('No device'));
    const { result } = renderHook(() => useCameraTest());
    await act(async () => {
      await result.current.toggleTest();
    });
    expect(result.current.error).toBe('Failed to access camera');
  });

  it('stops tracks on unmount', async () => {
    const { result, unmount } = renderHook(() => useCameraTest());
    await act(async () => {
      await result.current.toggleTest();
    });
    unmount();
    expect(mockTrackStop).toHaveBeenCalled();
  });
});
