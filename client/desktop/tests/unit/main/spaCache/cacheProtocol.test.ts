import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { buildSignedManifest, makeKeypair, type EphemeralKeypair } from './manifestTestUtils';

// One ephemeral keypair for the whole suite; the public-key module mock returns
// THIS pair's public key so the handler verifies against the same key the
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

import {
  resolveCachePath,
  handleCacheProtocolRequest,
  __resetCacheProtocolMemoForTests,
} from '@/main/spaCache/cacheProtocol';
import {
  getLiveDir,
  promoteStagingToLive,
  resetStaging,
  writeStagedFile,
} from '@/main/spaCache/cacheStore';
import { SPA_MANIFEST_FILENAME, SPA_MANIFEST_SIG_FILENAME } from '@/main/spaCache/manifestSchema';

// ─────────────────────────────────────────────────────────────────────────────
// Pure resolver (unchanged behavior — keep these regression-locked).
// ─────────────────────────────────────────────────────────────────────────────
describe('resolveCachePath (#1870, mirrors appProtocol)', () => {
  const LIVE_ROOT = path.resolve(
    '/Users/test/Library/Application Support/ConcordVoice/spa-cache/live'
  );

  it('returns 404 for a malformed URL', () => {
    const result = resolveCachePath('not a url', LIVE_ROOT);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.status).toBe(404);
  });

  it('returns 404 for the wrong host', () => {
    const result = resolveCachePath('spa-cache://other/index.html', LIVE_ROOT);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.status).toBe(404);
  });

  it('resolves spa-cache://concord/index.html to liveRoot/index.html', () => {
    const result = resolveCachePath('spa-cache://concord/index.html', LIVE_ROOT);
    expect(result.ok).toBe(true);
    if (result.ok) expect(result.absolutePath).toBe(path.join(LIVE_ROOT, 'index.html'));
  });

  it('defaults the root path "/" to /index.html', () => {
    const result = resolveCachePath('spa-cache://concord/', LIVE_ROOT);
    expect(result.ok).toBe(true);
    if (result.ok) expect(result.absolutePath).toBe(path.join(LIVE_ROOT, 'index.html'));
  });

  it('resolves a normal asset path inside liveRoot', () => {
    const result = resolveCachePath('spa-cache://concord/assets/app-abc123.js', LIVE_ROOT);
    expect(result.ok).toBe(true);
    if (result.ok)
      expect(result.absolutePath).toBe(path.join(LIVE_ROOT, 'assets', 'app-abc123.js'));
  });

  it('rejects path traversal with 403: spa-cache://concord/../../../etc/passwd', () => {
    const result = resolveCachePath('spa-cache://concord/../../../etc/passwd', LIVE_ROOT);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.status).toBe(403);
  });

  it('rejects nested traversal with 403: spa-cache://concord/foo/../../../bar', () => {
    const result = resolveCachePath('spa-cache://concord/foo/../../../bar', LIVE_ROOT);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.status).toBe(403);
  });

  it('rejects encoded traversal with 403: spa-cache://concord/%2E%2E/foo', () => {
    const result = resolveCachePath('spa-cache://concord/%2E%2E/foo', LIVE_ROOT);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.status).toBe(403);
  });

  it('rejects mixed-case scheme with traversal: SPA-CACHE://concord/../../etc → 403', () => {
    const result = resolveCachePath('SPA-CACHE://concord/../../etc/passwd', LIVE_ROOT);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.status).toBe(403);
  });
});

// ─────────────────────────────────────────────────────────────────────────────
// Serve-time byte re-bind (Finding A2). Build a real signed manifest + matching
// on-disk files in a temp liveDir, then tamper to exercise the rejection paths.
// ─────────────────────────────────────────────────────────────────────────────
const ENTRY_HTML = Buffer.from('<!doctype html><html><body>cache</body></html>');
const ASSET_JS = Buffer.from('export const x = 1;\n');

function installLiveCache(opts?: { entry?: Buffer; asset?: Buffer }): void {
  const entry = opts?.entry ?? ENTRY_HTML;
  const asset = opts?.asset ?? ASSET_JS;
  const { manifestBytes, signatureBase64 } = buildSignedManifest(KP, {
    entry: { path: 'index.html', bytes: entry },
    assets: [{ path: 'assets/app-abc123.js', bytes: asset }],
  });
  resetStaging();
  writeStagedFile(SPA_MANIFEST_FILENAME, manifestBytes);
  writeStagedFile(SPA_MANIFEST_SIG_FILENAME, Buffer.from(signatureBase64, 'utf8'));
  writeStagedFile('index.html', entry);
  writeStagedFile('assets/app-abc123.js', asset);
  promoteStagingToLive();
}

