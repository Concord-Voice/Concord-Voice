// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  checkApplicationsFolder,
  maybePromptMove,
  type ApplicationsFolderGateDeps,
} from '../../../src/main/applicationsFolderGate';

vi.mock('electron', () => ({
  app: {
    isPackaged: false,
    isInApplicationsFolder: vi.fn(() => true),
    getVersion: vi.fn(() => '1.0.0'),
    getPath: vi.fn(() => '/tmp/concord-user-data'),
    getAppPath: vi.fn(() => '/tmp/concord-app'),
    moveToApplicationsFolder: vi.fn(() => false),
  },
  dialog: { showMessageBoxSync: vi.fn(() => 0) },
  nativeImage: {
    createFromPath: vi.fn(() => ({ isEmpty: vi.fn(() => true) })),
  },
}));

const darwin = 'darwin' as NodeJS.Platform;

describe('applicationsFolderGate', () => {
  let tmpDir: string;
  let userDataDir: string;

  beforeEach(() => {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'applications-folder-gate-'));
    userDataDir = path.join(tmpDir, 'userData');
    fs.mkdirSync(userDataDir, { recursive: true });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    fs.rmSync(tmpDir, { recursive: true, force: true });
  });

  function makeDeps(
    overrides: Partial<ApplicationsFolderGateDeps> = {}
  ): ApplicationsFolderGateDeps {
    return {
      isPackaged: true,
      platform: darwin,
      envSkip: undefined,
      isInApplicationsFolder: vi.fn(() => false),
      getVersion: vi.fn(() => '1.0.0'),
      getUserDataPath: vi.fn(() => userDataDir),
      getIcon: vi.fn(() => undefined),
      showMessageBoxSync: vi.fn(() => 0),
      moveToApplicationsFolder: vi.fn(() => true),
      warn: vi.fn(),
      ...overrides,
    };
  }

  function preferencesPath(): string {
    return path.join(userDataDir, 'install-preferences.json');
  }

  describe('checkApplicationsFolder', () => {
    it('returns in-applications for packaged macOS apps already in Applications', () => {
      expect(
        checkApplicationsFolder({
          isPackaged: true,
          platform: darwin,
          isInApplicationsFolder: true,
        })
      ).toBe('in-applications');
    });

    it('returns not-applicable for dev, non-macOS, and env-skipped launches', () => {
      expect(
        checkApplicationsFolder({
          isPackaged: false,
          platform: darwin,
          isInApplicationsFolder: false,
        })
      ).toBe('not-applicable');
      expect(
        checkApplicationsFolder({
          isPackaged: true,
          platform: 'linux',
          isInApplicationsFolder: false,
        })
      ).toBe('not-applicable');
      expect(
        checkApplicationsFolder({
          isPackaged: true,
          platform: darwin,
          isInApplicationsFolder: false,
          envSkip: '1',
        })
      ).toBe('not-applicable');
    });

    it('returns needs-move for packaged macOS apps outside Applications', () => {
      expect(
        checkApplicationsFolder({
          isPackaged: true,
          platform: darwin,
          isInApplicationsFolder: false,
        })
      ).toBe('needs-move');
    });
  });

  describe('maybePromptMove', () => {
    it('does not call the macOS folder API for non-applicable launches', () => {
      const isInApplicationsFolder = vi.fn(() => {
        throw new Error('macOS-only API should not be called');
      });
      const deps = makeDeps({ isPackaged: false, isInApplicationsFolder });

      const moved = maybePromptMove(deps);

      expect(moved).toBe(false);
      expect(isInApplicationsFolder).not.toHaveBeenCalled();
    });

    it('skips the prompt when the current version is already suppressed', () => {
      fs.writeFileSync(
        preferencesPath(),
        JSON.stringify({ suppressedAt: '2026-06-28T00:00:00.000Z', suppressedForVersion: '1.0.0' })
      );
      const deps = makeDeps();

      const moved = maybePromptMove(deps);

      expect(moved).toBe(false);
      expect(deps.showMessageBoxSync).not.toHaveBeenCalled();
    });

    it('uses the version-bump copy when a previous suppression is stale', () => {
      fs.writeFileSync(
        preferencesPath(),
        JSON.stringify({ suppressedAt: '2026-06-28T00:00:00.000Z', suppressedForVersion: '1.0.0' })
      );
      const deps = makeDeps({ getVersion: vi.fn(() => '1.0.1') });

      maybePromptMove(deps);

      expect(deps.showMessageBoxSync).toHaveBeenCalledWith(
        expect.objectContaining({
          type: 'info',
          buttons: ['Remind Me Later', 'Move to Applications'],
          title: 'Move Concord Voice to Applications',
        })
      );
    });

    it('moves the app when the user accepts the first-launch prompt', () => {
      const deps = makeDeps({ showMessageBoxSync: vi.fn(() => 1) });

      const moved = maybePromptMove(deps);

      expect(moved).toBe(true);
      expect(deps.showMessageBoxSync).toHaveBeenCalledWith(
        expect.objectContaining({
          type: 'question',
          buttons: ['Not Now', 'Move to Applications'],
          defaultId: 1,
          cancelId: 0,
          title: 'Move Concord Voice to Applications?',
        })
      );
      expect(deps.moveToApplicationsFolder).toHaveBeenCalled();
    });

    it('persists suppression when the user dismisses the prompt', () => {
      const deps = makeDeps({ now: () => new Date('2026-06-28T12:00:00.000Z') });

      const moved = maybePromptMove(deps);

      expect(moved).toBe(false);
      expect(JSON.parse(fs.readFileSync(preferencesPath(), 'utf-8'))).toEqual({
        suppressedAt: '2026-06-28T12:00:00.000Z',
        suppressedForVersion: '1.0.0',
      });
    });

    it('keeps launching when suppression persistence fails', () => {
      const deps = makeDeps();
      vi.spyOn(fs, 'writeFileSync').mockImplementation(() => {
        throw new Error('disk full');
      });

      const moved = maybePromptMove(deps);

      expect(moved).toBe(false);
      expect(deps.warn).toHaveBeenCalledWith(
        '[ApplicationsFolderGate] failed to persist suppression: disk full'
      );
      expect(deps.moveToApplicationsFolder).not.toHaveBeenCalled();
    });

    it('allows replacing an existing app that is not running', () => {
      const deps = makeDeps({ showMessageBoxSync: vi.fn(() => 1) });

      maybePromptMove(deps);

      const [{ conflictHandler }] = vi.mocked(deps.moveToApplicationsFolder).mock.calls[0];

      expect(conflictHandler?.('exists')).toBe(true);
    });

    it('blocks move when another copy is running and shows the secondary dialog', () => {
      const deps = makeDeps({ showMessageBoxSync: vi.fn(() => 1) });

      maybePromptMove(deps);
      const [{ conflictHandler }] = vi.mocked(deps.moveToApplicationsFolder).mock.calls[0];

      expect(conflictHandler?.('existsAndRunning')).toBe(false);
      expect(deps.showMessageBoxSync).toHaveBeenLastCalledWith(
        expect.objectContaining({
          type: 'warning',
          message: 'Another copy of Concord Voice is currently running.',
        })
      );
    });
  });
});
