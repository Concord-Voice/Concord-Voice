import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useFileUpload, validateFiles } from '@/renderer/hooks/useFileUpload';

// Mock apiClient
const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (...args: unknown[]) => mockSafeJson(...args),
}));

// Mock e2eeService
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return true;
    },
    getChannelKey: vi.fn().mockResolvedValue({} as CryptoKey),
    getCurrentKeyVersion: vi.fn().mockReturnValue(1),
  },
}));

// Mock attachmentCrypto
vi.mock('@/renderer/utils/attachmentCrypto', async () => {
  const actual = await vi.importActual<typeof import('@/renderer/utils/attachmentCrypto')>(
    '@/renderer/utils/attachmentCrypto'
  );
  return {
    ...actual,
    encryptFile: vi.fn().mockResolvedValue(new ArrayBuffer(100)),
  };
});

function createMockFile(name: string, size: number, type: string): File {
  const buffer = new ArrayBuffer(size);
  return new File([buffer], name, { type });
}

describe('validateFiles', () => {
  it('returns null for valid files', () => {
    const files = [createMockFile('test.png', 1000, 'image/png')];
    expect(validateFiles(files, 0)).toBeNull();
  });

  it('rejects files exceeding max size', () => {
    const files = [createMockFile('big.zip', 30 * 1024 * 1024, 'application/zip')];
    const error = validateFiles(files, 0);
    expect(error).toContain('exceeds');
  });

  it('rejects when total count exceeds max', () => {
    const files = [createMockFile('test.png', 100, 'image/png')];
    const error = validateFiles(files, 5);
    expect(error).toContain('Maximum 5');
  });

  it('rejects empty files', () => {
    const files = [createMockFile('empty.txt', 0, 'text/plain')];
    const error = validateFiles(files, 0);
    expect(error).toContain('empty');
  });

  it('allows multiple valid files within limits', () => {
    const files = [
      createMockFile('a.png', 1000, 'image/png'),
      createMockFile('b.jpg', 2000, 'image/jpeg'),
      createMockFile('c.pdf', 3000, 'application/pdf'),
    ];
    expect(validateFiles(files, 0)).toBeNull();
  });
});