async function bodyBytes(res: Response): Promise<Buffer> {
  return Buffer.from(await res.arrayBuffer());
}

describe('handleCacheProtocolRequest (#1870 serve-time byte re-bind, Finding A2)', () => {
  const getLiveRoot = () => getLiveDir();

  beforeEach(() => {
    userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'spa-cache-protocol-'));
    __resetCacheProtocolMemoForTests();
  });

  afterEach(() => {
    if (userDataDir && fs.existsSync(userDataDir)) {
      fs.rmSync(userDataDir, { recursive: true, force: true });
    }
    __resetCacheProtocolMemoForTests();
  });

  it('serves verified entry bytes with text/html for a good cache', async () => {
    installLiveCache();
    const res = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/index.html'),
      getLiveRoot
    );
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toBe('text/html; charset=utf-8');
    expect(res.headers.get('cache-control')).toBe('no-store');
    expect((await bodyBytes(res)).equals(ENTRY_HTML)).toBe(true);
  });

  it('serves a verified asset with the by-extension content-type (js)', async () => {
    installLiveCache();
    const res = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/assets/app-abc123.js'),
      getLiveRoot
    );
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toBe('text/javascript; charset=utf-8');
    expect((await bodyBytes(res)).equals(ASSET_JS)).toBe(true);
  });

  it('returns 404 when no trustworthy manifest exists (empty live dir)', async () => {
    // userDataDir is fresh and empty — readLiveManifest returns null.
    const res = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/index.html'),
      getLiveRoot
    );
    expect(res.status).toBe(404);
  });

  it('returns 404 for a path NOT enumerated in the signed manifest', async () => {
    installLiveCache();
    // Plant an unlisted file directly into live/ — it must NOT be served.
    fs.writeFileSync(path.join(getLiveDir(), 'rogue.js'), Buffer.from('alert(1)'));
    const res = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/rogue.js'),
      getLiveRoot
    );
    expect(res.status).toBe(404);
  });

  it('returns 403 when an enumerated on-disk file is tampered (bytes changed, manifest+sig intact)', async () => {
    installLiveCache();
    // Overwrite the asset with same-length but different bytes → sha mismatch.
    const tampered = Buffer.from('export const x = 9;\n');
    expect(tampered.length).toBe(ASSET_JS.length);
    fs.writeFileSync(path.join(getLiveDir(), 'assets', 'app-abc123.js'), tampered);
    const res = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/assets/app-abc123.js'),
      getLiveRoot
    );
    expect(res.status).toBe(403);
  });

  it('returns 403 when an enumerated on-disk file is the wrong size', async () => {
    installLiveCache();
    fs.writeFileSync(
      path.join(getLiveDir(), 'assets', 'app-abc123.js'),
      Buffer.from('totally different length content')
    );
    const res = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/assets/app-abc123.js'),
      getLiveRoot
    );
    expect(res.status).toBe(403);
  });

  it('returns 403 when a live file is replaced by a symlink to an out-of-tree target', async () => {
    installLiveCache();
    const outsideDir = fs.mkdtempSync(path.join(os.tmpdir(), 'spa-cache-evil-'));
    const evilTarget = path.join(outsideDir, 'evil.js');
    fs.writeFileSync(evilTarget, Buffer.from('export const x = 666;\n'));
    const assetPath = path.join(getLiveDir(), 'assets', 'app-abc123.js');
    fs.rmSync(assetPath);
    try {
      fs.symlinkSync(evilTarget, assetPath);
    } catch {
      // Some CI sandboxes disallow symlink creation; skip rather than fail.
      fs.rmSync(outsideDir, { recursive: true, force: true });
      return;
    }
    const res = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/assets/app-abc123.js'),
      getLiveRoot
    );
    // The symlink target has a different size/hash than the manifest entry, so
    // the fd-bounded size check + sha256 reject it. The verified bytes are never
    // served — the handler does NOT follow the symlink to an unverified target.
    expect(res.status).toBe(403);
    fs.rmSync(outsideDir, { recursive: true, force: true });
  });

  it('rejects a traversal-shaped request (Request collapses `..`; resulting path is not enumerated → 404)', async () => {
    installLiveCache();
    // The WHATWG `Request` constructor normalizes `../../../etc/passwd` to
    // `/etc/passwd` before the handler sees it, so the raw-`..` 403 path is
    // exercised by the resolveCachePath suite (direct string input). At the
    // handler level the collapsed path stays inside liveRoot but is NOT
    // enumerated in the signed manifest, so the serve-only-listed-files gate
    // rejects it with 404 — a malicious `/etc/passwd` read is impossible either
    // way (it would also fail the on-disk size/hash verify).
    const res = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/../../../etc/passwd'),
      getLiveRoot
    );
    expect(res.status).toBe(404);
  });
});
