import { describe, it, expect, beforeEach, vi } from 'vitest';

vi.mock('electron', () => ({
  app: { quit: vi.fn() },
  ipcMain: { handle: vi.fn() },
}));

import { app, ipcMain } from 'electron';
import { registerWindowControlsIpc, getCachedClientBehavior } from '@/main/ipc/windowControls';
import { DEFAULT_CLIENT_BEHAVIOR } from '@/shared/clientBehavior';

const makeMockWindow = () => ({
  setTitleBarOverlay: vi.fn(),
  isDestroyed: vi.fn(() => false),
});

describe('registerWindowControlsIpc', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('starts with the safe default cached client behavior (close→tray, minimize→toolbar)', () => {
    const window = makeMockWindow();
    registerWindowControlsIpc(() => window as never);
    expect(getCachedClientBehavior()).toEqual(DEFAULT_CLIENT_BEHAVIOR);
  });

  it('registers all three IPC handlers', () => {
    registerWindowControlsIpc(() => makeMockWindow() as never);
    const channels = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.map((c) => c[0]);
    expect(channels).toContain('window:setClientBehavior');
    expect(channels).toContain('window:quit');
    expect(channels).toContain('window:setTitleBarOverlayColor');
  });

  it('updates the cached client behavior on setClientBehavior', () => {
    registerWindowControlsIpc(() => makeMockWindow() as never);
    const handler = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:setClientBehavior'
    )![1];
    handler({} as never, { toTray: 'minimize', toToolbar: 'close' });
    expect(getCachedClientBehavior()).toEqual({ toTray: 'minimize', toToolbar: 'close' });
  });

  it('window:quit handler calls app.quit()', () => {
    registerWindowControlsIpc(() => makeMockWindow() as never);
    const handler = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:quit'
    )![1];
    handler({} as never);
    expect(app.quit).toHaveBeenCalledTimes(1);
  });

  it('setTitleBarOverlayColor calls win.setTitleBarOverlay with passed color', () => {
    const window = makeMockWindow();
    registerWindowControlsIpc(() => window as never);
    const handler = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:setTitleBarOverlayColor'
    )![1];
    handler({} as never, { color: '#000000', symbolColor: '#ffffff' });
    expect(window.setTitleBarOverlay).toHaveBeenCalledWith(
      expect.objectContaining({ color: '#000000', symbolColor: '#ffffff' })
    );
  });

  it('setTitleBarOverlayColor is a no-op when window is destroyed', () => {
    const window = makeMockWindow();
    window.isDestroyed.mockReturnValue(true);
    registerWindowControlsIpc(() => window as never);
    const handler = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:setTitleBarOverlayColor'
    )![1];
    handler({} as never, { color: '#000', symbolColor: '#fff' });
    expect(window.setTitleBarOverlay).not.toHaveBeenCalled();
  });

  // Input-validation guards added during /reconcile-copilot on #806
  // (security-reviewer findings L1 + L2). The handlers must drop invalid
  // shapes at the IPC trust boundary rather than blindly trusting the
  // renderer's payload.
  it('setClientBehavior rejects invalid toTray value (keeps cache unchanged)', () => {
    registerWindowControlsIpc(() => makeMockWindow() as never);
    const handler = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:setClientBehavior'
    )![1];
    const before = getCachedClientBehavior();
    handler({} as never, { toTray: 'EVIL', toToolbar: 'minimize' });
    expect(getCachedClientBehavior()).toEqual(before);
  });

  it('setClientBehavior rejects non-object payload', () => {
    registerWindowControlsIpc(() => makeMockWindow() as never);
    const handler = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:setClientBehavior'
    )![1];
    const before = getCachedClientBehavior();
    handler({} as never, 'not-an-object');
    expect(getCachedClientBehavior()).toEqual(before);
  });

  it('setClientBehavior accepts the swap configuration', () => {
    registerWindowControlsIpc(() => makeMockWindow() as never);
    const handler = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:setClientBehavior'
    )![1];
    handler({} as never, { toTray: 'minimize', toToolbar: 'close' });
    expect(getCachedClientBehavior()).toEqual({ toTray: 'minimize', toToolbar: 'close' });
  });

  it('setTitleBarOverlayColor rejects non-CSS-color color value', () => {
    const window = makeMockWindow();
    registerWindowControlsIpc(() => window as never);
    const handler = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:setTitleBarOverlayColor'
    )![1];
    handler({} as never, { color: '<script>alert(1)</script>', symbolColor: '#fff' });
    expect(window.setTitleBarOverlay).not.toHaveBeenCalled();
  });

  it('setTitleBarOverlayColor accepts rgba() format', () => {
    const window = makeMockWindow();
    registerWindowControlsIpc(() => window as never);
    const handler = (ipcMain.handle as ReturnType<typeof vi.fn>).mock.calls.find(
      (c) => c[0] === 'window:setTitleBarOverlayColor'
    )![1];
    handler({} as never, { color: 'rgba(26, 22, 48, 1)', symbolColor: '#ffffff' });
    expect(window.setTitleBarOverlay).toHaveBeenCalled();
  });
});