describe('useFileUpload', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('starts with empty state', () => {
    const { result } = renderHook(() => useFileUpload());
    expect(result.current.files).toHaveLength(0);
    expect(result.current.isUploading).toBe(false);
    expect(result.current.hasFiles).toBe(false);
  });

  it('adds files to the queue', () => {
    const { result } = renderHook(() => useFileUpload());
    const file = createMockFile('test.png', 1000, 'image/png');

    act(() => {
      result.current.addFiles([file]);
    });

    expect(result.current.files).toHaveLength(1);
    expect(result.current.files[0].file.name).toBe('test.png');
    expect(result.current.files[0].status).toBe('pending');
    expect(result.current.hasFiles).toBe(true);
  });

  it('generates preview URL for images', () => {
    const { result } = renderHook(() => useFileUpload());
    const file = createMockFile('photo.png', 1000, 'image/png');

    act(() => {
      result.current.addFiles([file]);
    });

    expect(result.current.files[0].previewUrl).toBeDefined();
    expect(result.current.files[0].previewUrl).toContain('blob:');
  });

  it('does not generate preview URL for non-images', () => {
    const { result } = renderHook(() => useFileUpload());
    const file = createMockFile('doc.pdf', 1000, 'application/pdf');

    act(() => {
      result.current.addFiles([file]);
    });

    expect(result.current.files[0].previewUrl).toBeUndefined();
  });

  it('removes a file from the queue', () => {
    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([
        createMockFile('a.png', 100, 'image/png'),
        createMockFile('b.pdf', 200, 'application/pdf'),
      ]);
    });
    expect(result.current.files).toHaveLength(2);

    act(() => {
      result.current.removeFile(0);
    });
    expect(result.current.files).toHaveLength(1);
    expect(result.current.files[0].file.name).toBe('b.pdf');
  });

  it('clears all files', () => {
    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([
        createMockFile('a.png', 100, 'image/png'),
        createMockFile('b.pdf', 200, 'application/pdf'),
      ]);
    });
    expect(result.current.files).toHaveLength(2);

    act(() => {
      result.current.clearFiles();
    });
    expect(result.current.files).toHaveLength(0);
    expect(result.current.hasFiles).toBe(false);
  });

  it('returns validation error from addFiles', () => {
    const { result } = renderHook(() => useFileUpload());
    const bigFile = createMockFile('huge.zip', 30 * 1024 * 1024, 'application/zip');

    let error: string | null = null;
    act(() => {
      error = result.current.addFiles([bigFile]);
    });

    expect(error).toContain('exceeds');
    expect(result.current.files).toHaveLength(0);
  });

  it('uploads files and returns IDs', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({
      file_id: 'attach-uploaded-1',
      file_type: 'photo',
      file_size: 1000,
    });

    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([createMockFile('test.png', 1000, 'image/png')]);
    });

    let uploadResult: { ids: string[]; summaries: unknown[] } | undefined;
    await act(async () => {
      uploadResult = await result.current.uploadAll('channel-1');
    });

    expect(uploadResult?.ids).toContain('attach-uploaded-1');
    expect(uploadResult?.summaries).toHaveLength(1);
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/media/upload/attachment',
      expect.objectContaining({ method: 'POST' })
    );
  });

  it('handles upload errors gracefully', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      status: 500,
      text: () => Promise.resolve('Server error'),
    });

    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([createMockFile('test.png', 1000, 'image/png')]);
    });

    await act(async () => {
      await result.current.uploadAll('channel-1');
    });

    expect(result.current.files[0].status).toBe('error');
    expect(result.current.files[0].error).toBeDefined();
  });

  it('uploads with encryption (always)', async () => {
    const { encryptFile } = await import('@/renderer/utils/attachmentCrypto');
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({
      file_id: 'attach-enc-1',
      file_type: 'photo',
      file_size: 1000,
    });

    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([createMockFile('secret.png', 1000, 'image/png')]);
    });

    await act(async () => {
      await result.current.uploadAll('channel-1');
    });

    expect(encryptFile).toHaveBeenCalled();
    expect(result.current.files[0].status).toBe('done');
  });

  it('uploads with conversationId for DMs', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({
      file_id: 'attach-dm-1',
      file_type: 'photo',
      file_size: 1000,
    });

    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([createMockFile('dm.png', 1000, 'image/png')]);
    });

    await act(async () => {
      await result.current.uploadAll('channel-1', 'conv-123');
    });

    // Verify the FormData included conversation_id
    const callArgs = mockApiFetch.mock.calls[0];
    const body = callArgs[1].body as FormData;
    expect(body.get('conversation_id')).toBe('conv-123');
  });

  it('returns already-done files when no pending remain', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({
      file_id: 'attach-first',
      file_type: 'photo',
      file_size: 1000,
    });

    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([createMockFile('test.png', 1000, 'image/png')]);
    });

    // First upload
    await act(async () => {
      await result.current.uploadAll('channel-1');
    });

    // Second call — no pending files
    let secondResult: { ids: string[] } | undefined;
    await act(async () => {
      secondResult = await result.current.uploadAll('channel-1');
    });

    expect(secondResult?.ids).toContain('attach-first');
  });

  it('revokes preview URLs on clearFiles', () => {
    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([createMockFile('img.png', 1000, 'image/png')]);
    });

    expect(result.current.files[0].previewUrl).toBeDefined();

    act(() => {
      result.current.clearFiles();
    });

    expect(revokeSpy).toHaveBeenCalled();
    revokeSpy.mockRestore();
  });

  it('revokes preview URL on removeFile', () => {
    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([createMockFile('img.png', 1000, 'image/png')]);
    });

    act(() => {
      result.current.removeFile(0);
    });

    expect(revokeSpy).toHaveBeenCalled();
    revokeSpy.mockRestore();
  });
});

