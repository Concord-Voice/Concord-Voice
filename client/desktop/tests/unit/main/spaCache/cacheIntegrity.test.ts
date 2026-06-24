import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { buildSignedManifest, makeKeypair, type EphemeralKeypair } from './manifestTestUtils';
import { SpaManifestSchema, type SpaManifest } from '@/main/spaCache/manifestSchema';

const KP: EphemeralKeypair = makeKeypair();

let userDataDir = '';
vi.mock('electron', () => ({
  app: {
    getPath: vi.fn((name: string) => {
      if (name === 'userData') return userDataDir;
      return os.tmpdir();
    }),
  },
}));

import { readVerifiedLiveFile, verifyAllLiveAssets } from '@/main/spaCache/cacheIntegrity';
import {
  getLiveDir,
  promoteStagingToLive,
  resetStaging,
  writeStagedFile,
} from '@/main/spaCache/cacheStore';

const ENTRY_HTML = Buffer.from('<!doctype html><html><body>cache</body></html>');
const ASSET_JS = Buffer.from('export const x = 1;\n');

function buildManifest(): SpaManifest {
  const { manifestObject } = buildSignedManifest(KP, {
    entry: { path: 'index.html', bytes: ENTRY_HTML },
    assets: [{ path: 'assets/app-abc123.js', bytes: ASSET_JS }],
  });
  // Parse through the real schema so the test object is a genuine SpaManifest.
  return SpaManifestSchema.parse(manifestObject);
}

function installFiles(opts?: { entry?: Buffer; asset?: Buffer; skipAsset?: boolean }): void {
  resetStaging();
  writeStagedFile('index.html', opts?.entry ?? ENTRY_HTML);
  if (!opts?.skipAsset) {
    writeStagedFile('assets/app-abc123.js', opts?.asset ?? ASSET_JS);
  }
  promoteStagingToLive();
}

beforeEach(() => {
  userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'spa-cache-integrity-'));
});

afterEach(() => {
  if (userDataDir && fs.existsSync(userDataDir)) {
    fs.rmSync(userDataDir, { recursive: true, force: true });
  }
});

describe('cacheIntegrity (#1870 Finding A)', () => {
  it('readVerifiedLiveFile returns the bytes for a matching file', () => {
    const manifest = buildManifest();
    installFiles();
    const bytes = readVerifiedLiveFile(manifest.entry);
    expect(bytes).not.toBeNull();
    expect(bytes?.equals(ENTRY_HTML)).toBe(true);
  });

  it('readVerifiedLiveFile returns null when the file is missing', () => {
    const manifest = buildManifest();
    installFiles({ skipAsset: true });
    expect(readVerifiedLiveFile(manifest.assets[0]!)).toBeNull();
  });

  it('readVerifiedLiveFile returns null on a sha256 mismatch (same length, diff bytes)', () => {
    const manifest = buildManifest();
    const tampered = Buffer.from('export const x = 9;\n');
    expect(tampered.length).toBe(ASSET_JS.length);
    installFiles({ asset: tampered });
    expect(readVerifiedLiveFile(manifest.assets[0]!)).toBeNull();
  });

  it('readVerifiedLiveFile returns null on a size mismatch', () => {
    const manifest = buildManifest();
    installFiles({ asset: Buffer.from('different length entirely') });
    expect(readVerifiedLiveFile(manifest.assets[0]!)).toBeNull();
  });

  it('verifyAllLiveAssets returns true when entry + every asset match', () => {
    const manifest = buildManifest();
    installFiles();
    expect(verifyAllLiveAssets(manifest)).toBe(true);
  });

  it('verifyAllLiveAssets returns false when the entry is tampered', () => {
    const manifest = buildManifest();
    // Same length as ENTRY_HTML (replace `cache` with `EVIL!`), different bytes →
    // the sha256 (not the size) is what catches it.
    const tampered = Buffer.from('<!doctype html><html><body>EVIL!</body></html>');
    expect(tampered.length).toBe(ENTRY_HTML.length);
    installFiles({ entry: tampered });
    expect(verifyAllLiveAssets(manifest)).toBe(false);
  });

  it('verifyAllLiveAssets returns false when an asset is missing', () => {
    const manifest = buildManifest();
    installFiles({ skipAsset: true });
    expect(verifyAllLiveAssets(manifest)).toBe(false);
  });

  it('uses getLiveDir as the verification root', () => {
    // Sanity: the verified file lives under the live dir.
    const manifest = buildManifest();
    installFiles();
    expect(fs.existsSync(path.join(getLiveDir(), manifest.entry.path))).toBe(true);
  });
});
