import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import {
  useFileUpload,
  validateFiles,
  type AttachmentRejection,
} from '@/renderer/hooks/useFileUpload';
import {
  FREE_ATTACHMENT_BYTES,
  PREMIUM_ATTACHMENT_BYTES,
  resolveAttachmentLimit,
} from '@/renderer/utils/entitlementLimits';
import { useSubscriptionStore } from '@/renderer/stores/subscriptionStore';

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

/**
 * A minimal, structurally valid PNG: signature, IHDR, one IDAT, IEND.
 *
 * #2469: an image file's CONTENT now matters. The upload path strips metadata
 * before encrypting and fails closed on a buffer that declares a handled image
 * type but whose bytes match no image format — precisely so a renamed or corrupt
 * file cannot be uploaded unstripped. A zero-filled ArrayBuffer labelled
 * `image/png` is exactly that case, so these fixtures now carry real bytes.
 */
const MINIMAL_PNG = new Uint8Array([
  0x89,
  0x50,
  0x4e,
  0x47,
  0x0d,
  0x0a,
  0x1a,
  0x0a, // signature
  0x00,
  0x00,
  0x00,
  0x0d,
  0x49,
  0x48,
  0x44,
  0x52, // IHDR length + type
  0x00,
  0x00,
  0x00,
  0x01,
  0x00,
  0x00,
  0x00,
  0x01, // 1x1
  0x08,
  0x06,
  0x00,
  0x00,
  0x00,
  0x1f,
  0x15,
  0xc4,
  0x89,
  0x00,
  0x00,
  0x00,
  0x0a,
  0x49,
  0x44,
  0x41,
  0x54, // IDAT length + type
  0x78,
  0x9c,
  0x63,
  0x00,
  0x01,
  0x00,
  0x00,
  0x05,
  0x00,
  0x01,
  0x0d,
  0x0a,
  0x2d,
  0xb4,
  0x00,
  0x00,
  0x00,
  0x00,
  0x49,
  0x45,
  0x4e,
  0x44, // IEND
  0xae,
  0x42,
  0x60,
  0x82,
]);

function indexOf(haystack: Uint8Array, needle: number[]): number {
  outer: for (let i = 0; i + needle.length <= haystack.length; i++) {
    for (let j = 0; j < needle.length; j++) {
      if (haystack[i + j] !== needle[j]) continue outer;
    }
    return i;
  }
  return -1;
}

const IMAGE_TYPES = new Set(['image/png', 'image/apng', 'image/jpeg', 'image/jpg']);

/**
 * Builds a mock File of exactly `size` bytes. For an image type the buffer opens
 * with a real PNG and is padded to length — the padding sits after IEND, which
 * the chunk walker stops at, so it is inert. Keeping the requested size exact
 * matters because several tests drive the MAX_FILE_SIZE limit through this
 * helper.
 */
function createMockFile(name: string, size: number, type: string): File {
  if (!IMAGE_TYPES.has(type)) {
    return new File([new ArrayBuffer(size)], name, { type });
  }
  const buffer = new Uint8Array(Math.max(size, MINIMAL_PNG.byteLength));
  buffer.set(MINIMAL_PNG, 0);
  return new File([buffer], name, { type });
}

/** Fakes `size` instead of allocating it — a 40 MB ArrayBuffer per test case is
 *  real memory for no test value. */
function sizedFile(name: string, size: number, type = 'application/octet-stream'): File {
  const f = new File(['x'], name, { type });
  Object.defineProperty(f, 'size', { value: size });
  return f;
}

const freeLimit = resolveAttachmentLimit({ userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES });

