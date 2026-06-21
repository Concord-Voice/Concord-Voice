// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// ── Hoisted mocks ────────────────────────────────────────────────────────

const mockAutoUpdater = vi.hoisted(() => ({
  logger: null as unknown,
  autoDownload: true,
  autoInstallOnAppQuit: true,
  allowPrerelease: false,
  on: vi.fn(),
  setFeedURL: vi.fn(),
  checkForUpdates: vi.fn().mockResolvedValue(undefined),
  downloadUpdate: vi.fn().mockResolvedValue([]),
  quitAndInstall: vi.fn(),
  verifyUpdateCodeSignature: null as unknown,
}));

const mockLogger = vi.hoisted(() => ({
  info: vi.fn(),
  error: vi.fn(),
  warn: vi.fn(),
  debug: vi.fn(),
  getLogPath: vi.fn().mockReturnValue('/tmp/test-update.log'),
}));

vi.mock('electron-updater', () => ({
  autoUpdater: mockAutoUpdater,
}));

vi.mock('electron', () => ({
  app: {
    getVersion: vi.fn().mockReturnValue('1.0.0'),
    getPath: vi.fn().mockReturnValue('/tmp/test-userdata'),
    isPackaged: true,
  },
  BrowserWindow: vi.fn(),
}));

vi.mock('node:fs', () => ({
  default: {
    readFileSync: vi.fn().mockImplementation(() => {
      throw new Error('ENOENT');
    }),
    writeFileSync: vi.fn(),
    renameSync: vi.fn(),
  },
  readFileSync: vi.fn().mockImplementation(() => {
    throw new Error('ENOENT');
  }),
  writeFileSync: vi.fn(),
  renameSync: vi.fn(),
}));

vi.mock('../../../src/main/tokenManager', () => ({
  getPersistedApiBase: vi.fn().mockReturnValue(null),
}));

vi.mock('../../../src/main/updateLogger', () => ({
  createUpdateLogger: vi.fn().mockReturnValue(mockLogger),
}));

vi.mock('../../../src/main/updateSafety', () => ({
  prepareForUpdate: vi.fn().mockResolvedValue(true),
}));

vi.mock('../../../src/main/splashWindow', () => ({
  updateSplashStatus: vi.fn(),
  showSplashProgress: vi.fn(),
  updateSplashError: vi.fn(),
  showSplash: vi.fn(),
  closeSplash: vi.fn(),
}));

// ── Tests ────────────────────────────────────────────────────────────────

