import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { buildSignedManifest, makeKeypair, type EphemeralKeypair } from './manifestTestUtils';
import { SPA_CACHE_MAX_STALENESS_MS } from '@/main/spaCache/manifestSchema';

// One ephemeral keypair for the whole suite. The public-key module mock returns
// THIS pair's public key so resolveCachedSpa verifies against the same key the
// fixtures are signed with.
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

vi.mock('@/main/spaCache/spaManifestPublicKey', () => ({
  get SPA_MANIFEST_PUBLIC_KEY_PEM() {
    return KP.publicKeyPem;
  },
  isSpaManifestKeyConfigured: () => true,
}));

vi.mock('@/main/ipcContract', () => ({ IPC_CONTRACT_VERSION: 16 }));

import { resolveCachedSpa } from '@/main/spaCache/resolveCachedSpa';
import {
  getLiveDir,
  getStagingDir,
  promoteStagingToLive,
  writeStagedFile,
  resetStaging,
} from '@/main/spaCache/cacheStore';
import { SPA_MANIFEST_FILENAME, SPA_MANIFEST_SIG_FILENAME } from '@/main/spaCache/manifestSchema';

function installLiveCache(manifestBytes: Buffer, signatureBase64: string): void {
  resetStaging();
  writeStagedFile(SPA_MANIFEST_FILENAME, manifestBytes);
  writeStagedFile(SPA_MANIFEST_SIG_FILENAME, Buffer.from(signatureBase64, 'utf8'));
  writeStagedFile('index.html', Buffer.from('<html></html>'));
  promoteStagingToLive();
}

// Install a cache with a real entry + asset whose bytes are written to disk to
// match the signed manifest (so the serve-time byte re-verify passes), for the
// tamper tests below.
const ENTRY_HTML = Buffer.from('<!doctype html><html><body>cache</body></html>');
const ASSET_JS = Buffer.from('export const x = 1;\n');

function installLiveCacheWithAsset(): void {
  const { manifestBytes, signatureBase64 } = buildSignedManifest(KP, {
    entry: { path: 'index.html', bytes: ENTRY_HTML },
    assets: [{ path: 'assets/app-abc123.js', bytes: ASSET_JS }],
  });
  resetStaging();
  writeStagedFile(SPA_MANIFEST_FILENAME, manifestBytes);
  writeStagedFile(SPA_MANIFEST_SIG_FILENAME, Buffer.from(signatureBase64, 'utf8'));
  writeStagedFile('index.html', ENTRY_HTML);
  writeStagedFile('assets/app-abc123.js', ASSET_JS);
  promoteStagingToLive();
}

beforeEach(() => {
  userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'spa-cache-resolve-'));
});

afterEach(() => {
  if (userDataDir && fs.existsSync(userDataDir)) {
    fs.rmSync(userDataDir, { recursive: true, force: true });
  }
  // Touch getStagingDir/getLiveDir so the imports are not flagged unused by the
  // strict tsconfig (they document the live tree the cache occupies).
  void getStagingDir;
  void getLiveDir;
});

describe('resolveCachedSpa (#1870 serve-time gate)', () => {
  it('returns null when no cache exists', () => {
    expect(resolveCachedSpa()).toBeNull();
  });

  it('returns the spa-cache URL for a valid, fresh, verified cache', () => {
    const { manifestBytes, signatureBase64 } = buildSignedManifest(KP, {
      entry: { path: 'index.html', bytes: Buffer.from('<html></html>') },
    });
    installLiveCache(manifestBytes, signatureBase64);

    const decision = resolveCachedSpa();
    expect(decision).not.toBeNull();
    expect(decision?.url).toBe('spa-cache://concord/index.html');
  });

  it('returns null when the live manifest is tampered (signature fails)', () => {
    const { manifestBytes, signatureBase64 } = buildSignedManifest(KP, {
      entry: { path: 'index.html', bytes: Buffer.from('<html></html>') },
    });
    installLiveCache(manifestBytes, signatureBase64);

    // Tamper the on-disk manifest after install.
    const manifestPath = path.join(getLiveDir(), SPA_MANIFEST_FILENAME);
    const bytes = fs.readFileSync(manifestPath);
    bytes[bytes.length - 2] ^= 0xff;
    fs.writeFileSync(manifestPath, bytes);

    expect(resolveCachedSpa()).toBeNull();
  });

  it('returns null when the cache is stale beyond the freshness window', () => {
    const generatedAt = new Date(Date.now() - SPA_CACHE_MAX_STALENESS_MS - 60_000).toISOString();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(KP, {
      entry: { path: 'index.html', bytes: Buffer.from('<html></html>') },
      generatedAt,
    });
    installLiveCache(manifestBytes, signatureBase64);
    expect(resolveCachedSpa()).toBeNull();
  });

  it('returns null when spaIpcContract exceeds the shell contract', () => {
    const { manifestBytes, signatureBase64 } = buildSignedManifest(KP, {
      entry: { path: 'index.html', bytes: Buffer.from('<html></html>') },
      spaIpcContract: 999,
    });
    installLiveCache(manifestBytes, signatureBase64);
    expect(resolveCachedSpa()).toBeNull();
  });

  // ── Finding A1: bind on-disk bytes to the signed manifest at decision time ──
  it('returns the URL for a valid cache whose entry + asset bytes match the manifest', () => {
    installLiveCacheWithAsset();
    const decision = resolveCachedSpa();
    expect(decision).not.toBeNull();
    expect(decision?.url).toBe('spa-cache://concord/index.html');
  });

  it('returns null when an on-disk ASSET is tampered while manifest+sig stay intact (the fixed gap)', () => {
    installLiveCacheWithAsset();
    // Same length, different bytes → sha256 mismatch; manifest + sig untouched.
    const tampered = Buffer.from('export const x = 9;\n');
    expect(tampered.length).toBe(ASSET_JS.length);
    fs.writeFileSync(path.join(getLiveDir(), 'assets', 'app-abc123.js'), tampered);
    expect(resolveCachedSpa()).toBeNull();
  });

  it('returns null when the entry index.html is tampered while manifest+sig stay intact', () => {
    installLiveCacheWithAsset();
    // Same length as ENTRY_HTML (replace `cache` with `EVIL!`), different bytes →
    // the sha256 (not the size) is what catches it.
    const tampered = Buffer.from('<!doctype html><html><body>EVIL!</body></html>');
    expect(tampered.length).toBe(ENTRY_HTML.length);
    fs.writeFileSync(path.join(getLiveDir(), 'index.html'), tampered);
    expect(resolveCachedSpa()).toBeNull();
  });

  it('returns null when an enumerated asset is missing on disk', () => {
    installLiveCacheWithAsset();
    fs.rmSync(path.join(getLiveDir(), 'assets', 'app-abc123.js'));
    expect(resolveCachedSpa()).toBeNull();
  });
});
