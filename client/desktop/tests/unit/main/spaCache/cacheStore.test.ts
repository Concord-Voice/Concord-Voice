import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// Mock electron so app.getPath('userData') points at a per-test temp dir.
let userDataDir = '';
vi.mock('electron', () => ({
  app: {
    getPath: vi.fn((name: string) => {
      if (name === 'userData') return userDataDir;
      return os.tmpdir();
    }),
  },
}));

import {
  getLiveDir,
  getStagingDir,
  promoteStagingToLive,
  readLiveFile,
  readLiveManifest,
  resetStaging,
  resolveLivePath,
  writeStagedFile,
} from '@/main/spaCache/cacheStore';
import { SPA_MANIFEST_FILENAME, SPA_MANIFEST_SIG_FILENAME } from '@/main/spaCache/manifestSchema';

beforeEach(() => {
  userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'spa-cache-store-'));
});

afterEach(() => {
  if (userDataDir && fs.existsSync(userDataDir)) {
    fs.rmSync(userDataDir, { recursive: true, force: true });
  }
});

describe('cacheStore (#1870)', () => {
  it('readLiveManifest returns null when no cache exists', () => {
    expect(readLiveManifest()).toBeNull();
  });

  it('writeStagedFile writes inside staging and readLiveManifest reads after promote', () => {
    resetStaging();
    writeStagedFile('index.html', Buffer.from('<html></html>'));
    writeStagedFile(SPA_MANIFEST_FILENAME, Buffer.from('{"schemaVersion":1}'));
    writeStagedFile(SPA_MANIFEST_SIG_FILENAME, Buffer.from('c2ln'));
    promoteStagingToLive();

    const live = readLiveManifest();
    expect(live).not.toBeNull();
    if (live) {
      expect(live.manifestBytes.toString('utf8')).toBe('{"schemaVersion":1}');
      expect(live.signatureBase64).toBe('c2ln');
    }
    // index.html present in live.
    expect(fs.existsSync(path.join(getLiveDir(), 'index.html'))).toBe(true);
    // Staging is consumed by the rename.
    expect(fs.existsSync(getStagingDir())).toBe(false);
  });

  it('readLiveManifest returns null when the signature file is empty', () => {
    resetStaging();
    writeStagedFile(SPA_MANIFEST_FILENAME, Buffer.from('{}'));
    writeStagedFile(SPA_MANIFEST_SIG_FILENAME, Buffer.from('   '));
    promoteStagingToLive();
    expect(readLiveManifest()).toBeNull();
  });

  it('promoteStagingToLive atomically replaces an existing live tree', () => {
    // First promote: live has v1.
    resetStaging();
    writeStagedFile('marker.txt', Buffer.from('v1'));
    writeStagedFile(SPA_MANIFEST_FILENAME, Buffer.from('v1-manifest'));
    writeStagedFile(SPA_MANIFEST_SIG_FILENAME, Buffer.from('sig1'));
    promoteStagingToLive();
    expect(fs.readFileSync(path.join(getLiveDir(), 'marker.txt'), 'utf8')).toBe('v1');

    // Second promote: staging has only v2 marker — old v1 tree is removed.
    resetStaging();
    writeStagedFile('marker.txt', Buffer.from('v2'));
    writeStagedFile('only-in-v2.txt', Buffer.from('new'));
    writeStagedFile(SPA_MANIFEST_FILENAME, Buffer.from('v2-manifest'));
    writeStagedFile(SPA_MANIFEST_SIG_FILENAME, Buffer.from('sig2'));
    promoteStagingToLive();

    expect(fs.readFileSync(path.join(getLiveDir(), 'marker.txt'), 'utf8')).toBe('v2');
    expect(fs.existsSync(path.join(getLiveDir(), 'only-in-v2.txt'))).toBe(true);
    const live = readLiveManifest();
    expect(live?.manifestBytes.toString('utf8')).toBe('v2-manifest');
  });

  it('writeStagedFile creates intermediate directories', () => {
    resetStaging();
    writeStagedFile('assets/nested/app.js', Buffer.from('code'));
    expect(fs.existsSync(path.join(getStagingDir(), 'assets', 'nested', 'app.js'))).toBe(true);
  });

  it('writeStagedFile rejects a traversal path (..)', () => {
    resetStaging();
    expect(() => writeStagedFile('../escape.txt', Buffer.from('x'))).toThrow(/traversal|escapes/);
  });

  it('writeStagedFile rejects nested traversal that escapes staging', () => {
    resetStaging();
    expect(() => writeStagedFile('a/../../escape.txt', Buffer.from('x'))).toThrow(
      /traversal|escapes/
    );
  });

  it('writeStagedFile rejects an absolute path', () => {
    resetStaging();
    expect(() => writeStagedFile('/etc/passwd', Buffer.from('x'))).toThrow(/relative/);
  });

  it('writeStagedFile rejects a backslash path', () => {
    resetStaging();
    expect(() => writeStagedFile('a\\b.txt', Buffer.from('x'))).toThrow(/backslash|relative/);
  });

  it('resetStaging clears a previous staging tree', () => {
    resetStaging();
    writeStagedFile('old.txt', Buffer.from('old'));
    expect(fs.existsSync(path.join(getStagingDir(), 'old.txt'))).toBe(true);
    resetStaging();
    expect(fs.existsSync(path.join(getStagingDir(), 'old.txt'))).toBe(false);
    expect(fs.existsSync(getStagingDir())).toBe(true);
  });
});