// ---------------------------------------------------------------------------
// Image-dimension hydration (used by ImageAttachment to reserve space and
// avoid layout shift on send). The hook reads naturalWidth/naturalHeight
// off-screen via an Image element after addFiles, then patches the matching
// FileUploadState entry. We stub HTMLImageElement to fire onload synchronously
// with controllable natural dims.
// ---------------------------------------------------------------------------
describe('useFileUpload — image dimension hydration', () => {
  let originalImage: typeof globalThis.Image;

  beforeEach(() => {
    vi.clearAllMocks();
    originalImage = globalThis.Image;
  });

  afterEach(() => {
    globalThis.Image = originalImage;
  });

  function stubImage(naturalWidth: number, naturalHeight: number, fail = false) {
    class FakeImage {
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      naturalWidth = 0;
      naturalHeight = 0;
      private _src = '';
      get src() {
        return this._src;
      }
      set src(v: string) {
        this._src = v;
        // jsdom doesn't actually decode the blob; fire load (or error) on
        // the next microtask so React state updates can flush.
        queueMicrotask(() => {
          if (fail) {
            this.onerror?.();
          } else {
            this.naturalWidth = naturalWidth;
            this.naturalHeight = naturalHeight;
            this.onload?.();
          }
        });
      }
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (globalThis as any).Image = FakeImage;
  }

  it('hydrates width/height on the FileUploadState entry for image files', async () => {
    stubImage(640, 480);
    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      result.current.addFiles([createMockFile('photo.png', 1000, 'image/png')]);
      // Let the queued microtask + setState flush
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(result.current.files[0].width).toBe(640);
    expect(result.current.files[0].height).toBe(480);
  });

  it('does not hydrate dimensions for non-image files', async () => {
    stubImage(100, 100);
    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      result.current.addFiles([createMockFile('doc.pdf', 1000, 'application/pdf')]);
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(result.current.files[0].width).toBeUndefined();
    expect(result.current.files[0].height).toBeUndefined();
  });

  it('leaves width/height undefined when image decoding fails', async () => {
    stubImage(0, 0, true);
    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      result.current.addFiles([createMockFile('broken.png', 1000, 'image/png')]);
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(result.current.files[0].width).toBeUndefined();
    expect(result.current.files[0].height).toBeUndefined();
  });

  it('includes hydrated dimensions in the upload summary', async () => {
    stubImage(800, 600);
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({
      file_id: 'attach-with-dims',
      file_type: 'photo',
      file_size: 1000,
    });

    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      result.current.addFiles([createMockFile('shot.jpg', 1000, 'image/jpeg')]);
      await Promise.resolve();
      await Promise.resolve();
    });

    let uploadResult:
      | { ids: string[]; summaries: { width?: number; height?: number }[] }
      | undefined;
    await act(async () => {
      uploadResult = (await result.current.uploadAll('channel-1')) as typeof uploadResult;
    });

    expect(uploadResult?.summaries[0].width).toBe(800);
    expect(uploadResult?.summaries[0].height).toBe(600);
  });
});

