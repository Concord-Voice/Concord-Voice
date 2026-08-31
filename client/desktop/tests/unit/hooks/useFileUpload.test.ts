import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { renderHook, act } from '@testing-library/react';
import {
  useFileUpload,
  validateFiles,
  type AttachmentRejection,
} from '@/renderer/hooks/messaging/useFileUpload';
import {
  FREE_ATTACHMENT_BYTES,
  PREMIUM_ATTACHMENT_BYTES,
  IMAGE_STRIP_MAX_BYTES,
  resolveAttachmentLimit,
} from '@/renderer/utils/policy/entitlementLimits';
import { useSubscriptionStore } from '@/renderer/stores/auth/subscriptionStore';
import {
  CHUNK_PLAINTEXT_BYTES,
  ENVELOPE_HEADER_BYTES,
} from '@/renderer/utils/crypto/attachmentChunkedCrypto';

// Mock apiClient
const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();
const mockUploadAttachmentChunked = vi.fn();
const mockAbandonSession = vi.fn();
class MockUploadAbortedError extends Error {
  constructor() {
    super('Upload aborted');
    this.name = 'UploadAbortedError';
  }
}
vi.mock('@/renderer/services/messaging/attachmentUploadSession', () => ({
  uploadAttachmentChunked: (...args: unknown[]) => mockUploadAttachmentChunked(...args),
  abandonSessionOnUnload: (...args: unknown[]) => mockAbandonSession(...args),
  get UploadAbortedError() {
    return MockUploadAbortedError;
  },
}));

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (...args: unknown[]) => mockSafeJson(...args),
}));

// Mock e2eeService
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return true;
    },
    getChannelKey: vi.fn().mockResolvedValue({} as CryptoKey),
    getCurrentKeyVersion: vi.fn().mockReturnValue(1),
  },
}));

