// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { ALLOWED_WINDOWS_PUBLISHERS } from '../../../src/shared/allowedWindowsPublishers';

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

const mockVerifyLinuxArtifact = vi.hoisted(() => vi.fn());
const mockDiscardPreparedUpdate = vi.hoisted(() => vi.fn());
const mockPrepareForUpdate = vi.hoisted(() => vi.fn().mockResolvedValue(true));
const PUBLIC_RECOVERY_FEED_URL =
  'https://github.com/Concord-Voice/Concord-Voice/releases/latest/download';

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
  net: { fetch: vi.fn() },
}));

vi.mock('../../../src/main/verifyLinuxSignature', () => ({
  verifyLinuxArtifact: mockVerifyLinuxArtifact,
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
  prepareForUpdate: mockPrepareForUpdate,
  discardPreparedUpdate: mockDiscardPreparedUpdate,
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
    vi.useRealTimers();
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
      expect(result).toMatch(/signature verification failed/i);
      expect(result).not.toContain('PS failed');
      expect(JSON.stringify(mockLogger.error.mock.calls)).not.toContain('PS failed');
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
      expect(result).toMatch(/signature verification failed/i);
      expect(result).not.toContain('Evil Co');
      expect(mockLogger.error).toHaveBeenCalledWith(expect.stringMatching(/SECURITY/));
      expect(JSON.stringify(mockLogger.error.mock.calls)).not.toContain('Evil Co');
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

  describe('public recovery feed (#1981)', () => {
    it('configures and schedules the public recovery feed when no API base is persisted', async () => {
      vi.useFakeTimers();
      const tm = await import('../../../src/main/tokenManager');
      vi.mocked(tm.getPersistedApiBase).mockReturnValue(null);
      const { updater } = await loadModule();

      updater.initAutoUpdater(() => null);
      await vi.advanceTimersByTimeAsync(10_000);

      expect(mockAutoUpdater.setFeedURL).toHaveBeenCalledWith(
        expect.objectContaining({
          provider: 'generic',
          url: PUBLIC_RECOVERY_FEED_URL,
        })
      );
      expect(mockAutoUpdater.checkForUpdates).toHaveBeenCalledOnce();
    });

    it('does not replace the public recovery feed with the API feed after login', async () => {
      const { updater } = await loadModule();

      updater.setUpdateFeedUrl('https://api.concordvoice.chat');

      expect(mockAutoUpdater.setFeedURL).toHaveBeenCalledWith(
        expect.objectContaining({
          provider: 'generic',
          url: PUBLIC_RECOVERY_FEED_URL,
        })
      );
      expect(mockAutoUpdater.setFeedURL).not.toHaveBeenCalledWith(
        expect.objectContaining({
          url: 'https://api.concordvoice.chat/api/v1/updates',
        })
      );
    });
  });

  describe('Linux signature gate (#653)', () => {
    let originalPlatform: PropertyDescriptor | undefined;
    const mockSend = vi.fn();
    const mockWindow = {
      isDestroyed: () => false,
      webContents: { send: mockSend },
    } as unknown as import('electron').BrowserWindow;

    beforeEach(async () => {
      vi.clearAllMocks();
      vi.resetModules();
      mockSend.mockClear();
      originalPlatform = Object.getOwnPropertyDescriptor(process, 'platform');
      const tm = await import('../../../src/main/tokenManager');
      vi.mocked(tm.getPersistedApiBase).mockReturnValue('https://api.concordvoice.chat');
    });

    afterEach(() => {
      if (originalPlatform) Object.defineProperty(process, 'platform', originalPlatform);
    });

    function setPlatform(p: NodeJS.Platform): void {
      Object.defineProperty(process, 'platform', { value: p, configurable: true });
    }

    /** Arm a downloaded update (sets pendingUpdateVersion + pendingDownloadedFile). */
    function armDownloaded(downloadedFile: string | undefined): void {
      const call = mockAutoUpdater.on.mock.calls.find(
        (c: unknown[]) => c[0] === 'update-downloaded'
      );
      if (!call) throw new Error("no 'update-downloaded' handler registered");
      (call[1] as (e: unknown) => void)({ version: '2.0.0', downloadedFile });
    }

    it('refuses install + shows the security banner on a TAMPERED signature (linux)', async () => {
      setPlatform('linux');
      mockVerifyLinuxArtifact.mockResolvedValue({
        verified: false,
        reason: 'signature does not verify',
        kind: 'tampered',
      });
      const updater = await import('../../../src/main/updater');
      updater.initAutoUpdater(() => mockWindow);
      armDownloaded('/tmp/cache/ConcordVoice-2.0.0-linux-x64.AppImage');

      await updater.safeQuitAndInstall();

      expect(mockVerifyLinuxArtifact).toHaveBeenCalledOnce();
      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();
      expect(mockSend).toHaveBeenCalledWith(
        'update:error',
        expect.objectContaining({ securityEvent: true, subtype: 'signature-failure' })
      );
    });

    it('refuses install with a retryable, NON-security message when UNAVAILABLE (linux)', async () => {
      setPlatform('linux');
      mockVerifyLinuxArtifact.mockResolvedValue({
        verified: false,
        reason: 'signature fetch failed: HTTP 503',
        kind: 'unavailable',
      });
      const updater = await import('../../../src/main/updater');
      updater.initAutoUpdater(() => mockWindow);
      armDownloaded('/tmp/cache/ConcordVoice-2.0.0-linux-x64.AppImage');

      await updater.safeQuitAndInstall();

      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();
      // update:error IS dispatched, but NOT as a security event — a transient
      // blip must not raise the tamper banner (#653 / Gitar review).
      const errorCalls = mockSend.mock.calls.filter((c) => c[0] === 'update:error');
      expect(errorCalls.length).toBeGreaterThan(0);
      for (const [, payload] of errorCalls) {
        expect((payload as { securityEvent?: boolean }).securityEvent).not.toBe(true);
        expect((payload as { subtype?: string }).subtype).not.toBe('signature-failure');
      }
    });

    it('derives the .sig URL from the downloaded artifact basename (linux)', async () => {
      setPlatform('linux');
      mockVerifyLinuxArtifact.mockResolvedValue({ verified: true });
      const updater = await import('../../../src/main/updater');
      updater.initAutoUpdater(() => mockWindow);
      armDownloaded('/tmp/cache/ConcordVoice-2.0.0-linux-x64.AppImage');

      await updater.safeQuitAndInstall();

      expect(mockVerifyLinuxArtifact).toHaveBeenCalledWith(
        '/tmp/cache/ConcordVoice-2.0.0-linux-x64.AppImage',
        'https://github.com/Concord-Voice/Concord-Voice/releases/latest/download/ConcordVoice-2.0.0-linux-x64.AppImage.sig',
        expect.any(Function)
      );
      expect(mockAutoUpdater.quitAndInstall).toHaveBeenCalledOnce();
    });

    it('fail-closed (refuse) when no downloaded path was captured — unavailable, not tamper (linux)', async () => {
      setPlatform('linux');
      const updater = await import('../../../src/main/updater');
      updater.initAutoUpdater(() => mockWindow);
      armDownloaded(undefined);

      await updater.safeQuitAndInstall();

      // A missing path is a config/availability state, not evidence of
      // tampering: refuse without calling the verifier and without the banner.
      expect(mockVerifyLinuxArtifact).not.toHaveBeenCalled();
      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();
      const errorCalls = mockSend.mock.calls.filter((c) => c[0] === 'update:error');
      expect(errorCalls.length).toBeGreaterThan(0);
      for (const [, payload] of errorCalls) {
        expect((payload as { securityEvent?: boolean }).securityEvent).not.toBe(true);
      }
    });

    it('does NOT verify on darwin (mac path unchanged)', async () => {
      setPlatform('darwin');
      const updater = await import('../../../src/main/updater');
      updater.initAutoUpdater(() => mockWindow);
      armDownloaded('/tmp/cache/ConcordVoice-2.0.0-macos.zip');

      await updater.safeQuitAndInstall();

      expect(mockVerifyLinuxArtifact).not.toHaveBeenCalled();
      expect(mockAutoUpdater.quitAndInstall).toHaveBeenCalledOnce();
    });

    it('releases the close-to-tray quit veto before quitAndInstall', async () => {
      setPlatform('darwin');
      const releaseQuitVeto = vi.fn();
      const updater = await import('../../../src/main/updater');
      updater.initAutoUpdater(() => mockWindow, releaseQuitVeto);
      armDownloaded('/tmp/cache/ConcordVoice-2.0.0-macos.zip');

      await updater.safeQuitAndInstall();

      const releaseOrder = releaseQuitVeto.mock.invocationCallOrder[0];
      const quitOrder = mockAutoUpdater.quitAndInstall.mock.invocationCallOrder[0];
      expect(releaseQuitVeto).toHaveBeenCalledOnce();
      expect(releaseOrder).toBeDefined();
      expect(quitOrder).toBeDefined();
      expect(releaseOrder!).toBeLessThan(quitOrder!);
    });

    it('does not run the Linux verifier on win32', async () => {
      setPlatform('win32');
      const extractWindowsChain = vi.fn().mockResolvedValue([]);
      vi.doMock('../../../src/main/verifyWindowsSignature', () => ({
        extractChain: extractWindowsChain,
        verifyChain: vi.fn().mockReturnValue(null),
      }));
      const updater = await import('../../../src/main/updater');
      updater.initAutoUpdater(() => mockWindow);
      armDownloaded('C:/cache/ConcordVoice-2.0.0-windows-x64-Setup.exe');

      await updater.safeQuitAndInstall();

      expect(mockVerifyLinuxArtifact).not.toHaveBeenCalled();
      expect(extractWindowsChain).toHaveBeenCalledWith(
        'C:/cache/ConcordVoice-2.0.0-windows-x64-Setup.exe'
      );
      expect(mockAutoUpdater.quitAndInstall).toHaveBeenCalledOnce();
    });
  });

  describe('cached Windows install verification (#2455)', () => {
    let originalPlatform: PropertyDescriptor | undefined;
    const extractChain = vi.fn();
    const releaseQuitVeto = vi.fn();
    const send = vi.fn();
    const window = {
      isDestroyed: () => false,
      webContents: { send },
    } as unknown as import('electron').BrowserWindow;
    const installer = 'C:/owned-cache/ConcordVoice/pending/update.exe';
    const validChain = [
      { subjectCN: ALLOWED_WINDOWS_PUBLISHERS[0], issuerCN: 'Microsoft ID Verified CS EOC CA 01' },
      {
        subjectCN: 'Microsoft ID Verified CS EOC CA 01',
        issuerCN: 'Microsoft Identity Verification Root CA 2020',
      },
    ];

    beforeEach(() => {
      originalPlatform = Object.getOwnPropertyDescriptor(process, 'platform');
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
      extractChain.mockReset().mockResolvedValue(validChain);
      releaseQuitVeto.mockReset();
      send.mockReset();
      mockDiscardPreparedUpdate.mockReset();
      mockPrepareForUpdate.mockReset().mockResolvedValue(true);
      mockAutoUpdater.downloadUpdate.mockReset().mockResolvedValue([]);
      vi.doMock('../../../src/main/verifyWindowsSignature', async (importOriginal) => {
        const actual =
          await importOriginal<typeof import('../../../src/main/verifyWindowsSignature')>();
        return { ...actual, extractChain };
      });
    });

    afterEach(() => {
      if (originalPlatform) Object.defineProperty(process, 'platform', originalPlatform);
      vi.doUnmock('../../../src/main/verifyWindowsSignature');
    });

    async function armCachedUpdate(downloadedFile: string | undefined = installer) {
      const { updater } = await loadModule();
      updater.initAutoUpdater(() => window, releaseQuitVeto);
      // A cache hit emits this event without invoking the download-time hook.
      getEventHandler('update-downloaded')({ version: '2.0.0', downloadedFile });
      expect(extractChain).not.toHaveBeenCalled();
      expect(mockAutoUpdater.downloadUpdate).not.toHaveBeenCalled();
      return updater;
    }

    function deferred<T>() {
      let resolve!: (value: T) => void;
      const promise = new Promise<T>((done) => {
        resolve = done;
      });
      return { promise, resolve };
    }

    it('accepts the valid fixture through the real pinned-chain download hook (positive control)', async () => {
      await armCachedUpdate();
      const hook = mockAutoUpdater.verifyUpdateCodeSignature as (
        publishers: string[],
        file: string
      ) => Promise<string | null>;

      await expect(hook([...ALLOWED_WINDOWS_PUBLISHERS], installer)).resolves.toBeNull();
      expect(extractChain).toHaveBeenCalledWith(installer);
      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();
    });

    it('a refused cached installer sends the tray back to "Download update"', async () => {
      extractChain.mockRejectedValue(new Error('private PowerShell diagnostic'));
      const { updater } = await loadModule();
      const onTray = vi.fn();
      updater.initAutoUpdater(() => window, releaseQuitVeto, onTray);
      getEventHandler('update-downloaded')({ version: '2.0.0', downloadedFile: installer });
      expect(onTray).toHaveBeenLastCalledWith('ready');
      await updater.safeQuitAndInstall();
      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();
      expect(onTray).toHaveBeenLastCalledWith('available');
    });

    it('verifies the cached artifact before releasing the quit veto and installing', async () => {
      const updater = await armCachedUpdate();
      await updater.safeQuitAndInstall();

      expect(
        extractChain,
        'cached Windows installs must run Authenticode extraction'
      ).toHaveBeenCalledWith(installer);
      expect(mockAutoUpdater.quitAndInstall).toHaveBeenCalledOnce();
      expect(releaseQuitVeto).toHaveBeenCalledOnce();
      expect(extractChain.mock.invocationCallOrder[0]).toBeLessThan(
        releaseQuitVeto.mock.invocationCallOrder[0]
      );
      expect(releaseQuitVeto.mock.invocationCallOrder[0]).toBeLessThan(
        mockAutoUpdater.quitAndInstall.mock.invocationCallOrder[0]
      );
    });

    it.each(['wrong publisher', 'wrong intermediate', 'extraction unavailable'])(
      'refuses a cached artifact when %s and discards the rejected selection',
      async (failure) => {
        if (failure === 'wrong publisher') {
          extractChain.mockResolvedValue([
            { ...validChain[0], subjectCN: 'Untrusted Publisher' },
            validChain[1],
          ]);
        } else if (failure === 'wrong intermediate') {
          extractChain.mockResolvedValue([
            { ...validChain[0], issuerCN: 'Untrusted CA' },
            validChain[1],
          ]);
        } else {
          extractChain.mockRejectedValue(new Error('private PowerShell diagnostic'));
        }
        const updater = await armCachedUpdate();
        await updater.safeQuitAndInstall();

        expect(
          mockAutoUpdater.quitAndInstall,
          'cached artifacts must not install without a valid pinned Authenticode chain'
        ).not.toHaveBeenCalled();
        expect(releaseQuitVeto).not.toHaveBeenCalled();
        expect(mockDiscardPreparedUpdate).toHaveBeenCalled();
        expect(send).toHaveBeenCalledWith(
          'update:error',
          expect.objectContaining({ message: expect.any(String) })
        );
        expect(JSON.stringify(send.mock.calls)).not.toContain('private PowerShell diagnostic');
      }
    );

    it('refuses a cached Windows event with no artifact path', async () => {
      const updater = await armCachedUpdate('');
      await updater.safeQuitAndInstall();

      expect(
        mockAutoUpdater.quitAndInstall,
        'a missing Windows artifact cannot be verified or installed'
      ).not.toHaveBeenCalled();
      expect(releaseQuitVeto).not.toHaveBeenCalled();
      expect(extractChain).not.toHaveBeenCalled();
    });

    it('blocks installation while an admitted download is changing the upstream selection', async () => {
      const updater = await armCachedUpdate();
      const download = deferred<string[]>();
      mockAutoUpdater.downloadUpdate.mockReturnValue(download.promise);
      const downloading = updater.downloadUpdate();
      await updater.safeQuitAndInstall();

      expect(mockPrepareForUpdate).not.toHaveBeenCalled();
      expect(extractChain).not.toHaveBeenCalled();
      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();
      download.resolve([installer]);
      await downloading;
      await updater.safeQuitAndInstall();
      expect(extractChain).toHaveBeenCalledWith(installer);
      expect(mockAutoUpdater.quitAndInstall).toHaveBeenCalledOnce();
    });

    it('blocks a download and duplicate install while verification owns the updater', async () => {
      const updater = await armCachedUpdate();
      const verification = deferred<typeof validChain>();
      const started = deferred<void>();
      extractChain.mockImplementation(() => {
        started.resolve();
        return verification.promise;
      });
      const installing = updater.safeQuitAndInstall();
      await started.promise;

      await expect(updater.downloadUpdate()).rejects.toThrow(/install|progress/i);
      await updater.safeQuitAndInstall();
      expect(mockAutoUpdater.downloadUpdate).not.toHaveBeenCalled();
      expect(mockPrepareForUpdate).toHaveBeenCalledOnce();
      expect(extractChain).toHaveBeenCalledOnce();
      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();
      verification.resolve(validChain);
      await installing;
      expect(mockAutoUpdater.quitAndInstall).toHaveBeenCalledOnce();
    });

    it('a failed admitted download does not leave installation permanently blocked', async () => {
      const updater = await armCachedUpdate();
      mockAutoUpdater.downloadUpdate.mockRejectedValueOnce(new Error('download unavailable'));

      await expect(updater.downloadUpdate()).rejects.toThrow('download unavailable');
      await updater.safeQuitAndInstall();

      expect(extractChain).toHaveBeenCalledWith(installer);
      expect(mockAutoUpdater.quitAndInstall).toHaveBeenCalledOnce();
    });

    it('rejects a same-path same-version replacement event during verification without purging its bytes', async () => {
      const updater = await armCachedUpdate();
      const verification = deferred<typeof validChain>();
      const started = deferred<void>();
      extractChain.mockImplementationOnce(() => {
        started.resolve();
        return verification.promise;
      });
      const installing = updater.safeQuitAndInstall();
      await started.promise;
      getEventHandler('update-downloaded')({ version: '2.0.0', downloadedFile: installer });
      verification.resolve(validChain);
      await installing;

      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();
      expect(releaseQuitVeto).not.toHaveBeenCalled();
      expect(mockDiscardPreparedUpdate).toHaveBeenCalledExactlyOnceWith(mockLogger, false);
      await updater.safeQuitAndInstall();
      expect(extractChain).toHaveBeenCalledTimes(2);
      expect(mockAutoUpdater.quitAndInstall).toHaveBeenCalledOnce();
    });

    it('detects replacement during preparation before verifying or deleting the successor', async () => {
      const updater = await armCachedUpdate();
      const preparation = deferred<boolean>();
      mockPrepareForUpdate.mockReturnValueOnce(preparation.promise);
      const installing = updater.safeQuitAndInstall();
      getEventHandler('update-downloaded')({
        version: '3.0.0',
        downloadedFile: 'C:/owned-cache/new.exe',
      });
      preparation.resolve(true);
      await installing;

      expect(extractChain).not.toHaveBeenCalled();
      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();
      expect(mockDiscardPreparedUpdate).toHaveBeenCalledExactlyOnceWith(mockLogger, false);
    });

    it('a rejected selection cannot be retried and a later download can establish a valid new selection', async () => {
      const updater = await armCachedUpdate();
      extractChain.mockRejectedValueOnce(new Error('unavailable'));
      await updater.safeQuitAndInstall();
      await updater.safeQuitAndInstall();
      expect(extractChain).toHaveBeenCalledOnce();
      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();

      await updater.downloadUpdate();
      getEventHandler('update-downloaded')({ version: '2.0.0', downloadedFile: installer });
      await updater.safeQuitAndInstall();
      expect(extractChain).toHaveBeenCalledTimes(2);
      expect(mockAutoUpdater.quitAndInstall).toHaveBeenCalledOnce();
    });

    it('still refuses installation when rejected-artifact cleanup throws', async () => {
      const updater = await armCachedUpdate();
      extractChain.mockRejectedValue(new Error('unavailable'));
      mockDiscardPreparedUpdate.mockImplementationOnce(() => {
        throw new Error('cleanup failed');
      });
      // Whether the cleanup failure is reported or propagated, it cannot release
      // the quit veto or hand the rejected installer to the OS.
      await updater.safeQuitAndInstall().catch(() => undefined);

      expect(mockDiscardPreparedUpdate).toHaveBeenCalled();
      expect(releaseQuitVeto).not.toHaveBeenCalled();
      expect(mockAutoUpdater.quitAndInstall).not.toHaveBeenCalled();
      await updater.downloadUpdate();
      expect(mockAutoUpdater.downloadUpdate).toHaveBeenCalledOnce();
    });
  });
  // Status-aware tray (tray-status spec §4): main-owned update phase reaches the tray
  // through a callback injected at init, never through the renderer.
  describe('tray update state callback', () => {
    it('reports available → downloading(percent) → ready', async () => {
      const { updater } = await loadModule();
      const onTray = vi.fn();
      updater.initAutoUpdater(() => null, undefined, onTray);

      getEventHandler('update-available')({ version: '2.0.0', releaseNotes: '', releaseDate: '' });
      expect(onTray).toHaveBeenLastCalledWith('available');
      getEventHandler('download-progress')({
        percent: 42.5,
        transferred: 1,
        total: 2,
        bytesPerSecond: 1,
      });
      expect(onTray).toHaveBeenLastCalledWith('downloading', 42.5);
      getEventHandler('update-downloaded')({ version: '2.0.0', downloadedFile: '/tmp/u' });
      expect(onTray).toHaveBeenLastCalledWith('ready');
    });

    it('an error keeps a downloaded update ready, an announced one available, else none', async () => {
      const { updater } = await loadModule();
      const onTray = vi.fn();
      updater.initAutoUpdater(() => null, undefined, onTray);
      const fail = () => getEventHandler('error')(new Error('net down'));

      fail();
      expect(onTray).toHaveBeenLastCalledWith('none');
      getEventHandler('update-available')({ version: '2.0.0', releaseNotes: '', releaseDate: '' });
      fail();
      expect(onTray).toHaveBeenLastCalledWith('available');
      getEventHandler('update-downloaded')({ version: '2.0.0', downloadedFile: '/tmp/u' });
      fail();
      expect(onTray).toHaveBeenLastCalledWith('ready');
    });

    it('update-not-available clears an announced update but never a downloaded one', async () => {
      const { updater } = await loadModule();
      const onTray = vi.fn();
      updater.initAutoUpdater(() => null, undefined, onTray);
      getEventHandler('update-available')({ version: '2.0.0', releaseNotes: '', releaseDate: '' });
      getEventHandler('update-not-available')({ version: '1.0.0' });
      expect(onTray).toHaveBeenLastCalledWith('none');
      getEventHandler('update-downloaded')({ version: '2.0.0', downloadedFile: '/tmp/u' });
      onTray.mockClear();
      getEventHandler('update-not-available')({ version: '1.0.0' });
      expect(onTray).not.toHaveBeenCalled();
    });

    it('a throwing tray callback never breaks the updater event', async () => {
      const { updater, splash } = await loadModule();
      updater.initAutoUpdater(
        () => null,
        undefined,
        vi.fn(() => {
          throw new Error('tray gone');
        })
      );
      expect(() =>
        getEventHandler('update-downloaded')({ version: '2.0.0', downloadedFile: '/tmp/u' })
      ).not.toThrow();
      expect(splash.updateSplashStatus).toHaveBeenCalledWith('Ready for liftoff');
    });

    it('works without a callback (older call sites)', async () => {
      const { updater } = await loadModule();
      updater.initAutoUpdater(() => null);
      expect(() =>
        getEventHandler('update-available')({ version: '2.0.0', releaseNotes: '', releaseDate: '' })
      ).not.toThrow();
    });
  });
});