// ---------------------------------------------------------------------------
// uploadAll — additionalFiles paths (new code from round-2 extraction)
//
// These tests exercise the uploadAdditionalFiles + uploadPendingFiles helpers
// that were extracted to keep uploadAll's cognitive complexity ≤ 15.
// ---------------------------------------------------------------------------
describe('useFileUpload — uploadAll with additionalFiles', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({
      file_id: 'additional-file-1',
      file_type: 'file',
      file_size: 200,
    });
  });

  it('uploads additionalFiles when no pending files exist', async () => {
    const { result } = renderHook(() => useFileUpload());

    const overflowFile = createMockFile('overflow.md', 200, 'text/markdown');

    let uploadResult: { ids: string[]; summaries: unknown[] } | undefined;
    await act(async () => {
      uploadResult = await result.current.uploadAll('channel-1', undefined, [overflowFile]);
    });

    expect(uploadResult?.ids).toContain('additional-file-1');
    expect(uploadResult?.summaries).toHaveLength(1);
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/media/upload/attachment',
      expect.objectContaining({ method: 'POST' })
    );
  });

  it('uploads both pending files and additionalFiles when both are present', async () => {
    // Return different IDs for successive calls
    mockSafeJson
      .mockResolvedValueOnce({ file_id: 'pending-file-1', file_type: 'photo', file_size: 100 })
      .mockResolvedValueOnce({ file_id: 'additional-file-2', file_type: 'file', file_size: 200 });

    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([createMockFile('user.png', 100, 'image/png')]);
    });

    const overflowFile = createMockFile('overflow.md', 200, 'text/markdown');

    let uploadResult: { ids: string[] } | undefined;
    await act(async () => {
      uploadResult = await result.current.uploadAll('channel-1', undefined, [overflowFile]);
    });

    expect(uploadResult?.ids).toHaveLength(2);
    // Pending files come first (uploadPendingFiles), additionalFiles second (uploadAdditionalFiles)
    expect(uploadResult?.ids[0]).toBe('pending-file-1');
    expect(uploadResult?.ids[1]).toBe('additional-file-2');
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });

  it('returns empty result when no pending files and no additionalFiles', async () => {
    const { result } = renderHook(() => useFileUpload());

    let uploadResult: { ids: string[] } | undefined;
    await act(async () => {
      uploadResult = await result.current.uploadAll('channel-1', undefined, []);
    });

    expect(uploadResult?.ids).toHaveLength(0);
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('uploads multiple additionalFiles in order', async () => {
    mockSafeJson
      .mockResolvedValueOnce({ file_id: 'add-id-0', file_type: 'file', file_size: 100 })
      .mockResolvedValueOnce({ file_id: 'add-id-1', file_type: 'file', file_size: 100 })
      .mockResolvedValueOnce({ file_id: 'add-id-2', file_type: 'file', file_size: 100 });

    const { result } = renderHook(() => useFileUpload());

    const files = [
      createMockFile('a.md', 100, 'text/markdown'),
      createMockFile('b.md', 100, 'text/markdown'),
      createMockFile('c.md', 100, 'text/markdown'),
    ];

    let uploadResult: { ids: string[] } | undefined;
    await act(async () => {
      uploadResult = await result.current.uploadAll('channel-1', undefined, files);
    });

    expect(uploadResult?.ids).toEqual(['add-id-0', 'add-id-1', 'add-id-2']);
    expect(mockApiFetch).toHaveBeenCalledTimes(3);
  });

  it('propagates error when apiFetch rejects on an additionalFile upload', async () => {
    // First call succeeds, second throws
    mockApiFetch
      .mockResolvedValueOnce({ ok: true, status: 201 })
      .mockRejectedValueOnce(new Error('network error'));
    mockSafeJson.mockResolvedValueOnce({ file_id: 'add-id-0', file_type: 'file', file_size: 100 });

    const { result } = renderHook(() => useFileUpload());

    const files = [
      createMockFile('a.md', 100, 'text/markdown'),
      createMockFile('b.md', 100, 'text/markdown'),
    ];

    await expect(
      act(async () => {
        await result.current.uploadAll('channel-1', undefined, files);
      })
    ).rejects.toThrow('network error');

    // Two fetch calls: first succeeded, second threw
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });

  it('marks isUploading false after additionalFiles upload completes', async () => {
    const { result } = renderHook(() => useFileUpload());

    expect(result.current.isUploading).toBe(false);

    const overflowFile = createMockFile('overflow.md', 200, 'text/markdown');

    await act(async () => {
      await result.current.uploadAll('channel-1', undefined, [overflowFile]);
    });

    expect(result.current.isUploading).toBe(false);
  });
});
