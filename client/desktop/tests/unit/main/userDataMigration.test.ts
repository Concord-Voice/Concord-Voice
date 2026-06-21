// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';

// ── Mocks ────────────────────────────────────────────────────────────────

const mockGetPath = vi.hoisted(() => vi.fn());

vi.mock('electron', () => ({
  app: {
    getPath: mockGetPath,
  },
}));

// ── Helpers ──────────────────────────────────────────────────────────────

function makeTmpDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'userDataMigration-test-'));
}

// ── Tests ──────────────────────────────────────────────────────────────────

describe('userDataMigration', () => {
  let tmpDir: string;
  let parentDir: string;

  beforeEach(() => {
    tmpDir = makeTmpDir();
    parentDir = path.join(tmpDir, 'AppData');
    fs.mkdirSync(parentDir, { recursive: true });

    // app.getPath('userData') returns the PINNED path (<parent>/ConcordVoice)
    mockGetPath.mockImplementation((key: string) => {
      if (key === 'userData') return path.join(parentDir, 'ConcordVoice');
      return tmpDir;
    });

    vi.spyOn(console, 'debug').mockImplementation(() => {});
    vi.spyOn(console, 'warn').mockImplementation(() => {});
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
    return import('../../../src/main/userDataMigration');
  }

  // Write a file with a specific mtime (controls the liveness heuristic).
  function writeWithMtime(dir: string, file: string, contents: string, mtimeMs: number): void {
    fs.mkdirSync(dir, { recursive: true });
    const p = path.join(dir, file);
    fs.writeFileSync(p, contents);
    fs.utimesSync(p, new Date(mtimeMs), new Date(mtimeMs));
  }
  function bakDirs(parent: string, base: string): string[] {
    return fs.readdirSync(parent).filter((e) => e.startsWith(`${base}.bak-`));
  }

  describe('migrateUserData', () => {
    const SPACED = 'Concord Voice';
    const CANON = 'ConcordVoice';

    it('consolidates a lone legacy spaced tree into the pinned dir', async () => {
      const legacy = path.join(parentDir, SPACED);
      fs.mkdirSync(legacy, { recursive: true });
      fs.writeFileSync(path.join(legacy, 'secure-token.dat'), 'tok');

      const { migrateUserData } = await loadModule();
      migrateUserData();

      const target = path.join(parentDir, CANON);
      expect(fs.existsSync(legacy)).toBe(false);
      expect(fs.existsSync(path.join(target, 'secure-token.dat'))).toBe(true);
      expect(console.debug).toHaveBeenCalledWith(expect.stringContaining('Consolidated'));
    });

    it('does nothing when neither dir exists (fresh install)', async () => {
      const { migrateUserData } = await loadModule();
      migrateUserData();
      expect(fs.existsSync(path.join(parentDir, SPACED))).toBe(false);
      expect(fs.existsSync(path.join(parentDir, CANON))).toBe(false);
      expect(console.debug).not.toHaveBeenCalled();
    });

    it('does nothing when only the canonical dir exists (already pinned/consolidated)', async () => {
      const target = path.join(parentDir, CANON);
      fs.mkdirSync(target, { recursive: true });
      fs.writeFileSync(path.join(target, 'secure-token.dat'), 'tok');

      const { migrateUserData } = await loadModule();
      migrateUserData();

      expect(fs.existsSync(path.join(target, 'secure-token.dat'))).toBe(true);
      expect(bakDirs(parentDir, CANON)).toHaveLength(0);
      expect(console.debug).not.toHaveBeenCalled();
    });

    it('BOTH exist, legacy newer -> archives stale canonical, moves legacy in', async () => {
      writeWithMtime(path.join(parentDir, CANON), 'secure-token.dat', 'old', 1_000_000_000_000);
      writeWithMtime(path.join(parentDir, SPACED), 'secure-token.dat', 'new', 2_000_000_000_000);

      const { migrateUserData } = await loadModule();
      migrateUserData();

      const target = path.join(parentDir, CANON);
      // Live (legacy) content now in the pinned dir
      expect(fs.readFileSync(path.join(target, 'secure-token.dat'), 'utf-8')).toBe('new');
      // Stale canonical archived, not deleted
      const baks = bakDirs(parentDir, CANON);
      expect(baks).toHaveLength(1);
      expect(fs.readFileSync(path.join(parentDir, baks[0], 'secure-token.dat'), 'utf-8')).toBe(
        'old'
      );
      // Legacy source consumed
      expect(fs.existsSync(path.join(parentDir, SPACED))).toBe(false);
    });

    it('BOTH exist, canonical newer -> archives stale legacy, keeps canonical', async () => {
      writeWithMtime(path.join(parentDir, CANON), 'secure-token.dat', 'live', 2_000_000_000_000);
      writeWithMtime(path.join(parentDir, SPACED), 'secure-token.dat', 'stale', 1_000_000_000_000);

      const { migrateUserData } = await loadModule();
      migrateUserData();

      const target = path.join(parentDir, CANON);
      expect(fs.readFileSync(path.join(target, 'secure-token.dat'), 'utf-8')).toBe('live');
      const baks = bakDirs(parentDir, SPACED);
      expect(baks).toHaveLength(1);
      expect(fs.readFileSync(path.join(parentDir, baks[0], 'secure-token.dat'), 'utf-8')).toBe(
        'stale'
      );
      expect(fs.existsSync(path.join(parentDir, SPACED))).toBe(false);
    });

    it('skips when the legacy path is a file, not a directory', async () => {
      const legacyPath = path.join(parentDir, SPACED);
      fs.writeFileSync(legacyPath, 'not a dir');
      const { migrateUserData } = await loadModule();
      migrateUserData();
      expect(fs.readFileSync(legacyPath, 'utf-8')).toBe('not a dir');
      expect(console.debug).not.toHaveBeenCalled();
    });

    it('warns and continues when the consolidating rename fails', async () => {
      fs.mkdirSync(path.join(parentDir, SPACED), { recursive: true });
      vi.spyOn(fs, 'renameSync').mockImplementation(() => {
        throw new Error('EACCES: permission denied');
      });
      const { migrateUserData } = await loadModule();
      migrateUserData();
      expect(console.warn).toHaveBeenCalledWith(expect.stringContaining('Failed to consolidate'));
      vi.mocked(fs.renameSync).mockRestore();
    });

    it('is idempotent (second call is a no-op)', async () => {
      const legacy = path.join(parentDir, SPACED);
      fs.mkdirSync(legacy, { recursive: true });
      fs.writeFileSync(path.join(legacy, 'secure-token.dat'), 'tok');
      const { migrateUserData } = await loadModule();
      migrateUserData();
      const target = path.join(parentDir, CANON);
      expect(fs.existsSync(path.join(target, 'secure-token.dat'))).toBe(true);
      migrateUserData(); // only canonical exists now -> no-op
      expect(bakDirs(parentDir, CANON)).toHaveLength(0);
    });

    it('BOTH exist, legacy newer, but archiving the stale canonical fails -> leaves both, warns', async () => {
      writeWithMtime(path.join(parentDir, CANON), 'secure-token.dat', 'old', 1_000_000_000_000);
      writeWithMtime(path.join(parentDir, SPACED), 'secure-token.dat', 'new', 2_000_000_000_000);
      vi.spyOn(fs, 'renameSync').mockImplementation(() => {
        throw new Error('EACCES: archive blocked');
      });

      const { migrateUserData } = await loadModule();
      migrateUserData();

      // Archive failed -> migration bails; both trees remain, nothing moved or archived.
      expect(fs.existsSync(path.join(parentDir, SPACED, 'secure-token.dat'))).toBe(true);
      expect(fs.readFileSync(path.join(parentDir, CANON, 'secure-token.dat'), 'utf-8')).toBe('old');
      expect(bakDirs(parentDir, CANON)).toHaveLength(0);
      expect(console.warn).toHaveBeenCalledWith(expect.stringContaining('Failed to archive'));
      vi.mocked(fs.renameSync).mockRestore();
    });

    it('BOTH exist, legacy newer, archive OK but move fails -> rolls back to keep canonical', async () => {
      writeWithMtime(path.join(parentDir, CANON), 'secure-token.dat', 'canon', 1_000_000_000_000);
      writeWithMtime(path.join(parentDir, SPACED), 'secure-token.dat', 'legacy', 2_000_000_000_000);
      const realRename = fs.renameSync.bind(fs);
      vi.spyOn(fs, 'renameSync').mockImplementation((from, to) => {
        // Only the legacy->target move fails; archive + rollback use the real rename.
        if (String(from).endsWith(SPACED)) throw new Error('EXDEV: move failed');
        realRename(from, to);
      });

      const { migrateUserData } = await loadModule();
      migrateUserData();

      const target = path.join(parentDir, CANON);
      // Canonical restored from the archive — original content intact, dir present.
      expect(fs.existsSync(target)).toBe(true);
      expect(fs.readFileSync(path.join(target, 'secure-token.dat'), 'utf-8')).toBe('canon');
      expect(bakDirs(parentDir, CANON)).toHaveLength(0); // archive was rolled back
      // Legacy untouched (its move failed).
      expect(fs.readFileSync(path.join(parentDir, SPACED, 'secure-token.dat'), 'utf-8')).toBe(
        'legacy'
      );
      expect(console.warn).toHaveBeenCalledWith(expect.stringContaining('restored'));
      vi.mocked(fs.renameSync).mockRestore();
    });

    it('handles a non-Error thrown by the consolidating rename (String(err) fallback)', async () => {
      fs.mkdirSync(path.join(parentDir, SPACED), { recursive: true });
      vi.spyOn(fs, 'renameSync').mockImplementation(() => {
        throw 'disk gone'; // non-Error throw exercises the String(err) branch
      });

      const { migrateUserData } = await loadModule();
      migrateUserData();

      expect(console.warn).toHaveBeenCalledWith(expect.stringContaining('Failed to consolidate'));
      expect(console.warn).toHaveBeenCalledWith(expect.stringContaining('disk gone'));
      vi.mocked(fs.renameSync).mockRestore();
    });
  });

  describe('resolveUserDataParent', () => {
    it('returns the parent directory of userData path', async () => {
      const { resolveUserDataParent } = await loadModule();
      const parent = resolveUserDataParent();
      expect(parent).toBe(parentDir);
    });
  });
});
