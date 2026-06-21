// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';

// ── Hoisted mocks ────────────────────────────────────────────────────────

const mockGetPath = vi.hoisted(() => vi.fn());
const mockGetVersion = vi.hoisted(() => vi.fn());

vi.mock('electron', () => ({
  app: {
    getPath: mockGetPath,
    getVersion: mockGetVersion,
  },
}));

// ── Helpers ──────────────────────────────────────────────────────────────

function makeTmpDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'updateSafety-test-'));
}

function createMockLogger() {
  return {
    info: vi.fn(),
    warn: vi.fn(),
    error: vi.fn(),
    debug: vi.fn(),
    flush: vi.fn(),
    getLogPath: vi.fn(() => '/tmp/test-logs/update-2026-03-25.log'),
    getLogDir: vi.fn(() => '/tmp/test-logs'),
  };
}

// ── Tests ────────────────────────────────────────────────────────────────

describe('updateSafety', () => {
  let tmpDir: string;
  let userDataDir: string;

  beforeEach(() => {
    tmpDir = makeTmpDir();
    userDataDir = path.join(tmpDir, 'userData');
    fs.mkdirSync(userDataDir, { recursive: true });
    mockGetPath.mockImplementation((key: string) => {
      if (key === 'userData') return userDataDir;
      if (key === 'home') return tmpDir;
      return tmpDir;
    });
    mockGetVersion.mockReturnValue('1.0.0');
  });

  afterEach(() => {
    vi.restoreAllMocks();
    try {
      fs.rmSync(tmpDir, { recursive: true, force: true });
    } catch {
      // best effort
    }
  });

  async function loadModule() {
    vi.resetModules();
    return import('../../../src/main/updateSafety');
  }

  // ────────────────────────────────────────────────────────────────────
  // checkUpdateSentinel
  // ────────────────────────────────────────────────────────────────────

  describe('checkUpdateSentinel', () => {
    it('returns { type: "none" } when no sentinel file exists', async () => {
      const { checkUpdateSentinel } = await loadModule();
      const logger = createMockLogger();
      const result = checkUpdateSentinel(logger);
      expect(result.type).toBe('none');
    });

    it('returns "success" when current version matches toVersion', async () => {
      mockGetVersion.mockReturnValue('2.0.0');
      const sentinel = {
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
        backupPath: '',
        timestamp: Date.now(),
        platform: 'darwin',
      };
      fs.writeFileSync(path.join(userDataDir, 'update-sentinel.json'), JSON.stringify(sentinel));

      const { checkUpdateSentinel } = await loadModule();
      const logger = createMockLogger();
      const result = checkUpdateSentinel(logger);
      expect(result.type).toBe('success');
      expect(result.fromVersion).toBe('1.0.0');
      expect(result.toVersion).toBe('2.0.0');
    });

    it('returns "rollback" when current version matches fromVersion', async () => {
      mockGetVersion.mockReturnValue('1.0.0');
      const sentinel = {
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
        backupPath: '/tmp/backup.AppImage',
        timestamp: Date.now(),
        platform: 'linux',
      };
      fs.writeFileSync(path.join(userDataDir, 'update-sentinel.json'), JSON.stringify(sentinel));

      const { checkUpdateSentinel } = await loadModule();
      const logger = createMockLogger();
      const result = checkUpdateSentinel(logger);
      expect(result.type).toBe('rollback');
      expect(result.fromVersion).toBe('1.0.0');
      expect(result.toVersion).toBe('2.0.0');
      expect(result.backupPath).toBe('/tmp/backup.AppImage');
    });

    it('returns "unknown" and deletes sentinel when version matches neither', async () => {
      mockGetVersion.mockReturnValue('3.0.0');
      const sentinel = {
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
        backupPath: '',
        timestamp: Date.now(),
        platform: 'darwin',
      };
      const sentinelFile = path.join(userDataDir, 'update-sentinel.json');
      fs.writeFileSync(sentinelFile, JSON.stringify(sentinel));

      const { checkUpdateSentinel } = await loadModule();
      const logger = createMockLogger();
      const result = checkUpdateSentinel(logger);
      expect(result.type).toBe('unknown');
      expect(fs.existsSync(sentinelFile)).toBe(false);
      expect(logger.warn).toHaveBeenCalled();
    });

    it('returns "stale" and deletes sentinel when timestamp > 1h old', async () => {
      mockGetVersion.mockReturnValue('2.0.0');
      const sentinel = {
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
        backupPath: '',
        timestamp: Date.now() - 2 * 60 * 60 * 1000, // 2 hours ago
        platform: 'darwin',
      };
      const sentinelFile = path.join(userDataDir, 'update-sentinel.json');
      fs.writeFileSync(sentinelFile, JSON.stringify(sentinel));

      const { checkUpdateSentinel } = await loadModule();
      const logger = createMockLogger();
      const result = checkUpdateSentinel(logger);
      expect(result.type).toBe('stale');
      expect(fs.existsSync(sentinelFile)).toBe(false);
      expect(logger.warn).toHaveBeenCalled();
    });

    it('returns "none" and deletes corrupted sentinel JSON', async () => {
      const sentinelFile = path.join(userDataDir, 'update-sentinel.json');
      fs.writeFileSync(sentinelFile, '{invalid json!!!');

      const { checkUpdateSentinel } = await loadModule();
      const logger = createMockLogger();
      const result = checkUpdateSentinel(logger);
      expect(result.type).toBe('none');
      expect(fs.existsSync(sentinelFile)).toBe(false);
      expect(logger.error).toHaveBeenCalledWith(expect.stringContaining('Corrupted'));
    });
  });

  // ────────────────────────────────────────────────────────────────────
  // prepareForUpdate
  // ────────────────────────────────────────────────────────────────────

  describe('prepareForUpdate', () => {
    it('writes sentinel on macOS (no backup)', async () => {
      const origPlatform = process.platform;
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      try {
        const { prepareForUpdate } = await loadModule();
        const logger = createMockLogger();
        const result = await prepareForUpdate('1.0.0', '2.0.0', logger);
        expect(result).toBe(true);

        const sentinel = JSON.parse(
          fs.readFileSync(path.join(userDataDir, 'update-sentinel.json'), 'utf-8')
        );
        expect(sentinel.fromVersion).toBe('1.0.0');
        expect(sentinel.toVersion).toBe('2.0.0');
        expect(sentinel.backupPath).toBe('');
        expect(sentinel.platform).toBe('darwin');
      } finally {
        Object.defineProperty(process, 'platform', { value: origPlatform, configurable: true });
      }
    });

    it('writes sentinel on Windows (no backup)', async () => {
      const origPlatform = process.platform;
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
      try {
        const { prepareForUpdate } = await loadModule();
        const logger = createMockLogger();
        const result = await prepareForUpdate('1.0.0', '2.0.0', logger);
        expect(result).toBe(true);

        const sentinel = JSON.parse(
          fs.readFileSync(path.join(userDataDir, 'update-sentinel.json'), 'utf-8')
        );
        expect(sentinel.platform).toBe('win32');
        expect(sentinel.backupPath).toBe('');
      } finally {
        Object.defineProperty(process, 'platform', { value: origPlatform, configurable: true });
      }
    });

    it('backs up AppImage on Linux when APPIMAGE env is set', async () => {
      const origPlatform = process.platform;
      const origAppImage = process.env.APPIMAGE;
      Object.defineProperty(process, 'platform', { value: 'linux', configurable: true });

      const fakeAppImage = path.join(tmpDir, 'ConcordVoice.AppImage');
      fs.writeFileSync(fakeAppImage, 'fake-appimage-binary');
      process.env.APPIMAGE = fakeAppImage;

      try {
        const { prepareForUpdate } = await loadModule();
        const logger = createMockLogger();
        const result = await prepareForUpdate('1.0.0', '2.0.0', logger);
        expect(result).toBe(true);

        // Backup should exist
        expect(fs.existsSync(`${fakeAppImage}.backup`)).toBe(true);
        expect(fs.readFileSync(`${fakeAppImage}.backup`, 'utf-8')).toBe('fake-appimage-binary');

        // Sentinel should reference the backup
        const sentinel = JSON.parse(
          fs.readFileSync(path.join(userDataDir, 'update-sentinel.json'), 'utf-8')
        );
        expect(sentinel.backupPath).toBe(`${fakeAppImage}.backup`);
      } finally {
        Object.defineProperty(process, 'platform', { value: origPlatform, configurable: true });
        if (origAppImage === undefined) delete process.env.APPIMAGE;
        else process.env.APPIMAGE = origAppImage;
      }
    });

    it('returns false when AppImage backup fails (e.g. ENOSPC)', async () => {
      const origPlatform = process.platform;
      const origAppImage = process.env.APPIMAGE;
      Object.defineProperty(process, 'platform', { value: 'linux', configurable: true });
      // Point to a non-existent file to make copyFileSync fail
      process.env.APPIMAGE = path.join(tmpDir, 'nonexistent.AppImage');

      try {
        const { prepareForUpdate } = await loadModule();
        const logger = createMockLogger();
        const result = await prepareForUpdate('1.0.0', '2.0.0', logger);
        expect(result).toBe(false);
        expect(logger.error).toHaveBeenCalledWith(
          expect.stringContaining('Failed to back up AppImage')
        );

        // No sentinel should have been written
        expect(fs.existsSync(path.join(userDataDir, 'update-sentinel.json'))).toBe(false);
      } finally {
        Object.defineProperty(process, 'platform', { value: origPlatform, configurable: true });
        if (origAppImage === undefined) delete process.env.APPIMAGE;
        else process.env.APPIMAGE = origAppImage;
      }
    });

    it('writes sentinel on Linux without APPIMAGE env (non-AppImage format)', async () => {
      const origPlatform = process.platform;
      const origAppImage = process.env.APPIMAGE;
      Object.defineProperty(process, 'platform', { value: 'linux', configurable: true });
      delete process.env.APPIMAGE;

      try {
        const { prepareForUpdate } = await loadModule();
        const logger = createMockLogger();
        const result = await prepareForUpdate('1.0.0', '2.0.0', logger);
        expect(result).toBe(true);

        const sentinel = JSON.parse(
          fs.readFileSync(path.join(userDataDir, 'update-sentinel.json'), 'utf-8')
        );
        expect(sentinel.backupPath).toBe('');
      } finally {
        Object.defineProperty(process, 'platform', { value: origPlatform, configurable: true });
        if (origAppImage !== undefined) process.env.APPIMAGE = origAppImage;
      }
    });

    it('returns false when sentinel write fails', async () => {
      // Make userData a file to cause writeFileSync failure
      fs.rmSync(userDataDir, { recursive: true, force: true });
      fs.writeFileSync(userDataDir, 'block');

      const { prepareForUpdate } = await loadModule();
      const logger = createMockLogger();
      const result = await prepareForUpdate('1.0.0', '2.0.0', logger);
      expect(result).toBe(false);
      expect(logger.error).toHaveBeenCalledWith(
        expect.stringContaining('Failed to write update sentinel')
      );

      // Restore for cleanup
      fs.unlinkSync(userDataDir);
      fs.mkdirSync(userDataDir, { recursive: true });
    });
  });

  // ────────────────────────────────────────────────────────────────────
  // finalizeUpdate
  // ────────────────────────────────────────────────────────────────────

  describe('finalizeUpdate', () => {
    it('deletes sentinel file', async () => {
      const sentinelFile = path.join(userDataDir, 'update-sentinel.json');
      fs.writeFileSync(sentinelFile, '{}');

      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(path.join(tmpDir, 'logs'));

      await finalizeUpdate(logger, {
        type: 'success',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      expect(fs.existsSync(sentinelFile)).toBe(false);
    });

    it('deletes backup file (Linux)', async () => {
      const backupFile = path.join(tmpDir, 'app.AppImage.backup');
      fs.writeFileSync(backupFile, 'backup');
      const sentinelFile = path.join(userDataDir, 'update-sentinel.json');
      fs.writeFileSync(sentinelFile, '{}');

      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(path.join(tmpDir, 'logs'));

      await finalizeUpdate(logger, {
        type: 'success',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
        backupPath: backupFile,
      });

      expect(fs.existsSync(backupFile)).toBe(false);
    });

    it('handles already-cleaned backup gracefully', async () => {
      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(path.join(tmpDir, 'logs'));

      // Should not throw when backup doesn't exist
      await expect(
        finalizeUpdate(logger, {
          type: 'success',
          fromVersion: '1.0.0',
          toVersion: '2.0.0',
          backupPath: '/tmp/nonexistent.backup',
        })
      ).resolves.not.toThrow();
    });

    it('purges electron-updater cache directory', async () => {
      const cacheDir = path.join(userDataDir, 'concord-voice-updater');
      fs.mkdirSync(cacheDir, { recursive: true });
      fs.writeFileSync(path.join(cacheDir, 'downloaded-update.exe'), 'binary');
      fs.writeFileSync(path.join(cacheDir, 'manifest.yml'), 'yaml');

      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(path.join(tmpDir, 'logs'));

      await finalizeUpdate(logger, {
        type: 'success',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      // Cache dir should be empty (directory itself may still exist)
      const remaining = fs.readdirSync(cacheDir);
      expect(remaining.length).toBe(0);
    });

    it('prunes empty log files', async () => {
      const logDir = path.join(tmpDir, 'logs');
      fs.mkdirSync(logDir, { recursive: true });
      fs.writeFileSync(path.join(logDir, 'update-2026-03-20.log'), ''); // empty
      fs.writeFileSync(
        path.join(logDir, 'update-2026-03-21.log'),
        '[2026-03-21T10:00:00.000Z] [ERROR] Something broke\n'
      ); // substantive

      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(logDir);

      await finalizeUpdate(logger, {
        type: 'success',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      // Empty file pruned, error file preserved
      expect(fs.existsSync(path.join(logDir, 'update-2026-03-20.log'))).toBe(false);
      expect(fs.existsSync(path.join(logDir, 'update-2026-03-21.log'))).toBe(true);
    });

    it('prunes trivial log files (routine quit only)', async () => {
      const logDir = path.join(tmpDir, 'logs');
      fs.mkdirSync(logDir, { recursive: true });
      fs.writeFileSync(
        path.join(logDir, 'update-2026-03-20.log'),
        '[2026-03-20T10:00:00.000Z] [INFO] Application quitting\n'
      );

      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(logDir);

      await finalizeUpdate(logger, {
        type: 'success',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      expect(fs.existsSync(path.join(logDir, 'update-2026-03-20.log'))).toBe(false);
    });

    it('preserves log files with warnings', async () => {
      const logDir = path.join(tmpDir, 'logs');
      fs.mkdirSync(logDir, { recursive: true });
      fs.writeFileSync(
        path.join(logDir, 'update-2026-03-20.log'),
        '[2026-03-20T10:00:00.000Z] [WARN] Something suspicious\n'
      );

      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(logDir);

      await finalizeUpdate(logger, {
        type: 'success',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      expect(fs.existsSync(path.join(logDir, 'update-2026-03-20.log'))).toBe(true);
    });

    it('preserves short update audit logs (not just quit messages)', async () => {
      const logDir = path.join(tmpDir, 'logs');
      fs.mkdirSync(logDir, { recursive: true });
      fs.writeFileSync(
        path.join(logDir, 'update-2026-03-20.log'),
        '[2026-03-20T10:00:00.000Z] [INFO] Update available: v1.0.0 → v2.0.0\n'
      );

      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(logDir);

      await finalizeUpdate(logger, {
        type: 'success',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      // Short but substantive — must be preserved for troubleshooting
      expect(fs.existsSync(path.join(logDir, 'update-2026-03-20.log'))).toBe(true);
    });

    it('preserves log files with many lines', async () => {
      const logDir = path.join(tmpDir, 'logs');
      fs.mkdirSync(logDir, { recursive: true });
      const lines = Array.from(
        { length: 5 },
        (_, i) => `[2026-03-20T10:0${i}:00.000Z] [INFO] Step ${i + 1}\n`
      ).join('');
      fs.writeFileSync(path.join(logDir, 'update-2026-03-20.log'), lines);

      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(logDir);

      await finalizeUpdate(logger, {
        type: 'success',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      expect(fs.existsSync(path.join(logDir, 'update-2026-03-20.log'))).toBe(true);
    });

    it('removes cleanup manifest when done', async () => {
      const cleanupFile = path.join(userDataDir, 'update-cleanup.json');
      fs.writeFileSync(cleanupFile, '{}');

      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(path.join(tmpDir, 'logs'));

      await finalizeUpdate(logger, {
        type: 'success',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      expect(fs.existsSync(cleanupFile)).toBe(false);
    });

    it('removes stale electron-updater metadata files', async () => {
      fs.writeFileSync(path.join(userDataDir, 'pending-update-v2.json'), '{}');
      fs.writeFileSync(path.join(userDataDir, 'update-info.json'), '{}');

      const { finalizeUpdate } = await loadModule();
      const logger = createMockLogger();
      logger.getLogDir.mockReturnValue(path.join(tmpDir, 'logs'));

      await finalizeUpdate(logger, {
        type: 'success',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      expect(fs.existsSync(path.join(userDataDir, 'pending-update-v2.json'))).toBe(false);
      expect(fs.existsSync(path.join(userDataDir, 'update-info.json'))).toBe(false);
    });
  });

  // ────────────────────────────────────────────────────────────────────
  // finalizeRollback
  // ────────────────────────────────────────────────────────────────────

  describe('finalizeRollback', () => {
    it('deletes sentinel file', async () => {
      const sentinelFile = path.join(userDataDir, 'update-sentinel.json');
      fs.writeFileSync(sentinelFile, '{}');

      const { finalizeRollback } = await loadModule();
      const logger = createMockLogger();

      await finalizeRollback(logger, {
        type: 'rollback',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      expect(fs.existsSync(sentinelFile)).toBe(false);
    });

    it('purges download cache', async () => {
      const cacheDir = path.join(userDataDir, 'concord-voice-updater');
      fs.mkdirSync(cacheDir, { recursive: true });
      fs.writeFileSync(path.join(cacheDir, 'bad-update.exe'), 'bad');

      const { finalizeRollback } = await loadModule();
      const logger = createMockLogger();

      await finalizeRollback(logger, {
        type: 'rollback',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      expect(fs.readdirSync(cacheDir).length).toBe(0);
    });

    it('does NOT delete any log files', async () => {
      const logDir = path.join(tmpDir, 'logs');
      fs.mkdirSync(logDir, { recursive: true });
      fs.writeFileSync(path.join(logDir, 'update-2026-03-20.log'), ''); // empty
      fs.writeFileSync(
        path.join(logDir, 'update-2026-03-21.log'),
        '[2026-03-21T10:00:00.000Z] [INFO] Some log\n'
      );

      const { finalizeRollback } = await loadModule();
      const logger = createMockLogger();

      await finalizeRollback(logger, {
        type: 'rollback',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
      });

      // Both files should still exist
      expect(fs.existsSync(path.join(logDir, 'update-2026-03-20.log'))).toBe(true);
      expect(fs.existsSync(path.join(logDir, 'update-2026-03-21.log'))).toBe(true);
    });

    it('does NOT delete backup file', async () => {
      const backupFile = path.join(tmpDir, 'app.AppImage.backup');
      fs.writeFileSync(backupFile, 'backup');

      const { finalizeRollback } = await loadModule();
      const logger = createMockLogger();

      await finalizeRollback(logger, {
        type: 'rollback',
        fromVersion: '1.0.0',
        toVersion: '2.0.0',
        backupPath: backupFile,
      });

      expect(fs.existsSync(backupFile)).toBe(true);
    });
  });

  // ────────────────────────────────────────────────────────────────────
  // runDeferredCleanup
  // ────────────────────────────────────────────────────────────────────

  describe('runDeferredCleanup', () => {
    it('does nothing when no cleanup file exists', async () => {
      const { runDeferredCleanup } = await loadModule();
      const logger = createMockLogger();
      await expect(runDeferredCleanup(logger)).resolves.not.toThrow();
      expect(logger.info).not.toHaveBeenCalled();
    });

    it('performs cleanup when cleanup file is stale (>1h)', async () => {
      const backupFile = path.join(tmpDir, 'old.backup');
      fs.writeFileSync(backupFile, 'old-backup');

      const cacheDir = path.join(userDataDir, 'concord-voice-updater');
      fs.mkdirSync(cacheDir, { recursive: true });
      fs.writeFileSync(path.join(cacheDir, 'stale.exe'), 'stale');

      const manifest = {
        backupPath: backupFile,
        cacheDirPath: cacheDir,
        logDir: path.join(tmpDir, 'logs'),
        timestamp: Date.now() - 2 * 60 * 60 * 1000, // 2 hours ago
      };
      fs.writeFileSync(path.join(userDataDir, 'update-cleanup.json'), JSON.stringify(manifest));

      const { runDeferredCleanup } = await loadModule();
      const logger = createMockLogger();
      await runDeferredCleanup(logger);

      expect(fs.existsSync(backupFile)).toBe(false);
      expect(fs.readdirSync(cacheDir).length).toBe(0);
      expect(fs.existsSync(path.join(userDataDir, 'update-cleanup.json'))).toBe(false);
      expect(logger.info).toHaveBeenCalledWith(
        expect.stringContaining('Deferred cleanup complete')
      );
    });

    it('leaves fresh cleanup file alone (<1h)', async () => {
      const manifest = {
        backupPath: '',
        cacheDirPath: '',
        logDir: '',
        timestamp: Date.now() - 5 * 60 * 1000, // 5 minutes ago
      };
      const cleanupFile = path.join(userDataDir, 'update-cleanup.json');
      fs.writeFileSync(cleanupFile, JSON.stringify(manifest));

      const { runDeferredCleanup } = await loadModule();
      const logger = createMockLogger();
      await runDeferredCleanup(logger);

      // File should still exist — not acted on
      expect(fs.existsSync(cleanupFile)).toBe(true);
      expect(logger.info).not.toHaveBeenCalled();
    });

    it('treats corrupted cleanup file as no-op', async () => {
      const cleanupFile = path.join(userDataDir, 'update-cleanup.json');
      fs.writeFileSync(cleanupFile, '{broken json!!!');

      const { runDeferredCleanup } = await loadModule();
      const logger = createMockLogger();
      await runDeferredCleanup(logger);

      // safeReadJson returns null for corrupted JSON → no-op (no crash)
      expect(logger.info).not.toHaveBeenCalled();
    });
  });
});