describe('validateFiles', () => {
  it('accepts a file under the limit', () => {
    const r = validateFiles([sizedFile('test.png', 1000, 'image/png')], 0, freeLimit);
    expect(r.accepted).toHaveLength(1);
    expect(r.rejections).toEqual([]);
  });

  // THE BUG (#2157). 30 MiB is over the old flat 25 MiB constant but under the
  // 32 MiB free entitlement that the server and the pricing page both honour.
  // This case previously asserted rejection — it encoded the defect.
  it('accepts a 30 MiB file for a free user', () => {
    const r = validateFiles([sizedFile('big.zip', 30 * 1024 * 1024)], 0, freeLimit);
    expect(r.accepted).toHaveLength(1);
    expect(r.rejections).toEqual([]);
  });

  it('accepts a file exactly at the limit', () => {
    const r = validateFiles([sizedFile('edge.bin', FREE_ATTACHMENT_BYTES)], 0, freeLimit);
    expect(r.accepted).toHaveLength(1);
  });

  it('rejects one byte over the limit and carries the limit for the copy layer', () => {
    const r = validateFiles([sizedFile('over.bin', FREE_ATTACHMENT_BYTES + 1)], 0, freeLimit);
    expect(r.accepted).toEqual([]);
    expect(r.rejections).toHaveLength(1);
    expect(r.rejections[0]).toMatchObject({
      kind: 'over-limit',
      fileName: 'over.bin',
      fileSize: FREE_ATTACHMENT_BYTES + 1,
    });
    expect(r.rejections[0].limit).toEqual(freeLimit);
  });

  it('returns NO strings — copy belongs to the component', () => {
    const r = validateFiles([sizedFile('over.bin', FREE_ATTACHMENT_BYTES + 1)], 0, freeLimit);
    expect(JSON.stringify(r.rejections)).not.toMatch(/exceeds|limit is|MB/i);
  });

  it('rejects an empty file', () => {
    const r = validateFiles([sizedFile('empty.txt', 0, 'text/plain')], 0, freeLimit);
    expect(r.rejections[0].kind).toBe('empty');
    expect(r.accepted).toEqual([]);
  });

  // R6: one oversized file used to discard the whole drop.
  it('queues the valid files from a mixed batch and names only the bad one', () => {
    const r = validateFiles(
      [
        sizedFile('ok1.png', 1024),
        sizedFile('huge.bin', FREE_ATTACHMENT_BYTES + 1),
        sizedFile('ok2.png', 2048),
        sizedFile('ok3.png', 4096),
      ],
      0,
      freeLimit
    );
    expect(r.accepted.map((f) => f.name)).toEqual(['ok1.png', 'ok2.png', 'ok3.png']);
    expect(r.rejections).toHaveLength(1);
    expect(r.rejections[0].fileName).toBe('huge.bin');
  });

  it('fills remaining capacity then reports too-many exactly once', () => {
    const r = validateFiles(
      [sizedFile('a.png', 1), sizedFile('b.png', 1), sizedFile('c.png', 1), sizedFile('d.png', 1)],
      3,
      freeLimit
    );
    expect(r.accepted.map((f) => f.name)).toEqual(['a.png', 'b.png']);
    expect(r.rejections.filter((x) => x.kind === 'too-many')).toHaveLength(1);
  });

  it('reports too-many once when already full', () => {
    const r = validateFiles([sizedFile('a.png', 1)], 5, freeLimit);
    expect(r.accepted).toEqual([]);
    expect(r.rejections).toEqual([expect.objectContaining({ kind: 'too-many' })]);
  });

  it('returns nothing for an empty input', () => {
    const r = validateFiles([], 0, freeLimit);
    expect(r.accepted).toEqual([]);
    expect(r.rejections).toEqual([]);
  });

  it('allows multiple valid files within limits', () => {
    const r = validateFiles(
      [
        sizedFile('a.png', 1000, 'image/png'),
        sizedFile('b.jpg', 2000, 'image/jpeg'),
        sizedFile('c.pdf', 3000, 'application/pdf'),
      ],
      0,
      freeLimit
    );
    expect(r.accepted).toHaveLength(3);
    expect(r.rejections).toEqual([]);
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

  // filesRef is the SYNCHRONOUS mirror addFiles validates and rebuilds from.
  // addFiles writes it eagerly; removeFile/clearFiles used to write only through
  // setFiles and rely on a passive useEffect to catch the mirror up. Passive
  // effects run after paint, so an addFiles landing in that window read a stale
  // mirror and rebuilt the queue from it — resurrecting the entry just removed.
  //
  // Both calls sit in ONE act() deliberately: act flushes effects at its close,
  // so splitting them would let the reconciling effect run in between and hide
  // the race the test exists to catch.
  it('does not resurrect a removed file when addFiles runs before the effect', () => {
    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([createMockFile('gone.png', 100, 'image/png')]);
    });
    expect(result.current.files).toHaveLength(1);

    act(() => {
      result.current.removeFile(0);
      result.current.addFiles([createMockFile('kept.png', 100, 'image/png')]);
    });

    expect(result.current.files.map((f) => f.file.name)).toEqual(['kept.png']);
  });

  // Same mirror, same window, via clearFiles.
  it('does not resurrect cleared files when addFiles runs before the effect', () => {
    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([
        createMockFile('x.png', 100, 'image/png'),
        createMockFile('y.png', 100, 'image/png'),
      ]);
    });
    expect(result.current.files).toHaveLength(2);

    act(() => {
      result.current.clearFiles();
      result.current.addFiles([createMockFile('fresh.png', 100, 'image/png')]);
    });

    expect(result.current.files.map((f) => f.file.name)).toEqual(['fresh.png']);
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

  it('returns structured rejections from addFiles for an over-limit file', () => {
    const { result } = renderHook(() => useFileUpload());
    // 40 MiB is over the 32 MiB free floor the unhydrated store reports.
    const bigFile = sizedFile('huge.zip', 40 * 1024 * 1024, 'application/zip');

    let rejections: AttachmentRejection[] = [];
    act(() => {
      rejections = result.current.addFiles([bigFile]).rejections;
    });

    expect(rejections).toHaveLength(1);
    expect(rejections[0]).toMatchObject({ kind: 'over-limit', fileName: 'huge.zip' });
    expect(result.current.files).toHaveLength(0);
  });

  it('accepts a 30 MiB file that the old flat 25 MiB cap rejected', () => {
    const { result } = renderHook(() => useFileUpload());
    act(() => {
      result.current.addFiles([sizedFile('was-blocked.zip', 30 * 1024 * 1024)]);
    });
    expect(result.current.files).toHaveLength(1);
  });

  // VULN-004 (#2157 adversarial review): `too-many` is emitted ONCE for a whole
  // surplus, so `total - rejections.length` over-reports what was queued.
  // Dropping 8 files on an empty queue accepts 5 and discards 3, but produces a
  // single rejection — the old derivation claimed 7 were added.
  it('reports the ACCEPTED count explicitly, not derivable from the rejection count', () => {
    const { result } = renderHook(() => useFileUpload());
    let outcome = { accepted: -1, rejections: [] as AttachmentRejection[] };
    act(() => {
      outcome = result.current.addFiles(
        Array.from({ length: 8 }, (_, i) => sizedFile(`f${i}.png`, 1024))
      );
    });

    expect(outcome.accepted).toBe(5);
    expect(outcome.rejections).toHaveLength(1);
    // The trap: this subtraction says 7 and is wrong by the 3 silently dropped.
    expect(8 - outcome.rejections.length).not.toBe(outcome.accepted);
    expect(result.current.files).toHaveLength(5);
  });

  // Review row 2: addFiles used to compute inside a setFiles updater and read
  // the result from a ref straight after. React may defer the updater, so the
  // read could return a PREVIOUS selection's rejections.
  it("returns THIS selection's result on back-to-back calls", () => {
    const { result } = renderHook(() => useFileUpload());
    let first = { accepted: -1, rejections: [] as AttachmentRejection[] };
    let second = { accepted: -1, rejections: [] as AttachmentRejection[] };
    act(() => {
      first = result.current.addFiles([sizedFile('ok.png', 1024)]);
      // Same tick, before React has flushed anything from the first call.
      second = result.current.addFiles([sizedFile('huge.bin', 40 * 1024 * 1024)]);
    });

    expect(first).toEqual({ accepted: 1, rejections: [] });
    expect(second.accepted).toBe(0);
    expect(second.rejections).toHaveLength(1);
    expect(second.rejections[0].fileName).toBe('huge.bin');
    // The second call saw the first call's file in the queue.
    expect(result.current.files.map((f) => f.file.name)).toEqual(['ok.png']);
  });

  it('composes capacity across back-to-back calls', () => {
    const { result } = renderHook(() => useFileUpload());
    let last = { accepted: -1, rejections: [] as AttachmentRejection[] };
    act(() => {
      result.current.addFiles([
        sizedFile('a.png', 1),
        sizedFile('b.png', 1),
        sizedFile('c.png', 1),
      ]);
      last = result.current.addFiles([
        sizedFile('d.png', 1),
        sizedFile('e.png', 1),
        sizedFile('f.png', 1),
      ]);
    });
    // 3 queued, capacity 2 left, so 2 accepted and one too-many rejection.
    expect(last.accepted).toBe(2);
    expect(last.rejections).toEqual([expect.objectContaining({ kind: 'too-many' })]);
    expect(result.current.files).toHaveLength(5);
  });

  it('exposes the resolved limit so consumers never re-derive it', () => {
    const { result } = renderHook(() => useFileUpload());
    expect(result.current.limit.limitBytes).toBe(FREE_ATTACHMENT_BYTES);
    expect(result.current.limit.source).toBe('entitlement');
  });

  it('keeps addFiles identity stable across re-renders', () => {
    const { result, rerender } = renderHook(() => useFileUpload());
    const first = result.current.addFiles;
    rerender();
    // The limit travels by ref, not by dep — a dep would churn every memoized
    // consumer of addFiles on every render.
    expect(result.current.addFiles).toBe(first);
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

  // #2469: the bytes handed to encryptFile must be the STRIPPED ones. Asserting
  // only that stripFileMetadata was called would not prove that — the upload
  // could still encrypt the original. This inspects what encryptFile actually
  // received and requires the GPS marker to be absent from it.
  it('strips image metadata before the bytes reach encryptFile', async () => {
    const { encryptFile } = await import('@/renderer/utils/attachmentCrypto');
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({ file_id: 'strip-1', file_type: 'photo', file_size: 100 });

    // A real PNG carrying a tEXt chunk with a recognisable payload.
    const marker = [0x47, 0x50, 0x53, 0xde, 0xad, 0xbe, 0xef];
    const text = [0x74, 0x45, 0x58, 0x74]; // 'tEXt'
    const chunk = [
      0x00,
      0x00,
      0x00,
      marker.length,
      ...text,
      ...marker,
      0x00,
      0x00,
      0x00,
      0x00, // CRC (not validated on a chunk we drop)
    ];
    const base = Array.from(MINIMAL_PNG);
    // Splice the tEXt chunk in before IEND (the final 12 bytes).
    const withText = new Uint8Array([
      ...base.slice(0, base.length - 12),
      ...chunk,
      ...base.slice(base.length - 12),
    ]);
    expect(indexOf(withText, marker)).toBeGreaterThanOrEqual(0);

    const file = new File([withText], 'gps.png', { type: 'image/png' });
    const { result } = renderHook(() => useFileUpload());
    act(() => {
      result.current.addFiles([file]);
    });
    await act(async () => {
      await result.current.uploadAll('channel-1');
    });

    expect(encryptFile).toHaveBeenCalled();
    const passed = new Uint8Array(vi.mocked(encryptFile).mock.calls[0][0] as ArrayBuffer);
    expect(indexOf(passed, marker)).toBe(-1);
  });

  // #2843: the uploaded bytes must be the ENCRYPTED buffer, never the raw file.
  // encryptAndBuildForm used to read
  //   `const uploadData = channelKey ? await encryptFile(...) : fileData`
  // with `channelKey: CryptoKey | null` — so a null key uploaded the file in the
  // clear. That branch was residue from before #1024, when an unencrypted
  // channel legitimately sent plaintext, and #1031 removed the `isEncrypted`
  // selector without removing the branch it selected.
  //
  // Asserting `encryptFile` was CALLED is not enough: it would still pass if a
  // fallback path uploaded the original bytes alongside. This asserts on the
  // bytes that actually reach the transport.
  it('uploads the encrypted buffer, never the raw file bytes', async () => {
    const { encryptFile } = await import('@/renderer/utils/attachmentCrypto');
    const ciphertext = new ArrayBuffer(64);
    vi.mocked(encryptFile).mockResolvedValueOnce(ciphertext);

    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({
      file_id: 'attach-cipher-1',
      file_type: 'photo',
      file_size: 64,
    });

    const { result } = renderHook(() => useFileUpload());

    // Distinct size so raw-vs-ciphertext is unambiguous in the assertion.
    act(() => {
      result.current.addFiles([createMockFile('gps.jpg', 4096, 'image/jpeg')]);
    });

    await act(async () => {
      await result.current.uploadAll('channel-1');
    });

    const body = mockApiFetch.mock.calls[0][1].body as FormData;
    const sent = body.get('file') as Blob;
    expect(sent.size).toBe(ciphertext.byteLength);
    expect(sent.size).not.toBe(4096);
  });

  // #2843: key_version is now sent unconditionally. It used to be guarded by
  // `if (keyVersion !== undefined)`, which paired with the nullable key above —
  // omitting it let the server invent an epoch the sender never claimed, the
  // same defect fixed on the message-send path in #2832.
  it('always sends key_version on the upload form', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({
      file_id: 'attach-kv-1',
      file_type: 'photo',
      file_size: 1000,
    });

    const { result } = renderHook(() => useFileUpload());

    act(() => {
      result.current.addFiles([createMockFile('kv.png', 1000, 'image/png')]);
    });

    await act(async () => {
      await result.current.uploadAll('channel-1');
    });

    const body = mockApiFetch.mock.calls[0][1].body as FormData;
    expect(body.get('key_version')).toBe('1');
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
      { ids: string[]; summaries: { width?: number; height?: number }[] } | undefined;
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

// Review row 5: an entitlement can drop between queueing and sending.
describe('useFileUpload — entitlement downgrade after queueing', () => {
  afterEach(() => {
    useSubscriptionStore.getState().reset?.();
  });

  it('refuses a queued file that no longer fits, instead of uploading it', async () => {
    act(() => {
      useSubscriptionStore.getState().setEntitlement({
        ...useSubscriptionStore.getState().entitlement,
        tier: 'premium',
        maxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES,
      });
    });
    const { result } = renderHook(() => useFileUpload());

    // 40 MiB: fine under premium, over the 32 MiB free floor.
    act(() => {
      result.current.addFiles([sizedFile('premium.bin', 40 * 1024 * 1024)]);
    });
    expect(result.current.files).toHaveLength(1);

    // Downgrade while it sits in the queue.
    act(() => {
      useSubscriptionStore.getState().setEntitlement({
        ...useSubscriptionStore.getState().entitlement,
        tier: 'free',
        maxAttachmentBytes: FREE_ATTACHMENT_BYTES,
      });
    });

    mockApiFetch.mockClear();
    await act(async () => {
      await result.current.uploadAll('chan-1');
    });

    // Never uploaded; the entry carries the refusal.
    expect(mockApiFetch).not.toHaveBeenCalled();
    expect(result.current.files[0].status).toBe('error');
    expect(result.current.files[0].error).toMatch(/exceeds the 32 MB limit/);
  });
});
