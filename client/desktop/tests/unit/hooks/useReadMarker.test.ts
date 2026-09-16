import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useReadMarker } from '@/renderer/hooks/messaging/useReadMarker';

describe('useReadMarker', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('does not post immediately when markSeen is called', () => {
    const post = vi.fn().mockResolvedValue(undefined);
    const { result } = renderHook(() => useReadMarker(post, 'k'));

    act(() => {
      result.current.markSeen();
    });

    expect(post).not.toHaveBeenCalled();
  });

  it('posts once the delay elapses', async () => {
    const post = vi.fn().mockResolvedValue(undefined);
    const { result } = renderHook(() => useReadMarker(post, 'k', 3000));

    act(() => {
      result.current.markSeen();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    expect(post).toHaveBeenCalledTimes(1);
  });

  it('coalesces calls inside the window into a single post', async () => {
    const post = vi.fn().mockResolvedValue(undefined);
    const { result } = renderHook(() => useReadMarker(post, 'k', 3000));

    act(() => {
      result.current.markSeen();
    });
    act(() => {
      vi.advanceTimersByTime(1000);
      result.current.markSeen();
    });
    act(() => {
      vi.advanceTimersByTime(1000);
      result.current.markSeen();
    });
    // Each call restarts the window; only 1s of quiet has elapsed since the
    // last call, so nothing has posted yet.
    expect(post).not.toHaveBeenCalled();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    expect(post).toHaveBeenCalledTimes(1);
  });

  it('flushes a pending post immediately on unmount', () => {
    const post = vi.fn().mockResolvedValue(undefined);
    const { result, unmount } = renderHook(() => useReadMarker(post, 'k', 3000));

    act(() => {
      result.current.markSeen();
    });
    expect(post).not.toHaveBeenCalled();

    unmount();

    expect(post).toHaveBeenCalledTimes(1);
  });

  it('does not double-post when nothing is pending at unmount', async () => {
    const post = vi.fn().mockResolvedValue(undefined);
    const { result, unmount } = renderHook(() => useReadMarker(post, 'k', 3000));

    act(() => {
      result.current.markSeen();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });
    expect(post).toHaveBeenCalledTimes(1);

    unmount();

    expect(post).toHaveBeenCalledTimes(1);
  });

  it('swallows a rejected post without throwing', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const post = vi.fn().mockRejectedValue(new Error('network down'));
    const { result } = renderHook(() => useReadMarker(post, 'k', 3000));

    act(() => {
      result.current.markSeen();
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    expect(post).toHaveBeenCalledTimes(1);
    expect(consoleSpy).toHaveBeenCalledWith(
      '[useReadMarker] Failed to post read marker:',
      'network down'
    );
    consoleSpy.mockRestore();
  });

  it("flushes the previous thread's pending post when the key changes, with that thread's post", () => {
    const posts = {
      a: vi.fn().mockResolvedValue(undefined),
      b: vi.fn().mockResolvedValue(undefined),
    };
    const { result, rerender } = renderHook(
      ({ k }: { k: 'a' | 'b' }) => useReadMarker(() => posts[k](), k),
      { initialProps: { k: 'a' as 'a' | 'b' } }
    );

    act(() => {
      result.current.markSeen();
    });
    rerender({ k: 'b' });

    expect(posts.a).toHaveBeenCalledTimes(1);
    expect(posts.b).not.toHaveBeenCalled();
    act(() => {
      vi.advanceTimersByTime(3000);
    });
    expect(posts.b).not.toHaveBeenCalled(); // nothing was marked seen on b
  });

  it('keeps one debounce window when the post identity changes every render', async () => {
    const post = vi.fn().mockResolvedValue(undefined);
    const { result, rerender } = renderHook(() => useReadMarker(() => post(), 'k', 3000));

    act(() => {
      result.current.markSeen();
    });
    rerender(); // a new inline post; same key
    rerender();
    expect(post).not.toHaveBeenCalled();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });
    expect(post).toHaveBeenCalledTimes(1);
  });

  it('flush() posts the captured post immediately and disarms the timer', async () => {
    const post = vi.fn().mockResolvedValue(undefined);
    const { result } = renderHook(() => useReadMarker(post, 'k', 3000));
    act(() => {
      result.current.markSeen();
    });
    act(() => {
      result.current.flush();
    });
    expect(post).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });
    expect(post).toHaveBeenCalledTimes(1); // the timer did not fire a second time
  });

  it('flushes when the document becomes hidden', () => {
    const post = vi.fn().mockResolvedValue(undefined);
    const { result } = renderHook(() => useReadMarker(post, 'k', 3000));
    act(() => {
      result.current.markSeen();
    });
    Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true });
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
    expect(post).toHaveBeenCalledTimes(1);
  });

  it('flushes when the window loses focus', () => {
    const post = vi.fn().mockResolvedValue(undefined);
    const { result } = renderHook(() => useReadMarker(post, 'k', 3000));
    act(() => {
      result.current.markSeen();
    });
    act(() => {
      window.dispatchEvent(new Event('blur'));
    });
    expect(post).toHaveBeenCalledTimes(1);
  });

  it('logs a post that throws synchronously instead of throwing from the cleanup', () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { result, unmount } = renderHook(() =>
      useReadMarker(() => {
        throw new Error('sync boom');
      }, 'k')
    );
    act(() => {
      result.current.markSeen();
    });
    expect(() => unmount()).not.toThrow();
    expect(consoleSpy).toHaveBeenCalledWith(
      '[useReadMarker] Failed to post read marker:',
      'sync boom'
    );
    consoleSpy.mockRestore();
  });

  it('keeps the default window above the 2 s rate-limit floor', async () => {
    const post = vi.fn().mockResolvedValue(undefined);
    const { result } = renderHook(() => useReadMarker(post, 'k'));
    act(() => {
      result.current.markSeen();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(post).not.toHaveBeenCalled();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(post).toHaveBeenCalledTimes(1);
  });
});
