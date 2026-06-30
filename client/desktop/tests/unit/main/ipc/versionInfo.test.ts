import { describe, it, expect, beforeEach, vi } from 'vitest';

vi.mock('electron', () => ({
  app: { getVersion: vi.fn(() => '0.1.40') },
  ipcMain: { handle: vi.fn() },
}));

vi.mock('@/main/spaState', () => ({
  getRemoteSpaUrl: vi.fn(() => 'https://example.com/spa/abc123/index.html'),
  getSpaHash: vi.fn(() => ''),
  getSpaVersion: vi.fn(() => ''),
  onSpaStateChange: vi.fn(() => () => {}),
}));

import { ipcMain } from 'electron';
import { getRemoteSpaUrl, getSpaHash, getSpaVersion, onSpaStateChange } from '@/main/spaState';
import { registerVersionInfoIpc, extractSpaHash } from '@/main/ipc/versionInfo';

describe('extractSpaHash', () => {
  it('extracts the hash slug from a typical SPA URL with index.html suffix', () => {
    expect(extractSpaHash('https://example.com/spa/abc123/index.html')).toBe('abc123');
  });

  it('extracts the hash slug from a SPA URL with trailing slash', () => {
    expect(extractSpaHash('https://example.com/spa/abc123/')).toBe('abc123');
  });

  it('extracts the hash slug from a SPA URL without trailing slash', () => {
    expect(extractSpaHash('https://example.com/spa/def456')).toBe('def456');
  });

  it('returns null for malformed URL', () => {
    expect(extractSpaHash('not-a-url')).toBeNull();
  });

  it('returns null for null input', () => {
    expect(extractSpaHash(null)).toBeNull();
  });

  it('returns null when path is missing /spa/<hash>/ segment', () => {
    expect(extractSpaHash('https://example.com/other/path/')).toBeNull();
  });
});

describe('registerVersionInfoIpc', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(getRemoteSpaUrl).mockReturnValue('https://example.com/spa/abc123/index.html');
    vi.mocked(getSpaHash).mockReturnValue('');
    vi.mocked(getSpaVersion).mockReturnValue('');
  });

  it('registers the window:getVersionString handler', () => {
    const mockWindow = { webContents: { send: vi.fn() }, isDestroyed: () => false };
    registerVersionInfoIpc(() => mockWindow as never);
    expect(ipcMain.handle).toHaveBeenCalledWith('window:getVersionString', expect.any(Function));
  });

  it('handler returns appVersion + spaHash', () => {
    const mockWindow = { webContents: { send: vi.fn() }, isDestroyed: () => false };
    registerVersionInfoIpc(() => mockWindow as never);
    const handlerCall = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:getVersionString'
    );
    const handler = handlerCall![1];
    expect(handler()).toEqual({ appVersion: '0.1.40', spaHash: 'abc123' });
  });

  it('handler uses captured SPA version before legacy URL parsing', () => {
    vi.mocked(getSpaVersion).mockReturnValue('feedbee');
    const mockWindow = { webContents: { send: vi.fn() }, isDestroyed: () => false };
    registerVersionInfoIpc(() => mockWindow as never);
    const handlerCall = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:getVersionString'
    );
    const handler = handlerCall![1];
    expect(handler()).toEqual({ appVersion: '0.1.40', spaHash: 'feedbee' });
  });

  it('handler falls back to captured HTML hash for flat remote SPA URLs', () => {
    const htmlHash = `sha256:${'b'.repeat(64)}`;
    vi.mocked(getRemoteSpaUrl).mockReturnValue('https://spa.concordvoice.chat/index.html');
    vi.mocked(getSpaHash).mockReturnValue(htmlHash);
    const mockWindow = { webContents: { send: vi.fn() }, isDestroyed: () => false };
    registerVersionInfoIpc(() => mockWindow as never);
    const handlerCall = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:getVersionString'
    );
    const handler = handlerCall![1];
    expect(handler()).toEqual({ appVersion: '0.1.40', spaHash: htmlHash });
  });

  it('handler does not report a captured hash after remote SPA state is cleared', () => {
    const htmlHash = `sha256:${'b'.repeat(64)}`;
    vi.mocked(getRemoteSpaUrl).mockReturnValue(null);
    vi.mocked(getSpaHash).mockReturnValue(htmlHash);
    const mockWindow = { webContents: { send: vi.fn() }, isDestroyed: () => false };
    registerVersionInfoIpc(() => mockWindow as never);
    const handlerCall = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:getVersionString'
    );
    const handler = handlerCall![1];
    expect(handler()).toEqual({ appVersion: '0.1.40', spaHash: null });
  });

  it('subscribes to spa state changes', () => {
    const mockWindow = { webContents: { send: vi.fn() }, isDestroyed: () => false };
    registerVersionInfoIpc(() => mockWindow as never);
    expect(onSpaStateChange).toHaveBeenCalledWith(expect.any(Function));
  });

  it('forwards spa:versionChanged to the window when listener fires', () => {
    const mockWindow = { webContents: { send: vi.fn() }, isDestroyed: () => false };
    registerVersionInfoIpc(() => mockWindow as never);

    const listener = (onSpaStateChange as ReturnType<typeof vi.fn>).mock.calls[0][0];
    listener('https://example.com/spa/feedfa/index.html');

    expect(mockWindow.webContents.send).toHaveBeenCalledWith('spa:versionChanged', {
      spaHash: 'feedfa',
    });
  });

  it('skips sending when window is destroyed', () => {
    const mockWindow = { webContents: { send: vi.fn() }, isDestroyed: () => true };
    registerVersionInfoIpc(() => mockWindow as never);
    const listener = (onSpaStateChange as ReturnType<typeof vi.fn>).mock.calls[0][0];

    listener('https://example.com/spa/dead/index.html');

    expect(mockWindow.webContents.send).not.toHaveBeenCalled();
  });

  it('skips sending when window is null', () => {
    registerVersionInfoIpc(() => null);
    const listener = (onSpaStateChange as ReturnType<typeof vi.fn>).mock.calls[0][0];

    expect(() => listener('https://example.com/spa/nope/index.html')).not.toThrow();
  });
});