// Mock attachmentCrypto
vi.mock('@/renderer/utils/crypto/attachmentCrypto', async () => {
  const actual = await vi.importActual<typeof import('@/renderer/utils/crypto/attachmentCrypto')>(
    '@/renderer/utils/crypto/attachmentCrypto'
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

/** Like sizedFile, but with REAL leading bytes so the sniffer can dispatch on
 *  them. Size is still faked — the point is the magic, not the payload. */
function magicFile(
  name: string,
  size: number,
  magic: number[],
  type = 'application/octet-stream'
): File {
  const f = new File([new Uint8Array(magic)], name, { type });
  Object.defineProperty(f, 'size', { value: size });
  return f;
}

const JPEG_MAGIC = [0xff, 0xd8, 0xff, 0xe0];

const freeLimit = resolveAttachmentLimit({
  userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
  chunkedUploadSupported: false,
});

/** Premium on a server that supports chunking, so the entitlement is not
 *  clamped and the image ceiling is the only thing that can bite. */
const chunkedPremiumLimit = resolveAttachmentLimit({
  userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES,
  chunkedUploadSupported: true,
});

describe('validateFiles — image ceiling (sniffed, not declared)', () => {
  it('refuses an image above IMAGE_STRIP_MAX_BYTES before it is ever queued', async () => {
    const r = await validateFiles(
      [magicFile('huge.jpg', IMAGE_STRIP_MAX_BYTES + 1, JPEG_MAGIC, 'image/jpeg')],
      0,
      chunkedPremiumLimit
    );
    expect(r.accepted).toHaveLength(0);
    expect(r.rejections[0].kind).toBe('image-too-large');
  });

  it('sniffs a JPEG uploaded as application/octet-stream (#2469)', async () => {
    // Dispatching on the DECLARED type would let this bypass the whole-file
    // strip path, which is a privacy regression, not a miscategorisation.
    const r = await validateFiles(
      [magicFile('sneaky.bin', IMAGE_STRIP_MAX_BYTES + 1, JPEG_MAGIC)],
      0,
      chunkedPremiumLimit
    );
    expect(r.accepted).toHaveLength(0);
    expect(r.rejections[0].kind).toBe('image-too-large');
  });

  it('lets a NON-image of the same size through — the ceiling is image-only', async () => {
    // This is the whole point of the chunked format: a 200 MB archive is
    // chunk-read and bounded, so no ceiling applies to it.
    const r = await validateFiles(
      [magicFile('archive.bin', IMAGE_STRIP_MAX_BYTES + 1, [0x00, 0x01, 0x02, 0x03])],
      0,
      chunkedPremiumLimit
    );
    expect(r.rejections).toHaveLength(0);
    expect(r.accepted).toHaveLength(1);
  });

  it('accepts an image UNDER the ceiling', async () => {
    const r = await validateFiles(
      [magicFile('ok.jpg', 1_000_000, JPEG_MAGIC, 'image/jpeg')],
      0,
      chunkedPremiumLimit
    );
    expect(r.rejections).toHaveLength(0);
    expect(r.accepted).toHaveLength(1);
  });
});

describe('validateFiles', () => {
  it('accepts a file under the limit', async () => {
    const r = await validateFiles([sizedFile('test.png', 1000, 'image/png')], 0, freeLimit);
    expect(r.accepted).toHaveLength(1);
    expect(r.rejections).toEqual([]);
  });

  // THE BUG (#2157). 30 MiB is over the old flat 25 MiB constant but under the
  // 32 MiB free entitlement that the server and the pricing page both honour.
  // This case previously asserted rejection — it encoded the defect.
  it('accepts a 30 MiB file for a free user', async () => {
    const r = await validateFiles([sizedFile('big.zip', 30 * 1024 * 1024)], 0, freeLimit);
    expect(r.accepted).toHaveLength(1);
    expect(r.rejections).toEqual([]);
  });

  it('accepts a file exactly at the limit', async () => {
    const r = await validateFiles([sizedFile('edge.bin', FREE_ATTACHMENT_BYTES)], 0, freeLimit);
    expect(r.accepted).toHaveLength(1);
  });

  it('rejects one byte over the limit and carries the limit for the copy layer', async () => {
    const r = await validateFiles([sizedFile('over.bin', FREE_ATTACHMENT_BYTES + 1)], 0, freeLimit);
    expect(r.accepted).toEqual([]);
    expect(r.rejections).toHaveLength(1);
    expect(r.rejections[0]).toMatchObject({
      kind: 'over-limit',
      fileName: 'over.bin',
      fileSize: FREE_ATTACHMENT_BYTES + 1,
    });
    expect(r.rejections[0].limit).toEqual(freeLimit);
  });

  it('returns NO strings — copy belongs to the component', async () => {
    const r = await validateFiles([sizedFile('over.bin', FREE_ATTACHMENT_BYTES + 1)], 0, freeLimit);
    expect(JSON.stringify(r.rejections)).not.toMatch(/exceeds|limit is|MB/i);
  });

  it('rejects an empty file', async () => {
    const r = await validateFiles([sizedFile('empty.txt', 0, 'text/plain')], 0, freeLimit);
    expect(r.rejections[0].kind).toBe('empty');
    expect(r.accepted).toEqual([]);
  });

  // R6: one oversized file used to discard the whole drop.
  it('queues the valid files from a mixed batch and names only the bad one', async () => {
    const r = await validateFiles(
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

  it('fills remaining capacity then reports too-many exactly once', async () => {
    const r = await validateFiles(
      [sizedFile('a.png', 1), sizedFile('b.png', 1), sizedFile('c.png', 1), sizedFile('d.png', 1)],
      3,
      freeLimit
    );
    expect(r.accepted.map((f) => f.name)).toEqual(['a.png', 'b.png']);
    expect(r.rejections.filter((x) => x.kind === 'too-many')).toHaveLength(1);
  });

  it('reports too-many once when already full', async () => {
    const r = await validateFiles([sizedFile('a.png', 1)], 5, freeLimit);
    expect(r.accepted).toEqual([]);
    expect(r.rejections).toEqual([expect.objectContaining({ kind: 'too-many' })]);
  });

  it('returns nothing for an empty input', async () => {
    const r = await validateFiles([], 0, freeLimit);
    expect(r.accepted).toEqual([]);
    expect(r.rejections).toEqual([]);
  });

  it('allows multiple valid files within limits', async () => {
    const r = await validateFiles(
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

/** Sets the raw capability object AND the derived three-state the hook reads.
 *  They are separate on purpose -- `serverCapabilities: null` cannot distinguish
 *  "not fetched yet" from "fetch failed" -- but a fixture that sets only one of
 *  them silently stops exercising the path it names. */
function setChunkedCapability(supported: boolean): void {
  const store = useClientConfigStore.getState();
  store.setServerCapabilities({
    auth: { oauthProviders: [] },
    features: supported ? { chunkedAttachmentUpload: true } : {},
  });
  store.setChunkedUploadCapability({
    status: supported ? 'supported' : 'confirmed-unsupported',
  });
}

function setEnvelopeVersions(versions: number[] | undefined): void {
  const current = useClientConfigStore.getState().serverCapabilities;
  useClientConfigStore.getState().setServerCapabilities({
    auth: current?.auth ?? { oauthProviders: [] },
    features: {
      ...(current?.features ?? {}),
      ...(versions === undefined ? {} : { attachmentEnvelopeVersions: versions }),
    },
  });
}

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

  it('adds files to the queue', async () => {
    const { result } = renderHook(() => useFileUpload());
    const file = createMockFile('test.png', 1000, 'image/png');

    await act(async () => {
      await result.current.addFiles([file]);
    });

    expect(result.current.files).toHaveLength(1);
    expect(result.current.files[0].file.name).toBe('test.png');
    expect(result.current.files[0].status).toBe('pending');
    expect(result.current.hasFiles).toBe(true);
  });

  it('generates preview URL for images', async () => {
    const { result } = renderHook(() => useFileUpload());
    const file = createMockFile('photo.png', 1000, 'image/png');

    await act(async () => {
      await result.current.addFiles([file]);
    });

    expect(result.current.files[0].previewUrl).toBeDefined();
    expect(result.current.files[0].previewUrl).toContain('blob:');
  });

  it('does not generate preview URL for non-images', async () => {
    const { result } = renderHook(() => useFileUpload());
    const file = createMockFile('doc.pdf', 1000, 'application/pdf');

    await act(async () => {
      await result.current.addFiles([file]);
    });

    expect(result.current.files[0].previewUrl).toBeUndefined();
  });

  it('removes a file from the queue', async () => {
    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      await result.current.addFiles([
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
  it('does not resurrect a removed file when addFiles runs before the effect', async () => {
    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      await result.current.addFiles([createMockFile('gone.png', 100, 'image/png')]);
    });
    expect(result.current.files).toHaveLength(1);

    await act(async () => {
      result.current.removeFile(0);
      await result.current.addFiles([createMockFile('kept.png', 100, 'image/png')]);
    });

    expect(result.current.files.map((f) => f.file.name)).toEqual(['kept.png']);
  });

  // Same mirror, same window, via clearFiles.
  it('does not resurrect cleared files when addFiles runs before the effect', async () => {
    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      await result.current.addFiles([
        createMockFile('x.png', 100, 'image/png'),
        createMockFile('y.png', 100, 'image/png'),
      ]);
    });
    expect(result.current.files).toHaveLength(2);

    await act(async () => {
      result.current.clearFiles();
      await result.current.addFiles([createMockFile('fresh.png', 100, 'image/png')]);
    });

    expect(result.current.files.map((f) => f.file.name)).toEqual(['fresh.png']);
  });

  it('clears all files', async () => {
    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      await result.current.addFiles([
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

  it('returns structured rejections from addFiles for an over-limit file', async () => {
    const { result } = renderHook(() => useFileUpload());
    // 40 MiB is over the 32 MiB free floor the unhydrated store reports.
    const bigFile = sizedFile('huge.zip', 40 * 1024 * 1024, 'application/zip');

    let rejections: AttachmentRejection[] = [];
    await act(async () => {
      rejections = (await result.current.addFiles([bigFile])).rejections;
    });

    expect(rejections).toHaveLength(1);
    expect(rejections[0]).toMatchObject({ kind: 'over-limit', fileName: 'huge.zip' });
    expect(result.current.files).toHaveLength(0);
  });

  it('accepts a 30 MiB file that the old flat 25 MiB cap rejected', async () => {
    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([sizedFile('was-blocked.zip', 30 * 1024 * 1024)]);
    });
    expect(result.current.files).toHaveLength(1);
  });

  // VULN-004 (#2157 adversarial review): `too-many` is emitted ONCE for a whole
  // surplus, so `total - rejections.length` over-reports what was queued.
  // Dropping 8 files on an empty queue accepts 5 and discards 3, but produces a
  // single rejection — the old derivation claimed 7 were added.
  it('reports the ACCEPTED count explicitly, not derivable from the rejection count', async () => {
    const { result } = renderHook(() => useFileUpload());
    let outcome = { accepted: -1, rejections: [] as AttachmentRejection[] };
    await act(async () => {
      outcome = await result.current.addFiles(
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
  it("returns THIS selection's result on back-to-back calls", async () => {
    const { result } = renderHook(() => useFileUpload());
    let first = { accepted: -1, rejections: [] as AttachmentRejection[] };
    let second = { accepted: -1, rejections: [] as AttachmentRejection[] };
    await act(async () => {
      first = await result.current.addFiles([sizedFile('ok.png', 1024)]);
      // Same tick, before React has flushed anything from the first call.
      second = await result.current.addFiles([sizedFile('huge.bin', 40 * 1024 * 1024)]);
    });

    expect(first).toEqual({ accepted: 1, rejections: [] });
    expect(second.accepted).toBe(0);
    expect(second.rejections).toHaveLength(1);
    expect(second.rejections[0].fileName).toBe('huge.bin');
    // The second call saw the first call's file in the queue.
    expect(result.current.files.map((f) => f.file.name)).toEqual(['ok.png']);
  });

  it('composes capacity across back-to-back calls', async () => {
    const { result } = renderHook(() => useFileUpload());
    let last = { accepted: -1, rejections: [] as AttachmentRejection[] };
    await act(async () => {
      await result.current.addFiles([
        sizedFile('a.png', 1),
        sizedFile('b.png', 1),
        sizedFile('c.png', 1),
      ]);
      last = await result.current.addFiles([
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

  it('actually USES the chunked session when the server advertises it', async () => {
    // Written, tested, and unreachable is the failure mode this PR has already
    // hit twice -- session routes registered on no router, and this client with
    // no caller. A unit test of the session client cannot catch it; only a test
    // that drives the real dispatch can.
    setChunkedCapability(true);
    mockUploadAttachmentChunked.mockResolvedValue({
      file_id: 'chunked-1',
      storage_key: 'attachments/x',
      file_type: 'file',
      file_size: 4096,
    });

    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([createMockFile('big.bin', 4096)]);
    });

    let out: { ids: string[] } | undefined;
    await act(async () => {
      out = await result.current.uploadAll('channel-1');
    });

    expect(mockUploadAttachmentChunked).toHaveBeenCalledTimes(1);
    expect(out?.ids).toContain('chunked-1');
    // And the legacy multipart endpoint is NOT hit.
    expect(mockApiFetch).not.toHaveBeenCalledWith(
      '/api/v1/media/upload/attachment',
      expect.anything()
    );
  });

  it('falls back to the legacy path when the server does not advertise it', async () => {
    // Fail-closed is the whole point: an absent capability must never be read
    // as present.
    setChunkedCapability(false);
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({ file_id: 'legacy-1', file_type: 'file', file_size: 4096 });

    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([createMockFile('big.bin', 4096)]);
    });
    await act(async () => {
      await result.current.uploadAll('channel-1');
    });

    expect(mockUploadAttachmentChunked).not.toHaveBeenCalled();
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/media/upload/attachment',
      expect.objectContaining({ method: 'POST' })
    );
  });

  it('uploads files and returns IDs', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({
      file_id: 'attach-uploaded-1',
      file_type: 'photo',
      file_size: 1000,
    });

    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      await result.current.addFiles([createMockFile('test.png', 1000, 'image/png')]);
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

  it("never stamps a removed file's id onto a surviving one", async () => {
    // uploadPendingFiles iterates a SNAPSHOT and passes the snapshot index into
    // setFiles(prev => prev.map((f, idx) => idx === i ? ... )), where `prev` is
    // LIVE state. Remove an earlier file while its upload is in flight and the
    // completion write lands on whichever file shifted into that index -- which
    // would send the survivor as an attachment pointing at the removed file's
    // ciphertext. The window is one small upload today and minutes once uploads
    // are chunked.
    let releaseFirst = (): void => {};
    const firstInFlight = new Promise<void>((r) => {
      releaseFirst = r;
    });
    let call = 0;
    mockApiFetch.mockImplementation(async () => {
      call += 1;
      if (call === 1) await firstInFlight;
      return { ok: true, status: 201 };
    });
    mockSafeJson.mockImplementation(async () => ({
      file_id: `attach-${call}`,
      file_type: 'file',
      file_size: 1,
    }));

    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([
        createMockFile('doomed.png', 100, 'image/png'),
        createMockFile('survivor.png', 100, 'image/png'),
      ]);
    });

    await act(async () => {
      const running = result.current.uploadAll('channel-1');
      result.current.removeFile(0); // pull the in-flight file out from under it
      releaseFirst();
      await running;
    });

    const survivor = result.current.files.find((f) => f.file.name === 'survivor.png');
    // Assert it carries ITS OWN id, not merely "not the removed file's". A
    // not-equal assertion passes for any wrong-but-different value, including
    // ids produced by a positional write that happens to land correctly on the
    // last pass.
    expect(survivor?.id).toBe('attach-2');
    expect(result.current.files).toHaveLength(1);
  });

  it('handles upload errors gracefully', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      status: 500,
      text: () => Promise.resolve('Server error'),
    });

    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      await result.current.addFiles([createMockFile('test.png', 1000, 'image/png')]);
    });

    await act(async () => {
      await result.current.uploadAll('channel-1');
    });

    expect(result.current.files[0].status).toBe('error');
    expect(result.current.files[0].error).toBeDefined();
  });

  it('uploads with encryption (always)', async () => {
    const { encryptFile } = await import('@/renderer/utils/crypto/attachmentCrypto');
    mockApiFetch.mockResolvedValue({ ok: true, status: 201 });
    mockSafeJson.mockResolvedValue({
      file_id: 'attach-enc-1',
      file_type: 'photo',
      file_size: 1000,
    });

    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      await result.current.addFiles([createMockFile('secret.png', 1000, 'image/png')]);
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
    const { encryptFile } = await import('@/renderer/utils/crypto/attachmentCrypto');
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
    await act(async () => {
      await result.current.addFiles([file]);
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
    const { encryptFile } = await import('@/renderer/utils/crypto/attachmentCrypto');
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
    await act(async () => {
      await result.current.addFiles([createMockFile('gps.jpg', 4096, 'image/jpeg')]);
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

    await act(async () => {
      await result.current.addFiles([createMockFile('kv.png', 1000, 'image/png')]);
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

    await act(async () => {
      await result.current.addFiles([createMockFile('dm.png', 1000, 'image/png')]);
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

    await act(async () => {
      await result.current.addFiles([createMockFile('test.png', 1000, 'image/png')]);
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

  it('revokes preview URLs on clearFiles', async () => {
    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      await result.current.addFiles([createMockFile('img.png', 1000, 'image/png')]);
    });

    expect(result.current.files[0].previewUrl).toBeDefined();

    act(() => {
      result.current.clearFiles();
    });

    expect(revokeSpy).toHaveBeenCalled();
    revokeSpy.mockRestore();
  });

  it('revokes preview URL on removeFile', async () => {
    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
    const { result } = renderHook(() => useFileUpload());

    await act(async () => {
      await result.current.addFiles([createMockFile('img.png', 1000, 'image/png')]);
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

    await act(async () => {
      await result.current.addFiles([createMockFile('user.png', 100, 'image/png')]);
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
    await act(async () => {
      await result.current.addFiles([sizedFile('premium.bin', 40 * 1024 * 1024)]);
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

describe("useFileUpload — A10'-a: no whole-file buffering on the chunked path", () => {
  // STRUCTURAL, not behavioural. The point of the chunked format is that a
  // 256 MiB attachment is never held in memory whole -- but "we do not call
  // arrayBuffer()" is invisible to any assertion about the RESULT, because a
  // handler that buffered and then streamed would produce an identical upload.
  //
  // Making File.prototype.arrayBuffer THROW turns the absence of that call into
  // something a test can see. A non-image is used deliberately: images take the
  // metadata-strip path, which needs the whole file by construction and is
  // exactly why they carry their own ceiling.
  it('never calls File.arrayBuffer for a chunked non-image upload', async () => {
    const original = File.prototype.arrayBuffer;
    const calls: string[] = [];
    File.prototype.arrayBuffer = function (this: File) {
      calls.push(this.name);
      throw new Error('arrayBuffer() called — the whole file was buffered');
    };

    try {
      setChunkedCapability(true);
      mockUploadAttachmentChunked.mockResolvedValue({
        file_id: 'streamed-1',
        storage_key: 'attachments/x',
        file_type: 'file',
        file_size: 4096,
      });

      const { result } = renderHook(() => useFileUpload());
      await act(async () => {
        await result.current.addFiles([
          createMockFile('big.bin', 4096, 'application/octet-stream'),
        ]);
      });

      let out: { ids: string[] } | undefined;
      await act(async () => {
        out = await result.current.uploadAll('channel-1');
      });

      expect(calls).toEqual([]);
      expect(out?.ids).toContain('streamed-1');
      expect(result.current.files[0].status).toBe('done');
    } finally {
      File.prototype.arrayBuffer = original;
    }
  });

  it('POSITIVE CONTROL: the legacy path does buffer, and the stub proves it', async () => {
    // Without this, the test above cannot tell "we did not buffer" from "the
    // stub was never installed". A test that can only ever pass is not evidence.
    const original = File.prototype.arrayBuffer;
    const calls: string[] = [];
    File.prototype.arrayBuffer = function (this: File) {
      calls.push(this.name);
      throw new Error('arrayBuffer() called');
    };

    try {
      setChunkedCapability(false);

      const { result } = renderHook(() => useFileUpload());
      await act(async () => {
        await result.current.addFiles([
          createMockFile('big.bin', 4096, 'application/octet-stream'),
        ]);
      });
      await act(async () => {
        await result.current.uploadAll('channel-1');
      });

      expect(calls).toEqual(['big.bin']);
      expect(result.current.files[0].status).toBe('error');
    } finally {
      File.prototype.arrayBuffer = original;
    }
  });
});

describe('useFileUpload — cancel, stall, and unmount', () => {
  beforeEach(() => {
    // RTL auto-unmounts each prior hook at cleanup, and every one of those that
    // left a session open fires the abandon DELETE. Without this the counts
    // below measure the whole describe block, not the test.
    mockAbandonSession.mockClear();
    setChunkedCapability(true);
  });

  /** Drives the chunked upload up to a chosen chunk, then hands control back so
   *  the test can act mid-flight. */
  function pausableUpload() {
    let release!: () => void;
    const paused = new Promise<void>((r) => (release = r));
    let seenSignal: AbortSignal | undefined;
    let commit!: (i: number, total: number) => void;

    mockUploadAttachmentChunked.mockImplementation(
      async (
        _file: File,
        _key: unknown,
        _ctx: unknown,
        signal: AbortSignal,
        cb: {
          onChunkCommitted: (i: number, t: number) => void;
          onSessionOpened?: (id: string) => void;
        }
      ) => {
        seenSignal = signal;
        commit = cb.onChunkCommitted;
        cb.onSessionOpened?.('sess-live');
        await paused;
        if (signal.aborted) throw new MockUploadAbortedError();
        return {
          file_id: 'chunked-ok',
          storage_key: 'k',
          file_type: 'file',
          file_size: 4096,
        };
      }
    );
    return {
      release,
      commitChunk: (i: number, t: number) => commit(i, t),
      signal: () => seenSignal,
    };
  }

  it('a cancelled upload is CANCELLED, not an error', async () => {
    // A user who pressed stop has not hit a failure. Reporting one as `error`
    // puts a red row and a retry affordance in front of a deliberate act.
    const flight = pausableUpload();
    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([createMockFile('big.bin', 4096)]);
    });

    let done!: Promise<unknown>;
    await act(async () => {
      done = result.current.uploadAll('channel-1');
      await Promise.resolve();
    });

    const id = result.current.files[0].uploadId;
    await act(async () => {
      result.current.cancelUpload(id);
      flight.release();
      await done;
    });

    expect(result.current.files[0].status).toBe('cancelled');
    expect(result.current.files[0].error).toBeUndefined();
  });

  it('cancel actually aborts the in-flight request', async () => {
    // A status flip without an abort is a cancel that does not cancel: the
    // bytes keep uploading behind a row that says they stopped.
    const flight = pausableUpload();
    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([createMockFile('big.bin', 4096)]);
    });

    let done!: Promise<unknown>;
    await act(async () => {
      done = result.current.uploadAll('channel-1');
      await Promise.resolve();
    });

    expect(flight.signal()?.aborted).toBe(false);
    await act(async () => {
      result.current.cancelUpload(result.current.files[0].uploadId);
    });
    expect(flight.signal()?.aborted).toBe(true);

    await act(async () => {
      flight.release();
      await done;
    });
  });

  it('a cancelled file gets its OWN signal and does not poison the queue', async () => {
    // Uploads run sequentially, so "per-file" cannot be shown by cancelling an
    // idle row -- it has no controller registered, so the call is a no-op
    // whether the signal is per-file or shared, and the test proves nothing.
    // The property that actually distinguishes the two is that cancelling the
    // file IN FLIGHT stops that file while the next still completes, on a
    // signal of its own.
    let release!: () => void;
    const paused = new Promise<void>((r) => (release = r));
    const signals: AbortSignal[] = [];

    mockUploadAttachmentChunked.mockImplementation(
      async (
        _f: File,
        _k: unknown,
        _c: unknown,
        signal: AbortSignal,
        cb: { onSessionOpened?: (id: string) => void; onSessionClosed?: (id: string) => void }
      ) => {
        signals.push(signal);
        const nth = signals.length;
        cb.onSessionOpened?.(`sess-${nth}`);
        if (nth === 1) {
          await paused;
          if (signal.aborted) throw new MockUploadAbortedError();
        }
        cb.onSessionClosed?.(`sess-${nth}`);
        return { file_id: `file-${nth}`, storage_key: 'k', file_type: 'file', file_size: 4096 };
      }
    );

    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([createMockFile('a.bin', 4096), createMockFile('b.bin', 4096)]);
    });

    let done!: Promise<unknown>;
    await act(async () => {
      done = result.current.uploadAll('channel-1');
      await Promise.resolve();
    });

    await act(async () => {
      result.current.cancelUpload(result.current.files[0].uploadId);
      release();
      await done;
    });

    expect(result.current.files[0].status).toBe('cancelled');
    expect(result.current.files[1].status).toBe('done');
    // Distinct controllers, and the survivor's was never aborted.
    expect(signals[1]).not.toBe(signals[0]);
    expect(signals[1].aborted).toBe(false);
  });

  it('reports plaintext bytes the server ACCEPTED, not bytes handed to fetch', async () => {
    const flight = pausableUpload();
    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([createMockFile('big.bin', 4096)]);
    });

    let done!: Promise<unknown>;
    await act(async () => {
      done = result.current.uploadAll('channel-1');
      await Promise.resolve();
    });

    await act(async () => {
      flight.commitChunk(0, 4);
    });

    expect(result.current.files[0].status).toBe('uploading');
    expect(result.current.files[0].progress).toBe(25);
    // 4096-byte file, so one 8 MiB chunk covers all of it -- capped at the file
    // size rather than reporting a chunk that is larger than the file.
    expect(result.current.files[0].bytesSent).toBe(4096);

    await act(async () => {
      flight.release();
      await done;
    });
  });

  it('subtracts the v3 header reserve from committed plaintext progress', async () => {
    setEnvelopeVersions([2, 3]);
    const flight = pausableUpload();
    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([
        createMockFile('big.bin', CHUNK_PLAINTEXT_BYTES + 1, 'application/octet-stream'),
      ]);
    });

    let done!: Promise<unknown>;
    await act(async () => {
      done = result.current.uploadAll('channel-1');
      await Promise.resolve();
    });
    await act(async () => {
      flight.commitChunk(0, 2);
    });

    expect(result.current.files[0].bytesSent).toBe(CHUNK_PLAINTEXT_BYTES - ENVELOPE_HEADER_BYTES);

    await act(async () => {
      flight.release();
      await done;
    });
  });

  it('opens as preparing on the chunked path — nothing is committed yet', async () => {
    const flight = pausableUpload();
    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([createMockFile('big.bin', 4096)]);
    });

    let done!: Promise<unknown>;
    await act(async () => {
      done = result.current.uploadAll('channel-1');
      await Promise.resolve();
    });

    expect(result.current.files[0].status).toBe('preparing');
    expect(result.current.files[0].stalled).toBe(false);

    await act(async () => {
      flight.release();
      await done;
    });
  });

  it('never claims a stall before the first commit — there is no history yet', async () => {
    // A first chunk that never lands shows as plain progress at 0. Honest, if
    // unhelpful: with no observed chunk time there is nothing to compare
    // against, and any threshold would be invented.
    vi.useFakeTimers();
    try {
      const flight = pausableUpload();
      const { result } = renderHook(() => useFileUpload());
      await act(async () => {
        await result.current.addFiles([createMockFile('big.bin', 4096)]);
      });
      let done!: Promise<unknown>;
      await act(async () => {
        done = result.current.uploadAll('channel-1');
        await Promise.resolve();
      });

      await act(async () => {
        vi.advanceTimersByTime(600_000);
      });
      expect(result.current.files[0].stalled).toBe(false);
      expect(result.current.files[0].status).toBe('preparing');

      await act(async () => {
        flight.release();
        await done;
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it('raises the threshold on a slow link instead of firing at the floor', async () => {
    // 2x median, not a constant. An 8 MiB chunk at 1 Mbps takes ~67 s; firing at
    // 30 s there would mark every slow-but-healthy upload as stalled.
    vi.useFakeTimers();
    try {
      const flight = pausableUpload();
      const { result } = renderHook(() => useFileUpload());
      await act(async () => {
        await result.current.addFiles([createMockFile('big.bin', 4096)]);
      });
      let done!: Promise<unknown>;
      await act(async () => {
        done = result.current.uploadAll('channel-1');
        await Promise.resolve();
      });

      // A 70 s first chunk. Median is now 70 s, so the threshold is 140 s.
      await act(async () => {
        vi.advanceTimersByTime(70_000);
      });
      await act(async () => {
        flight.commitChunk(0, 4);
      });

      await act(async () => {
        vi.advanceTimersByTime(139_999);
      });
      expect(result.current.files[0].stalled).toBe(false);

      await act(async () => {
        vi.advanceTimersByTime(2);
      });
      expect(result.current.files[0].stalled).toBe(true);

      await act(async () => {
        flight.release();
        await done;
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it('goes stalled only after the floor, and clears on the next commit', async () => {
    // History-derived, with a 30 s floor. One 8 MiB chunk on a 1 Mbps link takes
    // ~67 s, so a short fixed timer would false-fire on every slow link and
    // train the user to ignore the signal entirely.
    vi.useFakeTimers();
    try {
      const flight = pausableUpload();
      const { result } = renderHook(() => useFileUpload());
      await act(async () => {
        await result.current.addFiles([createMockFile('big.bin', 4096)]);
      });

      let done!: Promise<unknown>;
      await act(async () => {
        done = result.current.uploadAll('channel-1');
        await Promise.resolve();
      });

      // Commit immediately: the gap is ~0, so the threshold falls to the floor.
      // Idling first would make THAT the median and push the threshold to twice
      // it -- correct behaviour, but it is not what this test is measuring.
      await act(async () => {
        flight.commitChunk(0, 4);
      });
      expect(result.current.files[0].stalled).toBe(false);

      await act(async () => {
        vi.advanceTimersByTime(29_999);
      });
      expect(result.current.files[0].stalled).toBe(false);

      await act(async () => {
        vi.advanceTimersByTime(2);
      });
      expect(result.current.files[0].stalled).toBe(true);

      // A commit is proof of life; the row must stop saying otherwise.
      await act(async () => {
        flight.commitChunk(1, 4);
      });
      expect(result.current.files[0].stalled).toBe(false);

      await act(async () => {
        flight.release();
        await done;
      });
      expect(result.current.files[0].stalled).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it('aborts the in-flight upload when its card is REMOVED', async () => {
    // removeFile dropped the row and revoked its preview URL but never aborted,
    // so the chunk PUT loop kept running against a session the UI no longer
    // showed -- burning the user's bandwidth and ingress budget on a file they
    // just deleted, and leaving the staged bytes to the sweeper's hard TTL.
    // Harmless before the chunked path existed; there was nothing to strand.
    const flight = pausableUpload();
    mockUploadAttachmentChunked.mockClear();
    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([createMockFile('doomed.bin', 4096)]);
    });

    let done!: Promise<unknown>;
    await act(async () => {
      done = result.current.uploadAll('channel-1');
      await Promise.resolve();
    });

    // POSITIVE CONTROL: the upload is genuinely in flight and NOT yet aborted,
    // so the assertion below cannot pass for the wrong reason.
    expect(flight.signal()).toBeDefined();
    expect(flight.signal()?.aborted).toBe(false);

    await act(async () => {
      result.current.removeFile(0);
    });

    expect(flight.signal()?.aborted).toBe(true);

    await act(async () => {
      flight.release();
      await done.catch(() => undefined);
    });
  });

  it('does not start the NEXT file after unmount', async () => {
    // The unmount drain aborts what is in flight, but uploadOnePending CATCHES
    // an abort and returns null, so the loop used to march on: a fresh
    // controller nothing would ever abort, and a new server session added to
    // the Set unmount had just cleared -- a session no client-side path can
    // abandon, left for the sweeper's hard TTL. The old abortRef looked like it
    // guarded exactly this, but was only ever assigned false.
    const flight = pausableUpload();
    // pausableUpload installs an implementation but keeps the call history this
    // describe's earlier tests left behind; the counts below are the assertion.
    mockUploadAttachmentChunked.mockClear();
    const { result, unmount } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([
        createMockFile('one.bin', 4096),
        createMockFile('two.bin', 4096),
      ]);
    });

    let done!: Promise<unknown>;
    await act(async () => {
      done = result.current.uploadAll('channel-1');
      await Promise.resolve();
    });

    // POSITIVE CONTROL: exactly ONE upload is in flight, so "one call" after
    // unmount means the second never started -- not that neither ever did.
    expect(mockUploadAttachmentChunked).toHaveBeenCalledTimes(1);

    unmount();

    await act(async () => {
      flight.release();
      await done.catch(() => undefined);
    });

    expect(mockUploadAttachmentChunked).toHaveBeenCalledTimes(1);
  });

  it('aborts an overflow-file upload on unmount, same as any other', async () => {
    // additionalFiles are not in React state, so they have no card and no
    // cancel control. They used to hold only a RUN-WIDE controller that nothing
    // ever aborted, so leaving the composer mid-upload left the request running
    // and its session staged. They now register in the same per-file map every
    // other upload uses, which the unmount effect drains.
    const flight = pausableUpload();
    const { result, unmount } = renderHook(() => useFileUpload());

    let done!: Promise<unknown>;
    await act(async () => {
      done = result.current.uploadAll('channel-1', undefined, [
        createMockFile('overflow.md', 4096, 'text/markdown'),
      ]);
      await Promise.resolve();
    });

    // POSITIVE CONTROL: the abort assertion below is vacuous unless the upload
    // is actually in flight and holding a signal at unmount time.
    expect(flight.signal()).toBeDefined();
    expect(flight.signal()?.aborted).toBe(false);

    unmount();

    expect(flight.signal()?.aborted).toBe(true);

    await act(async () => {
      flight.release();
      await done.catch(() => undefined);
    });
  });

  it('abandons live sessions on unmount so staged bytes are not orphaned', async () => {
    // Best-effort only: keepalive can still be dropped, which is exactly why the
    // server-side sweeper is load-bearing for correctness, not defence in depth.
    const flight = pausableUpload();
    const { result, unmount } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([createMockFile('big.bin', 4096)]);
    });

    let done!: Promise<unknown>;
    await act(async () => {
      done = result.current.uploadAll('channel-1');
      await Promise.resolve();
    });

    unmount();

    expect(mockAbandonSession).toHaveBeenCalledWith('sess-live');
    expect(flight.signal()?.aborted).toBe(true);

    await act(async () => {
      flight.release();
      await done.catch(() => undefined);
    });
  });

  it('does not chase a session the server already finished', async () => {
    // onSessionClosed fires on commit. Without it the unmount DELETEs a spent
    // id, which is a wasted authenticated request on every successful upload.
    mockUploadAttachmentChunked.mockImplementation(
      async (
        _f: File,
        _k: unknown,
        _c: unknown,
        _s: AbortSignal,
        cb: { onSessionOpened?: (id: string) => void; onSessionClosed?: (id: string) => void }
      ) => {
        cb.onSessionOpened?.('sess-done');
        cb.onSessionClosed?.('sess-done');
        return { file_id: 'x', storage_key: 'k', file_type: 'file', file_size: 4096 };
      }
    );

    const { result, unmount } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([createMockFile('big.bin', 4096)]);
    });
    await act(async () => {
      await result.current.uploadAll('channel-1');
    });

    unmount();

    expect(mockAbandonSession).not.toHaveBeenCalled();
  });
});

/** Shared by the legacy and chunked strip tests: a real PNG carrying a tEXt
 *  chunk with a recognisable payload. Duplicating the construction was how the
 *  chunked path ended up with no strip coverage at all. */
const STRIP_MARKER = [0x47, 0x50, 0x53, 0xde, 0xad, 0xbe, 0xef];

function createMockImageWithMetadata(name: string): File {
  const text = [0x74, 0x45, 0x58, 0x74]; // 'tEXt'
  const chunk = [0x00, 0x00, 0x00, STRIP_MARKER.length, ...text, ...STRIP_MARKER, 0, 0, 0, 0];
  const base = Array.from(MINIMAL_PNG);
  const withText = new Uint8Array([
    ...base.slice(0, base.length - 12),
    ...chunk,
    ...base.slice(base.length - 12),
  ]);
  return new File([withText], name, { type: 'image/png' });
}

function bytesContain(haystack: Uint8Array, needle: number[]): boolean {
  outer: for (let i = 0; i + needle.length <= haystack.length; i++) {
    for (let j = 0; j < needle.length; j++) if (haystack[i + j] !== needle[j]) continue outer;
    return true;
  }
  return false;
}

describe('useFileUpload — the chunked path strips metadata too', () => {
  beforeEach(() => {
    // The chunked mock is module-level and nothing resets it globally, so a
    // call-count assertion otherwise measures every prior test in the file.
    // (The analyzer flagged the same latent order-dependency across this suite.)
    mockUploadAttachmentChunked.mockReset();
  });

  // CRITICALITY 9. The only strip test asserts on encryptFile's arguments,
  // which exist ONLY on the legacy path, and every chunked test used a
  // non-image. mockUploadAttachmentChunked.mock.calls was inspected ZERO times
  // in this file, so changing `stripToUploadable(entry.file)` to `entry.file`
  // passed the whole suite -- shipping GPS and EXIF to every recipient over the
  // path any capability-advertising server selects, unremediable after the fact
  // because the server never sees plaintext.
  it('hands uploadAttachmentChunked a STRIPPED file, not the original', async () => {
    setChunkedCapability(true);
    mockUploadAttachmentChunked.mockResolvedValue({
      file_id: 'stripped-1',
      storage_key: 'attachments/x',
      file_type: 'photo',
      file_size: 100,
    });

    const original = createMockImageWithMetadata('geotagged.png');
    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([original]);
    });
    await act(async () => {
      await result.current.uploadAll('channel-1');
    });

    expect(mockUploadAttachmentChunked).toHaveBeenCalledTimes(1);
    const sent = mockUploadAttachmentChunked.mock.calls[0][0] as File;

    // Not the same object, and the marker bytes are gone.
    expect(sent).not.toBe(original);
    const bytes = new Uint8Array(await sent.arrayBuffer());
    expect(bytesContain(bytes, STRIP_MARKER)).toBe(false);

    // Positive control: the marker WAS present before the strip, so a test that
    // greps for its absence is not asserting against a file that never had it.
    const before = new Uint8Array(await original.arrayBuffer());
    expect(bytesContain(before, STRIP_MARKER)).toBe(true);
  });
});

describe('useFileUpload — envelope version negotiation', () => {
  beforeEach(() => {
    mockUploadAttachmentChunked.mockReset();
    mockUploadAttachmentChunked.mockResolvedValue({
      file_id: 'file-1',
      storage_key: 'attachments/x',
      file_type: 'file',
      file_size: 1,
    });
  });

  async function uploadWithVersions(versions: number[] | undefined): Promise<unknown> {
    setChunkedCapability(true);
    setEnvelopeVersions(versions);
    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([
        createMockFile('negotiated.bin', 4096, 'application/octet-stream'),
      ]);
    });
    await act(async () => {
      await result.current.uploadAll('channel-1');
    });
    return mockUploadAttachmentChunked.mock.calls[0]?.[2];
  }

  it('selects v2 when the server omits envelope versions', async () => {
    const ctx = (await uploadWithVersions(undefined)) as { envelopeVersion?: number };
    expect(ctx.envelopeVersion).toBe(2);
  });

  it('selects v3 only when the server advertises it', async () => {
    const ctx = (await uploadWithVersions([2, 3])) as { envelopeVersion?: number };
    expect(ctx.envelopeVersion).toBe(3);
  });
});
