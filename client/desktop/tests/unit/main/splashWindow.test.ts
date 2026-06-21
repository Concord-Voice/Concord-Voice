// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// ── Hoisted mocks ────────────────────────────────────────────────────────

const mockWebContents = vi.hoisted(() => ({
  executeJavaScript: vi.fn().mockResolvedValue(undefined),
}));

const mockSplashWindow = vi.hoisted(() => ({
  loadURL: vi.fn(),
  once: vi.fn(),
  show: vi.fn(),
  destroy: vi.fn(),
  isDestroyed: vi.fn(() => false),
  getBounds: vi.fn(() => ({ x: 100, y: 200, width: 300, height: 300 })),
  webContents: mockWebContents,
}));

const MockBrowserWindow = vi.hoisted(() =>
  vi.fn().mockImplementation(function () {
    return mockSplashWindow;
  })
);

const mockFs = vi.hoisted(() => ({
  readFileSync: vi.fn(),
  writeFileSync: vi.fn(),
}));

vi.mock('node:fs', () => ({
  default: mockFs,
  ...mockFs,
}));

vi.mock('electron', () => ({
  BrowserWindow: MockBrowserWindow,
  app: {
    getPath: vi.fn(() => '/tmp/test-userdata'),
  },
  screen: {
    getPrimaryDisplay: vi.fn(() => ({
      workAreaSize: { width: 1920, height: 1080 },
    })),
    getAllDisplays: vi.fn(() => [{ bounds: { x: 0, y: 0, width: 1920, height: 1080 } }]),
  },
}));

// ── Tests ────────────────────────────────────────────────────────────────

