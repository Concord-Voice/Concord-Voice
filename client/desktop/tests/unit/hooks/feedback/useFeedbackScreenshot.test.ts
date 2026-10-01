import { describe, it, expect, vi, beforeEach } from 'vitest';
import { renderHook, act, waitFor } from '@testing-library/react';
import { useFeedbackScreenshot } from '@/renderer/hooks/feedback/useFeedbackScreenshot';

// ── Mocks ──────────────────────────────────────────────────────────────────
// Mirrors the mocking pattern in tests/unit/hooks/useFileUpload.test.ts.
const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (...args: unknown[]) => mockSafeJson(...args),
}));

function makeFile(name: string, type: string, size: number): File {
  return new File([new Uint8Array(size)], name, { type });
}

const DEFAULT_OPTS = {
  maxCount: 4,
  maxSize: 5 * 1024 * 1024,
  allowedTypes: ['image/png', 'image/jpeg', 'image/webp'],
};

describe('useFeedbackScreenshot (#1747)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('uploads a valid image, marks it done with the server url, and surfaces it via uploadedRefs', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 200 });
    mockSafeJson.mockResolvedValue({ url: '/api/v1/media/feedback-screenshots/abc' });
    const onError = vi.fn();
    const { result } = renderHook(() => useFeedbackScreenshot({ ...DEFAULT_OPTS, onError }));

    act(() => {
      result.current.addFiles([makeFile('a.png', 'image/png', 1000)]);
    });

    // Optimistic insert is synchronous.
    expect(result.current.screenshots).toHaveLength(1);
    expect(result.current.screenshots[0].status).toBe('uploading');

    await waitFor(() => expect(result.current.screenshots[0].status).toBe('done'));

    expect(result.current.screenshots[0].url).toBe('/api/v1/media/feedback-screenshots/abc');
    expect(result.current.uploadedRefs()).toEqual([
      { url: '/api/v1/media/feedback-screenshots/abc' },
    ]);
    expect(onError).not.toHaveBeenCalled();

    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/media/upload/feedback-screenshot',
      expect.objectContaining({ method: 'POST' })
    );
    const body = mockApiFetch.mock.calls[0][1].body as FormData;
    const sent = body.get('file') as File;
    expect(sent).toBeInstanceOf(File);
    expect(sent.name).toBe('a.png');
  });

  it('rejects an over-size file, calls onError, and never uploads it', () => {
    const onError = vi.fn();
    const { result } = renderHook(() => useFeedbackScreenshot({ ...DEFAULT_OPTS, onError }));

    act(() => {
      result.current.addFiles([makeFile('big.png', 'image/png', 6 * 1024 * 1024)]);
    });

    expect(onError).toHaveBeenCalledWith(expect.stringMatching(/under 5 MB/));
    expect(result.current.screenshots).toHaveLength(0);
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('rejects a disallowed file type and calls onError', () => {
    const onError = vi.fn();
    const { result } = renderHook(() => useFeedbackScreenshot({ ...DEFAULT_OPTS, onError }));

    act(() => {
      result.current.addFiles([makeFile('doc.pdf', 'application/pdf', 1000)]);
    });

    expect(onError).toHaveBeenCalledWith(expect.stringMatching(/PNG, JPEG, and WebP/));
    expect(result.current.screenshots).toHaveLength(0);
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('rejects files beyond maxCount and calls onError', () => {
    mockApiFetch.mockReturnValue(new Promise(() => {})); // never resolves — irrelevant here
    const onError = vi.fn();
    const { result } = renderHook(() =>
      useFeedbackScreenshot({ ...DEFAULT_OPTS, maxCount: 2, onError })
    );

    act(() => {
      result.current.addFiles([
        makeFile('a.png', 'image/png', 1000),
        makeFile('b.png', 'image/png', 1000),
        makeFile('c.png', 'image/png', 1000),
      ]);
    });

    expect(result.current.screenshots).toHaveLength(2);
    expect(onError).toHaveBeenCalledWith(expect.stringMatching(/up to 2 screenshots/));
  });

  it('marks a failed upload (non-ok response) as error, calls onError, and excludes it from uploadedRefs', async () => {
    mockApiFetch.mockResolvedValue({ ok: false, status: 500 });
    const onError = vi.fn();
    const { result } = renderHook(() => useFeedbackScreenshot({ ...DEFAULT_OPTS, onError }));

    act(() => {
      result.current.addFiles([makeFile('a.png', 'image/png', 1000)]);
    });

    await waitFor(() => expect(result.current.screenshots[0].status).toBe('error'));
    expect(onError).toHaveBeenCalledWith(expect.stringMatching(/failed to upload/i));
    expect(result.current.uploadedRefs()).toEqual([]);
  });

  it('marks upload as error when apiFetch rejects outright (network failure)', async () => {
    mockApiFetch.mockRejectedValue(new Error('network down'));
    const onError = vi.fn();
    const { result } = renderHook(() => useFeedbackScreenshot({ ...DEFAULT_OPTS, onError }));

    act(() => {
      result.current.addFiles([makeFile('a.png', 'image/png', 1000)]);
    });

    await waitFor(() => expect(result.current.screenshots[0].status).toBe('error'));
    expect(onError).toHaveBeenCalledWith(expect.stringMatching(/failed to upload/i));
    expect(result.current.uploadedRefs()).toEqual([]);
  });

  it('marks upload as error when the response is ok but carries no url', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 200 });
    mockSafeJson.mockResolvedValue({});
    const onError = vi.fn();
    const { result } = renderHook(() => useFeedbackScreenshot({ ...DEFAULT_OPTS, onError }));

    act(() => {
      result.current.addFiles([makeFile('a.png', 'image/png', 1000)]);
    });

    await waitFor(() => expect(result.current.screenshots[0].status).toBe('error'));
    expect(result.current.uploadedRefs()).toEqual([]);
  });

  it('remove() drops the item', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 200 });
    mockSafeJson.mockResolvedValue({ url: '/api/v1/media/feedback-screenshots/abc' });
    const { result } = renderHook(() => useFeedbackScreenshot(DEFAULT_OPTS));

    act(() => {
      result.current.addFiles([makeFile('a.png', 'image/png', 1000)]);
    });
    await waitFor(() => expect(result.current.screenshots[0].status).toBe('done'));
    const id = result.current.screenshots[0].id;

    act(() => {
      result.current.remove(id);
    });

    expect(result.current.screenshots).toHaveLength(0);
    expect(result.current.uploadedRefs()).toEqual([]);
  });

  it('clear() drops every item', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 200 });
    mockSafeJson.mockResolvedValue({ url: '/api/v1/media/feedback-screenshots/abc' });
    const { result } = renderHook(() => useFeedbackScreenshot(DEFAULT_OPTS));

    act(() => {
      result.current.addFiles([
        makeFile('a.png', 'image/png', 1000),
        makeFile('b.png', 'image/png', 1000),
      ]);
    });
    await waitFor(() => expect(result.current.screenshots).toHaveLength(2));

    act(() => {
      result.current.clear();
    });

    expect(result.current.screenshots).toHaveLength(0);
  });

  it('remove() revokes the removed item’s object URL', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 200 });
    mockSafeJson.mockResolvedValue({ url: '/api/v1/media/feedback-screenshots/abc' });
    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
    const { result } = renderHook(() => useFeedbackScreenshot(DEFAULT_OPTS));

    act(() => {
      result.current.addFiles([makeFile('a.png', 'image/png', 1000)]);
    });
    await waitFor(() => expect(result.current.screenshots[0].status).toBe('done'));
    const { id, previewUrl } = result.current.screenshots[0];

    act(() => {
      result.current.remove(id);
    });

    expect(revokeSpy).toHaveBeenCalledWith(previewUrl);
    revokeSpy.mockRestore();
  });

  it('clear() revokes every item’s object URL', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 200 });
    mockSafeJson.mockResolvedValue({ url: '/api/v1/media/feedback-screenshots/abc' });
    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
    const { result } = renderHook(() => useFeedbackScreenshot(DEFAULT_OPTS));

    act(() => {
      result.current.addFiles([
        makeFile('a.png', 'image/png', 1000),
        makeFile('b.png', 'image/png', 1000),
      ]);
    });
    await waitFor(() => expect(result.current.screenshots).toHaveLength(2));
    const previewUrls = result.current.screenshots.map((s) => s.previewUrl);

    act(() => {
      result.current.clear();
    });

    expect(revokeSpy).toHaveBeenCalledTimes(previewUrls.length);
    previewUrls.forEach((url) => expect(revokeSpy).toHaveBeenCalledWith(url));
    revokeSpy.mockRestore();
  });

  it('revokes the outstanding object URL on unmount', async () => {
    mockApiFetch.mockReturnValue(new Promise(() => {})); // stays in flight — irrelevant here
    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
    const { result, unmount } = renderHook(() => useFeedbackScreenshot(DEFAULT_OPTS));

    act(() => {
      result.current.addFiles([makeFile('a.png', 'image/png', 1000)]);
    });
    const { previewUrl } = result.current.screenshots[0];

    unmount();

    expect(revokeSpy).toHaveBeenCalledWith(previewUrl);
    revokeSpy.mockRestore();
  });

  it('atCapacity is true once screenshots reach maxCount', () => {
    mockApiFetch.mockReturnValue(new Promise(() => {}));
    const { result } = renderHook(() => useFeedbackScreenshot({ ...DEFAULT_OPTS, maxCount: 1 }));

    expect(result.current.atCapacity).toBe(false);

    act(() => {
      result.current.addFiles([makeFile('a.png', 'image/png', 1000)]);
    });

    expect(result.current.atCapacity).toBe(true);
  });

  it('isUploading is true while an upload is in flight and false once it resolves', async () => {
    let resolveFetch!: (v: { ok: boolean; status: number }) => void;
    mockApiFetch.mockReturnValue(
      new Promise((resolve) => {
        resolveFetch = resolve;
      })
    );
    mockSafeJson.mockResolvedValue({ url: '/api/v1/media/feedback-screenshots/abc' });
    const { result } = renderHook(() => useFeedbackScreenshot(DEFAULT_OPTS));

    act(() => {
      result.current.addFiles([makeFile('a.png', 'image/png', 1000)]);
    });

    expect(result.current.isUploading).toBe(true);

    await act(async () => {
      resolveFetch({ ok: true, status: 200 });
    });

    await waitFor(() => expect(result.current.isUploading).toBe(false));
  });
});
