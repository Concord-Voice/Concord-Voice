import { describe, it, expect, vi, beforeEach } from 'vitest';

// Mock electron + fs before importing the handler. frameValidation is NOT mocked
// (pure URL parsing) so the real sender-frame allowlist runs — localhost:3001 is
// trusted, an arbitrary https origin is not, mirroring openExternalHandler.test.
vi.mock('electron', () => ({
  dialog: { showSaveDialog: vi.fn() },
  ipcMain: { handle: vi.fn() },
}));
// Mock node:fs/promises with a single writeFile spy exported both as the named
// binding (what the handler imports) and under `default` (node builtins carry a
// default export; omitting it trips vitest's default-export interop).
vi.mock('node:fs/promises', () => {
  const writeFile = vi.fn();
  return { writeFile, default: { writeFile } };
});

import { dialog, ipcMain } from 'electron';
import { writeFile } from 'node:fs/promises';
import { registerSaveImageHandler, safeImageName } from '@/main/ipc/saveImage';

const mockDialog = dialog as unknown as { showSaveDialog: ReturnType<typeof vi.fn> };
const mockIpcMain = ipcMain as unknown as { handle: ReturnType<typeof vi.fn> };
const mockWriteFile = writeFile as unknown as ReturnType<typeof vi.fn>;

interface FakeEvent {
  senderFrame: { url: string };
}
type Handler = (
  event: FakeEvent,
  bytes: unknown,
  name: unknown
) => Promise<{ ok: boolean; canceled?: boolean; reason?: string }>;

const TRUSTED: FakeEvent = { senderFrame: { url: 'http://localhost:3001' } };

describe('image:saveAs IPC handler', () => {
  let handler: Handler;

  beforeEach(() => {
    vi.clearAllMocks();
    mockWriteFile.mockResolvedValue(undefined);
    registerSaveImageHandler(() => null);
    expect(mockIpcMain.handle).toHaveBeenCalledWith('image:saveAs', expect.any(Function));
    handler = mockIpcMain.handle.mock.calls[0][1];
  });

  it('writes the bytes to the user-chosen path on success', async () => {
    mockDialog.showSaveDialog.mockResolvedValue({ canceled: false, filePath: '/tmp/pic.png' });
    const result = await handler(TRUSTED, new Uint8Array([1, 2, 3, 4]).buffer, 'image-1.png');
    expect(result).toEqual({ ok: true });
    expect(mockWriteFile).toHaveBeenCalledWith('/tmp/pic.png', expect.any(Buffer));
    expect([...(mockWriteFile.mock.calls[0][1] as Buffer)]).toEqual([1, 2, 3, 4]);
  });

  it('accepts a typed-array view payload', async () => {
    mockDialog.showSaveDialog.mockResolvedValue({ canceled: false, filePath: '/tmp/v.png' });
    const result = await handler(TRUSTED, new Uint8Array([9, 8, 7]), 'v.png');
    expect(result.ok).toBe(true);
    expect([...(mockWriteFile.mock.calls[0][1] as Buffer)]).toEqual([9, 8, 7]);
  });

  it('treats a user cancel as a no-op success (no write)', async () => {
    mockDialog.showSaveDialog.mockResolvedValue({ canceled: true, filePath: undefined });
    const result = await handler(TRUSTED, new Uint8Array([1]).buffer, 'x.png');
    expect(result).toEqual({ ok: true, canceled: true });
    expect(mockWriteFile).not.toHaveBeenCalled();
  });

  it('rejects an untrusted sender frame without opening a dialog', async () => {
    const result = await handler(
      { senderFrame: { url: 'https://attacker.example' } },
      new Uint8Array([1]).buffer,
      'x.png'
    );
    expect(result).toEqual({ ok: false, reason: 'untrusted-sender' });
    expect(mockDialog.showSaveDialog).not.toHaveBeenCalled();
    expect(mockWriteFile).not.toHaveBeenCalled();
  });

  it('rejects a non-buffer payload', async () => {
    const result = await handler(TRUSTED, 'not-bytes', 'x.png');
    expect(result).toEqual({ ok: false, reason: 'invalid-args' });
    expect(mockDialog.showSaveDialog).not.toHaveBeenCalled();
  });

  it('rejects an empty payload', async () => {
    const result = await handler(TRUSTED, new ArrayBuffer(0), 'x.png');
    expect(result).toEqual({ ok: false, reason: 'invalid-args' });
  });

  it('rejects a payload over the size cap', async () => {
    // 100 MB + 1. ArrayBuffer allocation is lazy and Buffer.from(ArrayBuffer) is a
    // view, so no bytes are paged in — the guard rejects on byteLength alone.
    const result = await handler(TRUSTED, new ArrayBuffer(100 * 1024 * 1024 + 1), 'x.png');
    expect(result).toEqual({ ok: false, reason: 'too-large' });
    expect(mockWriteFile).not.toHaveBeenCalled();
  });

  it('returns write-failed when the disk write throws', async () => {
    mockDialog.showSaveDialog.mockResolvedValue({ canceled: false, filePath: '/tmp/x.png' });
    mockWriteFile.mockRejectedValue(new Error('ENOSPC'));
    const result = await handler(TRUSTED, new Uint8Array([1]).buffer, 'x.png');
    expect(result).toEqual({ ok: false, reason: 'write-failed' });
  });
});

describe('safeImageName', () => {
  it('strips path components (no traversal)', () => {
    expect(safeImageName('../../etc/passwd')).toBe('passwd');
    expect(safeImageName('/abs/path/pic.png')).toBe('pic.png');
  });

  it('replaces reserved characters', () => {
    expect(safeImageName('a<b>c:d.png')).toBe('a_b_c_d.png');
  });

  it('falls back to "image" for empty / non-string input', () => {
    expect(safeImageName('')).toBe('image');
    expect(safeImageName(undefined)).toBe('image');
    expect(safeImageName(123)).toBe('image');
  });
});