describe('splashWindow', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockSplashWindow.isDestroyed.mockReturnValue(false);
    // Default: no saved position — readFileSync throws ENOENT
    mockFs.readFileSync.mockImplementation(() => {
      throw new Error('ENOENT');
    });
  });

  afterEach(() => {
    vi.resetModules();
  });

  async function loadModule() {
    vi.resetModules();
    return import('../../../src/main/splashWindow');
  }

  describe('showSplash', () => {
    it('creates a frameless, movable BrowserWindow with correct dimensions', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash();

      expect(MockBrowserWindow).toHaveBeenCalledWith(
        expect.objectContaining({
          width: 300,
          height: 300,
          frame: false,
          resizable: false,
          movable: true,
          alwaysOnTop: true,
          skipTaskbar: true,
          backgroundColor: '#0d0821',
          show: false,
        })
      );

      closeSplash();
    });

    it('centers the window on the primary display when no saved position', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash();

      const opts = MockBrowserWindow.mock.calls[0][0];
      // (1920 - 300) / 2 = 810, (1080 - 300) / 2 = 390
      expect(opts.x).toBe(810);
      expect(opts.y).toBe(390);

      closeSplash();
    });

    it('uses saved position when valid and on-screen', async () => {
      mockFs.readFileSync.mockReturnValue(JSON.stringify({ x: 500, y: 300 }));
      const { showSplash, closeSplash } = await loadModule();
      showSplash();

      const opts = MockBrowserWindow.mock.calls[0][0];
      expect(opts.x).toBe(500);
      expect(opts.y).toBe(300);

      closeSplash();
    });

    it('falls back to center when saved position is off-screen', async () => {
      // (9000, 9000) is outside the mocked 1920×1080 display
      mockFs.readFileSync.mockReturnValue(JSON.stringify({ x: 9000, y: 9000 }));
      const { showSplash, closeSplash } = await loadModule();
      showSplash();

      const opts = MockBrowserWindow.mock.calls[0][0];
      expect(opts.x).toBe(810);
      expect(opts.y).toBe(390);

      closeSplash();
    });

    it('loads a data URL with brand HTML', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash();

      expect(mockSplashWindow.loadURL).toHaveBeenCalledWith(
        expect.stringContaining('data:text/html;charset=utf-8,')
      );

      closeSplash();
    });

    it('registers ready-to-show handler that shows the window', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash();

      expect(mockSplashWindow.once).toHaveBeenCalledWith('ready-to-show', expect.any(Function));

      // Simulate ready-to-show
      const handler = mockSplashWindow.once.mock.calls.find(
        (c: unknown[]) => c[0] === 'ready-to-show'
      );
      handler![1]();
      expect(mockSplashWindow.show).toHaveBeenCalled();

      closeSplash();
    });

    it('does not create a second window if splash already open', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash();
      showSplash(); // second call — should be no-op

      expect(MockBrowserWindow).toHaveBeenCalledTimes(1);

      closeSplash();
    });

    it('embeds icon data URL in all three img placeholders', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash('data:image/png;base64,TESTICON');

      const url = mockSplashWindow.loadURL.mock.calls[0][0] as string;
      const html = decodeURIComponent(url.replace('data:text/html;charset=utf-8,', ''));
      // Three img elements (pulse logo, base, fill) should all contain the icon
      const matches = html.match(/TESTICON/g);
      expect(matches).toHaveLength(3);

      closeSplash();
    });
  });

  describe('updateSplashStatus', () => {
    it('executes JavaScript to update status text', async () => {
      const { showSplash, updateSplashStatus, closeSplash } = await loadModule();
      showSplash();
      updateSplashStatus('Downloading update...');

      expect(mockWebContents.executeJavaScript).toHaveBeenCalledWith(
        expect.stringContaining('Downloading update...')
      );

      closeSplash();
    });

    it('uses JSON.stringify for safe encoding (single quotes, backslashes, unicode line terminators)', async () => {
      const { showSplash, updateSplashStatus, closeSplash } = await loadModule();
      showSplash();
      updateSplashStatus("It's updating");

      // JSON.stringify wraps in double quotes — single quotes need no escaping
      expect(mockWebContents.executeJavaScript).toHaveBeenCalledWith(
        expect.stringContaining('"It\'s updating"')
      );

      closeSplash();
    });

    it('is a no-op when no splash window exists', async () => {
      const { updateSplashStatus } = await loadModule();
      // No showSplash() called — should not throw
      expect(() => updateSplashStatus('test')).not.toThrow();
      expect(mockWebContents.executeJavaScript).not.toHaveBeenCalled();
    });

    it('is a no-op when splash window is destroyed', async () => {
      const { showSplash, updateSplashStatus, closeSplash } = await loadModule();
      showSplash();
      closeSplash();
      mockSplashWindow.isDestroyed.mockReturnValue(true);

      expect(() => updateSplashStatus('test')).not.toThrow();
    });
  });

  describe('showSplashProgress', () => {
    it('switches to fill mode and sets clip-path at 50%', async () => {
      const { showSplash, showSplashProgress, closeSplash } = await loadModule();
      showSplash();
      showSplashProgress(50);

      const js = mockWebContents.executeJavaScript.mock.calls[0][0] as string;
      expect(js).toContain('state-pulse');
      expect(js).toContain('state-fill');
      expect(js).toContain('inset(50% 0 0 0)');

      closeSplash();
    });

    it('produces inset(100% 0 0 0) at percent=0 (nothing visible)', async () => {
      const { showSplash, showSplashProgress, closeSplash } = await loadModule();
      showSplash();
      showSplashProgress(0);

      expect(mockWebContents.executeJavaScript).toHaveBeenCalledWith(
        expect.stringContaining('inset(100% 0 0 0)')
      );

      closeSplash();
    });

    it('produces inset(0% 0 0 0) at percent=100 (fully visible)', async () => {
      const { showSplash, showSplashProgress, closeSplash } = await loadModule();
      showSplash();
      showSplashProgress(100);

      expect(mockWebContents.executeJavaScript).toHaveBeenCalledWith(
        expect.stringContaining('inset(0% 0 0 0)')
      );

      closeSplash();
    });

    it('clamps percent below 0 to 0 (inset(100%))', async () => {
      const { showSplash, showSplashProgress, closeSplash } = await loadModule();
      showSplash();
      showSplashProgress(-10);

      expect(mockWebContents.executeJavaScript).toHaveBeenCalledWith(
        expect.stringContaining('inset(100% 0 0 0)')
      );

      closeSplash();
    });

    it('clamps percent above 100 to 100 (inset(0%))', async () => {
      const { showSplash, showSplashProgress, closeSplash } = await loadModule();
      showSplash();
      showSplashProgress(110);

      expect(mockWebContents.executeJavaScript).toHaveBeenCalledWith(
        expect.stringContaining('inset(0% 0 0 0)')
      );

      closeSplash();
    });

    it('is a no-op when no splash window exists', async () => {
      const { showSplashProgress } = await loadModule();
      expect(() => showSplashProgress(50)).not.toThrow();
      expect(mockWebContents.executeJavaScript).not.toHaveBeenCalled();
    });

    it('is a no-op when splash window is destroyed', async () => {
      const { showSplash, showSplashProgress, closeSplash } = await loadModule();
      showSplash();
      closeSplash();
      mockSplashWindow.isDestroyed.mockReturnValue(true);

      expect(() => showSplashProgress(50)).not.toThrow();
    });
  });

  describe('updateSplashError', () => {
    it('applies logo-error class, sets message via JSON.stringify, and adds status-error class', async () => {
      const { showSplash, updateSplashError, closeSplash } = await loadModule();
      showSplash();
      updateSplashError('Update to v1.2.3 failed');

      expect(mockWebContents.executeJavaScript).toHaveBeenCalledWith(
        expect.stringContaining('logo-error')
      );
      expect(mockWebContents.executeJavaScript).toHaveBeenCalledWith(
        expect.stringContaining('"Update to v1.2.3 failed"')
      );
      expect(mockWebContents.executeJavaScript).toHaveBeenCalledWith(
        expect.stringContaining('status-error')
      );

      closeSplash();
    });

    it('switches back to pulse state so error is visible even after fill-progress mode', async () => {
      const { showSplash, showSplashProgress, updateSplashError, closeSplash } = await loadModule();
      showSplash();
      showSplashProgress(50); // enter fill mode
      updateSplashError('Something went wrong');

      const js = mockWebContents.executeJavaScript.mock.calls.at(-1)![0] as string;
      expect(js).toContain('state-fill');
      expect(js).toContain('state-pulse');
      expect(js).toContain('logo-error');

      closeSplash();
    });

    it('is a no-op when no splash window exists', async () => {
      const { updateSplashError } = await loadModule();
      expect(() => updateSplashError('error')).not.toThrow();
      expect(mockWebContents.executeJavaScript).not.toHaveBeenCalled();
    });

    it('is a no-op when splash window is destroyed', async () => {
      const { showSplash, updateSplashError, closeSplash } = await loadModule();
      showSplash();
      closeSplash();
      mockSplashWindow.isDestroyed.mockReturnValue(true);

      expect(() => updateSplashError('error')).not.toThrow();
    });
  });

  describe('closeSplash', () => {
    it('saves window position before destroying', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash();
      closeSplash();

      expect(mockSplashWindow.getBounds).toHaveBeenCalled();
      expect(mockFs.writeFileSync).toHaveBeenCalledWith(
        expect.stringContaining('splash-position.json'),
        JSON.stringify({ x: 100, y: 200 })
      );
    });

    it('destroys the splash window', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash();
      closeSplash();

      expect(mockSplashWindow.destroy).toHaveBeenCalled();
    });

    it('is idempotent (calling twice does not throw)', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash();
      closeSplash();
      mockSplashWindow.isDestroyed.mockReturnValue(true);

      expect(() => closeSplash()).not.toThrow();
      // destroy should only be called once
      expect(mockSplashWindow.destroy).toHaveBeenCalledTimes(1);
    });

    it('is a no-op when no splash window was created', async () => {
      const { closeSplash } = await loadModule();
      expect(() => closeSplash()).not.toThrow();
      expect(mockSplashWindow.destroy).not.toHaveBeenCalled();
    });

    it('clears the reference when window was already destroyed externally', async () => {
      const { showSplash, closeSplash, updateSplashStatus } = await loadModule();
      showSplash();
      // Simulate the window being destroyed by Electron (e.g. OS close)
      mockSplashWindow.isDestroyed.mockReturnValue(true);
      closeSplash();

      // destroy() should NOT have been called (already destroyed)
      expect(mockSplashWindow.destroy).not.toHaveBeenCalled();
      // Reference should be nulled — updateSplashStatus should be a no-op
      expect(() => updateSplashStatus('test')).not.toThrow();
      expect(mockWebContents.executeJavaScript).not.toHaveBeenCalled();
    });

    it('does not save position when window is already destroyed externally', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash();
      mockSplashWindow.isDestroyed.mockReturnValue(true);
      closeSplash();

      // savePosition() guards on isDestroyed() — no write should happen
      expect(mockFs.writeFileSync).not.toHaveBeenCalled();
    });

    it('allows showSplash to create a new window after close', async () => {
      const { showSplash, closeSplash } = await loadModule();
      showSplash();
      closeSplash();
      mockSplashWindow.isDestroyed.mockReturnValue(true);

      // Reset mock to track new call
      MockBrowserWindow.mockClear();
      showSplash();

      expect(MockBrowserWindow).toHaveBeenCalledTimes(1);
      closeSplash();
    });
  });
});
