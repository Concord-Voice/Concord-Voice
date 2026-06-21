// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';

// ── Hoisted mocks ────────────────────────────────────────────────────────

const mockGetPath = vi.hoisted(() => vi.fn());

vi.mock('electron', () => ({
  app: {
    getPath: mockGetPath,
  },
}));

// ── Helpers ──────────────────────────────────────────────────────────────

function makeTmpDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'updateLogger-test-'));
}

function todayStamp(): string {
  const now = new Date();
  const yyyy = now.getFullYear();
  const mm = String(now.getMonth() + 1).padStart(2, '0');
  const dd = String(now.getDate()).padStart(2, '0');
  return `${yyyy}-${mm}-${dd}`;
}

// ── Tests ────────────────────────────────────────────────────────────────

describe('updateLogger', () => {
  let tmpDir: string;

  beforeEach(() => {
    tmpDir = makeTmpDir();
    mockGetPath.mockImplementation((key: string) => {
      if (key === 'home') return tmpDir;
      return tmpDir;
    });
    vi.spyOn(console, 'log').mockImplementation(() => {});
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    vi.restoreAllMocks();
    // Clean up tmp dir
    try {
      fs.rmSync(tmpDir, { recursive: true, force: true });
    } catch {
      // best effort
    }
  });

  // Reimport for each test to get fresh module state
  async function loadModule() {
    // Clear module cache to get fresh imports
    vi.resetModules();
    return import('../../../src/main/updateLogger');
  }

  describe('resolveLogDir', () => {
    it('resolves macOS log directory', async () => {
      const { resolveLogDir } = await loadModule();
      const dir = resolveLogDir('darwin');
      expect(dir).toBe(path.join(tmpDir, 'Library', 'Logs', 'ConcordVoice'));
    });

    it('resolves Windows log directory with LOCALAPPDATA', async () => {
      const origEnv = process.env.LOCALAPPDATA;
      process.env.LOCALAPPDATA = path.join(tmpDir, 'AppData', 'Local');
      try {
        const { resolveLogDir } = await loadModule();
        const dir = resolveLogDir('win32');
        expect(dir).toBe(path.join(tmpDir, 'AppData', 'Local', 'ConcordVoice', 'logs'));
      } finally {
        if (origEnv === undefined) delete process.env.LOCALAPPDATA;
        else process.env.LOCALAPPDATA = origEnv;
      }
    });

    it('resolves Windows log directory without LOCALAPPDATA (fallback)', async () => {
      const origEnv = process.env.LOCALAPPDATA;
      delete process.env.LOCALAPPDATA;
      try {
        const { resolveLogDir } = await loadModule();
        const dir = resolveLogDir('win32');
        expect(dir).toBe(path.join(tmpDir, 'AppData', 'Local', 'ConcordVoice', 'logs'));
      } finally {
        if (origEnv !== undefined) process.env.LOCALAPPDATA = origEnv;
      }
    });

    it('resolves Linux log directory with XDG_DATA_HOME', async () => {
      const origEnv = process.env.XDG_DATA_HOME;
      process.env.XDG_DATA_HOME = path.join(tmpDir, 'custom-xdg');
      try {
        const { resolveLogDir } = await loadModule();
        const dir = resolveLogDir('linux');
        expect(dir).toBe(path.join(tmpDir, 'custom-xdg', 'ConcordVoice', 'logs'));
      } finally {
        if (origEnv === undefined) delete process.env.XDG_DATA_HOME;
        else process.env.XDG_DATA_HOME = origEnv;
      }
    });

    it('resolves Linux log directory without XDG_DATA_HOME (fallback)', async () => {
      const origEnv = process.env.XDG_DATA_HOME;
      delete process.env.XDG_DATA_HOME;
      try {
        const { resolveLogDir } = await loadModule();
        const dir = resolveLogDir('linux');
        expect(dir).toBe(path.join(tmpDir, '.local', 'share', 'ConcordVoice', 'logs'));
      } finally {
        if (origEnv !== undefined) process.env.XDG_DATA_HOME = origEnv;
      }
    });
  });

  describe('createUpdateLogger', () => {
    // Override resolveLogDir to point at our temp dir by setting platform env
    // We'll use macOS path since it's simplest
    it('creates log directory recursively on first call', async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();
      const logDir = logger.getLogDir();
      expect(fs.existsSync(logDir)).toBe(true);
    });

    it('returns object with all required interface methods', async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();
      expect(typeof logger.info).toBe('function');
      expect(typeof logger.warn).toBe('function');
      expect(typeof logger.error).toBe('function');
      expect(typeof logger.debug).toBe('function');
      expect(typeof logger.flush).toBe('function');
      expect(typeof logger.getLogPath).toBe('function');
      expect(typeof logger.getLogDir).toBe('function');
    });

    it("getLogPath returns path with today's date", async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();
      const logPath = logger.getLogPath();
      expect(logPath).toContain(`update-${todayStamp()}.log`);
    });
  });

  describe('log methods', () => {
    it('info writes formatted line to log file and console', async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();
      logger.info('Test info message');

      const content = fs.readFileSync(logger.getLogPath(), 'utf-8');
      expect(content).toMatch(/^\[.+\] \[INFO\] Test info message\n$/);
      expect(console.log).toHaveBeenCalledWith('[updater] Test info message');
    });

    it('warn writes to log file and console.warn', async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();
      logger.warn('Test warn message');

      const content = fs.readFileSync(logger.getLogPath(), 'utf-8');
      expect(content).toMatch(/\[WARN\] Test warn message/);
      expect(console.warn).toHaveBeenCalledWith('[updater] Test warn message');
    });

    it('error writes to log file and console.error', async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();
      logger.error('Test error message');

      const content = fs.readFileSync(logger.getLogPath(), 'utf-8');
      expect(content).toMatch(/\[ERROR\] Test error message/);
      expect(console.error).toHaveBeenCalledWith('[updater] Test error message');
    });

    it('debug writes to log file and console.log', async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();
      logger.debug('Test debug message');

      const content = fs.readFileSync(logger.getLogPath(), 'utf-8');
      expect(content).toMatch(/\[DEBUG\] Test debug message/);
      expect(console.log).toHaveBeenCalledWith('[updater] Test debug message');
    });

    it('appends multiple log entries to the same file', async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();
      logger.info('Line one');
      logger.warn('Line two');
      logger.error('Line three');

      const content = fs.readFileSync(logger.getLogPath(), 'utf-8');
      const lines = content.trim().split('\n');
      expect(lines).toHaveLength(3);
      expect(lines[0]).toContain('[INFO] Line one');
      expect(lines[1]).toContain('[WARN] Line two');
      expect(lines[2]).toContain('[ERROR] Line three');
    });

    it('includes ISO-8601 timestamp in each line', async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();
      logger.info('timestamp test');

      const content = fs.readFileSync(logger.getLogPath(), 'utf-8');
      // Match ISO 8601: [2026-03-25T14:30:00.000Z]
      expect(content).toMatch(/^\[\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z\]/);
    });

    it('handles write failure gracefully (logs to console.error)', async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();

      // Make the log directory read-only to trigger write failure
      const logFile = logger.getLogPath();

      // Create a directory where the log file should be, making appendFileSync fail
      fs.mkdirSync(logFile, { recursive: true });

      logger.info('This should fail to write but not crash');

      // Should have logged the failure to console.error
      expect(console.error).toHaveBeenCalledWith('[updateLogger] Failed to write to log file');

      // Clean up the directory we created
      fs.rmdirSync(logFile);
    });
  });

  describe('flush', () => {
    it('does not throw (no-op for sync writer)', async () => {
      const { createUpdateLogger } = await loadModule();
      const logger = createUpdateLogger();
      expect(() => logger.flush()).not.toThrow();
    });
  });

  describe('log rotation', () => {
    it('keeps 10 files when exactly 10 exist', async () => {
      const { resolveLogDir, createUpdateLogger } = await loadModule();
      const logDir = resolveLogDir();
      fs.mkdirSync(logDir, { recursive: true });

      // Create 10 log files
      for (let i = 1; i <= 10; i++) {
        const d = String(i).padStart(2, '0');
        fs.writeFileSync(path.join(logDir, `update-2026-01-${d}.log`), 'data');
      }

      // Creating the logger triggers pruning on init
      createUpdateLogger();

      const remaining = fs
        .readdirSync(logDir)
        .filter((f) => f.startsWith('update-') && f.endsWith('.log'));
      expect(remaining.length).toBe(10);
    });

    it('prunes oldest files when more than 10 exist', async () => {
      const { resolveLogDir, createUpdateLogger } = await loadModule();
      const logDir = resolveLogDir();
      fs.mkdirSync(logDir, { recursive: true });

      // Create 12 log files (Jan 01 through Jan 12)
      for (let i = 1; i <= 12; i++) {
        const d = String(i).padStart(2, '0');
        fs.writeFileSync(path.join(logDir, `update-2026-01-${d}.log`), 'data');
      }

      createUpdateLogger();

      const remaining = fs
        .readdirSync(logDir)
        .filter((f) => f.startsWith('update-') && f.endsWith('.log'))
        .sort();
      expect(remaining.length).toBe(10);
      // Oldest 2 (Jan 01, Jan 02) should be deleted
      expect(remaining[0]).toBe('update-2026-01-03.log');
      expect(remaining[9]).toBe('update-2026-01-12.log');
    });

    it('handles zero existing files without error', async () => {
      const { createUpdateLogger } = await loadModule();
      expect(() => createUpdateLogger()).not.toThrow();
    });

    it('does not prune non-log files', async () => {
      const { resolveLogDir, createUpdateLogger } = await loadModule();
      const logDir = resolveLogDir();
      fs.mkdirSync(logDir, { recursive: true });

      // Create 12 log files + 1 non-log file
      for (let i = 1; i <= 12; i++) {
        const d = String(i).padStart(2, '0');
        fs.writeFileSync(path.join(logDir, `update-2026-01-${d}.log`), 'data');
      }
      fs.writeFileSync(path.join(logDir, 'other-file.txt'), 'keep me');

      createUpdateLogger();

      expect(fs.existsSync(path.join(logDir, 'other-file.txt'))).toBe(true);
      const logFiles = fs
        .readdirSync(logDir)
        .filter((f) => f.startsWith('update-') && f.endsWith('.log'));
      expect(logFiles.length).toBe(10);
    });
  });

  describe('directory creation failure', () => {
    it('does not crash when log directory cannot be created', async () => {
      // Make getPath return a path under a file (not a directory) to cause mkdir failure
      const blockingFile = path.join(tmpDir, 'blocking-file');
      fs.writeFileSync(blockingFile, 'block');
      mockGetPath.mockImplementation((key: string) => {
        if (key === 'home') return blockingFile; // This will cause path.join to produce an invalid dir
        return blockingFile;
      });

      const { createUpdateLogger } = await loadModule();
      // Should not throw — falls back to console-only
      expect(() => createUpdateLogger()).not.toThrow();
    });
  });
});
