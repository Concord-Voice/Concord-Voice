/**
 * SPA LKG cache protocol handler — memo HIT reuse + invalidation (Finding E1).
 *
 * The serve handler memoizes the signature-verified manifest keyed by
 * sha256(on-disk manifest bytes): a memo HIT reuses the parsed fileMap WITHOUT
 * re-running the (RSA) verifyManifest, while a change in the on-disk manifest
 * bytes invalidates the memo and forces a re-verify. The sibling
 * `cacheProtocol.test.ts` resets the memo every case, so neither the HIT nor the
 * invalidation branch is exercised across two requests. This file deliberately
 * does NOT reset the memo between the two requests within each test so those
 * branches run.
 *
 * To observe whether verifyManifest re-ran, we mock the module with a spy that
 * delegates to the REAL implementation (isolated to this file so the existing
 * serve-time suite is unaffected). Keys are EPHEMERAL (per the shared signing
 * helpers); no private key is ever committed.
 */

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

// Spy wrapper around the REAL verifyManifest so we can count how many times the
// (RSA) verification ran across two consecutive requests — a memo HIT must not
// re-run it. importOriginal preserves real verification behavior.
const verifyManifestSpy = vi.fn();
vi.mock('@/main/spaCache/verifyManifest', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/main/spaCache/verifyManifest')>();
  return {
    ...actual,
    verifyManifest: (...args: Parameters<typeof actual.verifyManifest>) => {
      verifyManifestSpy(...args);
      return actual.verifyManifest(...args);
    },
  };
});

import {
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

const ENTRY_HTML = Buffer.from('<!doctype html><html><body>cache</body></html>');
const ASSET_JS = Buffer.from('export const x = 1;\n');

/** Install a good signed cache into live/ from the shared ephemeral key. */
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

describe('handleCacheProtocolRequest memo (#1870 Finding E1)', () => {
  const getLiveRoot = () => getLiveDir();

  beforeEach(() => {
    userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'spa-cache-memo-'));
    verifyManifestSpy.mockClear();
    __resetCacheProtocolMemoForTests();
  });

  afterEach(() => {
    if (userDataDir && fs.existsSync(userDataDir)) {
      fs.rmSync(userDataDir, { recursive: true, force: true });
    }
    __resetCacheProtocolMemoForTests();
  });

  it('(a) memo HIT — second request reuses the verified manifest without re-running verifyManifest', async () => {
    installLiveCache();

    // First request: a cold memo, so verifyManifest runs once.
    const first = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/index.html'),
      getLiveRoot
    );
    expect(first.status).toBe(200);
    expect(verifyManifestSpy).toHaveBeenCalledTimes(1);

    // Second request for a DIFFERENT path, WITHOUT resetting the memo. The
    // on-disk manifest bytes are unchanged, so the manifest hash matches the
    // memo key and the parsed fileMap is reused — verifyManifest must NOT run
    // again (still exactly one call total).
    const second = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/assets/app-abc123.js'),
      getLiveRoot
    );
    expect(second.status).toBe(200);
    expect(verifyManifestSpy).toHaveBeenCalledTimes(1);
  });

  it('(b) memo INVALIDATION — a swapped (differently-signed) manifest cannot ride a stale memo entry', async () => {
    installLiveCache();

    // Prime the memo with a good serve.
    const first = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/index.html'),
      getLiveRoot
    );
    expect(first.status).toBe(200);
    expect(verifyManifestSpy).toHaveBeenCalledTimes(1);

    // Overwrite live/spa-manifest.json with DIFFERENT bytes signed by a
    // DIFFERENT ephemeral key. The bytes (and thus their sha256 memo key) change,
    // so the handler recomputes the hash, sees the mismatch, and re-runs
    // verifyManifest — which now FAILS (signed by the wrong key).
    const otherKp = makeKeypair();
    const swapped = buildSignedManifest(otherKp, {
      entry: { path: 'index.html', bytes: ENTRY_HTML },
      assets: [{ path: 'assets/app-abc123.js', bytes: ASSET_JS }],
    });
    fs.writeFileSync(path.join(getLiveDir(), SPA_MANIFEST_FILENAME), swapped.manifestBytes);
    fs.writeFileSync(
      path.join(getLiveDir(), SPA_MANIFEST_SIG_FILENAME),
      Buffer.from(swapped.signatureBase64, 'utf8')
    );

    // WITHOUT resetting the memo: the changed on-disk bytes must invalidate it,
    // re-run verifyManifest, fail verification, and return 404 (no trustworthy
    // manifest) — the stale memo entry is NOT trusted.
    const second = await handleCacheProtocolRequest(
      new Request('spa-cache://concord/index.html'),
      getLiveRoot
    );
    expect(second.status).toBe(404);
    expect(verifyManifestSpy).toHaveBeenCalledTimes(2);
  });
});