describe('resolveLivePath (#1870 Finding A — null-on-reject, rooted at liveDir)', () => {
  it('resolves a normal relative path inside liveDir', () => {
    const abs = resolveLivePath('assets/app.js');
    expect(abs).toBe(path.join(getLiveDir(), 'assets', 'app.js'));
  });

  it('returns null for an empty path', () => {
    expect(resolveLivePath('')).toBeNull();
  });

  it('returns null for an absolute path', () => {
    expect(resolveLivePath('/etc/passwd')).toBeNull();
  });

  it('returns null for a drive-letter path', () => {
    expect(resolveLivePath('C:/Windows/system32')).toBeNull();
  });

  it('returns null for a backslash path', () => {
    expect(resolveLivePath('a\\b.txt')).toBeNull();
  });

  it('returns null for a traversal path', () => {
    expect(resolveLivePath('../escape.txt')).toBeNull();
    expect(resolveLivePath('a/../../escape.txt')).toBeNull();
  });

  it('returns null for dot / empty segments', () => {
    expect(resolveLivePath('a/./b')).toBeNull();
    expect(resolveLivePath('a//b')).toBeNull();
  });
});

describe('readLiveFile (#1870 Finding A — fd-bounded, TOCTOU-safe, size-checked)', () => {
  it('reads a file whose size matches the expected size', () => {
    resetStaging();
    writeStagedFile('assets/app.js', Buffer.from('hello world'));
    promoteStagingToLive();
    const bytes = readLiveFile('assets/app.js', 'hello world'.length);
    expect(bytes).not.toBeNull();
    expect(bytes?.toString('utf8')).toBe('hello world');
  });

  it('returns null when the on-disk size differs from the expected size', () => {
    resetStaging();
    writeStagedFile('assets/app.js', Buffer.from('hello world'));
    promoteStagingToLive();
    // Expect a smaller size than what is on disk → reject before reading.
    expect(readLiveFile('assets/app.js', 3)).toBeNull();
  });

  it('returns null when the file is missing', () => {
    resetStaging();
    promoteStagingToLive();
    expect(readLiveFile('nope.js', 5)).toBeNull();
  });

  it('returns null for a directory (not a regular file)', () => {
    resetStaging();
    writeStagedFile('assets/app.js', Buffer.from('x'));
    promoteStagingToLive();
    // `assets` is a directory — isFile() is false.
    expect(readLiveFile('assets', 0)).toBeNull();
  });

  it('returns null for a path that fails path-safety (traversal)', () => {
    resetStaging();
    promoteStagingToLive();
    expect(readLiveFile('../escape.txt', 1)).toBeNull();
  });
});