describe('updater', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    vi.resetModules();
  });

  async function loadModule() {
    vi.resetModules();
    const updater = await import('../../../src/main/updater');
    const splash = await import('../../../src/main/splashWindow');
    return { updater, splash };
  }

  /** Return the handler registered for a given autoUpdater event. */
  function getEventHandler(event: string): (...args: unknown[]) => void {
    const call = mockAutoUpdater.on.mock.calls.find((c: unknown[]) => c[0] === event);
    if (!call) throw new Error(`No handler registered for '${event}'`);
    return call[1] as (...args: unknown[]) => void;
  }

  describe('splash integration via updater events', () => {
    it('checking-for-update calls updateSplashStatus with airwaves copy', async () => {
      const { updater, splash } = await loadModule();
      updater.initAutoUpdater(() => null);

      getEventHandler('checking-for-update')();

      expect(splash.updateSplashStatus).toHaveBeenCalledWith('Checking the airwaves...');
    });

    it('update-available calls updateSplashStatus with version string', async () => {
      const { updater, splash } = await loadModule();
      updater.initAutoUpdater(() => null);

      getEventHandler('update-available')({
        version: '2.0.0',
        releaseDate: '2026-01-01',
        releaseNotes: '',
      });

      expect(splash.updateSplashStatus).toHaveBeenCalledWith('New flight plan detected: v2.0.0');
    });

    it('update-not-available calls updateSplashStatus with go copy', async () => {
      const { updater, splash } = await loadModule();
      updater.initAutoUpdater(() => null);

      getEventHandler('update-not-available')({ version: '1.0.0' });

      expect(splash.updateSplashStatus).toHaveBeenCalledWith('All systems go');
    });

    it('download-progress calls showSplashProgress with percent and updateSplashStatus with fuel copy', async () => {
      const { updater, splash } = await loadModule();
      updater.initAutoUpdater(() => null);

      getEventHandler('download-progress')({
        percent: 42.7,
        transferred: 1024,
        total: 2048,
        bytesPerSecond: 512,
      });

      expect(splash.showSplashProgress).toHaveBeenCalledWith(42.7);
      expect(splash.updateSplashStatus).toHaveBeenCalledWith('Fueling up... 43%');
    });

    it('update-downloaded calls showSplashProgress(100) and updateSplashStatus with liftoff copy', async () => {
      const { updater, splash } = await loadModule();
      updater.initAutoUpdater(() => null);

      getEventHandler('update-downloaded')({ version: '2.0.0' });

      expect(splash.showSplashProgress).toHaveBeenCalledWith(100);
      expect(splash.updateSplashStatus).toHaveBeenCalledWith('Ready for liftoff');
    });

    it('error calls updateSplashError', async () => {
      const { updater, splash } = await loadModule();
      updater.initAutoUpdater(() => null);

      getEventHandler('error')(new Error('Network failure'));

      expect(splash.updateSplashError).toHaveBeenCalledWith('Houston, we have a problem');
    });

    it('classifies cert-pin errors (ERR_CERT_*) with cert-pin splash + SECURITY log (#658)', async () => {
      const { updater, splash } = await loadModule();
      updater.initAutoUpdater(() => null);

      getEventHandler('error')(new Error('net::ERR_CERT_AUTHORITY_INVALID'));

      expect(splash.updateSplashError).toHaveBeenCalledWith(
        'Update blocked: cert verification failed'
      );
      expect(mockLogger.error).toHaveBeenCalledWith(
        expect.stringMatching(/SECURITY.*cert-pin verification failed/)
      );
    });

    it('classifies "certificate verify failed" as cert-pin failure (#658)', async () => {
      const { updater, splash } = await loadModule();
      updater.initAutoUpdater(() => null);

      getEventHandler('error')(new Error('certificate verify failed'));

      expect(splash.updateSplashError).toHaveBeenCalledWith(
        'Update blocked: cert verification failed'
      );
    });

    it('preserves publisher-failure classification after cert-pin extension (#658)', async () => {
      const { updater, splash } = await loadModule();
      updater.initAutoUpdater(() => null);

      getEventHandler('error')(new Error('publisher verification failed'));

      expect(splash.updateSplashError).toHaveBeenCalledWith(
        'Update blocked: signature verification failed'
      );
      expect(mockLogger.error).toHaveBeenCalledWith(
        expect.stringMatching(/SECURITY.*signer verification failed/)
      );
    });
  });

  describe('readUpdatePrefs error handling', () => {
    it('returns default on ENOENT without logging', async () => {
      vi.resetModules();
      const fs = await import('node:fs');
      const err = Object.assign(new Error('ENOENT: no such file'), { code: 'ENOENT' });
      // updater.ts uses `import fs from 'node:fs'` (default import), so mock the default export
      vi.mocked(fs.default.readFileSync).mockImplementationOnce(() => {
        throw err;
      });

      // Re-import updater so module-level code re-runs with the new mock
      const { initAutoUpdater } = await import('../../../src/main/updater');
      initAutoUpdater(() => null);

      // ENOENT should NOT surface to logger.error or console.error
      expect(mockLogger.error).not.toHaveBeenCalled();
      // Default is allowPrerelease: true
      expect(mockAutoUpdater.allowPrerelease).toBe(true);
    });

    it('logs and renames the file on SyntaxError', async () => {
      vi.resetModules();
      const fs = await import('node:fs');
      // updater.ts uses `import fs from 'node:fs'` (default import), so mock the default export
      vi.mocked(fs.default.readFileSync).mockImplementationOnce(() => 'not-json{{{');
      const renameSpy = vi.mocked(fs.default.renameSync);

      const { initAutoUpdater } = await import('../../../src/main/updater');
      initAutoUpdater(() => null);

      expect(mockLogger.error).toHaveBeenCalledWith(
        expect.stringMatching(/corrupt.*update-prefs|SyntaxError/i)
      );
      expect(renameSpy).toHaveBeenCalledWith(
        expect.stringMatching(/update-prefs\.json$/),
        expect.stringMatching(/update-prefs\.json\.corrupt\.\d+$/)
      );
      expect(mockAutoUpdater.allowPrerelease).toBe(true);
    });

    it('logs and returns default on EACCES', async () => {
      vi.resetModules();
      const fs = await import('node:fs');
      const err = Object.assign(new Error('EACCES: permission denied'), { code: 'EACCES' });
      // updater.ts uses `import fs from 'node:fs'` (default import), so mock the default export
      vi.mocked(fs.default.readFileSync).mockImplementationOnce(() => {
        throw err;
      });

      const { initAutoUpdater } = await import('../../../src/main/updater');
      initAutoUpdater(() => null);

      expect(mockLogger.error).toHaveBeenCalledWith(expect.stringMatching(/EACCES|permission/i));
      expect(mockAutoUpdater.allowPrerelease).toBe(true);
    });
  });

  describe('verifyUpdateCodeSignature hook (Windows)', () => {
    let originalPlatform: PropertyDescriptor | undefined;

    beforeEach(() => {
      vi.clearAllMocks();
      vi.resetModules();
      originalPlatform = Object.getOwnPropertyDescriptor(process, 'platform');
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
    });

    afterEach(() => {
      if (originalPlatform) Object.defineProperty(process, 'platform', originalPlatform);
    });

    it('returns a non-null rejection string when extractChain throws (fail-closed)', async () => {
      vi.doMock('../../../src/main/verifyWindowsSignature', () => ({
        extractChain: vi.fn().mockRejectedValue(new Error('PS failed')),
        verifyChain: vi.fn(),
      }));
      const { initAutoUpdater } = await import('../../../src/main/updater');
      initAutoUpdater(() => null);
      const hook = (
        mockAutoUpdater as unknown as {
          verifyUpdateCodeSignature: (p: string[], f: string) => Promise<string | null>;
        }
      ).verifyUpdateCodeSignature;
      expect(hook).toBeTypeOf('function');
      const result = await hook(['Concord Voice LLC'], 'C:\\fake\\installer.exe');
      expect(result).toMatch(/PS failed|extractChain threw/);
    });

    it('returns null (pass) when verifyChain returns null', async () => {
      vi.doMock('../../../src/main/verifyWindowsSignature', () => ({
        extractChain: vi.fn().mockResolvedValue([
          {
            subjectCN: 'Concord Voice LLC',
            issuerCN: 'Microsoft ID Verified CS EOC CA 01',
          },
          {
            subjectCN: 'Microsoft ID Verified CS EOC CA 01',
            issuerCN: 'Microsoft Identity Verification Root CA 2020',
          },
        ]),
        verifyChain: vi.fn().mockReturnValue(null),
      }));
      const { initAutoUpdater } = await import('../../../src/main/updater');
      initAutoUpdater(() => null);
      const hook = (
        mockAutoUpdater as unknown as {
          verifyUpdateCodeSignature: (p: string[], f: string) => Promise<string | null>;
        }
      ).verifyUpdateCodeSignature;
      const result = await hook(['Concord Voice LLC'], 'C:\\fake\\installer.exe');
      expect(result).toBeNull();
    });

    it('returns a rejection string and logs SECURITY when verifyChain rejects', async () => {
      vi.doMock('../../../src/main/verifyWindowsSignature', () => ({
        extractChain: vi.fn().mockResolvedValue([
          { subjectCN: 'Evil Co', issuerCN: 'Evil CA' },
          { subjectCN: 'Evil CA', issuerCN: 'Evil Root' },
        ]),
        verifyChain: vi.fn().mockReturnValue('unauthorized publisher: Evil Co'),
      }));
      const { initAutoUpdater } = await import('../../../src/main/updater');
      initAutoUpdater(() => null);
      const hook = (
        mockAutoUpdater as unknown as {
          verifyUpdateCodeSignature: (p: string[], f: string) => Promise<string | null>;
        }
      ).verifyUpdateCodeSignature;
      const result = await hook(['Concord Voice LLC'], 'C:\\fake\\installer.exe');
      expect(result).toMatch(/unauthorized publisher/);
      expect(mockLogger.error).toHaveBeenCalledWith(expect.stringMatching(/SECURITY/));
    });
  });

  describe('forceCheckForUpdates', () => {
    it('triggers autoUpdater.checkForUpdates for attestation_required', async () => {
      const { updater } = await loadModule();
      updater.initAutoUpdater(() => null);

      await updater.forceCheckForUpdates('attestation_required');

      expect(mockAutoUpdater.checkForUpdates).toHaveBeenCalledOnce();
      expect(mockLogger.info).toHaveBeenCalledWith('forceCheckForUpdates: attestation_required');
    });

    it('triggers autoUpdater.checkForUpdates for user_triggered', async () => {
      const { updater } = await loadModule();
      updater.initAutoUpdater(() => null);

      await updater.forceCheckForUpdates('user_triggered');

      expect(mockAutoUpdater.checkForUpdates).toHaveBeenCalledOnce();
      expect(mockLogger.info).toHaveBeenCalledWith('forceCheckForUpdates: user_triggered');
    });

    it('propagates the rejection when the underlying check fails', async () => {
      const { updater } = await loadModule();
      updater.initAutoUpdater(() => null);
      mockAutoUpdater.checkForUpdates.mockRejectedValueOnce(new Error('feed unreachable'));

      await expect(updater.forceCheckForUpdates('attestation_required')).rejects.toThrow(
        'feed unreachable'
      );
    });
  });
});
