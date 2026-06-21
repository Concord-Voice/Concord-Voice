import { describe, it, expect, vi, beforeEach } from 'vitest';

// Mock electron before importing
vi.mock('electron', () => {
  const openExternal = vi.fn().mockResolvedValue(undefined);
  return {
    shell: { openExternal },
    ipcMain: {
      handle: vi.fn(),
    },
    __mocks__: { openExternal },
  };
});

import { shell, ipcMain } from 'electron';
import { registerOpenExternalHandler } from '@/main/ipc/openExternal';

const mockShell = shell as unknown as { openExternal: ReturnType<typeof vi.fn> };
const mockIpcMain = ipcMain as unknown as { handle: ReturnType<typeof vi.fn> };

interface FakeEvent {
  senderFrame: { url: string };
}

describe('openExternal IPC handler', () => {
  let handler: (event: FakeEvent, url: string) => Promise<{ ok: boolean; reason?: string }>;

  beforeEach(() => {
    vi.clearAllMocks();
    registerOpenExternalHandler();
    expect(mockIpcMain.handle).toHaveBeenCalledWith('open-external', expect.any(Function));
    handler = mockIpcMain.handle.mock.calls[0][1];
  });

  it('opens https URLs from a trusted sender frame', async () => {
    const result = await handler(
      { senderFrame: { url: 'http://localhost:3001' } },
      'https://example.com'
    );
    expect(result.ok).toBe(true);
    expect(mockShell.openExternal).toHaveBeenCalledWith('https://example.com');
  });

  it('opens http URLs', async () => {
    const result = await handler(
      { senderFrame: { url: 'http://localhost:3001' } },
      'http://example.com'
    );
    expect(result.ok).toBe(true);
    expect(mockShell.openExternal).toHaveBeenCalledWith('http://example.com');
  });

  it('opens mailto URLs', async () => {
    const result = await handler(
      { senderFrame: { url: 'http://localhost:3001' } },
      'mailto:a@b.com'
    );
    expect(result.ok).toBe(true);
    expect(mockShell.openExternal).toHaveBeenCalledWith('mailto:a@b.com');
  });

  it('rejects javascript: URLs', async () => {
    const result = await handler(
      { senderFrame: { url: 'http://localhost:3001' } },
      'javascript:alert(1)'
    );
    expect(result.ok).toBe(false);
    expect(mockShell.openExternal).not.toHaveBeenCalled();
  });

  it('rejects data: URLs', async () => {
    const result = await handler(
      { senderFrame: { url: 'http://localhost:3001' } },
      'data:text/html,abc'
    );
    expect(result.ok).toBe(false);
    expect(mockShell.openExternal).not.toHaveBeenCalled();
  });

  it('rejects file: URLs', async () => {
    const result = await handler(
      { senderFrame: { url: 'http://localhost:3001' } },
      'file:///etc/passwd'
    );
    expect(result.ok).toBe(false);
    expect(mockShell.openExternal).not.toHaveBeenCalled();
  });

  it('rejects blob: URLs', async () => {
    const result = await handler(
      { senderFrame: { url: 'http://localhost:3001' } },
      'blob:http://x/1'
    );
    expect(result.ok).toBe(false);
    expect(mockShell.openExternal).not.toHaveBeenCalled();
  });

  it('rejects vbscript: URLs', async () => {
    const result = await handler(
      { senderFrame: { url: 'http://localhost:3001' } },
      'vbscript:msgbox(1)'
    );
    expect(result.ok).toBe(false);
    expect(mockShell.openExternal).not.toHaveBeenCalled();
  });

  it('rejects URLs with non-http/https/mailto protocol even if well-formed', async () => {
    const result = await handler(
      { senderFrame: { url: 'http://localhost:3001' } },
      'ftp://example.com'
    );
    expect(result.ok).toBe(false);
    expect(mockShell.openExternal).not.toHaveBeenCalled();
  });

  it('rejects senders from non-trusted origins', async () => {
    const result = await handler(
      { senderFrame: { url: 'https://attacker.com' } },
      'https://example.com'
    );
    expect(result.ok).toBe(false);
    expect(mockShell.openExternal).not.toHaveBeenCalled();
  });
});
